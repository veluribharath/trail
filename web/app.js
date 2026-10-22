"use strict";

const PROVIDERS = { claude: "Claude Code", codex: "Codex", opencode: "OpenCode", gemini: "Gemini CLI", kiro: "Kiro CLI" };
const PER_FOLDER = 6;
const WRITE_PREVIEW = 400;
const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
// A page written by trail -export carries one session and has no server behind it.
const EXPORTED = (() => { const el = document.getElementById("trail-export"); return el ? JSON.parse(el.textContent) : null; })();

const state = {
  data: null, // /api/sessions response
  sig: "",
  hidden: new Set(), // providers toggled off
  q: "",
  expanded: new Map(), // folder path -> bool
  showAll: new Set(), // folder paths showing every session
  current: null, // selected session key
  transcript: null,
  diffs: [],
  stats: null,
  fileFilter: null,
  tab: "files", // files | commits
  filter: "all", // all | talk | changes
  commits: null,
  pane: "log",
  find: null, // { q, key, events, i }: transcript search matches in the open session
  pendingJump: null, // { event, field } to show once a session opened from search renders
  notes: {}, // session key -> { bookmarked, note, steps: [{ event, label, note }] }
  onlyMarked: false, // sidebar shows only sessions with notes or bookmarks
};

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const h = (html) => { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstElementChild; };
const plural = (n, one, many = one + "s") => `${n} ${n === 1 ? one : many}`;

async function api(path) {
  const r = await fetch(path);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `Request failed (${r.status})`);
  return body;
}

/* ---------------- tokens and cost ---------------- */
// Costs are what the tokens would cost at API list prices. Someone on a
// subscription pays a flat fee instead, so they are labelled API-equivalent,
// and the person can show token counts instead.
const usagePref = { tokens: (() => { try { return localStorage.getItem("trail.usage") === "tokens"; } catch { return false; } })() };
function setUsagePref(tokens) {
  usagePref.tokens = tokens;
  try { localStorage.setItem("trail.usage", tokens ? "tokens" : "cost"); } catch {}
  if (!EXPORTED) renderSidebar();
  if (state.transcript && state.current) renderSession();
  if (!EXPORTED && onStats() && dash.data) renderStats();
}
function fmtTokens(n) {
  n = n || 0;
  if (n < 1000) return String(n);
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0).replace(/\.0$/, "") + "K";
  if (n < 1e9) return (n / 1e6).toFixed(n < 1e7 ? 1 : 0).replace(/\.0$/, "") + "M";
  return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
}
function fmtCost(c) {
  if (!(c > 0)) return "$0";
  if (c < 0.01) return "<$0.01";
  if (c < 100) return "$" + c.toFixed(2);
  return "$" + Math.round(c).toLocaleString();
}
const promptTokens = (u) => (u.input || 0) + (u.cacheRead || 0) + (u.cacheWrite || 0) + (u.cacheWrite1h || 0);
const totalTokens = (u) => promptTokens(u) + (u.output || 0);
// hitRate is the share of the prompt read from cache, as a label.
function hitRate(u) {
  const p = promptTokens(u), r = p ? (100 * (u.cacheRead || 0)) / p : 0;
  return r > 0 && r < 1 ? "<1%" : Math.round(r) + "%";
}
// costLabel is the short figure for a usage: its cost, "≥" when some
// responses had no price, or "—" when none did.
function costLabel(u) {
  if (u.unpriced && !(u.cost > 0)) return "—";
  return (u.unpriced ? "≥ " : "") + fmtCost(u.cost);
}
function usageShort(u) { return usagePref.tokens ? fmtTokens(totalTokens(u)) + " tok" : costLabel(u); }
function unpricedNote(u) {
  if (!u.unpriced) return "";
  const models = (u.unpricedModels || []).join(", ") || "its model";
  return `${plural(u.unpriced, "response")} not priced: no price for ${models}. Add one with a prices.json override (see trail -prices).`;
}
// usageLines is the full breakdown, for tooltips.
function usageLines(u, step) {
  const prompt = promptTokens(u);
  const lines = [
    `${step ? "Prompt" : "Read"}: ${prompt.toLocaleString()} tokens`,
    `  fresh input ${(u.input || 0).toLocaleString()}`,
  ];
  if (u.cacheRead) lines.push(`  cache hits ${u.cacheRead.toLocaleString()}${prompt ? ` (${hitRate(u)})` : ""}`);
  if (u.cacheWrite) lines.push(`  cache writes ${u.cacheWrite.toLocaleString()}${u.cacheWrite1h ? " (5 min)" : ""}`);
  if (u.cacheWrite1h) lines.push(`  cache writes ${u.cacheWrite1h.toLocaleString()} (1 hour)`);
  lines.push(`Output: ${(u.output || 0).toLocaleString()} tokens${u.reasoning ? `, ${u.reasoning.toLocaleString()} of them thinking` : ""}`);
  if (!step && u.context) lines.push(`Largest prompt: ${u.context.toLocaleString()} tokens`);
  if (!(u.unpriced && !(u.cost > 0))) lines.push(`API-equivalent cost: ${u.unpriced ? "at least " : ""}${u.cost < 0.01 && u.cost > 0 ? "$" + u.cost.toFixed(4) : fmtCost(u.cost)}`);
  if (u.unpriced) lines.push(unpricedNote(u));
  return lines.join("\n");
}

/* ---------------- notes and bookmarks ---------------- */
// The person can bookmark a session or any of its steps and write a note on
// either. trail keeps them in its own notes file, never in agent storage.
// Edits show at once and are saved a moment later; a refresh of the session
// list leaves alone anything not saved yet.
const notesSave = { timers: new Map(), inflight: new Map(), chain: Promise.resolve() };
const notesOf = (key) => state.notes[key] || null;
const hasNotes = (n) => !!n && !!(n.bookmarked || n.note || n.steps?.length);
const stepMark = (key, i) => notesOf(key)?.steps?.find((m) => m.event === i) || null;
const notesDirty = (key) => notesSave.timers.has(key) || (notesSave.inflight.get(key) || 0) > 0;
const marksShape = (n) => (n ? `${!!n.bookmarked}${!!n.note}${(n.steps || []).map((m) => m.event + (m.note ? "n" : "")).join(",")}` : "");

// stepLabel is one line of a step, kept with its bookmark so a list of
// bookmarks reads without opening the session.
function stepLabel(e) {
  if (!e) return "";
  const text = e.kind === "tool" ? `${e.tool}${e.summary ? ": " + e.summary : ""}` : e.kind === "thinking" ? "Thinking" : e.text || "";
  const line = text.replace(/\s+/g, " ").trim();
  return line.length > 200 ? line.slice(0, 199) + "…" : line;
}

// updateNotes changes one session's notes in place and schedules the save.
// Typing waits for a pause; clicks save at once.
function updateNotes(key, fn, delay = 0) {
  const before = notesOf(key);
  const n = before ? structuredClone(before) : { bookmarked: false, note: "", steps: [] };
  n.steps ||= [];
  fn(n);
  n.steps.sort((a, b) => a.event - b.event);
  n.updated = new Date().toISOString();
  if (hasNotes(n)) state.notes[key] = n; else delete state.notes[key];
  if (marksShape(before) !== marksShape(notesOf(key))) { renderSidebar(); trace.draw(); }
  clearTimeout(notesSave.timers.get(key));
  notesSave.timers.set(key, setTimeout(() => { notesSave.timers.delete(key); pushNotes(key); }, delay));
}

function pushNotes(key) {
  const n = notesOf(key) || {};
  const body = JSON.stringify({ bookmarked: !!n.bookmarked, note: n.note || "", steps: (n.steps || []).map(({ event, label, note }) => ({ event, label, note })) });
  notesSave.inflight.set(key, (notesSave.inflight.get(key) || 0) + 1);
  notesSave.chain = notesSave.chain.then(async () => {
    try {
      const r = await fetch("/api/notes?id=" + encodeURIComponent(key), { method: "PUT", headers: { "Content-Type": "application/json" }, body });
      const b = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(b.error || `Request failed (${r.status})`);
      if (state.current === key) noteStatus(n.note || n.steps?.some((m) => m.note) ? "Saved" : "");
    } catch (e) {
      if (state.current === key) noteStatus(`Not saved: ${e.message}`, true);
    } finally {
      notesSave.inflight.set(key, notesSave.inflight.get(key) - 1);
    }
  });
}

function noteStatus(msg, bad) {
  const el = $(".s-notes .note-status");
  if (!el) return;
  el.textContent = msg;
  el.classList.toggle("bad", !!bad);
}

// growArea sizes a note's text box to what's in it.
function growArea(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight + 2, 360) + "px";
}

function toggleBookmark() {
  const key = state.current;
  if (!key || EXPORTED || !state.transcript) return;
  updateNotes(key, (n) => { n.bookmarked = !n.bookmarked; });
  const b = $("#bookmark");
  if (b) setBookmarkButton(b, !!notesOf(key)?.bookmarked);
}

function setBookmarkButton(b, on) {
  b.setAttribute("aria-pressed", on);
  b.classList.toggle("on", on);
  $("span", b).textContent = on ? "Bookmarked" : "Bookmark";
}

// sessionNotes is the session's own note, under its header: a text box in
// trail, read-only text in an export.
function sessionNotes() {
  const t = state.transcript, key = t.session.key, n = notesOf(key);
  const el = h(`<section class="s-notes" aria-label="Notes"></section>`);
  if (EXPORTED) {
    if (n?.note) el.append(h(`<div class="note-read">${svg("note")}<div class="text">${richText(n.note)}</div></div>`));
    return el;
  }
  const open = () => {
    el.innerHTML = "";
    const ta = h(`<textarea class="note-in" rows="2" placeholder="Notes on this session. They stay on this machine, in trail’s own notes file." aria-label="Notes on this session"></textarea>`);
    ta.value = notesOf(key)?.note || "";
    ta.addEventListener("input", () => { growArea(ta); noteStatus("Saving…"); updateNotes(key, (x) => { x.note = ta.value; }, 600); });
    ta.addEventListener("keydown", (e) => { if (e.key === "Escape") ta.blur(); });
    el.append(ta, h(`<span class="note-status" aria-live="polite"></span>`));
    requestAnimationFrame(() => growArea(ta));
    return ta;
  };
  if (n?.note) open();
  else {
    const b = h(`<button class="linkish add-note">${svg("note")}Add a note</button>`);
    b.onclick = () => open().focus();
    el.append(b);
  }
  return el;
}

// renderMark draws a step's bookmark: the toggle on the row, and the step's
// note under it once marked.
function renderMark(row, i) {
  const key = state.transcript.session.key, m = stepMark(key, i);
  row.classList.toggle("marked", !!m);
  $(".step-note", row)?.remove();
  let btn = $(".mark-btn", row);
  if (!EXPORTED && !btn) {
    btn = h(`<button class="mark-btn" title="Bookmark this step">${svg("star")}</button>`);
    btn.onclick = (ev) => { ev.stopPropagation(); toggleStep(i); };
    row.append(btn);
  }
  if (btn) {
    btn.setAttribute("aria-pressed", !!m);
    btn.setAttribute("aria-label", m ? "Remove this step’s bookmark" : "Bookmark this step");
    btn.title = m ? "Remove bookmark" : "Bookmark this step";
  }
  if (!m) return;
  const body = $(".body", row);
  if (EXPORTED) {
    body.append(h(`<div class="step-note">${svg("star")}${m.note ? `<div class="text">${richText(m.note)}</div>` : `<span class="k">Bookmarked</span>`}</div>`));
    return;
  }
  const box = h(`<div class="step-note">${svg("star")}<textarea rows="1" placeholder="Bookmarked. Add a note to this step…" aria-label="Note on this step"></textarea></div>`);
  const ta = $("textarea", box);
  ta.value = m.note || "";
  ta.addEventListener("input", () => {
    growArea(ta);
    updateNotes(key, (x) => { const s = x.steps.find((y) => y.event === i); if (s) s.note = ta.value; }, 600);
  });
  ta.addEventListener("keydown", (e) => { if (e.key === "Escape") ta.blur(); });
  body.append(box);
  requestAnimationFrame(() => growArea(ta));
  return ta;
}

function toggleStep(i) {
  const t = state.transcript, key = t.session.key, m = stepMark(key, i);
  if (m?.note && !confirm("Remove this bookmark and its note?")) return;
  updateNotes(key, (n) => {
    if (m) n.steps = n.steps.filter((x) => x.event !== i);
    else n.steps.push({ event: i, label: stepLabel(t.events[i]), note: "" });
  });
  const row = document.getElementById("ev-" + i);
  if (!row) return;
  const ta = renderMark(row, i);
  if (ta) ta.focus();
  updateLegend();
  trace.update();
}

function updateLegend() {
  const lg = $(".trace-legend .lg-mark-item");
  if (lg) lg.hidden = !notesOf(state.transcript?.session.key)?.steps?.length;
}

// bookmarkItems lists bookmarks for the palette: a session bookmarked or
// annotated, and each bookmarked step, newest first.
function bookmarkItems(q) {
  const out = [];
  const byKey = new Map(state.data.sessions.map((s) => [s.key, s]));
  const keys = Object.keys(state.notes).sort((a, b) => ((state.notes[a].updated || "") < (state.notes[b].updated || "") ? 1 : -1));
  const hit = (...v) => !q || v.some((x) => x && x.toLowerCase().includes(q));
  for (const key of keys) {
    const s = byKey.get(key), n = state.notes[key];
    if (!s) continue;
    if ((n.bookmarked || n.note) && hit(s.title, s.prompt, n.note)) out.push({ s, notes: n });
    for (const m of n.steps || []) if (hit(m.label, m.note, s.title)) out.push({ s, mark: m });
  }
  return out;
}

// openMark opens a session at a bookmarked step.
function openMark(key, ev) {
  if (state.current === key && state.transcript?.session.key === key) { focusEvent(ev); return; }
  state.pendingJump = { event: ev };
  go(key);
}

/* ---------------- icons ---------------- */
const ICON = {
  file: '<path d="M4.5 2.5h4.5l2.5 2.5v8.5h-7z"/><path d="M9 2.5V5h2.5"/>',
  edit: '<path d="M10.2 3.3l2.5 2.5-6.9 6.9H3.3v-2.5z"/><path d="M8.8 4.7l2.5 2.5"/>',
  term: '<rect x="2.5" y="3" width="11" height="10" rx="2"/><path d="M5 6.5l2 1.5-2 1.5M8.5 10h2.5"/>',
  search: '<circle cx="7" cy="7" r="4.25"/><path d="M10.2 10.2 13.5 13.5"/>',
  web: '<circle cx="8" cy="8" r="5.5"/><path d="M2.5 8h11M8 2.5c1.7 1.9 1.7 9.1 0 11M8 2.5c-1.7 1.9-1.7 9.1 0 11"/>',
  agent: '<circle cx="4.5" cy="4" r="1.5"/><circle cx="4.5" cy="12" r="1.5"/><circle cx="11.5" cy="8" r="1.5"/><path d="M4.5 5.5v5M6 8h4"/>',
  other: '<rect x="3" y="3" width="10" height="10" rx="2.5"/><path d="M6 8h4"/>',
  think: '<path d="M6 13.5h4M6.2 11.5c-1.6-.8-2.7-2.4-2.7-4.2a4.5 4.5 0 0 1 9 0c0 1.8-1.1 3.4-2.7 4.2z"/>',
  chev: '<path d="M6 4l4 4-4 4"/>',
  down: '<path d="M4 6l4 4 4-4"/>',
  folder: '<path d="M2.5 4.5a1 1 0 0 1 1-1h3l1.5 1.5h4.5a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-9a1 1 0 0 1-1-1z"/>',
  copy: '<rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5V3.5a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2"/>',
  check: '<path d="M3.5 8.5l3 3 6-7"/>',
  download: '<path d="M8 2.5v8M4.8 7.5 8 10.7l3.2-3.2M3 13.5h10"/>',
  branch: '<circle cx="5" cy="3.8" r="1.4"/><circle cx="5" cy="12.2" r="1.4"/><circle cx="11" cy="6" r="1.4"/><path d="M5 5.2v5.6M11 7.4c0 2.4-6 1.6-6 3.4"/>',
  clock: '<circle cx="8" cy="8" r="5.5"/><path d="M8 5v3l2 1.5"/>',
  chip: '<rect x="4" y="4" width="8" height="8" rx="1.5"/><path d="M6.5 2v2M9.5 2v2M6.5 12v2M9.5 12v2M2 6.5h2M2 9.5h2M12 6.5h2M12 9.5h2"/>',
  lesson: '<rect x="3.5" y="3" width="9" height="11" rx="1.5"/><path d="M6 3V2h4v1M6 8.5l1.5 1.5 2.8-3"/>',
  star: '<path d="M8 2.4l1.7 3.5 3.8.55-2.75 2.7.65 3.8L8 11.15l-3.4 1.8.65-3.8L2.5 6.45l3.8-.55z"/>',
  note: '<path d="M3.5 3h9v6.5L9.5 12.5h-6z"/><path d="M9.5 12.5v-3h3M5.5 6h5M5.5 8.5h2.5"/>',
};
const svg = (name, cls = "") => `<svg class="${cls}" viewBox="0 0 16 16" aria-hidden="true">${ICON[name]}</svg>`;

function toolIcon(name) {
  const n = (name || "").toLowerCase();
  if (/^(edit|multiedit|write|apply_patch|notebookedit)$/.test(n)) return "edit";
  if (/^(read|view|notebookread|cat)$/.test(n)) return "file";
  if (/(bash|shell|exec|command|terminal)/.test(n)) return "term";
  if (/^(web|fetch|browser)|websearch|webfetch/.test(n)) return "web";
  if (/^(grep|glob|ls|find|search|rg)/.test(n)) return "search";
  if (/^(task|agent|subagent)/.test(n)) return "agent";
  return "other";
}

/* ---------------- time ---------------- */
function ago(iso) {
  const d = new Date(iso), s = (Date.now() - d) / 1000;
  if (!(s >= 0)) return "";
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  if (s < 7 * 86400) return `${Math.floor(s / 86400)}d`;
  const opts = { month: "short", day: "numeric" };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
  return d.toLocaleDateString(undefined, opts);
}
function clock(iso) {
  const d = new Date(iso);
  return isNaN(d) ? "" : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
}
function duration(a, b) {
  const mins = Math.round((new Date(b) - new Date(a)) / 60000);
  if (!(mins >= 0)) return "";
  if (mins < 1) return "under a minute";
  if (mins < 90) return `${mins} min`;
  if (mins < 48 * 60) return `${(mins / 60).toFixed(1).replace(/\.0$/, "")} h`;
  return `${Math.round(mins / 1440)} days`;
}
function when(a) {
  const s = new Date(a);
  if (isNaN(s)) return "";
  const today = new Date(); const y = new Date(Date.now() - 864e5);
  const same = (x, z) => x.toDateString() === z.toDateString();
  const day = same(s, today) ? "Today" : same(s, y) ? "Yesterday"
    : s.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", year: s.getFullYear() !== today.getFullYear() ? "numeric" : undefined });
  return `${day} at ${clock(a)}`;
}
function tildify(p) {
  const home = state.data?.home;
  if (home && p && (p === home || p.startsWith(home + "/"))) return "~" + p.slice(home.length);
  return p || "";
}

/* ---------------- sidebar ---------------- */
function matches(s, q) {
  const n = notesOf(s.key);
  return [s.title, s.prompt, s.cwd, s.branch, s.id, n?.note, ...(n?.steps || []).flatMap((m) => [m.note, m.label])]
    .some((v) => v && v.toLowerCase().includes(q));
}
function visibleSessions() {
  const q = state.q.trim().toLowerCase();
  return state.data.sessions.filter((s) => !state.hidden.has(s.provider) && (!state.onlyMarked || hasNotes(notesOf(s.key))) && (!q || matches(s, q)));
}

function buildTree(sessions) {
  const root = { kids: new Map(), sessions: [], path: "" };
  for (const s of sessions) {
    const disp = tildify(s.root);
    const parts = disp.startsWith("/") ? ["/", ...disp.slice(1).split("/")] : disp.split("/");
    let node = root, path = "";
    for (const part of parts.filter(Boolean)) {
      path = path ? (path === "/" ? "/" + part : path + "/" + part) : part;
      if (!node.kids.has(part)) node.kids.set(part, { name: part, kids: new Map(), sessions: [], path });
      node = node.kids.get(part);
    }
    node.sessions.push(s);
  }
  const compact = (node) => {
    for (const [k, kid] of [...node.kids]) {
      let n = kid;
      while (n.sessions.length === 0 && n.kids.size === 1) {
        const [child] = n.kids.values();
        child.name = (n.name === "/" ? "/" : n.name + "/") + child.name;
        n = child;
      }
      node.kids.set(k, n);
      compact(n);
    }
  };
  compact(root);
  const stats = (node) => {
    node.count = node.sessions.length;
    node.latest = node.sessions.reduce((m, s) => (s.ended > m ? s.ended : m), "");
    node.usage = null;
    const addUsage = (u) => {
      if (!u) return;
      const n = (node.usage ||= { input: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, output: 0, reasoning: 0, cost: 0, unpriced: 0, unpricedModels: [] });
      for (const k of ["input", "cacheRead", "cacheWrite", "cacheWrite1h", "output", "reasoning", "cost", "unpriced"]) n[k] += u[k] || 0;
      n.context = Math.max(n.context || 0, u.context || 0);
      for (const m of u.unpricedModels || []) if (!n.unpricedModels.includes(m)) n.unpricedModels.push(m);
    };
    for (const s of node.sessions) addUsage(s.usage);
    for (const kid of node.kids.values()) {
      stats(kid);
      node.count += kid.count;
      if (kid.latest > node.latest) node.latest = kid.latest;
      addUsage(kid.usage);
    }
  };
  stats(root);
  return root;
}

function renderProviders() {
  const el = $("#providers");
  el.innerHTML = "";
  for (const p of state.data.providers) {
    const b = h(`<button class="chip" aria-pressed="${!state.hidden.has(p.name)}" ${p.sessions ? "" : "disabled"}
      title="${esc(p.found ? tildify(p.root) : tildify(p.root) + " isn't on this machine")}"><span class="pmark ${esc(p.name)}"></span>${esc(PROVIDERS[p.name] || p.name)}<span class="n">${p.sessions}</span></button>`);
    b.onclick = () => {
      state.hidden.has(p.name) ? state.hidden.delete(p.name) : state.hidden.add(p.name);
      renderSidebar();
      if (onStats()) loadStats();
    };
    el.append(b);
  }
  const marked = state.data.sessions.filter((s) => hasNotes(notesOf(s.key))).length;
  if (!marked) state.onlyMarked = false;
  if (marked || state.onlyMarked) {
    const b = h(`<button class="chip marks" aria-pressed="${state.onlyMarked}" title="Show only sessions you bookmarked or wrote notes on">${svg("star")}Bookmarks<span class="n">${marked}</span></button>`);
    b.onclick = () => { state.onlyMarked = !state.onlyMarked; renderSidebar(); };
    el.append(b);
  }
}

function renderSidebar() {
  renderProviders();
  const tree = $("#tree");
  const sessions = visibleSessions();
  tree.innerHTML = "";
  if (!state.data.sessions.length) {
    tree.append(h(`<div class="empty">No sessions yet. Run Claude Code, Codex, OpenCode, Gemini CLI or Kiro CLI in a project, then rescan.</div>`));
    return;
  }
  if (!sessions.length && state.onlyMarked && !state.q.trim()) {
    tree.append(h(`<div class="empty">None of your bookmarked sessions are from the providers shown. Turn one back on, or turn off Bookmarks.</div>`));
    return;
  }
  if (!sessions.length) {
    tree.append(h(`<div class="empty">No session ${state.onlyMarked ? "you bookmarked " : ""}matches “${esc(state.q)}”. Clear the filter${state.onlyMarked ? ", turn off Bookmarks," : ""} or turn a provider back on.</div>`));
    tree.append(deepSearchRow());
    return;
  }
  const filtering = state.q.trim() !== "" || state.onlyMarked;
  const byRecent = (a, b) => (a.latest < b.latest ? 1 : -1);
  const folder = (node) => {
    const open = filtering || state.expanded.get(node.path) !== false;
    const slash = node.name.lastIndexOf("/");
    const pre = slash > 0 ? node.name.slice(0, slash + 1) : "";
    const last = slash > 0 ? node.name.slice(slash + 1) : node.name;
    const project = node.sessions.find((s) => tildify(s.root) === node.path)?.root;
    const el = h(`<div class="folder" aria-expanded="${open}">
      <button class="folder-row" title="${esc(node.path)}">${svg("down", "chev")}${svg("folder", "ficon")}
        <span class="name"><span class="pre">${esc(pre)}</span>${esc(last)}</span>${node.usage ? `<span class="fcost" title="${esc((usagePref.tokens ? "Tokens" : "API-equivalent cost") + " of every session in this folder\n\n" + usageLines(node.usage))}">${esc(usageShort(node.usage))}</span>` : ""}<span class="count">${node.count}</span>
      </button>${project ? `<button class="folder-retro" title="Lessons: rules worth adding to CLAUDE.md or AGENTS.md, from this project's sessions" aria-label="Lessons for ${esc(node.name)}">${svg("lesson")}</button>` : ""}<div class="kids"></div></div>`);
    if (project) $(".folder-retro", el).onclick = () => { location.hash = retroHash(project); closeDrawer(); };
    $(".folder-row", el).onclick = () => {
      const now = el.getAttribute("aria-expanded") !== "true";
      el.setAttribute("aria-expanded", now);
      state.expanded.set(node.path, now);
    };
    const kids = $(".kids", el);
    for (const kid of [...node.kids.values()].sort(byRecent)) kids.append(folder(kid));
    const list = [...node.sessions].sort((a, b) => (a.ended < b.ended ? 1 : -1));
    const all = filtering || state.showAll.has(node.path) || list.some((s, i) => i >= PER_FOLDER && s.key === state.current);
    for (const s of all ? list : list.slice(0, PER_FOLDER)) kids.append(sessionRow(s));
    if (!all && list.length > PER_FOLDER) {
      const more = h(`<button class="more">Show ${list.length - PER_FOLDER} older</button>`);
      more.onclick = () => { state.showAll.add(node.path); renderSidebar(); };
      kids.append(more);
    }
    return el;
  };
  const root = buildTree(sessions);
  for (const kid of [...root.kids.values()].sort(byRecent)) tree.append(folder(kid));
  for (const s of root.sessions) tree.append(sessionRow(s));
  if (state.q.trim()) tree.append(deepSearchRow());
}

// deepSearchRow hands the sidebar filter to the palette, which also searches
// inside transcripts.
function deepSearchRow() {
  const b = h(`<button class="deep-search">${svg("search")}<span>Search transcripts for “${esc(state.q.trim())}”</span></button>`);
  b.onclick = () => openPalette(state.q.trim());
  return b;
}

function sessionRow(s) {
  const bits = [];
  if (s.worktree) bits.push(`<span>worktree ${esc(s.worktree.split("/").pop())}</span>`);
  else if (s.cwd !== s.root) bits.push(`<span>${esc(tildify(s.cwd).replace(tildify(s.root) + "/", ""))}/</span>`);
  if (s.branch) bits.push(`<span>${esc(s.branch)}</span>`);
  if (s.edits) bits.push(`<span class="edits">${plural(s.edits, "edit")}</span>`);
  if (s.usage) bits.push(`<span class="cost">${esc(usageShort(s.usage))}</span>`);
  const n = notesOf(s.key);
  let tip = s.titleKind !== "prompt" && s.prompt ? `${s.title}\n\n${s.prompt}` : s.prompt || s.title;
  if (n?.note) tip += `\n\nYour note: ${n.note}`;
  const marks = hasNotes(n) ? `<span class="marks">${n.bookmarked ? `<span title="Bookmarked">${svg("star", "on")}</span>` : ""}${n.note || n.steps?.some((m) => m.note) ? `<span title="Has notes">${svg("note")}</span>` : ""}${n.steps?.length ? `<span class="n" title="${plural(n.steps.length, "bookmarked step")}">${n.steps.length}</span>` : ""}</span>` : "";
  const el = h(`<button class="sess" data-key="${esc(s.key)}" aria-current="${s.key === state.current}" title="${esc(tip)}">
    <span class="pmark ${esc(s.provider)}" aria-label="${esc(PROVIDERS[s.provider] || s.provider)}"></span>
    <span class="t ${s.titleKind === "prompt" ? "unnamed" : ""}">${esc(s.title)}</span><span class="when">${marks}${esc(ago(s.ended))}</span>
    ${bits.length ? `<span class="sub">${bits.join("")}</span>` : ""}</button>`);
  el.onclick = () => go(s.key);
  return el;
}

function go(key) {
  location.hash = "s=" + encodeURIComponent(key);
  closeDrawer();
}

/* ---------------- home ---------------- */
function renderHome() {
  const d = state.data;
  const folders = new Set(d.sessions.map((s) => s.root)).size;
  const lastDay = d.sessions.filter((s) => Date.now() - new Date(s.ended) < 864e5).length;
  const view = $("#view");
  view.innerHTML = "";
  if (!d.sessions.length) {
    view.append(h(`<div class="home"><div class="home-inner"><h1>No sessions yet</h1>
      <p class="lede">trail reads the sessions Claude Code, Codex, OpenCode, Gemini CLI and Kiro CLI keep on this machine. Run any of them in a project, then rescan.</p>
      <h2>Where trail looks</h2><div class="sources"></div></div></div>`));
  } else {
    view.append(h(`<div class="home"><div class="home-inner">
      <h1>${plural(d.sessions.length, "session")} across ${plural(folders, "folder")}</h1>
      <p class="lede">${lastDay ? `${plural(lastDay, "session")} ran in the last day. ` : ""}Open one to read what the agent did next to every file it changed, or see <a class="lede-link" href="#stats">stats across every project</a>.</p>
      <h2>Pick up where you left off</h2><div class="recent"></div>
      <h2>Who wrote this?</h2>${blameForm("")}
      <h2>Where trail looks</h2><div class="sources"></div></div></div>`));
    const recent = $(".recent", view);
    for (const s of d.sessions.slice(0, 8)) {
      const where = [tildify(s.root), s.branch].filter(Boolean).join("   ");
      const row = h(`<button class="recent-row"><span class="pmark ${esc(s.provider)}"></span>
        <span class="t">${esc(s.title)}</span><span class="edits ${s.edits ? "" : "none"}">${s.edits ? plural(s.edits, "edit") : "no edits"}</span>
        <span class="when">${esc(ago(s.ended))}</span><span class="where">${esc(where)}</span></button>`);
      row.onclick = () => go(s.key);
      recent.append(row);
    }
  }
  setupBlameForm(view);
  const src = $(".sources", view);
  for (const p of d.providers) {
    src.append(h(`<div class="src"><span class="pmark ${esc(p.name)}"></span><span>${esc(PROVIDERS[p.name] || p.name)}</span>
      <span class="path" title="${esc(p.root)}">${esc(tildify(p.root))}</span>
      <span class="state ${p.found ? "" : "missing"}">${p.found ? plural(p.sessions, "session") : "Not on this machine"}</span></div>`));
  }
}

/* ---------------- diff ---------------- */
function lineDiff(a, b) {
  const lines = (x) => (x === "" ? [] : x.replace(/\n$/, "").split("\n"));
  const A = lines(a), B = lines(b);
  const n = A.length, m = B.length;
  if (n * m > 4e6) return [...A.map((t) => ["d", t]), ...B.map((t) => ["a", t])];
  const dp = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--)
      dp[i][j] = A[i] === B[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out = [];
  let i = 0, j = 0;
  while (i < n && j < m) {
    if (A[i] === B[j]) { out.push([" ", A[i]]); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push(["d", A[i++]]);
    else out.push(["a", B[j++]]);
  }
  while (i < n) out.push(["d", A[i++]]);
  while (j < m) out.push(["a", B[j++]]);
  return out;
}

function changeDiff(c) {
  const hunks = c.op === "delete" ? [] : (c.hunks || []).map((hk) => lineDiff(hk.old || "", hk.new || ""));
  let add = 0, del = 0;
  for (const rows of hunks) for (const [k] of rows) { if (k === "a") add++; else if (k === "d") del++; }
  return { hunks, add, del };
}

const OPS = { edit: "Edited", write: "Wrote file", add: "Added", delete: "Deleted", update: "Updated", move: "Moved" };
const counts = (d) => `<span class="plus">+${d.add}</span><span class="minus">−${d.del}</span>`;

function bar5(add, del) {
  const total = add + del;
  let a = 0, d = 0;
  if (total) {
    const scale = Math.min(5, total) / total;
    a = Math.round(add * scale); d = Math.round(del * scale);
    if (add && !a) a = 1; if (del && !d) d = 1;
    while (a + d > 5) a > d ? a-- : d--;
  }
  return `<span class="bar5" aria-hidden="true">${Array.from({ length: 5 }, (_, i) => `<i class="${i < a ? "a" : i < a + d ? "d" : ""}"></i>`).join("")}</span>`;
}

function renderDiffRows(rows, limit) {
  const pre = document.createElement("div");
  pre.className = "diff";
  const shown = limit ? rows.slice(0, limit) : rows;
  pre.innerHTML = shown.map(([k, t]) => `<div class="${k === "a" ? "a" : k === "d" ? "d" : ""}">${esc(t)}</div>`).join("");
  if (limit && rows.length > limit) {
    const more = h(`<button class="more">Show ${rows.length - limit} more lines</button>`);
    more.onclick = () => pre.replaceWith(renderDiffRows(rows, 0));
    pre.append(more);
  }
  return pre;
}

/* ---------------- session ---------------- */
async function openSession(key) {
  stopLive();
  if (state.find && state.find.key !== key) state.find = null;
  state.current = key;
  state.fileFilter = null;
  state.commits = null;
  state.tab = "files";
  renderSidebar();
  $$(".sess").find((b) => b.dataset.key === key)?.scrollIntoView({ block: "nearest" });
  const view = $("#view");
  const showError = (msg) => {
    view.innerHTML = "";
    view.append(h(`<div class="empty-pane"><strong>Couldn’t open this session</strong>${esc(msg)}</div>`));
  };
  view.innerHTML = `<div class="loading">Opening session…</div>`;
  let t;
  try {
    t = await api("/api/session?id=" + encodeURIComponent(key));
  } catch (e) {
    if (state.current === key) showError(e.message);
    return;
  }
  if (state.current !== key) return;
  t.events = t.events || [];
  t.changes = t.changes || [];
  try {
    state.transcript = t;
    state.diffs = t.changes.map(changeDiff);
    state.stats = sessionStats(t);
    if (!t.changes.length) state.tab = "commits";
    renderSession();
    startLive();
    const jump = state.pendingJump;
    state.pendingJump = null;
    if (jump) requestAnimationFrame(() => showMatch(jump.event, jump.field));
  } catch (e) {
    console.error(e);
    showError(`The page hit an error while drawing it: ${e.message}`);
  }
}

function sessionStats(t) {
  let tools = 0, failed = 0, prompts = 0;
  for (const e of t.events) {
    if (e.kind === "tool") { tools++; if (e.error) failed++; }
    if (e.kind === "user") prompts++;
  }
  return { tools, failed, prompts, files: new Set(t.changes.map((c) => c.rel)).size };
}

function sessionHeader() {
  const t = state.transcript, s = t.session, st = state.stats;
  const named = s.titleKind !== "prompt";
  const meta = [
    `<span class="prov"><span class="pmark ${esc(s.provider)}"></span>${esc(PROVIDERS[s.provider] || s.provider)}</span>`,
    s.model && `<span>${svg("chip")}${esc(s.model)}</span>`,
    s.branch && `<span>${svg("branch")}${esc(s.branch)}</span>`,
    `<span title="${esc(s.cwd)}">${svg("folder")}<span class="cwd"><bdi>${esc(tildify(s.cwd))}</bdi></span></span>`,
    `<span>${svg("clock")}${esc(when(s.started))}</span>`,
  ].filter(Boolean).join("");
  const facts = [
    `<span>${esc(duration(s.started, s.ended))}</span>`,
    `<span><b>${st.prompts}</b> ${st.prompts === 1 ? "prompt" : "prompts"}</span>`,
    `<span><b>${st.tools}</b> ${st.tools === 1 ? "tool call" : "tool calls"}</span>`,
    t.changes.length ? `<span class="blz"><b>${t.changes.length}</b> ${t.changes.length === 1 ? "edit" : "edits"} in <b>${st.files}</b> ${st.files === 1 ? "file" : "files"}</span>` : `<span>No edits</span>`,
    st.failed ? `<span class="bad"><b>${st.failed}</b> failed</span>` : "",
  ].join("");
  const u = s.usage;
  const usage = u ? `<div class="usage" title="${esc(usageLines(u))}">
      <span class="u-main">${usagePref.tokens
        ? `<b>${esc(fmtTokens(totalTokens(u)))}</b> tokens`
        : `<b>${esc(costLabel(u))}</b> API-equivalent`}</span>
      <span><b>${esc(fmtTokens(promptTokens(u)))}</b> in</span>
      ${u.cacheRead ? `<span><b>${hitRate(u)}</b> cache hits</span>` : ""}
      <span><b>${esc(fmtTokens(u.output))}</b> out${u.reasoning ? ` (${esc(fmtTokens(u.reasoning))} thinking)` : ""}</span>
      <span>${plural(u.responses, "response")}</span>
      ${u.unpriced ? `<span class="warn">${plural(u.unpriced, "response")} not priced</span>` : ""}
      <button class="linkish" id="usage-toggle">${usagePref.tokens ? "Show cost" : "Show tokens"}</button>
    </div>` : "";

  const head = h(`<header class="s-head">
      <div class="titles">
        <h1 class="${named ? "" : "unnamed"}">${esc(s.title)}</h1>
        ${named && s.prompt ? `<p class="first-prompt" title="${esc(s.prompt)}">${esc(s.prompt)}</p>` : ""}
        <div class="meta">${meta}</div>
        <div class="facts">${facts}</div>
        ${usage}
      </div>
      <div class="actions">${EXPORTED ? exportedActions() : `<span class="live" ${live.on ? "" : "hidden"} title="The agent is still writing this session. New steps appear as it works.">Live</span><button class="btn bookmark" id="bookmark" aria-pressed="false" title="Bookmark this session (B)">${svg("star")}<span>Bookmark</span></button><button class="btn" id="retro" title="Rules worth adding to CLAUDE.md or AGENTS.md, from every session in this project">${svg("lesson")}<span>Lessons</span></button><button class="btn" id="export" title="Save this session as one HTML file">${svg("download")}<span>Export</span></button><button class="btn" id="copy" title="${esc(s.resume)}">${svg("copy")}<span>Copy resume command</span></button>`}</div>
    </header>`);
  const tog = $("#usage-toggle", head);
  if (tog) tog.onclick = (e) => { e.stopPropagation(); setUsagePref(!usagePref.tokens); };
  if (EXPORTED) {
    setupExportFind(head);
    if (notesOf(s.key)?.bookmarked) $(".titles h1", head).insertAdjacentHTML("afterbegin", `<span class="h-star" title="Bookmarked">${svg("star", "on")}</span>`);
    return head;
  }
  $("#export", head).onclick = () => exportDialog(s);
  $("#retro", head).onclick = () => { location.hash = retroHash(s.root); };
  setBookmarkButton($("#bookmark", head), !!notesOf(s.key)?.bookmarked);
  $("#bookmark", head).onclick = toggleBookmark;
  $("#copy", head).onclick = async (e) => {
    const b = e.currentTarget, label = $("span", b);
    try {
      await navigator.clipboard.writeText(s.resume);
      b.classList.add("done"); b.querySelector("svg").innerHTML = ICON.check; label.textContent = "Copied";
    } catch { label.textContent = "Couldn’t copy. Hover to read it."; }
    setTimeout(() => { b.classList.remove("done"); b.querySelector("svg").innerHTML = ICON.copy; label.textContent = "Copy resume command"; }, 1800);
  };
  return head;
}

/* ---------------- export ---------------- */
function exportDialog(s) {
  $(".dlg")?.remove();
  const d = h(`<dialog class="dlg" aria-labelledby="dlg-title"><form method="dialog">
    <h2 id="dlg-title">Export this session</h2>
    <p>Saves the transcript, its edits and the commits that touch them as one HTML file. It opens in any browser without trail and never goes online.</p>
    <p class="warn">Transcripts can contain secrets: API keys, tokens, environment variables, file contents and command output. Read the file before you share it.</p>
    ${hasNotes(notesOf(s.key)) ? `<label class="dlg-check"><input type="checkbox" checked> Include my notes and bookmarks</label>` : ""}
    <div class="dlg-actions"><button class="btn" value="cancel">Cancel</button><a class="btn primary" href="/api/export?id=${encodeURIComponent(s.key)}" download>${svg("download")}<span>Download HTML</span></a></div>
  </form></dialog>`);
  const a = $("a", d), check = $(".dlg-check input", d);
  if (check) check.onchange = () => { a.href = `/api/export?id=${encodeURIComponent(s.key)}${check.checked ? "" : "&notes=0"}`; };
  a.onclick = () => setTimeout(() => d.close(), 0);
  d.addEventListener("close", () => d.remove());
  document.body.append(d);
  d.showModal();
}

function exportedActions() {
  const on = new Date(EXPORTED.exported);
  const date = isNaN(on) ? "" : on.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
  return `<label class="find-in">${svg("search")}<input type="search" placeholder="Find in this session" aria-label="Find in this session" autocomplete="off" spellcheck="false"></label>
    <span class="exported-on" title="Exported with trail ${esc(EXPORTED.version)}">Exported${date ? ` ${esc(date)}` : ""}</span>`;
}

// setupExportFind matches steps in the page itself, since there is no search
// server; matches go on the trace like a search from the palette.
function setupExportFind(head) {
  const input = $(".find-in input", head);
  if (state.find) input.value = state.find.q;
  let timer = 0;
  input.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      const t = state.transcript, words = input.value.toLowerCase().split(/\s+/).filter(Boolean);
      state.find = null;
      if (words.length) {
        const events = [];
        t.events.forEach((e, i) => {
          const hay = [e.text, e.tool, e.summary, e.input, e.output].filter(Boolean).join("\n").toLowerCase();
          if (words.every((w) => hay.includes(w))) events.push(i);
        });
        state.find = { q: input.value, key: t.session.key, events, i: -1 };
      }
      renderFind();
      trace.draw();
    }, 120);
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); stepFind(e.shiftKey ? -1 : 1); }
    if (e.key === "Escape") { input.value = ""; input.dispatchEvent(new Event("input")); input.blur(); }
  });
}

function initExport() {
  const t = EXPORTED.transcript;
  t.events = t.events || [];
  t.changes = t.changes || [];
  state.data = { home: EXPORTED.home, sessions: [] };
  state.current = t.session.key;
  state.transcript = t;
  state.diffs = t.changes.map(changeDiff);
  state.stats = sessionStats(t);
  state.commits = EXPORTED.commits;
  if (EXPORTED.notes) state.notes = { [t.session.key]: EXPORTED.notes };
  if (!t.changes.length) state.tab = "commits";
  renderSession();
  document.addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName);
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "/") { e.preventDefault(); $(".find-in input")?.focus(); }
    if (e.key === "n" || e.key === "N") stepFind(e.shiftKey ? -1 : 1);
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => trace.draw());
}

function renderSession() {
  const t = state.transcript;
  const view = $("#view");
  view.innerHTML = "";
  const el = h(`<div class="session">
    <div class="trace-wrap">
      <div class="trace" id="trace" role="slider" tabindex="0" aria-label="Session trace. Drag or use arrow keys to move through the transcript." aria-valuemin="1" aria-valuemax="${t.events.length}" aria-valuenow="1">
        <div class="win"></div><canvas></canvas>
      </div>
      <div class="trace-legend"><span><i class="lg-prompt"></i>Prompt</span><span><i class="lg-tool"></i>Tool call</span><span><i class="lg-edit"></i>Edit</span><span><i class="lg-err"></i>Failed</span><span class="lg-mark-item" hidden><i class="lg-mark"></i>Bookmark</span><span class="find" id="find" hidden></span></div>
    </div>
    <div class="panes" data-show="${state.pane}">
      <div class="pane-tabs"><div class="seg" role="tablist">
        <button role="tab" data-pane="log" aria-selected="${state.pane === "log"}">Transcript</button>
        <button role="tab" data-pane="ch" aria-selected="${state.pane === "ch"}">Changes<span class="n">${t.changes.length}</span></button>
      </div></div>
      <section class="pane log" aria-label="Transcript">
        <div class="pane-head">
          <div class="seg" role="group" aria-label="Show">
            <button data-f="all" aria-pressed="${state.filter === "all"}">Every step</button>
            <button data-f="talk" aria-pressed="${state.filter === "talk"}">Conversation</button>
            <button data-f="changes" aria-pressed="${state.filter === "changes"}">Edits and failures</button>
          </div>
        </div>
        <div class="log-inner"></div>
      </section>
      <section class="pane ch" aria-label="Changes"></section>
    </div></div>`);

  el.prepend(sessionHeader(), sessionNotes());
  $$(".pane-tabs button", el).forEach((b) => (b.onclick = () => setPane(b.dataset.pane)));
  $$("[data-f]", el).forEach((b) => (b.onclick = () => { state.filter = b.dataset.f; applyFilter(); }));
  view.append(el);
  renderLog($(".log-inner", el));
  applyFilter();
  renderChangesPane();
  setupTrace();
  renderFind();
  updateLegend();
}

function setPane(p) {
  state.pane = p;
  const panes = $(".panes");
  if (!panes) return;
  panes.dataset.show = p;
  $$(".pane-tabs button", panes).forEach((b) => b.setAttribute("aria-selected", b.dataset.pane === p));
  trace.update();
}

function richText(s) {
  const parts = s.split(/```[^\n]*\n?/);
  return parts.map((p, i) => {
    if (i % 2) return `<pre><code>${esc(p.replace(/\n$/, ""))}</code></pre>`;
    return esc(p).replace(/`([^`\n]+)`/g, "<code>$1</code>").replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  }).join("");
}

function evClass(e) {
  const c = [e.kind];
  if (e.changes?.length) c.push("edit");
  if (e.error) c.push("err");
  if (e.sidechain) c.push("side-chain");
  return c.join(" ");
}

// renderLog appends rows for events from index `from` on.
function renderLog(root, from = 0) {
  const t = state.transcript;
  if (!t.events.length) {
    root.append(h(`<div class="empty-pane"><strong>Empty transcript</strong>This session has no messages trail can read.</div>`));
    return;
  }
  const frag = document.createDocumentFragment();
  const marked = new Set((notesOf(t.session.key)?.steps || []).map((m) => m.event));
  let lastClock = "";
  for (let i = from - 1; i >= 0 && !lastClock; i--) lastClock = clock(t.events[i].time);
  t.events.forEach((e, i) => {
    if (i < from) return;
    const c = clock(e.time);
    const timeCell = c && c !== lastClock ? c : "";
    if (c) lastClock = c;
    const row = h(`<div class="ev ${esc(evClass(e))}" id="ev-${i}" data-i="${i}"><div class="time">${esc(timeCell)}</div><div class="rail"></div><div class="body"></div></div>`);
    const body = $(".body", row);
    if (e.sidechain) body.append(h(`<span class="sub-label">${svg("agent")}Subagent</span>`));
    if (e.kind === "user") {
      body.append(h(`<div class="prompt text">${richText(e.text)}</div>`));
    } else if (e.kind === "assistant") {
      body.append(h(`<div class="text">${richText(e.text)}</div>`));
    } else if (e.kind === "thinking") {
      body.append(h(`<details class="think"><summary>${svg("think")}Thinking</summary><div class="text">${richText(e.text)}</div></details>`));
    } else if (e.kind === "tool") {
      const d = h(`<details class="tool ${e.error ? "err" : ""}"><summary>${svg(toolIcon(e.tool), "ic")}<span class="tn">${esc(e.tool)}</span><span class="ts">${esc(e.summary || "")}</span>
        <span class="st">${e.error ? "Failed" : ""}</span>${svg("chev", "chev2")}</summary><div class="io"></div></details>`);
      d.addEventListener("toggle", () => {
        const io = $(".io", d);
        if (!d.open || io.childElementCount) return;
        if (e.input) io.append(h(`<pre>${esc(e.input)}</pre>`));
        if (e.output) { io.append(h(`<div class="lbl">${e.error ? "Error" : "Output"}</div>`)); io.append(h(`<pre class="${e.error ? "err-out" : ""}">${esc(e.output)}</pre>`)); }
        if (!e.input && !e.output) io.append(h(`<span class="sub-label">No input or output recorded.</span>`));
        trace.update();
      });
      body.append(d);
      if (e.changes?.length) {
        const links = h(`<div class="links"></div>`);
        for (const ci of e.changes) {
          const ch = t.changes[ci], df = state.diffs[ci];
          const b = h(`<button class="clink" data-change="${ci}" title="Show this change"><span class="p">${esc(ch.rel)}</span>${ch.op === "delete" ? `<span class="minus">deleted</span>` : counts(df)}</button>`);
          b.onclick = () => focusChange(ci);
          links.append(b);
        }
        body.append(links);
      }
    }
    if (e.usage) {
      const u = e.usage;
      body.append(h(`<div class="step-usage" title="${esc(usageLines(u, true))}">
        <span>${esc(fmtTokens(promptTokens(u)))} in${u.cacheRead ? ` · ${hitRate(u)} cached` : ""}</span>
        <span>${esc(fmtTokens(u.output))} out</span>
        ${usagePref.tokens ? "" : `<span class="c">${esc(costLabel(u))}</span>`}
        ${u.responses > 1 ? `<span>${u.responses} responses</span>` : ""}</div>`));
    }
    if (marked.has(i) || !EXPORTED) renderMark(row, i);
    frag.append(row);
  });
  root.append(frag);
}

function visibleUnder(e, f) {
  if (f === "all") return true;
  if (f === "talk") return e.kind === "user" || e.kind === "assistant";
  return e.kind === "user" || (e.kind === "tool" && (e.error || e.changes?.length));
}

function applyFilter() {
  const t = state.transcript, f = state.filter;
  $$("[data-f]").forEach((b) => b.setAttribute("aria-pressed", b.dataset.f === f));
  let hiddenCount = 0;
  for (const row of $$(".log-inner .ev")) {
    const show = visibleUnder(t.events[+row.dataset.i], f);
    row.hidden = !show;
    if (!show) hiddenCount++;
  }
  $(".filtered-note")?.remove();
  if (hiddenCount) {
    const note = h(`<div class="filtered-note">${plural(hiddenCount, "step")} hidden by this view.</div>`);
    $(".log-inner")?.append(note);
  }
  trace.draw();
  trace.update();
}

/* ---------------- trace ---------------- */
const trace = {
  el: null, canvas: null, win: null, ro: null, n: 0,
  pad: 8,
  xOf(i) { const w = this.el.clientWidth - this.pad * 2; return this.pad + ((i + 0.5) * w) / this.n; },
  iAt(x) { const w = this.el.clientWidth - this.pad * 2; return Math.max(0, Math.min(this.n - 1, Math.floor(((x - this.pad) / w) * this.n))); },
  draw() {
    if (!this.el || !this.el.isConnected) return;
    const t = state.transcript, dpr = window.devicePixelRatio || 1;
    const W = this.el.clientWidth, H = this.el.clientHeight;
    if (!W || !H) return;
    this.canvas.width = W * dpr; this.canvas.height = H * dpr;
    const ctx = this.canvas.getContext("2d");
    ctx.scale(dpr, dpr);
    const css = getComputedStyle(document.documentElement);
    const col = (v) => css.getPropertyValue(v).trim();
    const C = { user: col("--ink"), assistant: col("--muted"), tool: col("--faint"), thinking: col("--line"), edit: col("--blaze"), err: col("--del") };
    const step = (W - this.pad * 2) / this.n;
    const bw = Math.max(1, Math.min(3, step * 0.6));
    const mid = H / 2;
    t.events.forEach((e, i) => {
      let kind = e.kind, hgt;
      if (e.kind === "tool" && e.error) kind = "err";
      else if (e.kind === "tool" && e.changes?.length) kind = "edit";
      hgt = { user: 0.72, edit: 0.56, err: 0.56, tool: 0.32, assistant: 0.22, thinking: 0.16 }[kind] * H;
      ctx.globalAlpha = visibleUnder(e, state.filter) ? 1 : 0.22;
      ctx.fillStyle = C[kind];
      const x = this.xOf(i) - bw / 2;
      ctx.beginPath();
      ctx.roundRect ? ctx.roundRect(x, mid - hgt / 2, bw, hgt, bw / 2) : ctx.rect(x, mid - hgt / 2, bw, hgt);
      ctx.fill();
    });
    const marks = notesOf(t.session.key)?.steps;
    if (marks?.length) {
      ctx.globalAlpha = 1;
      ctx.fillStyle = col("--blaze");
      const mw = Math.max(3, bw + 1);
      for (const m of marks) if (m.event < t.events.length) ctx.fillRect(this.xOf(m.event) - mw / 2, H - 4, mw, 4);
    }
    const f = activeFind();
    if (f) {
      ctx.globalAlpha = 1;
      ctx.fillStyle = col("--find");
      const mw = Math.max(2, bw);
      for (const i of f.events) ctx.fillRect(this.xOf(i) - mw / 2, 0, mw, 4);
    }
  },
  update() {
    if (!this.el || !this.el.isConnected) return;
    const pane = $(".pane.log");
    const rows = $$(".log-inner .ev:not([hidden])");
    if (!pane || !rows.length || pane.offsetParent === null) { this.win.style.width = "0"; return; }
    const top = pane.scrollTop + 46, bottom = pane.scrollTop + pane.clientHeight;
    let first = null, last = null;
    for (const r of rows) {
      const y = r.offsetTop, y2 = y + r.offsetHeight;
      if (y2 > top && first === null) first = +r.dataset.i;
      if (y < bottom) last = +r.dataset.i;
    }
    if (first === null) first = last = +rows[rows.length - 1].dataset.i;
    const x1 = this.xOf(first) - 4, x2 = this.xOf(last) + 4;
    this.win.style.left = `${x1}px`;
    this.win.style.width = `${Math.max(6, x2 - x1)}px`;
    this.el.setAttribute("aria-valuenow", first + 1);
  },
  scrollTo(i, smooth) {
    const t = state.transcript;
    let j = i;
    while (j < t.events.length && !visibleUnder(t.events[j], state.filter)) j++;
    if (j >= t.events.length) { j = i; while (j > 0 && !visibleUnder(t.events[j], state.filter)) j--; }
    const row = document.getElementById("ev-" + j), pane = $(".pane.log");
    if (!row || !pane) return;
    if (state.pane !== "log") setPane("log");
    pane.scrollTo({ top: row.offsetTop - 56, behavior: smooth ? "smooth" : "auto" });
  },
  tip(e, x, y) {
    const ev = state.transcript.events[e], tip = $("#tip");
    let label;
    if (ev.kind === "user") label = `<span class="k">Prompt</span><br>${esc(ev.text.slice(0, 160))}`;
    else if (ev.kind === "assistant") label = `<span class="k">Reply</span><br>${esc(ev.text.slice(0, 160))}`;
    else if (ev.kind === "thinking") label = `<span class="k">Thinking</span>`;
    else label = `<span class="k">${esc(ev.tool)}${ev.error ? " failed" : ev.changes?.length ? `, ${plural(ev.changes.length, "edit")}` : ""}</span><br>${esc(ev.summary || "")}`;
    const m = stepMark(state.transcript.session.key, e);
    if (m) label += `<br><span class="k">★ ${m.note ? esc(m.note.slice(0, 160)) : "Bookmarked"}</span>`;
    const c = clock(ev.time);
    tip.innerHTML = (c ? `<span class="k">${esc(c)}</span>&ensp;` : "") + label;
    tip.hidden = false;
    const r = tip.getBoundingClientRect();
    tip.style.left = `${Math.max(8, Math.min(window.innerWidth - r.width - 8, x - r.width / 2))}px`;
    tip.style.top = `${y + 14}px`;
  },
};

function setupTrace() {
  const t = state.transcript;
  trace.el = $("#trace");
  trace.canvas = $("canvas", trace.el);
  trace.win = $(".win", trace.el);
  trace.n = Math.max(1, t.events.length);
  if (trace.ro) trace.ro.disconnect();
  trace.ro = new ResizeObserver(() => { trace.draw(); trace.update(); });
  trace.ro.observe(trace.el);
  $(".pane.log").addEventListener("scroll", () => requestAnimationFrame(() => trace.update()), { passive: true });

  let dragging = false;
  const at = (ev) => trace.iAt(ev.clientX - trace.el.getBoundingClientRect().left);
  trace.el.addEventListener("pointerdown", (ev) => { dragging = true; trace.el.setPointerCapture(ev.pointerId); trace.scrollTo(at(ev), false); });
  trace.el.addEventListener("pointermove", (ev) => {
    if (!t.events.length) return;
    const i = at(ev);
    trace.tip(i, ev.clientX, trace.el.getBoundingClientRect().bottom - 6);
    if (dragging) trace.scrollTo(i, false);
  });
  const end = () => { dragging = false; };
  trace.el.addEventListener("pointerup", end);
  trace.el.addEventListener("pointercancel", end);
  trace.el.addEventListener("pointerleave", () => { $("#tip").hidden = true; });
  trace.el.addEventListener("keydown", (ev) => {
    const now = +trace.el.getAttribute("aria-valuenow") - 1;
    const prompts = t.events.map((e, i) => (e.kind === "user" ? i : -1)).filter((i) => i >= 0);
    let to = null;
    if (ev.key === "ArrowRight") to = Math.min(t.events.length - 1, now + 1);
    if (ev.key === "ArrowLeft") to = Math.max(0, now - 1);
    if (ev.key === "PageDown" || (ev.key === "ArrowDown")) to = prompts.find((i) => i > now) ?? t.events.length - 1;
    if (ev.key === "PageUp" || (ev.key === "ArrowUp")) to = [...prompts].reverse().find((i) => i < now) ?? 0;
    if (ev.key === "Home") to = 0;
    if (ev.key === "End") to = t.events.length - 1;
    if (to !== null) { ev.preventDefault(); trace.scrollTo(to, true); }
  });
  requestAnimationFrame(() => { trace.draw(); trace.update(); });
}

/* ---------------- live ---------------- */
// A session the agent wrote to in the last few minutes is followed live: the
// server streams what changed and new steps are drawn as they arrive.
const LIVE_WINDOW = 10 * 60 * 1000;
const live = { es: null, key: null, on: false };

function setLive(on) {
  live.on = on;
  const b = $(".s-head .live");
  if (b) b.hidden = !on;
}

function stopLive() {
  live.es?.close();
  live.es = live.key = null;
  setLive(false);
}

// startLive follows the open session if it looks recent, by its own last event
// or by the list's newer copy of it.
function startLive() {
  const t = state.transcript, key = state.current;
  if (EXPORTED || !t || !key || live.key === key || t.session.key !== key) return;
  const listed = state.data?.sessions.find((s) => s.key === key);
  const ended = Math.max(Date.parse(t.session.ended) || 0, Date.parse(listed?.ended) || 0);
  if (Date.now() - ended > LIVE_WINDOW) return;
  live.key = key;
  const es = new EventSource(`/api/live?id=${encodeURIComponent(key)}&size=${t.size}`);
  live.es = es;
  es.addEventListener("ready", () => setLive(true));
  es.addEventListener("update", (e) => {
    if (state.current !== key || state.transcript !== t) return;
    setLive(true);
    try { applyLive(JSON.parse(e.data)); } catch (err) { console.error(err); }
  });
  // Idle or gone: close, or the browser would reconnect. The 20 s list
  // refresh starts it again if the session moves.
  const end = () => { if (live.es === es) { es.close(); live.es = live.key = null; setLive(false); } };
  es.addEventListener("idle", end);
  es.addEventListener("gone", end);
  es.onerror = () => setLive(false);
}

function applyLive(u) {
  const t = state.transcript;
  const pane = $(".pane.log");
  const atEnd = pane && pane.scrollHeight - pane.scrollTop - pane.clientHeight < 80;
  const hadEvents = t.events.length > 0;
  const changesBefore = t.changes.length;
  t.events = t.events.slice(0, u.from).concat(u.events || []);
  t.changes = t.changes.slice(0, u.changesFrom).concat(u.changes || []);
  state.diffs = state.diffs.slice(0, u.changesFrom).concat((u.changes || []).map(changeDiff));
  t.session = u.session;
  t.size = u.size;
  state.stats = sessionStats(t);
  if (!hadEvents || u.from === 0 || !pane) {
    renderSession();
    return;
  }
  $(".s-head").replaceWith(sessionHeader());
  $(".pane-tabs [data-pane=ch] .n").textContent = t.changes.length;
  const root = $(".log-inner");
  for (const row of $$(".ev", root)) if (+row.dataset.i >= u.from) row.remove();
  renderLog(root, u.from);
  trace.n = Math.max(1, t.events.length);
  trace.el.setAttribute("aria-valuemax", t.events.length);
  applyFilter();
  if (state.tab === "files" && (u.changes?.length || u.changesFrom < changesBefore)) renderChangesPane();
  if (atEnd) pane.scrollTop = pane.scrollHeight;
  requestAnimationFrame(() => trace.update());
}

/* ---------------- transcript matches ---------------- */
function activeFind() {
  const f = state.find;
  return f && state.transcript && f.key === state.transcript.session.key && f.events.length ? f : null;
}

function renderFind() {
  const el = $("#find");
  if (!el) return;
  const f = activeFind();
  el.hidden = !f;
  if (!f) return;
  const pos = f.i >= 0 ? `${f.i + 1} of ${f.events.length}` : plural(f.events.length, "step");
  el.innerHTML = `<i class="lg-find"></i><span>${pos} matching “${esc(f.q.trim())}”</span>
    <button class="find-btn" data-d="-1" title="Previous match (Shift+N)" aria-label="Previous match">${svg("chev")}</button>
    <button class="find-btn" data-d="1" title="Next match (N)" aria-label="Next match">${svg("chev")}</button>
    <button class="find-btn x" title="Clear matches" aria-label="Clear matches"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 4.5l7 7M11.5 4.5l-7 7"/></svg></button>`;
  $$("[data-d]", el).forEach((b) => (b.onclick = () => stepFind(+b.dataset.d)));
  $(".x", el).onclick = () => {
    state.find = null; renderFind(); trace.draw();
    const input = $(".find-in input");
    if (input) input.value = "";
  };
}

function stepFind(d) {
  const f = activeFind();
  if (!f) return;
  if (f.i < 0) {
    const now = +($("#trace")?.getAttribute("aria-valuenow") || 1) - 1;
    f.i = d > 0 ? f.events.findIndex((e) => e >= now) : f.events.findLastIndex((e) => e <= now);
    if (f.i < 0) f.i = d > 0 ? 0 : f.events.length - 1;
  } else {
    f.i = (f.i + d + f.events.length) % f.events.length;
  }
  renderFind();
  showMatch(f.events[f.i]);
}

// showMatch scrolls to a matching step, opening a folded tool call or
// thinking block so the match is on screen.
function showMatch(ev, field) {
  const row = document.getElementById("ev-" + ev);
  const d = row && $("details", row);
  if (d && field !== "prompt" && field !== "reply") d.open = true;
  focusEvent(ev);
}

// openMatch opens a session at one step of a transcript search, keeping every
// matching step marked on its trace.
function openMatch(sh, hit, q) {
  state.find = { q, key: sh.key, events: sh.events, i: sh.events.indexOf(hit.event) };
  if (state.current === sh.key && state.transcript?.session.key === sh.key) {
    renderFind();
    trace.draw();
    showMatch(hit.event, hit.field);
    return;
  }
  state.pendingJump = { event: hit.event, field: hit.field };
  go(sh.key);
}

/* ---------------- changes ---------------- */
function renderChangesPane() {
  const t = state.transcript;
  const pane = $(".pane.ch");
  pane.innerHTML = "";
  const files = new Map();
  t.changes.forEach((c, i) => { if (!files.has(c.rel)) files.set(c.rel, []); files.get(c.rel).push(i); });
  const head = h(`<div class="pane-head"><div class="seg" role="tablist">
    <button role="tab" data-tab="files" aria-selected="${state.tab === "files"}">Files<span class="n">${files.size}</span></button>
    <button role="tab" data-tab="commits" aria-selected="${state.tab === "commits"}">Commits${state.commits ? `<span class="n">${state.commits.commits.length}</span>` : ""}</button>
  </div><span class="spacer"></span>${state.tab === "files" && t.changes.length ? `<span class="hint">${plural(t.changes.length, "edit")} from the transcript</span>` : ""}</div>`);
  $$("[data-tab]", head).forEach((b) => (b.onclick = () => { state.tab = b.dataset.tab; renderChangesPane(); }));
  pane.append(head);
  if (state.tab === "commits") return renderCommits(pane);

  if (!t.changes.length) {
    pane.append(h(`<div class="empty-pane"><strong>No file edits in this session</strong>The agent only read files or ran commands. Anything committed while it ran is under Commits.</div>`));
    return;
  }
  const list = h(`<div class="files" role="group" aria-label="Changed files"></div>`);
  for (const [rel, idx] of files) {
    let add = 0, del = 0;
    idx.forEach((i) => { add += state.diffs[i].add; del += state.diffs[i].del; });
    const slash = rel.lastIndexOf("/");
    const b = h(`<button class="frow" aria-pressed="${state.fileFilter === rel}" title="${esc(rel)}${idx.length > 1 ? ` (${idx.length} edits)` : ""}">
      <span class="fp"><span class="dir">${esc(slash >= 0 ? rel.slice(0, slash + 1) : "")}</span>${esc(rel.slice(slash + 1))}</span>
      <span class="nums">${counts({ add, del })}</span>${bar5(add, del)}</button>`);
    b.onclick = () => { state.fileFilter = state.fileFilter === rel ? null : rel; renderChangesPane(); };
    list.append(b);
  }
  pane.append(list);
  if (state.fileFilter) pane.append(h(`<div class="files-note">Showing edits to one file. Select it again to show all.</div>`));

  const cards = h(`<div class="changes"></div>`);
  t.changes.forEach((c, i) => {
    if (state.fileFilter && c.rel !== state.fileFilter) return;
    const d = state.diffs[i];
    const target = c.moveTo ? ` to ${esc(tildify(c.moveTo))}` : "";
    const card = h(`<article class="change" id="ch-${i}">
      <div class="change-head"><span class="p" title="${esc(c.path)}"><bdi>${esc(c.rel)}</bdi></span>
        <span class="op ${esc(c.op)}">${esc(OPS[c.op] || c.op)}${target}</span>${c.op === "delete" ? "" : `<span class="nums mono">${counts(d)}</span>`}
        ${EXPORTED || c.op === "delete" ? "" : `<button class="jump who" title="See which session wrote each line of this file as it is now">Who wrote this</button>`}
        <button class="jump step" title="Scroll the transcript to the step that made this change">Show step</button></div></article>`);
    $(".jump.step", card).onclick = () => focusEvent(c.event, i);
    const who = $(".jump.who", card);
    if (who) who.onclick = () => goBlame(c.moveTo || c.path, 0, state.current);
    if (c.op === "delete") card.append(h(`<div class="note">The agent deleted this file.</div>`));
    else if (!d.hunks.length || d.hunks.every((r) => !r.length)) card.append(h(`<div class="note">No content recorded for this change.</div>`));
    for (const rows of d.hunks) {
      const hk = h(`<div class="hunk"></div>`);
      hk.append(renderDiffRows(rows, c.op === "write" || c.op === "add" ? WRITE_PREVIEW : 0));
      card.append(hk);
    }
    if (c.op === "write") card.append(h(`<div class="note">Whole-file write. The transcript doesn’t record what was there before, so every line shows as added.</div>`));
    cards.append(card);
  });
  pane.append(cards);
}

function flash(el) {
  if (!el) return;
  el.classList.remove("flash");
  void el.offsetWidth;
  el.classList.add("flash");
}

function markChange(ci) {
  $$(".clink[aria-current]").forEach((b) => b.removeAttribute("aria-current"));
  $$(`.clink[data-change="${ci}"]`).forEach((b) => b.setAttribute("aria-current", "true"));
}

function focusChange(i) {
  if (state.tab !== "files" || (state.fileFilter && state.fileFilter !== state.transcript.changes[i].rel)) {
    state.tab = "files";
    state.fileFilter = null;
    renderChangesPane();
  }
  setPane("ch");
  markChange(i);
  const card = document.getElementById("ch-" + i);
  if (card) { card.scrollIntoView({ block: "start", behavior: "smooth" }); flash(card); }
}

function focusEvent(ev, ci) {
  const e = state.transcript.events[ev];
  if (e && !visibleUnder(e, state.filter)) { state.filter = "all"; applyFilter(); }
  setPane("log");
  const row = document.getElementById("ev-" + ev);
  if (!row) return;
  $(".pane.log").scrollTo({ top: row.offsetTop - 120, behavior: "smooth" });
  flash($(".body", row));
  markChange(ci);
}

/* ---------------- commits ---------------- */
async function renderCommits(pane) {
  const key = state.current;
  if (!state.commits) {
    pane.append(h(`<div class="loading">Reading git history…</div>`));
    try { state.commits = await api("/api/commits?id=" + encodeURIComponent(key)); }
    catch (e) { state.commits = { available: false, reason: e.message, commits: [] }; }
    if (state.current !== key || state.tab !== "commits") return;
    return renderChangesPane();
  }
  const r = state.commits;
  if (!r.available) {
    pane.append(h(EXPORTED
      ? `<div class="empty-pane"><strong>No commits in this export</strong>When it was exported: ${esc(r.reason)}</div>`
      : `<div class="empty-pane"><strong>Commits aren’t available</strong>${esc(r.reason)}</div>`));
    return;
  }
  if (EXPORTED) pane.append(h(`<div class="window">Commits on any branch of <span class="mono">${esc(tildify(r.toplevel))}</span> from 10 minutes before this session started to 2 hours after it ended that touch files the agent edited. Other commits in that window aren’t part of the export.</div>`));
  else pane.append(h(`<div class="window">Commits on any branch of <span class="mono">${esc(tildify(r.toplevel))}</span> from 10 minutes before this session started to 2 hours after it ended. Commits that touch files the agent edited come first.</div>`));
  if (!r.commits.length) {
    pane.append(h(`<div class="empty-pane"><strong>No commits in that window</strong>${EXPORTED ? "None of the commits made while this session ran touch the files it edited." : "The changes may still be uncommitted, or were committed later."}</div>`));
    return;
  }
  const list = h(`<div class="changes"></div>`);
  const sorted = [...r.commits].sort((a, b) => b.overlap.length - a.overlap.length || (a.when < b.when ? 1 : -1));
  for (const c of sorted) {
    const squares = c.files.slice(0, 12).map((f) => `<i class="${c.overlap.includes(f) ? "on" : ""}"></i>`).join("");
    const info = c.overlap.length
      ? `<span class="hit">${c.overlap.length} of ${plural(c.files.length, "file")} edited in this session</span>`
      : `<span>${plural(c.files.length, "file")}, none edited in this session</span>`;
    const b = h(`<button class="commit ${c.overlap.length ? "" : "dim"}" aria-expanded="false">
      <span class="sha">${esc(c.short)}</span><span class="subj">${esc(c.subject)}</span><span class="when">${esc(ago(c.when))}</span>
      <span class="info"><span class="overlap" aria-hidden="true">${squares}</span>${info}<span>${esc(c.author)}</span></span></button>`);
    let patchEl = null;
    b.onclick = async () => {
      if (patchEl) { patchEl.remove(); patchEl = null; b.setAttribute("aria-expanded", "false"); return; }
      b.setAttribute("aria-expanded", "true");
      patchEl = h(`<div class="patch"><div class="note">Loading…</div></div>`);
      b.after(patchEl);
      try {
        const { patch } = EXPORTED
          ? { patch: EXPORTED.patches?.[c.sha] ?? (() => { throw new Error("This commit’s patch couldn’t be read when the session was exported."); })() }
          : await api(`/api/commit?id=${encodeURIComponent(key)}&sha=${encodeURIComponent(c.sha)}`);
        const rows = patch.split("\n").map((l) => {
          const k = l.startsWith("diff --git") || l.startsWith("commit ") ? "f"
            : l.startsWith("@@") ? "h" : l.startsWith("+") && !l.startsWith("+++") ? "a" : l.startsWith("-") && !l.startsWith("---") ? "d" : "";
          return `<div class="${k}">${esc(l)}</div>`;
        });
        patchEl.innerHTML = `<div class="diff">${rows.join("")}</div>`;
      } catch (e) { patchEl.innerHTML = `<div class="note">${esc(e.message)}</div>`; }
    };
    list.append(b);
  }
  pane.append(list);
}

/* ---------------- who wrote this ---------------- */
// The blame view: a file as it is now, each run of lines under the session
// step that wrote it, or the commit when no session did.
const blame = { seq: 0 };

function goBlame(path, line, id) {
  const q = new URLSearchParams({ f: path });
  if (line) q.set("l", line);
  if (id && !path.startsWith("/") && !/^[A-Za-z]:[\\/]/.test(path)) q.set("id", id);
  location.hash = q.toString();
  closeDrawer();
}

function blameForm(value) {
  return `<form class="blame-form" role="search"><label>${svg("file")}<input name="f" value="${esc(value)}" placeholder="~/code/app/main.go:42" autocomplete="off" spellcheck="false" aria-label="File, with an optional line number"></label>
    <button class="btn" type="submit">Find the session</button></form>`;
}

function setupBlameForm(root) {
  const f = $(".blame-form", root);
  if (!f) return;
  f.onsubmit = (e) => {
    e.preventDefault();
    const v = f.elements.f.value.trim();
    if (!v) return;
    const m = v.match(/^(.*?):(\d+)(?:-\d+)?$/);
    goBlame(m ? m[1] : v, m ? +m[2] : 0);
  };
}

async function openBlame(path, line, id) {
  const seq = ++blame.seq;
  const view = $("#view");
  view.innerHTML = `<div class="loading">Reading ${esc(tildify(path))} and the sessions that touched it…</div>`;
  let r;
  try {
    r = await api(`/api/blame?path=${encodeURIComponent(path)}${id ? `&id=${encodeURIComponent(id)}` : ""}`);
  } catch (e) { r = { available: false, reason: e.message }; }
  if (seq !== blame.seq) return;
  if (!r.available) {
    view.innerHTML = "";
    const el = h(`<div class="home"><div class="home-inner"><h1>Who wrote this?</h1>${blameForm(path)}
      <div class="empty-pane"><strong>Couldn’t read that file</strong>${esc(r.reason)}</div></div></div>`);
    view.append(el);
    setupBlameForm(el);
    return;
  }
  renderBlame(r, line);
}

function blameStats(r) {
  const keys = new Set(r.sources.map((s) => s.key));
  const agent = r.lines.filter((l) => l.source >= 0).length;
  return { keys, agent };
}

function renderBlame(r, line) {
  const view = $("#view");
  view.innerHTML = "";
  const st = blameStats(r);
  const total = r.lines.length;
  const pct = total ? Math.round((st.agent / total) * 100) : 0;
  const el = h(`<div class="blame">
    <header class="s-head b-head"><div class="titles">
      <p class="eyebrow">Who wrote this</p>
      <h1 class="mono" title="${esc(r.path)}">${esc(r.rel)}</h1>
      <div class="meta"><span title="${esc(r.path)}">${svg("folder")}<span class="cwd"><bdi>${esc(tildify(r.toplevel || r.path))}</bdi></span></span>
        ${r.toplevel ? "" : `<span>Not in a git repository, so every line counts as uncommitted</span>`}</div>
      <div class="facts"><span><b>${st.agent}</b> of <b>${total}</b> ${total === 1 ? "line" : "lines"} (${pct}%) from ${plural(st.keys.size, "session")}</span>
        <span>${plural(r.searched, "session")} read</span></div>
    </div><div class="actions">${blameForm(r.rel === r.path ? r.path : tildify(r.path))}</div></header>
    <div class="b-body"><div class="b-who"></div><div class="b-code" role="table" aria-label="Lines of ${esc(r.rel)}"></div></div></div>`);
  setupBlameForm(el);
  view.append(el);

  // Sessions, most lines first.
  const bySession = new Map();
  r.sources.forEach((s) => {
    const e = bySession.get(s.key) || { s, lines: 0, steps: 0 };
    e.lines += s.lines; e.steps++;
    bySession.set(s.key, e);
  });
  const who = $(".b-who", el);
  if (!bySession.size) {
    who.append(h(`<div class="b-none">No session’s recorded edits contain these lines. A person wrote them, or an agent did in a way its transcript doesn’t record.</div>`));
  }
  for (const { s, lines, steps } of [...bySession.values()].sort((a, b) => b.lines - a.lines)) {
    const b = h(`<button class="b-sess" title="${esc(s.prompt || s.title)}"><span class="pmark ${esc(s.provider)}"></span>
      <span class="t">${esc(s.title)}</span><span class="n">${plural(lines, "line")}</span>
      <span class="sub">${esc(PROVIDERS[s.provider] || s.provider)} · ${plural(steps, "step")} · ${esc(ago(s.time))}</span></button>`);
    b.onclick = () => openStep(s);
    who.append(b);
  }

  const code = $(".b-code", el);
  const LIMIT = 4000;
  const draw = (upto) => {
    code.innerHTML = "";
    const width = String(total).length;
    let i = 0;
    while (i < Math.min(upto, total)) {
      const l = r.lines[i];
      let j = i + 1;
      while (j < total && r.lines[j].source === l.source && r.lines[j].commit === l.commit) j++;
      code.append(blameRun(r, i, j, width, line));
      i = j;
    }
    if (total > upto) {
      const more = h(`<button class="more btn">Show the other ${total - upto} lines</button>`);
      more.onclick = () => draw(total);
      code.append(more);
    }
  };
  draw(line > LIMIT ? total : LIMIT);
  if (r.truncated) code.append(h(`<div class="note">Only the first ${r.lines.length} lines were read.</div>`));
  if (line) {
    const row = document.getElementById("bl-" + line);
    if (row) requestAnimationFrame(() => { row.scrollIntoView({ block: "center" }); flash(row); });
  }
}

function blameRun(r, from, to, width, target) {
  const l = r.lines[from];
  const s = l.source >= 0 ? r.sources[l.source] : null;
  const c = l.commit >= 0 ? r.commits[l.commit] : null;
  const commit = c ? `<span class="sha" title="${esc(c.subject)}">${esc(c.short)}</span><span>${esc(c.author)}, ${esc(ago(c.when))}</span>`
    : `<span>Not committed yet</span>`;
  const head = s
    ? `<button class="b-run-head agent" title="${esc(s.prompt ? "Asked: " + s.prompt : s.title)}"><span class="pmark ${esc(s.provider)}"></span>
        <span class="t">${esc(s.title)}</span>
        <span class="step">Step ${s.event + 1}${s.summary ? ` · ${esc(s.summary)}` : ""}${s.op === "shell" ? ` <em title="No edit records these lines, but this shell command contains them and names the file">probably</em>` : ""}</span>
        <span class="c">${commit}</span></button>`
    : `<div class="b-run-head"><span class="t">No session</span><span class="c">${commit}</span></div>`;
  const rows = [];
  for (let i = from; i < to; i++) {
    const n = i + 1;
    rows.push(`<div class="b-line${n === target ? " on" : ""}" id="bl-${n}" role="row"><a class="ln" href="#${esc(new URLSearchParams({ f: r.path, l: n }).toString())}" role="cell">${String(n).padStart(width, " ")}</a><span class="tx" role="cell">${esc(r.lines[i].text) || " "}</span></div>`);
  }
  const run = h(`<section class="b-run ${s ? "agent " + esc(s.provider) : "none"}">${head}<div class="b-lines">${rows.join("")}</div></section>`);
  if (s) $(".b-run-head", run).onclick = () => openStep(s);
  return run;
}

// openStep opens a session at the step that wrote some lines.
function openStep(s) {
  state.pendingJump = { event: s.event, field: "input" };
  go(s.key);
}

/* ---------------- palette ---------------- */
const palette = { items: [], sel: 0, hits: null, seq: 0, timer: 0 };

function score(s, q) {
  if (!q) return 1;
  const t = s.title.toLowerCase(), p = (s.prompt || "").toLowerCase(), f = tildify(s.root).toLowerCase();
  if (t.startsWith(q)) return 100;
  if (t.includes(q)) return 80 - t.indexOf(q) / 100;
  if (f.includes(q)) return 50;
  if (p.includes(q)) return 40;
  // every word somewhere
  const words = q.split(/\s+/).filter(Boolean);
  if (words.length > 1 && words.every((w) => t.includes(w) || p.includes(w) || f.includes(w))) return 30;
  return 0;
}
function hilite(text, q) {
  const i = q ? text.toLowerCase().indexOf(q) : -1;
  if (i < 0) return esc(text);
  return esc(text.slice(0, i)) + `<mark>${esc(text.slice(i, i + q.length))}</mark>` + esc(text.slice(i + q.length));
}
const FIELD = { prompt: "Prompt", reply: "Reply", thinking: "Thinking", related: "Related" };
const snippetHTML = (segs) => segs.map((g) => (g.m ? `<mark>${esc(g.t)}</mark>` : esc(g.t))).join("");

function renderPalette() {
  const raw = $("#palette-q").value;
  const q = raw.trim().toLowerCase();
  const list = $("#palette-list");
  const byKey = new Map(state.data.sessions.map((s) => [s.key, s]));
  const marks = bookmarkItems(q).slice(0, q ? 6 : 8);
  palette.items = [...marks, ...state.data.sessions
    .map((s) => [score(s, q), s]).filter(([sc]) => sc > 0)
    .sort((a, b) => b[0] - a[0] || (a[1].ended < b[1].ended ? 1 : -1))
    .slice(0, q ? 6 : 40).map(([, s]) => ({ s }))];
  const nMarks = marks.length, nNamed = palette.items.length;
  const r = q && palette.hits?.q === raw ? palette.hits.r : null;
  if (r) for (const sh of r.sessions) if (byKey.has(sh.key) && sh.hits.length) palette.items.push({ s: byKey.get(sh.key), sh });
  palette.sel = Math.min(palette.sel, Math.max(0, palette.items.length - 1));
  list.innerHTML = "";

  palette.items.forEach((it, i) => {
    if (i === 0 && nMarks) list.append(h(`<div class="palette-group">Bookmarks</div>`));
    if (i === nMarks && i < nNamed && (q || nMarks)) list.append(h(`<div class="palette-group">${q ? "Sessions" : "Recent"}</div>`));
    if (i === nNamed) list.append(h(`<div class="palette-group">In transcripts</div>`));
    const s = it.s, where = esc([tildify(s.root), s.branch].filter(Boolean).join("   "));
    let row;
    if (it.mark || it.notes) {
      const text = it.mark ? `<span class="k">Step ${it.mark.event + 1}</span>${hilite(it.mark.note || it.mark.label, q)}` : `<span class="k">${it.notes.bookmarked ? "Session" : "Note"}</span>${it.notes.note ? hilite(it.notes.note, q) : esc(s.prompt || "")}`;
      row = h(`<button class="prow hrow mrow" role="option" aria-selected="${i === palette.sel}">${svg("star", "on")}
        <span class="t">${hilite(s.title, q)}</span><span class="when">${esc(ago(s.ended))}</span>
        <span class="snip">${text}</span>
        <span class="where">${it.mark && it.mark.note ? esc(it.mark.label) + `<span class="sep"></span>` : ""}${where}</span></button>`);
    } else if (!it.sh) {
      row = h(`<button class="prow" role="option" aria-selected="${i === palette.sel}"><span class="pmark ${esc(s.provider)}"></span>
        <span class="t">${hilite(s.title, q)}</span><span class="when">${esc(ago(s.ended))}</span>
        <span class="where">${where}</span></button>`);
    } else {
      const hit = it.sh.hits[0];
      const label = FIELD[hit.field] || (hit.field === "output" ? `${hit.tool} output` : hit.tool);
      row = h(`<button class="prow hrow" role="option" aria-selected="${i === palette.sel}"><span class="pmark ${esc(s.provider)}"></span>
        <span class="t">${esc(s.title)}</span><span class="when">${esc(ago(s.ended))}</span>
        <span class="snip"><span class="k">${esc(label)}</span>${snippetHTML(hit.snippet)}</span>
        <span class="where">${plural(it.sh.count, "matching step")}<span class="sep"></span>${where}</span></button>`);
    }
    row.onmousemove = () => { if (palette.sel !== i) { palette.sel = i; markPalette(); } };
    row.onclick = () => choosePalette(it);
    list.append(row);
  });

  if (!q) return;
  let status = "";
  if (!r) status = "Searching transcripts…";
  else if (r.building) status = `Reading transcripts, ${r.indexed} of ${r.total} so far…`;
  else if (!r.sessions.length) status = nNamed ? "No transcript matches." : `Nothing matches “${esc(raw.trim())}”.`;
  else if (r.more) status = "More sessions match. Add a word to narrow it down.";
  if (status) list.append(h(`<div class="palette-${!r || r.building || r.sessions.length ? "note" : "empty"}">${status}</div>`));
  const sem = r?.semantic;
  if (sem?.error) list.append(h(`<div class="palette-note">Related sessions are off: ${esc(sem.error)}</div>`));
  else if (sem && sem.embedded < sem.total) list.append(h(`<div class="palette-note">Finding related sessions: ${sem.embedded} of ${sem.total} read by ${esc(sem.model)} so far…</div>`));
}

async function searchTranscripts() {
  clearTimeout(palette.timer);
  const raw = $("#palette-q").value;
  if (!raw.trim() || $("#palette").hidden) return;
  const seq = ++palette.seq;
  let r;
  try { r = await api(`/api/search?limit=30&q=${encodeURIComponent(raw)}`); }
  catch (e) { r = { sessions: [], error: e.message }; }
  if (seq !== palette.seq || $("#palette").hidden) return;
  palette.hits = { q: raw, r };
  renderPalette();
  if (r.building) palette.timer = setTimeout(searchTranscripts, 800);
  else if (r.semantic && !r.semantic.error && r.semantic.embedded < r.semantic.total) palette.timer = setTimeout(searchTranscripts, 3000);
}

function choosePalette(it) {
  closePalette();
  if (it.mark) openMark(it.s.key, it.mark.event);
  else if (it.sh) openMatch(it.sh, it.sh.hits[0], palette.hits.q);
  else go(it.s.key);
}
function markPalette() {
  $$("#palette-list .prow").forEach((r, i) => r.setAttribute("aria-selected", i === palette.sel));
  $$("#palette-list .prow")[palette.sel]?.scrollIntoView({ block: "nearest" });
}
function openPalette(q = "") {
  if (!state.data) return;
  $("#palette").hidden = false;
  $("#palette-q").value = q;
  palette.sel = 0;
  palette.hits = null;
  renderPalette();
  $("#palette-q").focus();
  if (q) searchTranscripts();
}
// The input is blurred by hand: a hidden element can keep focus until the
// next frame, and keys pressed before then would be typed into it.
function closePalette() { $("#palette").hidden = true; $("#palette-q").blur(); clearTimeout(palette.timer); palette.seq++; }

/* ---------------- lessons ---------------- */
// Lessons for one project: what the person kept telling the agents and
// commands that kept failing, as rules for CLAUDE.md or AGENTS.md. trail
// only suggests them; it never writes to the project.
const retro = { seq: 0, data: null };
const retroHash = (root) => "#retro=" + encodeURIComponent(root);
const onRetro = () => location.hash.startsWith("#retro=");
const times = (n) => (n === 1 ? "once" : n === 2 ? "twice" : `${n} times`);
function agoWords(iso) {
  const a = ago(iso);
  return /^\d+[mhd]$/.test(a) ? a + " ago" : a === "now" ? "just now" : a;
}
const ruleHTML = (r) => esc(r).replace(/`([^`]+)`/g, "<code>$1</code>");

async function openRetro(root) {
  stopLive();
  state.current = null;
  renderSidebar();
  const view = $("#view");
  const n = state.data.sessions.filter((s) => s.root === root).length;
  view.innerHTML = `<div class="loading">Reading ${plural(n, "session")} in ${esc(tildify(root))}…</div>`;
  const seq = ++retro.seq;
  try {
    const d = await api("/api/retro?root=" + encodeURIComponent(root));
    if (seq !== retro.seq || !onRetro()) return;
    retro.data = d;
    renderRetro();
  } catch (e) {
    if (seq !== retro.seq) return;
    view.innerHTML = "";
    view.append(h(`<div class="empty-pane"><strong>Couldn’t read this project</strong>${esc(e.message)}</div>`));
  }
}

function renderRetro() {
  const d = retro.data, view = $("#view");
  const fresh = d.lessons.filter((l) => l.rule && !l.covered);
  const open = d.lessons.filter((l) => !l.rule && !l.covered);
  const covered = d.lessons.filter((l) => l.covered);
  const files = d.targets.map((t) => `<code>${esc(t.file)}</code>`);
  const readers = d.targets.map((t) => `${t.agents.map((a) => PROVIDERS[a] || a).join(", ")} ${t.agents.length === 1 ? "reads" : "read"} <code>${esc(t.file)}</code>`).join("; ");
  const el = h(`<div class="home retro"><div class="home-inner">
    <p class="eyebrow">${svg("lesson")}Lessons</p>
    <h1 title="${esc(d.root)}">${esc(tildify(d.root))}</h1>
    <p class="lede">From ${plural(d.sessions, "session")}: what you kept telling the agents, and commands that kept failing the same way, as rules worth adding to ${files.join(" or ") || "your instructions file"}. trail only suggests them; it never edits your files.</p>
    ${d.targets.length ? `<p class="retro-files">${readers}.${d.files.length ? ` Found ${d.files.map((f) => `<code>${esc(f)}</code>`).join(", ")}.` : ""}</p>` : ""}
    </div></div>`);
  const inner = $(".home-inner", el);
  if (!d.lessons.length) {
    inner.append(h(`<div class="retro-empty"><strong>Nothing repeats yet</strong>No instruction you gave in more than one session, and no command that kept failing the same way. Lessons show up here once something does.</div>`));
  }
  if (fresh.length) {
    const head = h(`<div class="retro-head"><h2>Suggested rules</h2><button class="btn" id="retro-copy">${svg("copy")}<span>Copy rules</span></button></div>`);
    inner.append(head);
    const list = h(`<div class="lessons"></div>`);
    for (const l of fresh) list.append(lessonCard(l, true));
    inner.append(list);
    const copy = $("#retro-copy", head);
    const label = () => {
      const n = $$(".lesson input:checked", list).length;
      $("span", copy).textContent = n === fresh.length ? "Copy rules" : `Copy ${plural(n, "rule")}`;
      copy.disabled = !n;
    };
    list.addEventListener("change", label);
    copy.onclick = async () => {
      const text = $$(".lesson", list).filter((c) => $("input", c).checked).map((c) => "- " + c.dataset.rule).join("\n") + "\n";
      try {
        await navigator.clipboard.writeText(text);
        copy.classList.add("done"); $("svg", copy).innerHTML = ICON.check; $("span", copy).textContent = "Copied";
        setTimeout(() => { copy.classList.remove("done"); $("svg", copy).innerHTML = ICON.copy; label(); }, 1800);
      } catch { $("span", copy).textContent = "Couldn’t copy"; }
    };
  }
  if (open.length) {
    inner.append(h(`<h2>Kept failing, no fix seen</h2>`));
    inner.append(h(`<p class="retro-note">trail saw these fail again and again but not what works instead. A line on how to run them saves the next session the same detour.</p>`));
    const list = h(`<div class="lessons"></div>`);
    for (const l of open) list.append(lessonCard(l, false));
    inner.append(list);
  }
  if (covered.length) {
    inner.append(h(`<h2>Already in your instructions</h2>`));
    inner.append(h(`<p class="retro-note">Your instructions seem to say these already, yet they still came up. They may need to be clearer, or nearer the top.</p>`));
    const list = h(`<div class="lessons"></div>`);
    for (const l of covered) list.append(lessonCard(l, false));
    inner.append(list);
  }
  view.innerHTML = "";
  view.append(el);
}

function lessonCard(l, pick) {
  const cmd = (c) => `<code>${esc(c)}</code>`;
  const where = `in ${plural(l.sessions, "session")}, last ${esc(agoWords(l.last))}`;
  const why = l.kind === "correction" ? `You said this ${times(l.count)} ${where}.`
    : l.kind === "rejected" ? `You refused ${cmd(l.command)} ${times(l.count)} ${where}.`
    : `${cmd(l.command)} failed ${times(l.count)} ${where}.${l.fix ? ` ${cmd(l.fix)} worked right after ${l.fixes === 1 ? "one" : l.fixes === l.count ? (l.count === 2 ? "both" : "all of them") : `${l.fixes} of them`}.` : ""}`;
  const title = l.rule ? ruleHTML(l.rule) : `${cmd(l.command)} keeps failing`;
  const el = h(`<article class="lesson ${esc(l.kind)}" data-rule="${esc(l.rule || "")}">
    ${pick ? `<label class="l-pick"><input type="checkbox" checked aria-label="Include this rule when copying"></label>` : ""}
    <div class="l-body">
      <div class="l-rule">${title}${l.covered ? `<span class="l-in">In ${esc(l.covered)}</span>` : ""}</div>
      <p class="l-why">${why}</p>
      ${l.error && l.kind === "command" ? `<p class="l-err">${esc(l.error)}</p>` : ""}
      <details class="l-seen"><summary>${l.seen.length < l.count ? `Latest ${l.seen.length} of ${l.count}` : plural(l.seen.length, "step")}</summary><div class="l-rows"></div></details>
    </div></article>`);
  const rows = $(".l-rows", el);
  for (const e of l.seen) {
    const prov = e.key.split(":")[0];
    const what = l.kind === "correction" ? `“${esc(e.text)}”` : `<span class="mono">$ ${esc(e.text)}</span>${e.fix ? `<span class="l-fix">then <span class="mono">${esc(e.fix)}</span></span>` : ""}`;
    const row = h(`<button class="l-ex" title="Open this step"><span class="pmark ${esc(prov)}"></span><span class="q">${what}</span>
      <span class="when">${esc(ago(e.time))}</span><span class="t">${esc(e.title)} · step ${e.event + 1}</span></button>`);
    row.onclick = () => { state.pendingJump = { event: e.event, field: l.kind === "correction" ? "prompt" : "tool" }; go(e.key); };
    rows.append(row);
  }
  return el;
}

/* ---------------- stats ---------------- */
const RANGES = [["7", "7 days"], ["30", "30 days"], ["90", "90 days"], ["365", "1 year"], ["all", "All time"]];
const NAMED_PROJECTS = 5; // the rest of the stack folds into Other
const FEW_CALLS = 20; // below this a tool's failure rate is drawn faded
const dash = { days: "90", root: "", data: null, seq: 0, timer: 0, table: new Set(), colors: new Map(), ro: null };

const compact = (n) => (n >= 1e6 ? (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + "M" : n >= 1e4 ? Math.round(n / 1e3) + "K" : n >= 1e3 ? (n / 1e3).toFixed(1) + "K" : String(n)).replace(".0", "");
const rate = (n, of) => (of ? (100 * n / of).toFixed(1).replace(/\.0$/, "") + "%" : "—");
const baseName = (p) => (p === "(unknown)" ? p : p.split("/").filter(Boolean).pop() || p);

function statsQuery() {
  const p = new URLSearchParams({ days: dash.days });
  if (dash.root) p.set("root", dash.root);
  const shown = state.data.providers.filter((x) => !state.hidden.has(x.name)).map((x) => x.name);
  if (shown.length < state.data.providers.length) p.set("agents", shown.join(",") || "-");
  return p.toString();
}

function openStats() {
  stopLive();
  state.current = null;
  renderSidebar();
  const m = location.hash.match(/^#stats\??(.*)$/);
  const p = new URLSearchParams(m ? m[1] : "");
  if (RANGES.some(([v]) => v === p.get("days"))) dash.days = p.get("days");
  dash.root = p.get("root") || "";
  loadStats();
}

function statsHash() {
  const p = new URLSearchParams({ days: dash.days });
  if (dash.root) p.set("root", dash.root);
  return "#stats?" + p;
}

async function loadStats() {
  const seq = ++dash.seq;
  clearTimeout(dash.timer);
  if (!$(".stats", $("#view"))) renderStatsShell();
  $(".stats-body")?.classList.add("stale");
  try {
    const d = await api("/api/stats?" + statsQuery());
    if (seq !== dash.seq) return;
    dash.data = d;
    renderStats();
    // Tool and file numbers come from reading whole transcripts, which
    // happens in the background after trail starts.
    if (d.read < d.total) dash.timer = setTimeout(() => location.hash.startsWith("#stats") && loadStats(), 1200);
  } catch (e) {
    if (seq === dash.seq) $(".stats-body").innerHTML = `<div class="empty-pane"><strong>Couldn’t load stats</strong>${esc(e.message)}</div>`;
  }
}

function renderStatsShell() {
  const view = $("#view");
  view.innerHTML = "";
  const roots = [...new Set(state.data.sessions.map((s) => s.root))].sort((a, b) => tildify(a).localeCompare(tildify(b)));
  if (dash.root && !roots.includes(dash.root)) roots.unshift(dash.root);
  const page = h(`<div class="stats"><div class="stats-inner">
    <header class="stats-head"><h1>Stats</h1>
      <div class="stats-filters">
        <div class="seg" role="group" aria-label="Time range">${RANGES.map(([v, l]) => `<button data-days="${v}" aria-pressed="${v === dash.days}">${l}</button>`).join("")}</div>
        <select class="select" aria-label="Project"><option value="">All projects</option>${roots.map((r) => `<option value="${esc(r)}" ${r === dash.root ? "selected" : ""}>${esc(tildify(r))}</option>`).join("")}</select>
        <span class="stats-agents"></span>
      </div>
    </header>
    <div class="stats-body"></div></div></div>`);
  for (const b of $$(".seg button", page)) b.onclick = () => { dash.days = b.dataset.days; history.replaceState(null, "", statsHash()); syncFilters(); loadStats(); };
  $("select", page).onchange = (e) => { dash.root = e.target.value; history.replaceState(null, "", statsHash()); loadStats(); };
  view.append(page);
  syncFilters();
}

function syncFilters() {
  for (const b of $$(".stats .seg button")) b.setAttribute("aria-pressed", b.dataset.days === dash.days);
  const sel = $(".stats select");
  if (sel) sel.value = dash.root;
  const hidden = state.data.providers.filter((p) => state.hidden.has(p.name));
  const note = $(".stats-agents");
  if (note) note.textContent = hidden.length ? `Leaving out ${hidden.map((p) => PROVIDERS[p.name] || p.name).join(", ")} (set in the sidebar)` : "";
}

// projectColors gives each named project a palette slot. A project keeps its
// slot while it stays on screen, so changing the range doesn't repaint it.
function projectColors(roots) {
  const out = new Map(), used = new Set();
  for (const r of roots) {
    const c = dash.colors.get(r);
    if (c && !used.has(c)) { out.set(r, c); used.add(c); }
  }
  for (const r of roots) {
    if (out.has(r)) continue;
    let c = 1;
    while (used.has(c)) c++;
    out.set(r, c); used.add(c);
  }
  for (const [r, c] of out) dash.colors.set(r, c);
  return out;
}

function bucketLabel(iso, bucket, long = false) {
  const d = new Date(iso);
  if (bucket === "month") return d.toLocaleDateString(undefined, { month: long ? "long" : "short", year: "numeric" });
  const day = d.toLocaleDateString(undefined, { month: "short", day: "numeric", ...(long ? { year: "numeric" } : {}) });
  return bucket === "week" && long ? `Week of ${day}` : day;
}

function renderStats() {
  const d = dash.data, t = d.totals, body = $(".stats-body");
  syncFilters();
  hideTip();
  body.classList.remove("stale");
  body.innerHTML = "";
  dash.ro?.disconnect();
  if (!t.sessions) {
    const ways = ["pick a longer range", dash.root && "another project", state.hidden.size && "turn an agent back on in the sidebar"].filter(Boolean);
    const say = ways.length > 1 ? ways.slice(0, -1).join(", ") + " or " + ways.at(-1) : ways[0];
    body.append(h(`<div class="empty-pane"><strong>No sessions in this range</strong>${esc(say[0].toUpperCase() + say.slice(1))}.</div>`));
    return;
  }
  const reading = d.read < d.total;
  if (reading) body.append(h(`<p class="stats-note">Reading transcripts: ${d.read} of ${d.total}. Tool calls and files fill in as they are read.</p>`));

  const per = { day: "day", week: "week", month: "month" }[d.bucket];
  const u = t.usage;
  body.append(h(`<div class="tiles">
    <div class="tile"><span class="label">Sessions</span><span class="value">${compact(t.sessions)}</span><span class="sub">${u ? `in ${plural(t.projects, "project")}, ` : ""}on ${plural(t.activeDays, "day")}</span></div>
    ${u ? "" : `<div class="tile"><span class="label">Projects</span><span class="value">${compact(t.projects)}</span><span class="sub">${plural(d.agents.length, "agent")}</span></div>`}
    <div class="tile"><span class="label">Edits</span><span class="value">${compact(t.edits)}</span><span class="sub">to ${plural(t.files, "file")}</span></div>
    <div class="tile"><span class="label">Failed tool calls</span><span class="value">${rate(t.failed, t.tools)}</span><span class="sub">${compact(t.failed)} of ${plural(t.tools, "call")}</span></div>
    ${u ? `<div class="tile" title="${esc(usageLines(u))}"><span class="label">${usagePref.tokens ? "Tokens" : "API-equivalent cost"}</span><span class="value">${esc(usagePref.tokens ? fmtTokens(totalTokens(u)) : costLabel(u))}</span><span class="sub">${esc(usagePref.tokens ? costLabel(u) + " API-equivalent" : fmtTokens(totalTokens(u)) + " tokens")}</span></div>` : ""}
  </div>`));

  // Sessions over time, stacked by project.
  const named = d.projects.slice(0, NAMED_PROJECTS).map((p) => p.root);
  const colors = projectColors(named);
  const series = [...named.map((r) => ({ key: r, name: baseName(r), title: tildify(r), cls: `s${colors.get(r)}` }))];
  const otherRoots = d.projects.length > named.length;
  if (otherRoots) series.push({ key: "", name: `Other (${d.projects.length - named.length})`, title: "Every other project", cls: "other" });
  const rows = d.series.map((b) => {
    const vals = named.map((r) => b.byRoot[r] || 0);
    if (otherRoots) vals.push(Object.entries(b.byRoot).reduce((n, [r, v]) => (named.includes(r) ? n : n + v), 0));
    return { label: bucketLabel(b.start, d.bucket), long: bucketLabel(b.start, d.bucket, true), vals, total: vals.reduce((a, v) => a + v, 0) };
  });
  const sessCard = card("sessions", `Sessions per ${per}`, series.length > 1 ? legend(series) : `<p class="card-sub">${esc(series[0].title)}</p>`);
  body.append(sessCard);
  const drawSessions = () => {
    const box = $(".card-body", sessCard);
    if (dash.table.has("sessions")) return tableView(box, [d.bucket[0].toUpperCase() + d.bucket.slice(1), ...series.map((s) => s.name), "Total"], rows.filter((r) => r.total).map((r) => [r.long, ...r.vals, r.total]));
    stackedColumns(box, rows, series, { fmt: compact, total: (v) => plural(v, "session") });
  };

  // Tokens or API-equivalent cost over time, stacked the same way.
  let drawUsage = () => {};
  const usageCard = u && card("usage", `${usagePref.tokens ? "Tokens" : "API-equivalent cost"} per ${per}`,
    `<p class="card-sub">${usagePref.tokens ? "Tokens read and written" : "What the tokens would cost at API list prices"}${u.unpriced ? "; some models have no price, so this is a floor" : ""}. <button class="linkish" data-usage>${usagePref.tokens ? "Show cost" : "Show tokens"}</button></p>`);
  if (usageCard) {
    body.append(usageCard);
    $("[data-usage]", usageCard).onclick = () => setUsagePref(!usagePref.tokens);
    const key = usagePref.tokens ? "tokensByRoot" : "costByRoot";
    const fmt = usagePref.tokens ? fmtTokens : (v) => (v > 0 && v < 1 ? "$" + v.toFixed(2).replace(/^0/, "") : fmtCost(v).replace(/\.00$/, ""));
    const uRows = d.series.map((b) => {
      const by = b[key] || {};
      const vals = named.map((r) => by[r] || 0);
      if (otherRoots) vals.push(Object.entries(by).reduce((n, [r, v]) => (named.includes(r) ? n : n + v), 0));
      return { label: bucketLabel(b.start, d.bucket), long: bucketLabel(b.start, d.bucket, true), vals, total: vals.reduce((a, v) => a + v, 0) };
    });
    const show = usagePref.tokens ? fmtTokens : fmtCost;
    drawUsage = () => {
      const box = $(".card-body", usageCard);
      if (dash.table.has("usage")) return tableView(box, [d.bucket[0].toUpperCase() + d.bucket.slice(1), ...series.map((s) => s.name), "Total"], uRows.filter((r) => r.total).map((r) => [r.long, ...r.vals.map(show), show(r.total)]));
      stackedColumns(box, uRows, series, { fmt, total: (v) => (usagePref.tokens ? fmtTokens(v) + " tokens" : fmtCost(v)), value: show });
    };
  }

  // Failure rate over time.
  const failRows = d.series.map((b) => ({ label: bucketLabel(b.start, d.bucket), long: bucketLabel(b.start, d.bucket, true), tools: b.tools, failed: b.failed }));
  const failCard = card("failures", `Failed tool calls per ${per}`, `<p class="card-sub">Share of tool calls that returned an error</p>`);
  body.append(failCard);
  const drawFailures = () => {
    const box = $(".card-body", failCard);
    if (dash.table.has("failures")) return tableView(box, [d.bucket[0].toUpperCase() + d.bucket.slice(1), "Tool calls", "Failed", "Rate"], failRows.filter((r) => r.tools).map((r) => [r.long, r.tools, r.failed, rate(r.failed, r.tools)]));
    if (!t.tools) { box.innerHTML = `<p class="card-empty">${reading ? "Still reading transcripts." : "No tool calls in this range."}</p>`; return; }
    rateLine(box, failRows);
  };

  const pair = h(`<div class="card-pair"></div>`);
  body.append(pair);

  const multi = !dash.root && t.projects > 1;
  const fileCard = card("files", "Most edited files", `<p class="card-sub">Edits recorded in transcripts${multi ? ", across projects" : ""}</p>`, false);
  const maxEdits = Math.max(1, ...d.files.map((f) => f.edits));
  $(".card-body", fileCard).append(d.files.length ? h(`<ol class="bars">${d.files.map((f) => {
    const where = multi && !f.path.startsWith("/") ? baseName(f.root) + "/" : "";
    return `<li title="${esc(f.path.startsWith("/") ? tildify(f.path) : tildify(f.root) + "/" + f.path)}"><span class="name"><span class="path rtl"><bdi><span class="pre">${esc(where)}</span>${esc(f.path.startsWith("/") ? tildify(f.path) : f.path)}</bdi></span></span>
      <span class="track"><span class="fill edits" style="width:${(100 * f.edits) / maxEdits}%"></span></span>
      <span class="val"><b>${f.edits}</b> in ${plural(f.sessions, "session")}</span></li>`;
  }).join("")}</ol>`) : h(`<p class="card-empty">${reading ? "Still reading transcripts." : "No edits in this range."}</p>`));
  pair.append(fileCard);

  const toolCard = card("tools", "Failure rate by tool", `<p class="card-sub">The most used tools, by how often a call failed</p>`, false);
  const maxRate = Math.max(0.0001, ...d.tools.map((r) => r.failed / r.calls));
  $(".card-body", toolCard).append(d.tools.length ? h(`<ol class="bars">${d.tools.map((r) => `<li class="${r.calls < FEW_CALLS ? "few" : ""}" title="${esc(PROVIDERS[r.provider] || r.provider)}: ${esc(r.tool)}${r.calls < FEW_CALLS ? " (too few calls for the rate to mean much)" : ""}">
      <span class="name"><span class="pmark ${esc(r.provider)}"></span><span class="path">${esc(r.tool)}</span></span>
      <span class="track"><span class="fill fails" style="width:${(100 * r.failed / r.calls) / maxRate}%"></span></span>
      <span class="val"><b>${rate(r.failed, r.calls)}</b> ${compact(r.failed)} of ${compact(r.calls)}</span></li>`).join("")}</ol>`) : h(`<p class="card-empty">${reading ? "Still reading transcripts." : "No tool calls in this range."}</p>`));
  pair.append(toolCard);

  // Projects and agents, as tables: they are the table view for the tiles.
  if (!dash.root) {
    const projCard = card("projects", "Projects", "", false);
    const tb = h(`<table class="stats-table"><thead><tr><th>Project</th><th>Sessions</th><th>Edits</th><th>Failed calls</th>${u ? `<th>${usagePref.tokens ? "Tokens" : "Cost"}</th>` : ""}<th>Last session</th></tr></thead><tbody></tbody></table>`);
    for (const p of d.projects) {
      const c = colors.get(p.root);
      const tr = h(`<tr tabindex="0" title="Show only ${esc(tildify(p.root))}"><td><span class="cell"><span class="key ${c ? "s" + c : "other"}"></span><span class="proj">${esc(tildify(p.root))}</span></span></td>
        <td>${p.sessions}</td><td>${p.edits}</td><td>${rate(p.failed, p.tools)}</td>${u ? `<td>${p.usage ? esc(usageShort(p.usage)) : "—"}</td>` : ""}<td>${esc(ago(p.last))}</td></tr>`);
      const pick = () => { dash.root = p.root; history.replaceState(null, "", statsHash()); loadStats(); $(".stats").scrollTop = 0; };
      tr.onclick = pick;
      tr.onkeydown = (e) => { if (e.key === "Enter") pick(); };
      $("tbody", tb).append(tr);
    }
    $(".card-body", projCard).append(tb);
    body.append(projCard);
  }
  const agentCard = card("agents", "Agents", "", false);
  const ta = h(`<table class="stats-table"><thead><tr><th>Agent</th><th>Sessions</th><th>Edits</th><th>Tool calls</th><th>Failed calls</th>${u ? `<th>${usagePref.tokens ? "Tokens" : "Cost"}</th>` : ""}</tr></thead><tbody></tbody></table>`);
  for (const a of d.agents) {
    $("tbody", ta).append(h(`<tr><td><span class="cell"><span class="pmark ${esc(a.provider)}"></span>${esc(PROVIDERS[a.provider] || a.provider)}</span></td>
      <td>${a.sessions}</td><td>${a.edits}</td><td>${a.tools}</td><td>${rate(a.failed, a.tools)}</td>${u ? `<td>${a.usage ? esc(usageShort(a.usage)) : "—"}</td>` : ""}</tr>`));
  }
  $(".card-body", agentCard).append(ta);
  body.append(agentCard);

  const draw = () => { drawSessions(); drawUsage(); drawFailures(); };
  draw();
  for (const [name, c, fn] of [["sessions", sessCard, drawSessions], ["usage", usageCard, drawUsage], ["failures", failCard, drawFailures]]) {
    if (!c) continue;
    const b = $(".card-toggle", c);
    b.onclick = () => {
      dash.table.has(name) ? dash.table.delete(name) : dash.table.add(name);
      b.setAttribute("aria-pressed", dash.table.has(name));
      fn();
    };
  }
  let w = $(".stats-body").clientWidth;
  dash.ro = new ResizeObserver(() => { const now = $(".stats-body")?.clientWidth; if (now && now !== w) { w = now; draw(); } });
  dash.ro.observe(body);
}

function card(name, title, extra, toggle = true) {
  return h(`<section class="card" data-card="${name}"><div class="card-head"><div><h2>${esc(title)}</h2>${extra}</div>
    ${toggle ? `<button class="card-toggle" aria-pressed="${dash.table.has(name)}">Table</button>` : ""}</div><div class="card-body"></div></section>`);
}

function legend(series) {
  return `<ul class="legend">${series.map((s) => `<li title="${esc(s.title)}"><span class="key ${s.cls}"></span>${esc(s.name)}</li>`).join("")}</ul>`;
}

function tableView(box, head, rows) {
  box.innerHTML = "";
  const t = h(`<div class="table-scroll"><table class="stats-table"><thead><tr>${head.map((x) => `<th>${esc(x)}</th>`).join("")}</tr></thead><tbody></tbody></table></div>`);
  for (const r of rows) $("tbody", t).append(h(`<tr>${r.map((v) => `<td>${esc(v)}</td>`).join("")}</tr>`));
  if (!rows.length) $("tbody", t).append(h(`<tr><td colspan="${head.length}">Nothing in this range.</td></tr>`));
  box.append(t);
}

// niceMax picks a tick step of 1, 2 or 5 times a power of ten that gives at
// most five gridlines, and rounds the axis maximum up to it.
function niceMax(v) {
  if (v <= 0) return { max: 1, step: 1 };
  for (let p = 10 ** Math.floor(Math.log10(v / 5)); ; p *= 10) {
    for (const m of [1, 2, 5]) {
      const step = m * p;
      if (Math.ceil(v / step - 1e-9) <= 5) return { max: Math.ceil(v / step - 1e-9) * step, step };
    }
  }
}

const NS = "http://www.w3.org/2000/svg";
function sv(tag, attrs, parent) {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  parent?.append(n);
  return n;
}

// roundTop is a column segment with 4px corners on its free end only.
function roundTop(x, y, w, hgt, r) {
  r = Math.min(r, w / 2, hgt);
  return `M${x},${y + hgt}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + hgt}Z`;
}

function chartFrame(box, height) {
  box.innerHTML = "";
  const W = Math.max(280, box.clientWidth), H = height;
  const pad = { l: 36, r: 8, t: 8, b: 26 };
  const svgEl = sv("svg", { class: "chart", viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img" });
  box.append(svgEl);
  return { svgEl, W, H, pad, iw: W - pad.l - pad.r, ih: H - pad.t - pad.b };
}

function yAxis(f, max, step, fmt) {
  const g = sv("g", { class: "axis" }, f.svgEl);
  for (let i = 0; i * step <= max + 1e-9; i++) {
    const v = i * step;
    const y = f.pad.t + f.ih - (v / max) * f.ih;
    sv("line", { x1: f.pad.l, x2: f.W - f.pad.r, y1: y, y2: y, class: v === 0 ? "base" : "grid" }, g);
    sv("text", { x: f.pad.l - 8, y: y + 4, "text-anchor": "end" }, g).textContent = fmt(v);
  }
}

function xLabels(f, rows, x) {
  const g = sv("g", { class: "axis" }, f.svgEl);
  const every = Math.ceil(rows.length / Math.max(2, Math.floor(f.iw / 64)));
  rows.forEach((r, i) => {
    if (i % every) return;
    sv("text", { x: x(i), y: f.H - 6, "text-anchor": "middle" }, g).textContent = r.label;
  });
}

function showTip(html, x, y) {
  const tip = $("#tip");
  tip.innerHTML = html;
  tip.hidden = false;
  const r = tip.getBoundingClientRect();
  tip.style.left = `${Math.max(8, Math.min(window.innerWidth - r.width - 8, x - r.width / 2))}px`;
  tip.style.top = `${y - r.height - 12 < 8 ? y + 18 : y - r.height - 12}px`;
}
const hideTip = () => { $("#tip").hidden = true; };

// stackedColumns draws one column per bucket. opts.fmt labels the axis,
// opts.total the column's total and opts.value each series in the tooltip.
function stackedColumns(box, rows, series, opts) {
  const value = opts.value || String;
  const f = chartFrame(box, 240);
  const { max, step } = niceMax(Math.max(...rows.map((r) => r.total)));
  yAxis(f, max, step, opts.fmt);
  const band = f.iw / rows.length, bw = Math.max(2, Math.min(24, band * 0.62));
  const x = (i) => f.pad.l + band * i + band / 2;
  xLabels(f, rows, x);
  const y = (v) => (v / max) * f.ih;
  const g = sv("g", {}, f.svgEl);
  rows.forEach((r, i) => {
    const col = sv("g", { class: "col", tabindex: 0, "aria-label": `${r.long}: ${opts.total(r.total)}` }, g);
    sv("rect", { class: "hit", x: f.pad.l + band * i, y: f.pad.t, width: band, height: f.ih }, col);
    let top = f.pad.t + f.ih;
    const segs = r.vals.map((v, si) => ({ v, si })).filter((s) => s.v > 0);
    segs.forEach(({ v, si }, k) => {
      const hgt = Math.max(1, y(v) - (k ? 2 : 0)); // 2px surface gap between segments
      top -= hgt + (k ? 2 : 0);
      const last = k === segs.length - 1;
      sv(last ? "path" : "rect", last ? { d: roundTop(x(i) - bw / 2, top, bw, hgt, 4), class: `seg ${series[si].cls}` } : { x: x(i) - bw / 2, y: top, width: bw, height: hgt, class: `seg ${series[si].cls}` }, col);
    });
    const tip = () => {
      const lines = series.map((s, si) => (r.vals[si] ? `<div class="tip-row"><span class="tip-key ${s.cls}"></span><b>${esc(value(r.vals[si]))}</b> <span class="k">${esc(s.name)}</span></div>` : "")).join("");
      const b = col.getBoundingClientRect();
      showTip(`<div class="k">${esc(r.long)}</div><div><b>${esc(opts.total(r.total))}</b></div>${lines}`, b.left + b.width / 2, b.top + f.ih - y(r.total));
    };
    col.addEventListener("pointerenter", tip);
    col.addEventListener("focus", tip);
    col.addEventListener("pointerleave", hideTip);
    col.addEventListener("blur", hideTip);
  });
}

function rateLine(box, rows) {
  const f = chartFrame(box, 180);
  const rates = rows.map((r) => (r.tools ? r.failed / r.tools : null));
  const { max, step } = niceMax(Math.max(0.05, ...rates.filter((v) => v != null)));
  yAxis(f, max, step, (v) => `${+(v * 100).toFixed(1)}%`);
  const band = f.iw / rows.length;
  const x = (i) => f.pad.l + band * i + band / 2;
  xLabels(f, rows, x);
  const y = (v) => f.pad.t + f.ih - (v / max) * f.ih;
  // One path per run of buckets that had tool calls; empty buckets are gaps.
  let d = "", area = "", run = [];
  const flush = () => {
    if (!run.length) return;
    d += run.map((i, k) => `${k ? "L" : "M"}${x(i)},${y(rates[i])}`).join("");
    area += `M${x(run[0])},${y(0)}` + run.map((i) => `L${x(i)},${y(rates[i])}`).join("") + `L${x(run.at(-1))},${y(0)}Z`;
    run = [];
  };
  rates.forEach((v, i) => (v == null ? flush() : run.push(i)));
  flush();
  sv("path", { d: area, class: "rate-area" }, f.svgEl);
  sv("path", { d, class: "rate-line" }, f.svgEl);
  rates.forEach((v, i) => {
    const alone = v != null && rates[i - 1] == null && rates[i + 1] == null;
    if (alone) sv("circle", { cx: x(i), cy: y(v), r: 4, class: "rate-dot" }, f.svgEl);
  });
  const cross = sv("line", { class: "cross", y1: f.pad.t, y2: f.pad.t + f.ih, x1: 0, x2: 0, visibility: "hidden" }, f.svgEl);
  const dot = sv("circle", { class: "rate-dot", r: 4, visibility: "hidden" }, f.svgEl);
  const hit = sv("rect", { class: "hit", x: f.pad.l, y: f.pad.t, width: f.iw, height: f.ih, tabindex: 0, "aria-label": "Failure rate over time; use the arrow keys to read each point" }, f.svgEl);
  let at = -1;
  const show = (i) => {
    at = Math.max(0, Math.min(rows.length - 1, i));
    const r = rows[at], v = rates[at];
    cross.setAttribute("x1", x(at)); cross.setAttribute("x2", x(at)); cross.setAttribute("visibility", "visible");
    if (v != null) { dot.setAttribute("cx", x(at)); dot.setAttribute("cy", y(v)); dot.setAttribute("visibility", "visible"); } else dot.setAttribute("visibility", "hidden");
    const b = f.svgEl.getBoundingClientRect(), sx = b.width / f.W;
    showTip(`<div class="k">${esc(r.long)}</div>` + (r.tools ? `<div><b>${rate(r.failed, r.tools)}</b> <span class="k">failed</span></div><div class="k">${r.failed} of ${plural(r.tools, "call")}</div>` : `<div class="k">No tool calls</div>`),
      b.left + x(at) * sx, b.top + (v != null ? y(v) : f.pad.t + f.ih) * sx);
  };
  const hide = () => { cross.setAttribute("visibility", "hidden"); dot.setAttribute("visibility", "hidden"); hideTip(); };
  hit.addEventListener("pointermove", (e) => {
    const b = f.svgEl.getBoundingClientRect();
    show(Math.floor(((e.clientX - b.left) * (f.W / b.width) - f.pad.l) / band));
  });
  hit.addEventListener("pointerleave", hide);
  hit.addEventListener("blur", hide);
  hit.addEventListener("focus", () => show(at < 0 ? rows.length - 1 : at));
  hit.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft" || e.key === "ArrowRight") { e.preventDefault(); show(at + (e.key === "ArrowLeft" ? -1 : 1)); }
  });
}

/* ---------------- routing & refresh ---------------- */
const onStats = () => location.hash.startsWith("#stats");
const onBlame = () => location.hash.startsWith("#f=");

function route() {
  const r = location.hash.match(/^#retro=(.+)$/);
  if (r) return openRetro(decodeURIComponent(r[1]));
  if (onStats()) return openStats();
  if (onBlame()) {
    const q = new URLSearchParams(location.hash.slice(1));
    stopLive();
    state.current = null;
    renderSidebar();
    return openBlame(q.get("f"), +q.get("l") || 0, q.get("id") || "");
  }
  const m = location.hash.match(/^#s=(.+)$/);
  const key = m ? decodeURIComponent(m[1]) : null;
  if (key && state.data.sessions.some((s) => s.key === key)) {
    if (key !== state.current) openSession(key);
  } else {
    stopLive();
    state.current = null;
    renderSidebar();
    renderHome();
  }
}

async function load(refresh = false) {
  const btn = $("#refresh");
  btn.classList.add("spin");
  try {
    const data = await api("/api/sessions" + (refresh ? "?refresh=1" : ""));
    // Notes edited here and not saved yet win over the server's copy.
    const notes = data.notes || {};
    for (const key of Object.keys(state.notes)) if (notesDirty(key)) notes[key] = state.notes[key];
    for (const key of Object.keys(notes)) if (notesDirty(key) && !state.notes[key]) delete notes[key];
    const marks = Object.keys(notes).sort().map((k) => k + marksShape(notes[k]) + (notes[k].note || "")).join("|");
    const sig = data.sessions.map((s) => s.key + s.ended + s.edits + s.title).join("|");
    const changed = sig !== state.sig;
    const marksChanged = marks !== state.marksSig;
    state.data = data;
    state.notes = notes;
    state.sig = sig;
    state.marksSig = marks;
    if (data.notesError && !state.notesWarned) { state.notesWarned = true; console.warn("trail: notes:", data.notesError); }
    if (changed || marksChanged || refresh) renderSidebar();
    return changed;
  } catch (e) {
    if (!state.data) $("#view").innerHTML = `<div class="empty-pane"><strong>trail isn’t responding</strong>${esc(e.message)}. Check that the trail process is still running.</div>`;
    return false;
  } finally {
    btn.classList.remove("spin");
  }
}

function closeDrawer() { $("#app").classList.remove("drawer"); }

function moveSession(dir) {
  const rows = $$(".tree .sess");
  if (!rows.length) return;
  const i = rows.findIndex((r) => r.dataset.key === state.current);
  const next = rows[i < 0 ? 0 : Math.max(0, Math.min(rows.length - 1, i + dir))];
  if (next) go(next.dataset.key);
}

async function init() {
  $("#mod-key").textContent = IS_MAC ? "⌘" : "Ctrl";
  await load();
  if (!state.data) return;
  route();
  window.addEventListener("hashchange", route);
  $("#refresh").onclick = async () => { await load(true); if (onStats()) loadStats(); else if (onRetro()) route(); else if (!state.current && !onBlame()) renderHome(); };
  $("#stats-open").onclick = () => { location.hash = statsHash(); closeDrawer(); };
  $("#q").addEventListener("input", (e) => { state.q = e.target.value; renderSidebar(); });
  $("#q").addEventListener("keydown", (e) => { if (e.key === "Escape") { e.target.value = ""; state.q = ""; renderSidebar(); e.target.blur(); } });
  $("#q").addEventListener("focus", () => ($("#q-hint").hidden = true));
  $("#q").addEventListener("blur", () => ($("#q-hint").hidden = false));
  $("#menu").onclick = () => $("#app").classList.toggle("drawer");
  $("#palette-open").onclick = () => openPalette();
  $("#palette").addEventListener("mousedown", (e) => { if (e.target.id === "palette") closePalette(); });
  $("#palette-q").addEventListener("input", () => {
    palette.sel = 0;
    renderPalette();
    clearTimeout(palette.timer);
    palette.timer = setTimeout(searchTranscripts, 140);
  });
  $("#palette-q").addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); palette.sel = Math.min(palette.items.length - 1, palette.sel + 1); markPalette(); }
    if (e.key === "ArrowUp") { e.preventDefault(); palette.sel = Math.max(0, palette.sel - 1); markPalette(); }
    if (e.key === "Enter" && palette.items[palette.sel]) choosePalette(palette.items[palette.sel]);
  });
  document.addEventListener("click", (e) => {
    if ($("#app").classList.contains("drawer") && !e.target.closest("#side, #menu")) closeDrawer();
  });
  document.addEventListener("keydown", (e) => {
    // The export dialog is modal: j/k behind it would change which session it exports.
    if ($(".dlg[open]")) return;
    const typing = /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName);
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); $("#palette").hidden ? openPalette() : closePalette(); return; }
    if (e.key === "Escape") { closePalette(); closeDrawer(); return; }
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "/") { e.preventDefault(); $("#q").focus(); }
    if (e.key === "j") moveSession(1);
    if (e.key === "k") moveSession(-1);
    if (e.key === "n" || e.key === "N") stepFind(e.shiftKey ? -1 : 1);
    if (e.key === "b") toggleBookmark();
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => trace.draw());
  setInterval(async () => {
    if (document.hidden) return;
    const changed = await load();
    if (changed && onStats()) loadStats();
    else if (changed && !state.current && !onBlame() && !onRetro()) renderHome();
    if (changed) startLive();
  }, 20000);
}

if (EXPORTED) initExport(); else init();

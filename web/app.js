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
  return [s.title, s.prompt, s.cwd, s.branch, s.id].some((v) => v && v.toLowerCase().includes(q));
}
function visibleSessions() {
  const q = state.q.trim().toLowerCase();
  return state.data.sessions.filter((s) => !state.hidden.has(s.provider) && (!q || matches(s, q)));
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
    };
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
  if (!sessions.length) {
    tree.append(h(`<div class="empty">No session name matches “${esc(state.q)}”. Clear the filter or turn a provider back on.</div>`));
    tree.append(deepSearchRow());
    return;
  }
  const filtering = state.q.trim() !== "";
  const byRecent = (a, b) => (a.latest < b.latest ? 1 : -1);
  const folder = (node) => {
    const open = filtering || state.expanded.get(node.path) !== false;
    const slash = node.name.lastIndexOf("/");
    const pre = slash > 0 ? node.name.slice(0, slash + 1) : "";
    const last = slash > 0 ? node.name.slice(slash + 1) : node.name;
    const el = h(`<div class="folder" aria-expanded="${open}">
      <button class="folder-row" title="${esc(node.path)}">${svg("down", "chev")}${svg("folder", "ficon")}
        <span class="name"><span class="pre">${esc(pre)}</span>${esc(last)}</span>${node.usage ? `<span class="fcost" title="${esc((usagePref.tokens ? "Tokens" : "API-equivalent cost") + " of every session in this folder\n\n" + usageLines(node.usage))}">${esc(usageShort(node.usage))}</span>` : ""}<span class="count">${node.count}</span>
      </button><div class="kids"></div></div>`);
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
  if (filtering) tree.append(deepSearchRow());
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
  const tip = s.titleKind !== "prompt" && s.prompt ? `${s.title}\n\n${s.prompt}` : s.prompt || s.title;
  const el = h(`<button class="sess" data-key="${esc(s.key)}" aria-current="${s.key === state.current}" title="${esc(tip)}">
    <span class="pmark ${esc(s.provider)}" aria-label="${esc(PROVIDERS[s.provider] || s.provider)}"></span>
    <span class="t ${s.titleKind === "prompt" ? "unnamed" : ""}">${esc(s.title)}</span><span class="when">${esc(ago(s.ended))}</span>
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
      <p class="lede">${lastDay ? `${plural(lastDay, "session")} ran in the last day. ` : ""}Open one to read what the agent did next to every file it changed.</p>
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
      <div class="actions">${EXPORTED ? exportedActions() : `<span class="live" ${live.on ? "" : "hidden"} title="The agent is still writing this session. New steps appear as it works.">Live</span><button class="btn" id="export" title="Save this session as one HTML file">${svg("download")}<span>Export</span></button><button class="btn" id="copy" title="${esc(s.resume)}">${svg("copy")}<span>Copy resume command</span></button>`}</div>
    </header>`);
  const tog = $("#usage-toggle", head);
  if (tog) tog.onclick = (e) => { e.stopPropagation(); setUsagePref(!usagePref.tokens); };
  if (EXPORTED) { setupExportFind(head); return head; }
  $("#export", head).onclick = () => exportDialog(s);
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
    <div class="dlg-actions"><button class="btn" value="cancel">Cancel</button><a class="btn primary" href="/api/export?id=${encodeURIComponent(s.key)}" download>${svg("download")}<span>Download HTML</span></a></div>
  </form></dialog>`);
  $("a", d).onclick = () => setTimeout(() => d.close(), 0);
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
      <div class="trace-legend"><span><i class="lg-prompt"></i>Prompt</span><span><i class="lg-tool"></i>Tool call</span><span><i class="lg-edit"></i>Edit</span><span><i class="lg-err"></i>Failed</span><span class="find" id="find" hidden></span></div>
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

  el.prepend(sessionHeader());
  $$(".pane-tabs button", el).forEach((b) => (b.onclick = () => setPane(b.dataset.pane)));
  $$("[data-f]", el).forEach((b) => (b.onclick = () => { state.filter = b.dataset.f; applyFilter(); }));
  view.append(el);
  renderLog($(".log-inner", el));
  applyFilter();
  renderChangesPane();
  setupTrace();
  renderFind();
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
  palette.items = state.data.sessions
    .map((s) => [score(s, q), s]).filter(([sc]) => sc > 0)
    .sort((a, b) => b[0] - a[0] || (a[1].ended < b[1].ended ? 1 : -1))
    .slice(0, q ? 6 : 40).map(([, s]) => ({ s }));
  const nNamed = palette.items.length;
  const r = q && palette.hits?.q === raw ? palette.hits.r : null;
  if (r) for (const sh of r.sessions) if (byKey.has(sh.key) && sh.hits.length) palette.items.push({ s: byKey.get(sh.key), sh });
  palette.sel = Math.min(palette.sel, Math.max(0, palette.items.length - 1));
  list.innerHTML = "";

  palette.items.forEach((it, i) => {
    if (q && i === 0 && nNamed) list.append(h(`<div class="palette-group">Sessions</div>`));
    if (i === nNamed) list.append(h(`<div class="palette-group">In transcripts</div>`));
    const s = it.s, where = esc([tildify(s.root), s.branch].filter(Boolean).join("   "));
    let row;
    if (!it.sh) {
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
  if (it.sh) openMatch(it.sh, it.sh.hits[0], palette.hits.q);
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
function closePalette() { $("#palette").hidden = true; clearTimeout(palette.timer); palette.seq++; }

/* ---------------- routing & refresh ---------------- */
function route() {
  if (location.hash.startsWith("#f=")) {
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
    const sig = data.sessions.map((s) => s.key + s.ended + s.edits + s.title).join("|");
    const changed = sig !== state.sig;
    state.data = data;
    state.sig = sig;
    if (changed || refresh) renderSidebar();
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
  $("#refresh").onclick = async () => { await load(true); if (!state.current) renderHome(); };
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
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => trace.draw());
  setInterval(async () => {
    if (document.hidden) return;
    const changed = await load();
    if (changed && !state.current) renderHome();
    if (changed) startLive();
  }, 20000);
}

if (EXPORTED) initExport(); else init();

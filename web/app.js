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
  compare: '<rect x="2.5" y="3" width="4.5" height="10" rx="1"/><rect x="9" y="3" width="4.5" height="10" rx="1"/>',
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
      if (onStats()) loadStats();
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
      <p class="lede">${lastDay ? `${plural(lastDay, "session")} ran in the last day. ` : ""}Open one to read what the agent did next to every file it changed, or see <a class="lede-link" href="#stats">stats across every project</a>.</p>
      <h2>Pick up where you left off</h2><div class="recent"></div>
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
  return seqDiff(lines(a), lines(b));
}

// seqDiff is the longest-common-subsequence diff of two lists of strings.
function seqDiff(A, B) {
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
      <div class="actions">${EXPORTED ? exportedActions() : `<span class="live" ${live.on ? "" : "hidden"} title="The agent is still writing this session. New steps appear as it works.">Live</span><button class="btn" id="compare" title="Put this session next to another one">${svg("compare")}<span>Compare</span></button><button class="btn" id="export" title="Save this session as one HTML file">${svg("download")}<span>Export</span></button><button class="btn" id="copy" title="${esc(s.resume)}">${svg("copy")}<span>Copy resume command</span></button>`}</div>
    </header>`);
  const tog = $("#usage-toggle", head);
  if (tog) tog.onclick = (e) => { e.stopPropagation(); setUsagePref(!usagePref.tokens); };
  if (EXPORTED) { setupExportFind(head); return head; }
  $("#export", head).onclick = () => exportDialog(s);
  $("#compare", head).onclick = () => compareWith(s);
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
// stepKind is how a step is drawn on a trace, and STEP_H its bar height.
function stepKind(e) {
  if (e.kind === "tool" && e.error) return "err";
  if (e.kind === "tool" && e.changes?.length) return "edit";
  return e.kind;
}
const STEP_H = { user: 0.72, edit: 0.56, err: 0.56, tool: 0.32, assistant: 0.22, thinking: 0.16 };
function traceColors() {
  const css = getComputedStyle(document.documentElement);
  const col = (v) => css.getPropertyValue(v).trim();
  return { user: col("--ink"), assistant: col("--muted"), tool: col("--faint"), thinking: col("--line"), edit: col("--blaze"), err: col("--del"), find: col("--find") };
}
// stepTip shows what a step on a trace is, under the pointer.
function stepTip(ev, x, y) {
  const tip = $("#tip");
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
}

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
    const C = traceColors();
    const step = (W - this.pad * 2) / this.n;
    const bw = Math.max(1, Math.min(3, step * 0.6));
    const mid = H / 2;
    t.events.forEach((e, i) => {
      const kind = stepKind(e), hgt = STEP_H[kind] * H;
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
      ctx.fillStyle = C.find;
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
  tip(e, x, y) { stepTip(state.transcript.events[e], x, y); },
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
        <button class="jump" title="Scroll the transcript to the step that made this change">Show step</button></div></article>`);
    $(".jump", card).onclick = () => focusEvent(c.event, i);
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

/* ---------------- compare ---------------- */
// Two sessions side by side: two agents given the same task, or one agent
// before and after a prompt change. The server works out the numbers; the
// page lays out each side's trace, prompts, edits and failures next to the
// other's.
const cmp = { a: null, b: null, data: null, seq: 0, open: new Set(), ro: null };

function compareHash(a, b) {
  return "#compare?" + new URLSearchParams(b ? { a, b } : { a }).toString();
}
function compareRoute() {
  const m = location.hash.match(/^#compare\?(.*)$/);
  if (!m) return null;
  const p = new URLSearchParams(m[1]);
  return { a: p.get("a"), b: p.get("b") };
}
const onCompare = () => location.hash.startsWith("#compare?");

// pickSession asks for a session in a dialog, nearest first: the same folder
// as near, then everything else by recency.
function pickSession({ title, near, exclude }, done) {
  $(".dlg")?.remove();
  const d = h(`<dialog class="dlg pick" aria-labelledby="pick-title"><form method="dialog">
    <h2 id="pick-title">${esc(title)}</h2>
    <div class="palette-input"><svg viewBox="0 0 16 16" aria-hidden="true">${ICON.search}</svg>
      <input placeholder="Filter by name, prompt or folder" autocomplete="off" spellcheck="false" aria-label="Filter sessions" aria-controls="pick-list"></div>
    <div class="palette-list" id="pick-list" role="listbox"></div>
    <div class="dlg-actions"><button class="btn" value="cancel">Cancel</button></div>
  </form></dialog>`);
  const input = $("input", d), list = $(".palette-list", d);
  let items = [], sel = 0;
  const draw = () => {
    const q = input.value.trim().toLowerCase();
    const all = state.data.sessions.filter((s) => s.key !== exclude);
    const byRecent = (a, b) => (a.ended < b.ended ? 1 : -1);
    let groups;
    if (q) {
      groups = [["", all.map((s) => [score(s, q), s]).filter(([sc]) => sc > 0).sort((a, b) => b[0] - a[0] || byRecent(a[1], b[1])).slice(0, 40).map(([, s]) => s)]];
    } else {
      const here = near ? all.filter((s) => s.root === near.root).sort(byRecent) : [];
      const rest = all.filter((s) => !here.includes(s)).sort(byRecent).slice(0, 40);
      groups = [[here.length ? "Same folder" : "", here], [here.length ? "Everywhere else" : "", rest]];
    }
    items = [];
    list.innerHTML = "";
    for (const [label, sessions] of groups) {
      if (!sessions.length) continue;
      if (label) list.append(h(`<div class="palette-group">${esc(label)}</div>`));
      for (const s of sessions) {
        const i = items.length;
        items.push(s);
        const where = esc([tildify(s.root), s.branch].filter(Boolean).join("   "));
        const row = h(`<button type="button" class="prow" role="option" aria-selected="${i === sel}"><span class="pmark ${esc(s.provider)}"></span>
          <span class="t">${hilite(s.title, q)}</span><span class="when">${esc(ago(s.ended))}</span>
          <span class="where">${esc(PROVIDERS[s.provider] || s.provider)}<span class="sep"></span>${where}</span></button>`);
        row.onmousemove = () => { if (sel !== i) { sel = i; mark(); } };
        row.onclick = () => choose(s);
        list.append(row);
      }
    }
    if (!items.length) list.append(h(`<div class="palette-empty">${q ? `No session matches “${esc(input.value.trim())}”.` : "There is no other session to compare with."}</div>`));
  };
  const mark = () => {
    $$(".prow", list).forEach((r, i) => r.setAttribute("aria-selected", i === sel));
    $$(".prow", list)[sel]?.scrollIntoView({ block: "nearest" });
  };
  const choose = (s) => { d.close(); done(s); };
  input.addEventListener("input", () => { sel = 0; draw(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); sel = Math.min(items.length - 1, sel + 1); mark(); }
    if (e.key === "ArrowUp") { e.preventDefault(); sel = Math.max(0, sel - 1); mark(); }
    if (e.key === "Enter") { e.preventDefault(); if (items[sel]) choose(items[sel]); }
  });
  d.addEventListener("close", () => d.remove());
  document.body.append(d);
  draw();
  d.showModal();
  input.focus();
}

function compareWith(s) {
  pickSession({ title: "Compare with…", near: s, exclude: s.key }, (other) => { location.hash = compareHash(s.key, other.key); });
}

async function openCompare(a, b) {
  const view = $("#view");
  const known = (k) => state.data.sessions.find((s) => s.key === k);
  if (!a || !known(a) || !b || !known(b)) {
    const have = known(a) || known(b);
    view.innerHTML = "";
    view.append(h(`<div class="empty-pane"><strong>Pick two sessions to compare</strong>${have ? `Comparing “${esc(have.title)}” with another session.` : "One of these sessions isn’t on this machine any more."}</div>`));
    if (have) compareWith(have);
    return;
  }
  const seq = ++cmp.seq;
  if (cmp.a !== a || cmp.b !== b) cmp.open.clear();
  cmp.a = a; cmp.b = b;
  view.innerHTML = `<div class="loading">Reading both sessions…</div>`;
  let d;
  try {
    d = await api(`/api/compare?a=${encodeURIComponent(a)}&b=${encodeURIComponent(b)}`);
  } catch (e) {
    if (seq === cmp.seq) view.innerHTML = `<div class="empty-pane"><strong>Couldn’t compare these sessions</strong>${esc(e.message)}</div>`;
    return;
  }
  if (seq !== cmp.seq || !onCompare()) return;
  for (const t of [d.a, d.b]) {
    t.events = t.events || [];
    t.changes = t.changes || [];
    t.diffs = t.changes.map(changeDiff);
  }
  cmp.data = d;
  try {
    renderCompare();
  } catch (e) {
    console.error(e);
    view.innerHTML = `<div class="empty-pane"><strong>Couldn’t compare these sessions</strong>The page hit an error while drawing it: ${esc(e.message)}</div>`;
  }
}

// fmtSpan is a length of time in milliseconds, for side-by-side numbers.
function fmtSpan(ms) {
  const s = Math.round((ms || 0) / 1000);
  if (s < 60) return `${s} s`;
  if (s < 5400) return `${Math.round(s / 60)} min`;
  return `${(s / 3600).toFixed(1).replace(/\.0$/, "")} h`;
}

const SIDES = ["a", "b"];

function renderCompare() {
  const d = cmp.data, c = d.compare;
  const view = $("#view");
  view.innerHTML = "";
  const page = h(`<div class="cmp"><div class="cmp-inner">
    <header class="cmp-head"><h1>Compare sessions</h1><div class="cmp-sides"></div></header>
    <section class="cmp-sec"><h2>At a glance</h2><div class="cmp-glance"></div></section>
    <section class="cmp-sec"><h2>Steps</h2><p class="cmp-note">Each bar is a step, on one scale, so the longer session runs further. Hover to read a step; click to open it.</p><div class="cmp-strips"></div></section>
    <section class="cmp-sec"><h2>Prompts</h2><div class="cmp-prompts"></div></section>
    <section class="cmp-sec"><h2>Files</h2><div class="cmp-files"></div></section>
    <section class="cmp-sec"><h2>Tools</h2><div class="cmp-tools"></div></section>
    <section class="cmp-sec"><h2>Failed tool calls</h2><div class="cmp-fails"></div></section>
  </div></div>`);
  view.append(page);

  const sides = $(".cmp-sides", page);
  sides.append(sideCard("a", d.a.session));
  const swap = h(`<button class="icon-btn cmp-swap" title="Swap A and B" aria-label="Swap A and B"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3 5.5h9.5M10 3l2.5 2.5L10 8M13 10.5H3.5M6 8l-2.5 2.5L6 13"/></svg></button>`);
  swap.onclick = () => { location.hash = compareHash(cmp.b, cmp.a); };
  sides.append(swap);
  sides.append(sideCard("b", d.b.session));

  renderGlance($(".cmp-glance", page), c, d);
  renderStrips($(".cmp-strips", page));
  renderComparePrompts($(".cmp-prompts", page));
  renderCompareFiles($(".cmp-files", page), c.files);
  renderCompareTools($(".cmp-tools", page), c);
  renderCompareFails($(".cmp-fails", page));
}

const sideMark = (k) => `<span class="cmp-mark ${k}">${k.toUpperCase()}</span>`;

function sideCard(k, s) {
  const where = s.worktree ? `worktree ${s.worktree.split("/").pop()}` : tildify(s.cwd);
  const meta = [
    `<span class="prov"><span class="pmark ${esc(s.provider)}"></span>${esc(PROVIDERS[s.provider] || s.provider)}</span>`,
    s.model && `<span>${svg("chip")}${esc(s.model)}</span>`,
    s.branch && `<span>${svg("branch")}${esc(s.branch)}</span>`,
    `<span title="${esc(s.cwd)}">${svg("folder")}<span class="cwd"><bdi>${esc(where)}</bdi></span></span>`,
    `<span>${svg("clock")}${esc(when(s.started))}</span>`,
  ].filter(Boolean).join("");
  const el = h(`<div class="cmp-side ${k}">
    <div class="cmp-side-top">${sideMark(k)}<a class="cmp-title ${s.titleKind === "prompt" ? "unnamed" : ""}" href="#s=${encodeURIComponent(s.key)}" title="Open this session">${esc(s.title)}</a>
      <button class="linkish">Change</button></div>
    <div class="meta">${meta}</div></div>`);
  $(".linkish", el).onclick = () => {
    const other = k === "a" ? cmp.b : cmp.a;
    pickSession({ title: `Replace ${k.toUpperCase()} with…`, near: s, exclude: other }, (pick) => {
      location.hash = k === "a" ? compareHash(pick.key, cmp.b) : compareHash(cmp.a, pick.key);
    });
  };
  return el;
}

// renderGlance is the table of numbers, with B's difference from A. Lower is
// marked better only where it plainly is: time, failures, tokens and cost.
function renderGlance(root, c, d) {
  const A = c.a, B = c.b;
  const ua = A.usage, ub = B.usage;
  const rows = [
    { label: "Active time", a: A.active, b: B.active, fmt: fmtSpan, lower: true, tip: "Time between steps, leaving out pauses longer than 5 minutes" },
    { label: "Wall clock", a: A.wall, b: B.wall, fmt: fmtSpan, tip: "First step to last, pauses included" },
    { label: "Steps", a: A.steps, b: B.steps, tip: "Every prompt, reply, thought and tool call" },
    { label: "Prompts", a: A.prompts, b: B.prompts },
    { label: "Tool calls", a: A.toolCalls, b: B.toolCalls },
    { label: "Failed tool calls", a: A.failed, b: B.failed, lower: true, bad: true },
    { label: "Edits", a: A.edits, b: B.edits, edit: true },
    { label: "Files changed", a: A.files, b: B.files, edit: true },
    { label: "Lines added", a: A.added, b: B.added, add: true },
    { label: "Lines removed", a: A.removed, b: B.removed, del: true },
  ];
  if (ua || ub) {
    const z = { input: 0, output: 0, cost: 0 };
    const xa = ua || z, xb = ub || z;
    // A session that recorded no token counts has nothing to subtract from.
    const one = !ua || !ub;
    rows.push({ label: "Tokens", a: totalTokens(xa), b: totalTokens(xb), fmt: fmtTokens, lower: true, approx: one,
      labelA: ua ? undefined : "—", labelB: ub ? undefined : "—", tipA: ua && usageLines(ua), tipB: ub && usageLines(ub) });
    rows.push({ label: "API-equivalent cost", a: xa.cost || 0, b: xb.cost || 0, fmt: fmtCost, lower: true, labelA: ua ? costLabel(ua) : "—", labelB: ub ? costLabel(ub) : "—",
      approx: one || !!(xa.unpriced || xb.unpriced), tipA: ua && usageLines(ua), tipB: ub && usageLines(ub) });
  }
  const t = h(`<table class="cmp-table"><thead><tr><th></th><th>${sideMark("a")}</th><th>${sideMark("b")}</th><th>B − A</th></tr></thead><tbody></tbody></table>`);
  const body = $("tbody", t);
  for (const r of rows) {
    const f = r.fmt || ((n) => n.toLocaleString());
    const top = Math.max(r.a, r.b) || 1;
    const cls = r.bad ? "bad" : r.edit ? "blz" : r.add ? "plus" : r.del ? "minus" : "";
    const cell = (v, label, tip, k) => `<td class="v" ${tip ? `title="${esc(tip)}"` : ""}><span class="num ${v ? cls : ""}">${esc(label ?? f(v))}</span><span class="bar ${k}" style="width:${(100 * v) / top}%"></span></td>`;
    const diff = r.b - r.a;
    let delta, tone = "";
    if (r.approx) delta = "—";
    else if (diff === 0) delta = "same";
    else {
      delta = (diff > 0 ? "+" : "−") + f(Math.abs(diff));
      if (r.a > 0) {
        const pct = Math.round((100 * diff) / r.a);
        if (pct !== 0 && Math.abs(pct) < 1000) delta += ` <span class="pct">(${pct > 0 ? "+" : "−"}${Math.abs(pct)}%)</span>`;
      }
      if (r.lower) tone = diff < 0 ? "good" : "worse";
    }
    body.append(h(`<tr><th scope="row" ${r.tip ? `title="${esc(r.tip)}"` : ""}>${esc(r.label)}</th>${cell(r.a, r.labelA, r.tipA, "a")}${cell(r.b, r.labelB, r.tipB, "b")}<td class="d ${tone}">${delta}</td></tr>`));
  }
  root.append(t);
  const files = c.files.length;
  if (files) {
    root.append(h(`<p class="cmp-note">${c.shared ? `Both changed <b>${plural(c.shared, "file")}</b> of the ${plural(files, "file")} either one touched.` : `They changed different files: none of the ${plural(files, "file")} overlap.`}${(ua?.unpriced || ub?.unpriced) ? " Some responses have no price, so costs are lower bounds." : ""}</p>`));
  }
}

// renderStrips draws each session's steps as a trace, both on the scale of the
// longer one.
function renderStrips(root) {
  const d = cmp.data;
  const n = Math.max(1, d.a.events.length, d.b.events.length);
  const strips = SIDES.map((k) => {
    const t = d[k];
    const el = h(`<div class="cmp-strip">${sideMark(k)}<div class="cmp-canvas"><canvas></canvas></div><span class="n">${plural(t.events.length, "step")}</span></div>`);
    root.append(el);
    const box = $(".cmp-canvas", el), canvas = $("canvas", el), pad = 4;
    const iAt = (x) => Math.floor(((x - pad) / (box.clientWidth - pad * 2)) * n);
    const draw = () => {
      const W = box.clientWidth, H = box.clientHeight, dpr = window.devicePixelRatio || 1;
      if (!W || !H) return;
      canvas.width = W * dpr; canvas.height = H * dpr;
      const ctx = canvas.getContext("2d");
      ctx.scale(dpr, dpr);
      const C = traceColors(), step = (W - pad * 2) / n, bw = Math.max(1, Math.min(3, step * 0.6));
      t.events.forEach((e, i) => {
        const kind = stepKind(e), hgt = STEP_H[kind] * H;
        ctx.fillStyle = C[kind];
        const x = pad + (i + 0.5) * step - bw / 2;
        ctx.beginPath();
        ctx.roundRect ? ctx.roundRect(x, H / 2 - hgt / 2, bw, hgt, bw / 2) : ctx.rect(x, H / 2 - hgt / 2, bw, hgt);
        ctx.fill();
      });
    };
    box.addEventListener("pointermove", (ev) => {
      const i = iAt(ev.clientX - box.getBoundingClientRect().left);
      if (i < 0 || i >= t.events.length) { $("#tip").hidden = true; box.style.cursor = ""; return; }
      box.style.cursor = "pointer";
      stepTip(t.events[i], ev.clientX, box.getBoundingClientRect().bottom - 6);
    });
    box.addEventListener("pointerleave", () => { $("#tip").hidden = true; });
    box.addEventListener("click", (ev) => {
      const i = iAt(ev.clientX - box.getBoundingClientRect().left);
      if (i >= 0 && i < t.events.length) openStep(t.session.key, i);
    });
    return draw;
  });
  const drawAll = () => strips.forEach((f) => f());
  if (cmp.ro) cmp.ro.disconnect();
  cmp.ro = new ResizeObserver(drawAll);
  cmp.ro.observe(root);
  root.append(h(`<div class="trace-legend"><span><i class="lg-prompt"></i>Prompt</span><span><i class="lg-tool"></i>Tool call</span><span><i class="lg-edit"></i>Edit</span><span><i class="lg-err"></i>Failed</span></div>`));
  requestAnimationFrame(drawAll);
}

// openStep opens a session scrolled to one of its steps.
function openStep(key, event) {
  $("#tip").hidden = true;
  state.pendingJump = { event, field: "" };
  go(key);
}

// cols lays out one block per side, next to each other on a wide screen and
// one under the other on a narrow one.
function cols(root, fill) {
  const grid = h(`<div class="cmp-cols"></div>`);
  for (const k of SIDES) {
    const col = h(`<div class="cmp-col ${k}"><div class="cmp-col-head">${sideMark(k)}<span>${esc(oneLine(cmp.data[k].session.title, 60))}</span></div></div>`);
    fill(col, k, cmp.data[k]);
    grid.append(col);
  }
  root.append(grid);
}

const oneLine = (s, n) => { s = String(s || "").replace(/\s+/g, " ").trim(); return s.length > n ? s.slice(0, n - 1) + "…" : s; };

function renderComparePrompts(root) {
  const d = cmp.data;
  const prompts = (t) => t.events.map((e, i) => [e, i]).filter(([e]) => e.kind === "user");
  const pa = prompts(d.a), pb = prompts(d.b);
  if (pa.length && pb.length) {
    const x = pa[0][0].text.trim(), y = pb[0][0].text.trim();
    if (x === y) {
      root.append(h(`<p class="cmp-note">Both sessions start from the same prompt.</p>`));
    } else {
      const words = (s) => s.split(/(\s+)/).filter(Boolean);
      const html = wordDiffHTML(seqDiff(words(x), words(y)));
      const box = h(`<div class="cmp-wdiff"><div class="cmp-wdiff-head">How the first prompt changed from A to B</div><div class="cmp-wdiff-body"></div></div>`);
      $(".cmp-wdiff-body", box).innerHTML = html;
      root.append(box);
    }
  }
  cols(root, (col, k, t) => {
    const list = prompts(t);
    if (!list.length) { col.append(h(`<div class="cmp-empty">No prompts recorded.</div>`)); return; }
    const ol = h(`<ol class="cmp-plist"></ol>`);
    for (const [e, i] of list) {
      const li = h(`<li><button class="cmp-prompt" title="Open this step">${esc(oneLine(e.text, 400))}</button></li>`);
      $("button", li).onclick = () => openStep(t.session.key, i);
      ol.append(li);
    }
    col.append(ol);
  });
}

// wordDiffHTML marks a word diff as whole runs: everything removed, then
// everything added. A space between two changed words joins their runs, so a
// rewritten phrase reads as one phrase and not as interleaved words.
function wordDiffHTML(rows) {
  let out = "", del = "", ins = "";
  const flush = () => {
    if (del) out += `<del>${esc(del)}</del>`;
    if (ins) out += `<ins>${esc(ins)}</ins>`;
    del = ins = "";
  };
  rows.forEach(([k, w], i) => {
    if (k === "d") del += w;
    else if (k === "a") ins += w;
    else if ((del || ins) && !w.trim() && rows[i + 1] && rows[i + 1][0] !== " ") { del += w; ins += w; }
    else { flush(); out += esc(w); }
  });
  flush();
  return out;
}

function renderCompareFiles(root, files) {
  if (!files.length) { root.append(h(`<div class="cmp-empty">Neither session changed a file.</div>`)); return; }
  const d = cmp.data;
  const list = h(`<div class="cmp-flist"></div>`);
  const cell = (f) => !f ? `<span class="none">not changed</span>`
    : f.deleted ? `<span class="minus">deleted</span>`
    : `<span class="plus">+${f.added}</span><span class="minus">−${f.removed}</span><span class="ed">${plural(f.edits, "edit")}</span>`;
  for (const f of files) {
    const tag = f.a && f.b ? `<span class="tag both">Both</span>` : f.a ? `<span class="tag">Only A</span>` : `<span class="tag">Only B</span>`;
    const slash = f.path.lastIndexOf("/");
    const row = h(`<div class="cmp-file" aria-expanded="${cmp.open.has(f.path)}">
      <button class="cmp-frow" title="${esc(f.path)}">${svg("down", "chev")}<span class="fp"><span class="dir">${esc(slash >= 0 ? f.path.slice(0, slash + 1) : "")}</span>${esc(f.path.slice(slash + 1))}</span>${tag}
        <span class="cmp-fcell a">${sideMark("a")}${cell(f.a)}</span><span class="cmp-fcell b">${sideMark("b")}${cell(f.b)}</span></button></div>`);
    const btn = $("button", row);
    const body = () => {
      const box = h(`<div class="cmp-fbody"></div>`);
      cols(box, (col, k, t) => {
        const tally = f[k];
        if (!tally) { col.append(h(`<div class="cmp-empty">This session didn’t change this file.</div>`)); return; }
        for (const ci of tally.changes) {
          const ch = t.changes[ci], df = t.diffs[ci];
          const card = h(`<article class="change"><div class="change-head"><span class="op ${esc(ch.op)}">${esc(OPS[ch.op] || ch.op)}</span>${ch.op === "delete" ? "" : `<span class="nums mono">${counts(df)}</span>`}
            <button class="jump" title="Open the step that made this change">Show step</button></div></article>`);
          $(".jump", card).onclick = () => openStep(t.session.key, ch.event);
          if (ch.op === "delete") card.append(h(`<div class="note">The agent deleted this file.</div>`));
          else if (!df.hunks.length || df.hunks.every((r) => !r.length)) card.append(h(`<div class="note">No content recorded for this change.</div>`));
          for (const rows of df.hunks) {
            const hk = h(`<div class="hunk"></div>`);
            hk.append(renderDiffRows(rows, 200));
            card.append(hk);
          }
          col.append(card);
        }
      });
      return box;
    };
    if (cmp.open.has(f.path)) row.append(body());
    btn.onclick = () => {
      const open = !cmp.open.has(f.path);
      open ? cmp.open.add(f.path) : cmp.open.delete(f.path);
      row.setAttribute("aria-expanded", open);
      if (open) row.append(body()); else $(".cmp-fbody", row)?.remove();
    };
    list.append(row);
  }
  root.append(list);
  if (files.some((f) => f.a && f.b)) root.append(h(`<p class="cmp-note">Open a file to read each session’s edits to it side by side.</p>`));
}

function renderCompareTools(root, c) {
  const names = new Map();
  for (const k of SIDES) for (const t of c[k].tools) {
    if (!names.has(t.name)) names.set(t.name, { a: null, b: null });
    names.get(t.name)[k] = t;
  }
  if (!names.size) { root.append(h(`<div class="cmp-empty">Neither session called a tool.</div>`)); return; }
  const rows = [...names].sort((x, y) => ((y[1].a?.calls || 0) + (y[1].b?.calls || 0)) - ((x[1].a?.calls || 0) + (x[1].b?.calls || 0)) || (x[0] < y[0] ? -1 : 1));
  const top = Math.max(...rows.flatMap(([, v]) => [v.a?.calls || 0, v.b?.calls || 0])) || 1;
  const t = h(`<table class="cmp-table tools"><thead><tr><th>Tool</th><th>${sideMark("a")}</th><th>${sideMark("b")}</th></tr></thead><tbody></tbody></table>`);
  const cell = (x, k) => {
    const n = x?.calls || 0;
    return `<td class="v"><span class="num">${n || "·"}${x?.failed ? ` <span class="bad">${x.failed} failed</span>` : ""}</span><span class="bar ${k}" style="width:${(100 * n) / top}%"></span></td>`;
  };
  for (const [name, v] of rows) {
    $("tbody", t).append(h(`<tr><th scope="row"><span class="tool-name">${svg(toolIcon(name), "ic")}${esc(name)}</span></th>${cell(v.a, "a")}${cell(v.b, "b")}</tr>`));
  }
  root.append(t);
}

function renderCompareFails(root) {
  cols(root, (col, k, t) => {
    const fails = t.events.map((e, i) => [e, i]).filter(([e]) => e.kind === "tool" && e.error);
    if (!fails.length) { col.append(h(`<div class="cmp-empty">No failed tool calls.</div>`)); return; }
    for (const [e, i] of fails) {
      const out = (e.output || "").split("\n").find((l) => l.trim()) || "";
      const b = h(`<button class="cmp-fail" title="Open this step">${svg(toolIcon(e.tool), "ic")}<span class="tn">${esc(e.tool)}</span><span class="ts">${esc(e.summary || "")}</span>
        ${out ? `<span class="out">${esc(oneLine(out, 200))}</span>` : ""}</button>`);
      b.onclick = () => openStep(t.session.key, i);
      col.append(b);
    }
  });
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

function route() {
  const c = compareRoute();
  if (c) {
    stopLive();
    state.current = null;
    renderSidebar();
    openCompare(c.a, c.b);
    return;
  }
  cmp.seq++;
  if (onStats()) return openStats();
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
  $("#refresh").onclick = async () => { await load(true); if (onStats()) loadStats(); else if (!state.current && !onCompare()) renderHome(); };
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
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => trace.draw());
  setInterval(async () => {
    if (document.hidden) return;
    const changed = await load();
    if (changed && onStats()) loadStats();
    else if (changed && !state.current && !onCompare()) renderHome();
    if (changed) startLive();
  }, 20000);
}

if (EXPORTED) initExport(); else init();

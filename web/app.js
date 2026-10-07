"use strict";

const PROVIDERS = { claude: "Claude Code", codex: "Codex" };
const PER_FOLDER = 6;
const WRITE_PREVIEW = 400;
const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

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
    for (const kid of node.kids.values()) {
      stats(kid);
      node.count += kid.count;
      if (kid.latest > node.latest) node.latest = kid.latest;
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
    tree.append(h(`<div class="empty">No sessions yet. Run Claude Code or Codex in a project, then rescan.</div>`));
    return;
  }
  if (!sessions.length) {
    tree.append(h(`<div class="empty">Nothing matches “${esc(state.q)}”. Clear the filter or turn a provider back on.</div>`));
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
        <span class="name"><span class="pre">${esc(pre)}</span>${esc(last)}</span><span class="count">${node.count}</span>
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
}

function sessionRow(s) {
  const bits = [];
  if (s.worktree) bits.push(`<span>worktree ${esc(s.worktree.split("/").pop())}</span>`);
  else if (s.cwd !== s.root) bits.push(`<span>${esc(tildify(s.cwd).replace(tildify(s.root) + "/", ""))}/</span>`);
  if (s.branch) bits.push(`<span>${esc(s.branch)}</span>`);
  if (s.edits) bits.push(`<span class="edits">${plural(s.edits, "edit")}</span>`);
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
      <p class="lede">trail reads the logs Claude Code and Codex keep on this machine. Run either one in a project, then rescan.</p>
      <h2>Where trail looks</h2><div class="sources"></div></div></div>`));
  } else {
    view.append(h(`<div class="home"><div class="home-inner">
      <h1>${plural(d.sessions.length, "session")} across ${plural(folders, "folder")}</h1>
      <p class="lede">${lastDay ? `${plural(lastDay, "session")} ran in the last day. ` : ""}Open one to read what the agent did next to every file it changed.</p>
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

function renderSession() {
  const t = state.transcript, s = t.session, st = state.stats;
  const view = $("#view");
  view.innerHTML = "";
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

  const el = h(`<div class="session">
    <header class="s-head">
      <div class="titles">
        <h1 class="${named ? "" : "unnamed"}">${esc(s.title)}</h1>
        ${named && s.prompt ? `<p class="first-prompt" title="${esc(s.prompt)}">${esc(s.prompt)}</p>` : ""}
        <div class="meta">${meta}</div>
        <div class="facts">${facts}</div>
      </div>
      <div class="actions"><button class="btn" id="copy" title="${esc(s.resume)}">${svg("copy")}<span>Copy resume command</span></button></div>
    </header>
    <div class="trace-wrap">
      <div class="trace" id="trace" role="slider" tabindex="0" aria-label="Session trace. Drag or use arrow keys to move through the transcript." aria-valuemin="1" aria-valuemax="${t.events.length}" aria-valuenow="1">
        <div class="win"></div><canvas></canvas>
      </div>
      <div class="trace-legend"><span><i class="lg-prompt"></i>Prompt</span><span><i class="lg-tool"></i>Tool call</span><span><i class="lg-edit"></i>Edit</span><span><i class="lg-err"></i>Failed</span></div>
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

  $("#copy", el).onclick = async (e) => {
    const b = e.currentTarget, label = $("span", b);
    try {
      await navigator.clipboard.writeText(s.resume);
      b.classList.add("done"); b.querySelector("svg").innerHTML = ICON.check; label.textContent = "Copied";
    } catch { label.textContent = "Couldn’t copy. Hover to read it."; }
    setTimeout(() => { b.classList.remove("done"); b.querySelector("svg").innerHTML = ICON.copy; label.textContent = "Copy resume command"; }, 1800);
  };
  $$(".pane-tabs button", el).forEach((b) => (b.onclick = () => setPane(b.dataset.pane)));
  $$("[data-f]", el).forEach((b) => (b.onclick = () => { state.filter = b.dataset.f; applyFilter(); }));
  view.append(el);
  renderLog($(".log-inner", el));
  applyFilter();
  renderChangesPane();
  setupTrace();
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

function renderLog(root) {
  const t = state.transcript;
  if (!t.events.length) {
    root.append(h(`<div class="empty-pane"><strong>Empty transcript</strong>This session has no messages trail can read.</div>`));
    return;
  }
  const frag = document.createDocumentFragment();
  let lastClock = "";
  t.events.forEach((e, i) => {
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
    pane.append(h(`<div class="empty-pane"><strong>Commits aren’t available</strong>${esc(r.reason)}</div>`));
    return;
  }
  pane.append(h(`<div class="window">Commits on any branch of <span class="mono">${esc(tildify(r.toplevel))}</span> from 10 minutes before this session started to 2 hours after it ended. Commits that touch files the agent edited come first.</div>`));
  if (!r.commits.length) {
    pane.append(h(`<div class="empty-pane"><strong>No commits in that window</strong>The changes may still be uncommitted, or were committed later.</div>`));
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
        const { patch } = await api(`/api/commit?id=${encodeURIComponent(key)}&sha=${encodeURIComponent(c.sha)}`);
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

/* ---------------- palette ---------------- */
const palette = { items: [], sel: 0 };

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
function renderPalette() {
  const q = $("#palette-q").value.trim().toLowerCase();
  const list = $("#palette-list");
  palette.items = state.data.sessions
    .map((s) => [score(s, q), s]).filter(([sc]) => sc > 0)
    .sort((a, b) => b[0] - a[0] || (a[1].ended < b[1].ended ? 1 : -1))
    .slice(0, 40).map(([, s]) => s);
  palette.sel = Math.min(palette.sel, Math.max(0, palette.items.length - 1));
  list.innerHTML = "";
  if (!palette.items.length) { list.append(h(`<div class="palette-empty">No session matches “${esc(q)}”.</div>`)); return; }
  palette.items.forEach((s, i) => {
    const row = h(`<button class="prow" role="option" aria-selected="${i === palette.sel}"><span class="pmark ${esc(s.provider)}"></span>
      <span class="t">${hilite(s.title, q)}</span><span class="when">${esc(ago(s.ended))}</span>
      <span class="where">${esc([tildify(s.root), s.branch].filter(Boolean).join("   "))}</span></button>`);
    row.onmousemove = () => { if (palette.sel !== i) { palette.sel = i; markPalette(); } };
    row.onclick = () => { closePalette(); go(s.key); };
    list.append(row);
  });
}
function markPalette() {
  $$("#palette-list .prow").forEach((r, i) => r.setAttribute("aria-selected", i === palette.sel));
  $$("#palette-list .prow")[palette.sel]?.scrollIntoView({ block: "nearest" });
}
function openPalette() {
  if (!state.data) return;
  $("#palette").hidden = false;
  $("#palette-q").value = "";
  palette.sel = 0;
  renderPalette();
  $("#palette-q").focus();
}
function closePalette() { $("#palette").hidden = true; }

/* ---------------- routing & refresh ---------------- */
function route() {
  const m = location.hash.match(/^#s=(.+)$/);
  const key = m ? decodeURIComponent(m[1]) : null;
  if (key && state.data.sessions.some((s) => s.key === key)) {
    if (key !== state.current) openSession(key);
  } else {
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
  $("#palette-open").onclick = openPalette;
  $("#palette").addEventListener("mousedown", (e) => { if (e.target.id === "palette") closePalette(); });
  $("#palette-q").addEventListener("input", () => { palette.sel = 0; renderPalette(); });
  $("#palette-q").addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); palette.sel = Math.min(palette.items.length - 1, palette.sel + 1); markPalette(); }
    if (e.key === "ArrowUp") { e.preventDefault(); palette.sel = Math.max(0, palette.sel - 1); markPalette(); }
    if (e.key === "Enter" && palette.items[palette.sel]) { const s = palette.items[palette.sel]; closePalette(); go(s.key); }
  });
  document.addEventListener("click", (e) => {
    if ($("#app").classList.contains("drawer") && !e.target.closest("#side, #menu")) closeDrawer();
  });
  document.addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName);
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); $("#palette").hidden ? openPalette() : closePalette(); return; }
    if (e.key === "Escape") { closePalette(); closeDrawer(); return; }
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "/") { e.preventDefault(); $("#q").focus(); }
    if (e.key === "j") moveSession(1);
    if (e.key === "k") moveSession(-1);
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => trace.draw());
  setInterval(async () => {
    if (document.hidden) return;
    const changed = await load();
    if (changed && !state.current) renderHome();
  }, 20000);
}

init();

// Copy buttons and the interactive trace demo. No dependencies.

for (const b of document.querySelectorAll(".copy")) {
  b.addEventListener("click", async () => {
    const text = b.closest("[data-copy]").dataset.copy;
    try { await navigator.clipboard.writeText(text); } catch { return; }
    b.textContent = "Copied";
    b.classList.add("done");
    setTimeout(() => { b.textContent = "Copy"; b.classList.remove("done"); }, 1600);
  });
}

// The session from the screenshot, one entry per step.
const STEPS = [
  { k: "prompt", t: "17:35", title: "Prompt", body: "Cart totals are off by a cent for some orders (e.g. 3 × $19.99). Find the cause and fix it, with a test." },
  { k: "say", t: "", title: "Claude", body: "This smells like float truncation. Let me look at Total." },
  { k: "tool", t: "", title: "Read", arg: "cart.go" },
  { k: "say", t: "17:36", title: "Claude", body: "Found it. int(sum * 100) truncates, so 59.97 × 100 = 5996.9999… becomes 5996. Rounding before converting fixes it." },
  { k: "edit", t: "", title: "Edit", arg: "cart.go", add: 2, del: 0 },
  { k: "edit", t: "", title: "Edit", arg: "cart.go", add: 1, del: 1 },
  { k: "edit", t: "17:37", title: "Write", arg: "cart_test.go", add: 10, del: 0 },
  { k: "fail", t: "", title: "Bash", arg: "go test ./...", body: "FAIL: TestTotalRounding (want 5997, got 5996)" },
  { k: "tool", t: "", title: "Read", arg: "cart.go" },
  { k: "tool", t: "", title: "Bash", arg: "go test ./...", body: "ok  shop  0.012s" },
  { k: "prompt", t: "17:39", title: "Prompt", body: "Also note it in the README" },
  { k: "edit", t: "", title: "Edit", arg: "README.md", add: 2, del: 0 },
];

const strip = document.getElementById("strip");
const stepEl = document.getElementById("step");
const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

if (strip) {
  const win = document.createElement("div");
  win.className = "win";
  strip.append(win);
  const ticks = STEPS.map((s) => {
    const i = document.createElement("i");
    i.className = "tk " + s.k;
    strip.append(i);
    return i;
  });
  strip.setAttribute("aria-valuemax", STEPS.length);

  let cur = -1;
  const show = (n) => {
    n = Math.max(0, Math.min(STEPS.length - 1, n));
    if (n === cur) return;
    cur = n;
    ticks.forEach((t, i) => { t.classList.toggle("on", i === n); t.classList.toggle("past", i <= n); });
    // The window frames the prompt this step belongs to, through the step itself.
    let start = n;
    while (start > 0 && STEPS[start].k !== "prompt") start--;
    const r = strip.getBoundingClientRect();
    const a = ticks[start].getBoundingClientRect(), b = ticks[n].getBoundingClientRect();
    win.style.left = a.left - r.left - 8 + "px";
    win.style.width = b.right - a.left + 16 + "px";

    const s = STEPS[n];
    const kind = `<div class="kind">${esc(s.title)}${s.arg ? `<code>${esc(s.arg)}</code>` : ""}</div>`;
    let rest = "";
    if (s.body) rest = `<div class="body${s.k === "prompt" ? " bubble" : ""}">${esc(s.body)}</div>`;
    if (s.add != null) rest = `<div class="diff">${esc(s.arg)}<span class="a">+${s.add}</span><span class="d">−${s.del}</span></div>`;
    stepEl.className = "step " + s.k;
    stepEl.innerHTML = `<div class="time">${s.t}</div>${kind}${rest}`;
    strip.setAttribute("aria-valuenow", n + 1);
    strip.setAttribute("aria-valuetext", `Step ${n + 1}: ${s.title}${s.arg ? " " + s.arg : ""}`);
  };
  const at = (x) => {
    let best = 0, dist = Infinity;
    ticks.forEach((t, i) => {
      const r = t.getBoundingClientRect();
      const d = Math.abs(r.left + r.width / 2 - x);
      if (d < dist) { dist = d; best = i; }
    });
    return best;
  };

  let drag = false;
  strip.addEventListener("pointerdown", (e) => { drag = true; strip.setPointerCapture(e.pointerId); show(at(e.clientX)); });
  strip.addEventListener("pointermove", (e) => { if (drag || e.pointerType === "mouse") show(at(e.clientX)); });
  strip.addEventListener("pointerup", () => { drag = false; });
  strip.addEventListener("keydown", (e) => {
    const to = { ArrowRight: cur + 1, ArrowLeft: cur - 1, Home: 0, End: STEPS.length - 1 }[e.key];
    if (to != null) { e.preventDefault(); show(to); }
  });
  addEventListener("resize", () => { const n = cur; cur = -1; show(n); });
  show(5);
}

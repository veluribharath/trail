"""Print the A/B table from run.sh's results.jsonl as Markdown."""
import json
import statistics
import sys


def mean(xs):
    xs = [x for x in xs if x is not None]
    return statistics.mean(xs) if xs else float("nan")


rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
groups = {}
for r in rows:
    groups.setdefault((r["kind"], r["task"], r["arm"]), []).append(r)

print("| task | kind | hook | pass | turns | cost $ | input tok | output tok | wall s | injected | hook ms |")
print("|---|---|---|---|---|---|---|---|---|---|---|")
ARMS = ["off", "on", "loose"]
order = lambda arm: ARMS.index(arm) if arm in ARMS else len(ARMS)
for (kind, task, arm), rs in sorted(groups.items(), key=lambda kv: (kv[0][0] != "repeat", kv[0][1], order(kv[0][2]))):
    lat = [x for r in rs for x in r["hook_latency_ms"]]
    inj = "-" if arm == "off" else "%d/%d" % (sum(r["hook_injected"] for r in rs), sum(r["hook_calls"] for r in rs))
    print("| %s | %s | %s | %d/%d | %.1f | %.3f | %.0f | %.0f | %.0f | %s | %s |" % (
        task, kind, arm, sum(r["pass"] for r in rs), len(rs), mean([r["turns"] for r in rs]), mean([r["cost_usd"] for r in rs]),
        mean([r["input_tokens"] for r in rs]), mean([r["output_tokens"] for r in rs]), mean([r["wall_ms"] for r in rs]) / 1000,
        inj, "%.0f" % mean(lat) if lat else "-"))

print()
print("| kind | hook | pass | turns | cost $ | input tok | wall s | injected |")
print("|---|---|---|---|---|---|---|---|")
for kind in ("repeat", "new"):
    for arm in sorted({r["arm"] for r in rows}, key=order):
        rs = [r for r in rows if r["kind"] == kind and r["arm"] == arm]
        if rs:
            inj = "-" if arm == "off" else "%d/%d" % (sum(r["hook_injected"] for r in rs), sum(r["hook_calls"] for r in rs))
            print("| %s | %s | %d/%d | %.1f | %.3f | %.0f | %.0f | %s |" % (kind, arm, sum(r["pass"] for r in rs), len(rs),
                  mean([r["turns"] for r in rs]), mean([r["cost_usd"] for r in rs]), mean([r["input_tokens"] for r in rs]),
                  mean([r["wall_ms"] for r in rs]) / 1000, inj))

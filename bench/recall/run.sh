#!/usr/bin/env bash
# A/B benchmark for trail recall: the same tasks run headless with
# `claude -p`, once without the hook and once with it, several times each.
#
#   bench/recall/run.sh [out-dir]
#
# Arms: off (no hook), on (the hook as shipped), loose (the hook with
# -min-score 0.15, so it injects on most prompts: what injection itself does).
#
# Environment: ARMS (default "off on loose"), MODEL (sonnet), RUNS (3), PAR (4 runs at once),
# TASKS (ids from tasks.json, default all), TRAIL (a trail binary; built
# from this checkout when unset), MAX_TURNS (40).
#
# Every run gets a fresh copy of fixture/ and its own made-up history
# (history.py), which only the hook reads. Results land in out-dir; then
# summarize.py prints the table.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/out/$(date +%Y%m%d-%H%M%S)}
MODEL=${MODEL:-sonnet}
RUNS=${RUNS:-3}
PAR=${PAR:-4}
MAX_TURNS=${MAX_TURNS:-40}
ARMS=${ARMS:-off on loose}
mkdir -p "$out"
out=$(cd "$out" && pwd)

if [ -z "${TRAIL:-}" ]; then
  TRAIL=$out/trail
  (cd "$here/../.." && go build -o "$TRAIL" .)
fi
export TRAIL MODEL MAX_TURNS here out

tasks=${TASKS:-$(python3 -c 'import json,sys; print(" ".join(t["id"] for t in json.load(open(sys.argv[1]))))' "$here/tasks.json")}

one() {
  set -euo pipefail
  local task=$1 arm=$2 run=$3
  local dir=$out/runs/$task-$arm-$run
  rm -rf "$dir"
  mkdir -p "$dir/empty"
  cp -r "$here/fixture" "$dir/proj"
  (cd "$dir/proj" && git init -q && git add -A && git -c user.name=bench -c user.email=bench@example.com commit -qm fixture)
  python3 "$here/history.py" "$dir/proj" "$dir/history"

  local prompt check settings
  prompt=$(python3 -c 'import json,sys; print(next(t["prompt"] for t in json.load(open(sys.argv[1])) if t["id"]==sys.argv[2]))' "$here/tasks.json" "$task")
  check=$(python3 -c 'import json,sys; print(next(t["check"] for t in json.load(open(sys.argv[1])) if t["id"]==sys.argv[2]))' "$here/tasks.json" "$task")
  settings='{}'
  if [ "$arm" != off ]; then
    local e=$dir/empty extra=""
    [ "$arm" = loose ] && extra="-min-score 0.15"
    local cmd="$TRAIL -claude-dir $dir/history -codex-dir $e -gemini-dir $e -opencode-dir $e -kiro-dir $e -kiro-data-dir $e recall -no-cache $extra -log $dir/recall.jsonl"
    settings=$(python3 -c 'import json,sys; print(json.dumps({"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":sys.argv[1],"timeout":10}]}]}}))' "$cmd")
  fi

  local sid start end
  sid=$(python3 -c 'import uuid; print(uuid.uuid4())')
  start=$(date +%s%N)
  (cd "$dir/proj" && claude -p "$prompt" --model "$MODEL" --output-format json --session-id "$sid" \
      --setting-sources project --settings "$settings" --permission-mode bypassPermissions \
      --max-turns "$MAX_TURNS" </dev/null >"$dir/result.json" 2>"$dir/stderr.txt") || true
  end=$(date +%s%N)

  cp "$here/checks/$check.py" "$dir/proj/tests/test_zz_check.py"
  local pass=false
  if (cd "$dir/proj" && timeout 120 python3 -m unittest discover -s tests -t . >"$dir/check.txt" 2>&1); then pass=true; fi
  python3 - "$dir" "$task" "$arm" "$run" "$pass" $(( (end-start)/1000000 )) <<'PY' >>"$out/results.jsonl"
import json, sys
d, task, arm, run, ok, wall = sys.argv[1:]
kind = next(t["kind"] for t in json.load(open(__import__("os").environ["here"] + "/tasks.json")) if t["id"] == task)
try:
    r = json.load(open(d + "/result.json"))
except Exception:
    r = {}
u = r.get("usage") or {}
hook = []
try:
    hook = [json.loads(l) for l in open(d + "/recall.jsonl")]
except FileNotFoundError:
    pass
print(json.dumps({
    "task": task, "kind": kind, "arm": arm, "run": int(run), "pass": ok == "true",
    "turns": r.get("num_turns"), "cost_usd": r.get("total_cost_usd"), "wall_ms": int(wall),
    "duration_ms": r.get("duration_ms"), "is_error": r.get("is_error"),
    "input_tokens": u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0) + u.get("cache_creation_input_tokens", 0),
    "output_tokens": u.get("output_tokens", 0),
    "hook_calls": len(hook), "hook_injected": sum(1 for h in hook if h.get("injected")),
    "hook_tokens": sum(h.get("tokens", 0) for h in hook), "hook_latency_ms": [h.get("latency_ms") for h in hook],
    "hook_sessions": [s for h in hook for s in h.get("sessions") or []],
}))
PY
  echo "$task $arm $run pass=$pass"
}
export -f one

for task in $tasks; do
  for run in $(seq 1 "$RUNS"); do
    for arm in $ARMS; do echo "$task $arm $run"; done
  done
done | xargs -P "$PAR" -L 1 bash -c 'one "$@"' _

python3 "$here/summarize.py" "$out/results.jsonl" | tee "$out/summary.md"

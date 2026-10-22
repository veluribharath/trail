# Builds the synthetic Kiro CLI fixtures used by the tests:
#   python3 testdata/kiro/gen.py testdata/kiro
# Shapes follow Kiro CLI 2.x (sessions/cli), the V3 engine (sess_ folders)
# and kiro-cli's data.sqlite3. Nothing here comes from a real session.
import json, os, shutil, sqlite3, sys

out = sys.argv[1]
for d in ("home", "data"):
    shutil.rmtree(os.path.join(out, d), ignore_errors=True)


def write(path, text):
    path = os.path.join(out, path)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def jsonl(*records):
    return "".join(json.dumps(r) + "\n" for r in records)


def rec(kind, mid, content, ts=None):
    data = {"message_id": mid, "content": content}
    if ts is not None:
        data["meta"] = {"timestamp": ts}
    return {"version": "v1", "kind": kind, "data": data}


text = lambda s: {"kind": "text", "data": s}
use = lambda i, name, inp: {"kind": "toolUse", "data": {"toolUseId": i, "name": name, "input": inp}}
result = lambda i, content, status="success": {"kind": "toolResult", "data": {"toolUseId": i, "content": content, "status": status}}

# 2026-10-03T09:00:00Z
T = 1791018000

# CLI 2.x: a session with a streamed reply, edits, a failed command and a subagent.
K1 = "0b1c2d3e-0000-4000-8000-000000000001"
SUB = "0b1c2d3e-0000-4000-8000-0000000000aa"
write(f"home/sessions/cli/{K1}.json", json.dumps({
    "session_id": K1, "cwd": "/work/shop", "title": "Fix the coupon rounding",
    "created_at": "2026-10-03T09:00:00Z", "updated_at": "2026-10-03T09:05:00Z",
    "session_state": {"rts_model_state": {"model_info": {"model_id": "claude-sonnet-4.5"}}}}))
write(f"home/sessions/cli/{K1}.jsonl", jsonl(
    rec("Prompt", "m1", [text("Coupons round the wrong way, fix it")], T + 1),
    rec("AssistantMessage", "m2", [text("Looking at "), ], T + 5),
    rec("AssistantMessage", "m2", [text("the coupon code."),
                                   use("t1", "read", {"operations": [{"mode": "Line", "path": "/work/shop/coupon.go"}]}),
                                   use("t2", "write", {"command": "strReplace", "path": "/work/shop/coupon.go",
                                                       "oldStr": "math.Floor(x)", "newStr": "math.Round(x)"})]),
    rec("ToolResults", "m3", [result("t1", [text("package shop\n")]),
                              result("t2", [text("Replaced 1 occurrence")])]),
    rec("AssistantMessage", "m4", [text("Running the tests."),
                                   use("t3", "shell", {"command": "go test ./..."}),
                                   use("t4", "write", {"command": "create", "path": "coupon_test.go", "content": "package shop\n"}),
                                   use("t5", "subagent", {"task": "Check the other rounding call sites"})]),
    rec("ToolResults", "m5", [result("t3", [{"kind": "json", "data": {"exit_status": "exit status: 1", "stdout": "FAIL coupon_test.go", "stderr": ""}}]),
                              result("t4", [text("denied")], "error"),
                              result("t5", [text("Two other call sites, both fine.")])]),
    rec("AssistantMessage", "m6", [text("Fixed; one test still fails.")]),
    rec("Prompt", "m7", [text("thanks")], T + 120),
) + '{"version":"v1","kind":"Prom')  # a line still being written

write(f"home/sessions/cli/{SUB}.json", json.dumps({
    "session_id": SUB, "cwd": "/work/shop", "parent_session_id": K1, "session_created_reason": "subagent"}))
write(f"home/sessions/cli/{SUB}.jsonl", jsonl(
    rec("Prompt", "s1", [text("Check the other rounding call sites")], T + 60),
    rec("AssistantMessage", "s2", [text("Checking."), use("u1", "write", {"command": "append", "path": "/work/shop/NOTES.md", "content": "rounding checked\n"})]),
    rec("ToolResults", "s3", [result("u1", [text("ok")])]),
    rec("AssistantMessage", "s4", [text("Two other call sites, both fine.")]),
))

# V3 engine: a tool call written at each state, encrypted thinking, metadata.
V3 = "sess_00000000-0000-4000-8000-0000000000b3"
write(f"home/sessions/notes-1a2b/{V3}/session.json", json.dumps({
    "schemaVersion": "1.0.0", "id": V3, "title": "Tidy the notes index", "modelId": "auto",
    "workspacePaths": ["/work/notes"], "createdAt": "2026-10-04T10:00:00.000Z", "lastModifiedAt": "2026-10-04T10:02:00.000Z"}))
call = lambda i, name, args, status, ts: {"id": f"{i}-call", "timestamp": ts, "payload": {
    "type": "tool_call", "toolCallId": i, "toolName": name, "args": args, "status": status, "actionType": "x"}}
write(f"home/sessions/notes-1a2b/{V3}/messages.jsonl", jsonl(
    {"id": "u1", "timestamp": "2026-10-04T10:00:01.000Z", "payload": {"type": "user", "content": "Sort the index and drop the dead link"}},
    {"id": "r1-say", "timestamp": "2026-10-04T10:00:02.000Z", "payload": {"type": "assistant", "content": "...", "operationType": "Reasoning", "reasoningSignature": "x"}},
    {"id": "m1", "timestamp": "2026-10-04T10:00:03.000Z", "payload": {"type": "session_metadata", "key": "contextUsage", "value": {"usagePercentage": 2}}},
    call("c1", "str_replace", {"path": "/work/notes/index.md", "oldStr": "- [dead](x)\n", "newStr": ""}, "awaiting_approval", "2026-10-04T10:00:04.000Z"),
    call("c1", "str_replace", {"path": "/work/notes/index.md", "oldStr": "- [dead](x)\n", "newStr": ""}, "executing", "2026-10-04T10:00:05.000Z"),
    call("c1", "str_replace", {"path": "/work/notes/index.md", "oldStr": "- [dead](x)\n", "newStr": ""}, "completed", "2026-10-04T10:00:06.000Z"),
    {"id": "c1-result", "timestamp": "2026-10-04T10:00:06.000Z", "payload": {"type": "tool_result", "toolCallId": "c1", "content": "Replaced", "success": True}},
    call("c2", "fs_write", {"path": "/work/notes/old.md", "text": "gone\n"}, "denied", "2026-10-04T10:00:07.000Z"),
    call("c3", "execute_bash", {"command": "sort -o index.md index.md"}, "completed", "2026-10-04T10:00:08.000Z"),
    {"id": "c3-result", "timestamp": "2026-10-04T10:00:08.000Z", "payload": {"type": "tool_result", "toolCallId": "c3", "content": "Output:\n\nExit Code: 0", "success": True}},
    {"id": "a1-say", "timestamp": "2026-10-04T10:00:09.000Z", "payload": {"type": "assistant", "content": "Sorted, and the dead link is gone.", "operationType": "Say"}},
    {"id": "e1", "timestamp": "2026-10-04T10:00:09.000Z", "payload": {"type": "turn_end", "stopReason": "end_turn"}},
))
write("home/session-index/notes-1a2b.jsonl", jsonl({"op": "add", "sessionPath": f"notes-1a2b/{V3}"}))

# Database: a headless conversation, a copy of the CLI session (read from
# its file instead), and a Kiro CLI 1.x conversation.
HL = "9f8e7d6c-0000-4000-8000-000000000002"
headless = {"conversation_id": HL, "model_info": {"model_id": "claude-haiku-4.5"}, "history": [
    {"user": {"content": {"Prompt": {"prompt": "Add a changelog entry for 1.4"}}, "timestamp": "2026-10-05T08:00:00+00:00",
              "env_context": {"env_state": {"current_working_directory": "/work/api"}}},
     "assistant": {"ToolUse": {"message_id": "a1", "content": "Adding it.", "tool_uses": [
         {"id": "w1", "name": "fs_write", "args": {"command": "insert", "path": "/work/api/CHANGELOG.md", "insert_line": 1, "new_str": "## 1.4\n"}},
         {"id": "b1", "name": "execute_bash", "args": {"command": "git diff --stat"}}]}}},
    {"user": {"content": {"ToolUseResults": {"tool_use_results": [
        {"tool_use_id": "w1", "content": [{"Text": ""}], "status": "Success"},
        {"tool_use_id": "b1", "content": [{"Json": {"exit_status": "0", "stdout": " CHANGELOG.md | 1 +", "stderr": ""}}], "status": "Success"}]}},
              "timestamp": None},
     "assistant": {"Response": {"message_id": "a2", "content": "Done: 1.4 is at the top of the changelog."}}},
]}
legacy = {"conversation_id": "5a5a5a5a-0000-4000-8000-000000000003", "history": [
    {"user": {"content": {"Prompt": {"prompt": "What does the deploy script do?"}}, "timestamp": "2025-11-20T12:00:00+00:00"},
     "assistant": {"Response": {"message_id": "x1", "content": "It builds the image and pushes it."}}},
]}
os.makedirs(os.path.join(out, "data"), exist_ok=True)
db = sqlite3.connect(os.path.join(out, "data", "data.sqlite3"))
db.executescript("""
CREATE TABLE conversations (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE conversations_v2 (
    key TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    value TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (key, conversation_id)
);
""")
db.execute("INSERT INTO conversations_v2 VALUES (?,?,?,?,?)", ("/work/api", HL, json.dumps(headless), 1791187200000, 1791187260000))
db.execute("INSERT INTO conversations_v2 VALUES (?,?,?,?,?)", ("/work/shop", K1, json.dumps({"conversation_id": K1, "history": []}), 1791018000000, 1791018300000))
db.execute("INSERT INTO conversations VALUES (?,?)", ("/work/ops", json.dumps(legacy)))
db.commit()
db.close()

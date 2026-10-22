# Builds the synthetic Gemini CLI fixtures used by the tests:
#   python3 testdata/gemini/gen.py testdata/gemini
# Shapes follow Gemini CLI's chatRecordingService (0.67). Nothing here comes
# from a real session.
import json, os, sys

out = sys.argv[1]


def write(path, text):
    path = os.path.join(out, path)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def jsonl(*records):
    return "".join(json.dumps(r) + "\n" for r in records)


SID = "1a2b3c4d-0000-4000-8000-000000000001"
fr = lambda name, output: [{"functionResponse": {"id": "x", "name": name, "response": {"output": output}}}]

write("projects.json", json.dumps({"projects": {"/work/shop": "shop", "/work/notes": "notes"}}, indent=2))
write("tmp/shop/.project_root", "/work/shop")

shell = {"id": "call-1", "name": "run_shell_command", "args": {"command": "go test ./...", "description": "Run tests"},
         "status": "executing", "timestamp": "2026-10-02T09:00:20.000Z"}
shell_done = dict(shell, status="success", result=fr("run_shell_command", "FAIL cart_test.go"),
                  resultDisplay="FAIL cart_test.go")
edit = {"id": "call-2", "name": "replace", "status": "success", "timestamp": "2026-10-02T09:00:30.000Z",
        "args": {"file_path": "/work/shop/cart.go", "instruction": "Round the total", "old_string": "total := sum", "new_string": "total := math.Round(sum*100) / 100"},
        "result": fr("replace", "Successfully modified file"), "resultDisplay": {"fileDiff": "...", "fileName": "cart.go"}}
bad = {"id": "call-3", "name": "replace", "status": "error", "timestamp": "2026-10-02T09:00:31.000Z",
       "args": {"file_path": "/work/shop/missing.go", "old_string": "a", "new_string": "b"},
       "result": [{"functionResponse": {"id": "x", "name": "replace", "response": {"error": "File not found: missing.go"}}}]}
write_ = {"id": "call-4", "name": "write_file", "status": "success", "timestamp": "2026-10-02T09:00:40.000Z",
          "args": {"file_path": "cart_test.go", "content": "package cart\n\n// new test\n"},
          "result": fr("write_file", "Wrote file"),
          "resultDisplay": {"fileDiff": "...", "fileName": "cart_test.go", "originalContent": "package cart\n", "newContent": "package cart\n\n// new test\n"}}
created = dict(write_, id="call-5", timestamp="2026-10-02T09:00:41.000Z",
               args={"file_path": "/work/shop/README.md", "content": "# shop\n"},
               resultDisplay={"fileDiff": "...", "fileName": "README.md", "originalContent": None, "newContent": "# shop\n"})

write("tmp/shop/chats/session-2026-10-02T09-00-1a2b3c4d.jsonl", jsonl(
    {"sessionId": SID, "projectHash": "abc", "startTime": "2026-10-02T09:00:00.000Z", "lastUpdated": "2026-10-02T09:00:00.000Z", "kind": "main"},
    {"id": "m1", "timestamp": "2026-10-02T09:00:05.000Z", "type": "user",
     "content": [{"text": "Cart totals are off by a cent, fix it\n--- Content from referenced files ---\n(long file)"}],
     "displayContent": [{"text": "Cart totals are off by a cent, fix it"}]},
    {"id": "m2", "timestamp": "2026-10-02T09:00:10.000Z", "type": "gemini", "content": "", "model": "gemini-3-pro",
     "thoughts": [{"subject": "Checking the tests", "description": "Run them first.", "timestamp": "2026-10-02T09:00:09.000Z"}],
     "toolCalls": [shell]},
    # The same message again once the shell call finished: last write wins, position kept.
    {"id": "m2", "timestamp": "2026-10-02T09:00:10.000Z", "type": "gemini", "content": "", "model": "gemini-3-pro",
     "thoughts": [{"subject": "Checking the tests", "description": "Run them first.", "timestamp": "2026-10-02T09:00:09.000Z"}],
     "toolCalls": [shell_done, edit, bad, write_, created]},
    {"id": "m3", "timestamp": "2026-10-02T09:00:50.000Z", "type": "gemini", "content": "draft", "model": "gemini-3-pro"},
    {"$patch": {"id": "m3", "content": [{"text": "thinking...", "thought": True}, {"text": "Fixed the rounding and added a test."}]}},
    {"id": "m4", "timestamp": "2026-10-02T09:01:00.000Z", "type": "info", "content": "Checkpoint saved"},
    # A turn the person rewound: it must not show.
    {"id": "m5", "timestamp": "2026-10-02T09:02:00.000Z", "type": "user", "content": "Actually delete the cart"},
    {"id": "m6", "timestamp": "2026-10-02T09:02:05.000Z", "type": "gemini", "content": "Deleting it.", "model": "gemini-3-pro"},
    {"$rewindTo": "m5"},
    {"id": "m7", "timestamp": "2026-10-02T09:03:00.000Z", "type": "user", "content": "/stats"},
    {"$set": {"summary": "Fix cart rounding", "lastUpdated": "2026-10-02T09:03:00.000Z"}},
))

# A subagent run, filed under its parent: not a session of its own.
write("tmp/shop/chats/%s/sub-1.jsonl" % SID, jsonl(
    {"sessionId": "sub-1", "projectHash": "abc", "startTime": "2026-10-02T09:00:12.000Z", "kind": "subagent"},
    {"id": "s1", "timestamp": "2026-10-02T09:00:12.000Z", "type": "user", "content": "investigate"},
))

# An older session: one JSON document, in a project known only from projects.json.
legacy = {"sessionId": "22222222-0000-4000-8000-000000000002", "projectHash": "def",
          "startTime": "2026-05-01T10:00:00.000Z", "lastUpdated": "2026-05-01T10:05:00.000Z",
          "messages": [
              {"id": "a", "timestamp": "2026-05-01T10:00:00.000Z", "type": "user", "content": [{"text": "Summarise my notes"}]},
              {"id": "b", "timestamp": "2026-05-01T10:01:00.000Z", "type": "gemini", "content": "Here is a summary.", "model": "gemini-2.5-pro"},
          ]}
write("tmp/notes/chats/session-2026-05-01T10-00-22222222.json", json.dumps(legacy, indent=2))
# A legacy session that was resumed: the .json stays, the .jsonl is the session.
write("tmp/notes/chats/session-2026-05-02T10-00-33333333.json", json.dumps(dict(legacy, sessionId="33333333-0000-4000-8000-000000000003"), indent=2))
write("tmp/notes/chats/session-2026-05-02T10-00-33333333.jsonl", jsonl(
    {"sessionId": "33333333-0000-4000-8000-000000000003", "projectHash": "def", "startTime": "2026-05-02T10:00:00.000Z"},
    {"id": "a", "timestamp": "2026-05-02T10:00:00.000Z", "type": "user", "content": "Resumed notes chat"},
))

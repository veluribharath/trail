# Builds the synthetic OpenCode database used by the tests:
#   python3 testdata/opencode/gen.py testdata/opencode
# The schema follows OpenCode 1.19 (the 1.2 tables plus later ALTERs). Nothing
# here comes from a real session. The last transaction is left in the WAL,
# un-checkpointed, followed by a torn frame that readers must ignore.
import json, os, shutil, sqlite3, sys, tempfile

out = sys.argv[1]
tmp = tempfile.mkdtemp()
path = os.path.join(tmp, "opencode.db")
db = sqlite3.connect(path, isolation_level=None)
db.execute("PRAGMA page_size = 1024")
db.execute("PRAGMA journal_mode = WAL")
db.execute("PRAGMA wal_autocheckpoint = 0")

db.executescript("""
CREATE TABLE `project` (
  `id` text PRIMARY KEY,
  `worktree` text NOT NULL,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `sandboxes` text NOT NULL
);
CREATE TABLE `message` (
  `id` text PRIMARY KEY,
  `session_id` text NOT NULL,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `data` text NOT NULL,
  CONSTRAINT `fk_message_session_id_session_id_fk` FOREIGN KEY (`session_id`) REFERENCES `session`(`id`) ON DELETE CASCADE
);
CREATE TABLE `part` (
  `id` text PRIMARY KEY,
  `message_id` text NOT NULL,
  `session_id` text NOT NULL,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `data` text NOT NULL,
  CONSTRAINT `fk_part_message_id_message_id_fk` FOREIGN KEY (`message_id`) REFERENCES `message`(`id`) ON DELETE CASCADE
);
CREATE TABLE `session` (
  `id` text PRIMARY KEY,
  `project_id` text NOT NULL,
  `parent_id` text,
  `slug` text NOT NULL,
  `directory` text NOT NULL,
  `title` text NOT NULL,
  `version` text NOT NULL,
  `share_url` text,
  `summary_additions` integer,
  `summary_deletions` integer,
  `summary_files` integer,
  `summary_diffs` text,
  `revert` text,
  `permission` text,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `time_compacting` integer,
  `time_archived` integer,
  CONSTRAINT `fk_session_project_id_project_id_fk` FOREIGN KEY (`project_id`) REFERENCES `project`(`id`) ON DELETE CASCADE
);
CREATE INDEX `part_message_idx` ON `part` (`message_id`);
CREATE INDEX `part_session_idx` ON `part` (`session_id`);
CREATE INDEX `session_parent_idx` ON `session` (`parent_id`);
ALTER TABLE `session` ADD `workspace_id` text;
ALTER TABLE `session` ADD `path` text;
ALTER TABLE `session` ADD `metadata` text;
ALTER TABLE `session` ADD `cost` real DEFAULT 0 NOT NULL;
ALTER TABLE `session` ADD `tokens_input` integer DEFAULT 0 NOT NULL;
ALTER TABLE `session` ADD `tokens_output` integer DEFAULT 0 NOT NULL;
ALTER TABLE `session` ADD `agent` text;
ALTER TABLE `session` ADD `model` text;
CREATE INDEX `message_session_time_created_id_idx` ON `message` (`session_id`,`time_created`,`id`);
""")

T0 = 1790000000000  # 2026-09-21T14:13:20Z
seq = [0]


def nid(prefix):
    seq[0] += 1
    return "%s_%012x" % (prefix, seq[0])


def session(sid, title, directory="/work/shop", parent=None, t=T0, cost=0.0):
    db.execute(
        "INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated, cost, tokens_input, tokens_output) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (sid, "proj1", parent, "slug", directory, title, "1.19.3", t, t, cost, 10, 20),
    )


def message(sid, role, t, **extra):
    mid = nid("msg")
    data = {"role": role, "time": {"created": t}}
    if role == "assistant":
        data.update({"modelID": "claude-sonnet-4-5", "providerID": "anthropic", "mode": "build", "path": {"cwd": "/work/shop", "root": "/work/shop"}})
        data["time"]["completed"] = t + 5000
    else:
        data.update({"agent": "build", "model": {"providerID": "anthropic", "modelID": "claude-sonnet-4-5"}})
    data.update(extra)
    db.execute("INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)", (mid, sid, t, t, json.dumps(data)))
    return mid


def part(sid, mid, t, data):
    pid = nid("prt")
    db.execute("INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)", (pid, mid, sid, t, t, json.dumps(data)))
    return pid


def tool(sid, mid, t, name, inp, output="", status="completed", **meta):
    state = {"status": status, "input": inp, "time": {"start": t, "end": t + 100}}
    if status == "completed":
        state.update({"output": output, "title": name, "metadata": meta})
    elif status == "error":
        state.update({"error": output})
    return part(sid, mid, t, {"type": "tool", "callID": nid("call"), "tool": name, "state": state})


db.execute("INSERT INTO project VALUES ('proj1', '/work/shop', ?, ?, '[]')", (T0, T0))

# o1: a session with every kind of part, and a subagent.
db.execute("BEGIN")
session("ses_o1", "Fix the login redirect", t=T0)
session("ses_o1child", "Child session - 2026-09-21T14:14:00.000Z", parent="ses_o1", t=T0 + 40000)
u = message("ses_o1", "user", T0 + 1000)
part("ses_o1", u, T0 + 1000, {"type": "text", "text": "The login page redirects to /home even when ?next is set. Fix it."})
part("ses_o1", u, T0 + 1000, {"type": "text", "text": "Called the Read tool with the following input", "synthetic": True})
a = message("ses_o1", "assistant", T0 + 2000)
part("ses_o1", a, T0 + 2000, {"type": "step-start"})
part("ses_o1", a, T0 + 2000, {"type": "reasoning", "text": "Look at the redirect handler first.", "time": {"start": T0 + 2000}})
tool("ses_o1", a, T0 + 3000, "bash", {"command": "grep -rn redirect src/", "description": "Find redirect code"}, "src/login.ts:12: redirect('/home')")
tool("ses_o1", a, T0 + 4000, "edit", {"filePath": "/work/shop/src/login.ts", "oldString": "redirect('/home')", "newString": "redirect(next ?? '/home')"}, "Edit applied")
tool("ses_o1", a, T0 + 5000, "edit", {"filePath": "/work/shop/src/missing.ts", "oldString": "x", "newString": "y"}, "File not found", status="error")
tool("ses_o1", a, T0 + 6000, "write", {"filePath": "/work/shop/src/next.ts", "content": "export const next = 1\n"}, "Wrote file")
tool("ses_o1", a, T0 + 7000, "apply_patch", {"patchText": "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old()\n+new()\n*** End Patch"}, "Success")
tool("ses_o1", a, T0 + 8000, "task", {"description": "Add a test", "prompt": "Write a test for the redirect", "subagent_type": "general"}, "Done", sessionId="ses_o1child")
part("ses_o1", a, T0 + 9000, {"type": "text", "text": "Fixed: the redirect now honours ?next."})
part("ses_o1", a, T0 + 9000, {"type": "step-finish", "reason": "stop", "cost": 0.01, "tokens": {"input": 1, "output": 2, "reasoning": 0, "cache": {"read": 0, "write": 0}}})
cu = message("ses_o1child", "user", T0 + 8100)
part("ses_o1child", cu, T0 + 8100, {"type": "text", "text": "Write a test for the redirect"})
ca = message("ses_o1child", "assistant", T0 + 8200)
tool("ses_o1child", ca, T0 + 8300, "write", {"filePath": "/work/shop/test/login.test.ts", "content": "test('next', () => {})\n"}, "Wrote file")
part("ses_o1child", ca, T0 + 8400, {"type": "text", "text": "Added test/login.test.ts."})
db.execute("COMMIT")

# o2: still has its placeholder title, has a tool output that spills onto
# overflow pages, and has enough parts to need interior b-tree pages.
db.execute("BEGIN")
session("ses_o2", "New session - 2026-09-22T09:00:00.000Z", directory="/work/other", t=T0 + 86400000)
u = message("ses_o2", "user", T0 + 86401000)
part("ses_o2", u, T0 + 86401000, {"type": "text", "text": "List every file"})
a = message("ses_o2", "assistant", T0 + 86402000)
tool("ses_o2", a, T0 + 86403000, "bash", {"command": "find ."}, "\n".join("./file%05d.txt" % i for i in range(400)))
for i in range(200):
    tool("ses_o2", a, T0 + 86404000 + i, "read", {"filePath": "/work/other/file%05d.txt" % i}, "contents of file %d" % i)
part("ses_o2", a, T0 + 86900000, {"type": "text", "text": "Listed them all."})
db.execute("COMMIT")

db.execute("PRAGMA wal_checkpoint(TRUNCATE)")

# Left in the WAL: OpenCode renames o1 and o1 gets another turn.
db.execute("BEGIN")
db.execute("UPDATE session SET title = 'Honour ?next on login', time_updated = ? WHERE id = 'ses_o1'", (T0 + 60000,))
u = message("ses_o1", "user", T0 + 50000)
part("ses_o1", u, T0 + 50000, {"type": "text", "text": "Also handle an empty next"})
a = message("ses_o1", "assistant", T0 + 51000)
part("ses_o1", a, T0 + 51000, {"type": "text", "text": "Empty next now falls back to /home."})
db.execute("COMMIT")

os.makedirs(out, exist_ok=True)
shutil.copyfile(path, os.path.join(out, "opencode.db"))
wal = open(path + "-wal", "rb").read()
# A torn frame: a valid-looking header and salt but a page that doesn't match
# the checksum, as if the writer was still mid-write.
frame = 24 + 1024
torn = bytearray(wal[32 : 32 + frame])
torn[24:] = b"\xff" * 1024
open(os.path.join(out, "opencode.db-wal"), "wb").write(wal + bytes(torn))
db.close()
shutil.rmtree(tmp)

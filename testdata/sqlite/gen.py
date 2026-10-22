# Builds the small database the SQLite reader's fuzz test starts from:
#   python3 testdata/sqlite/gen.py testdata/sqlite
# 512-byte pages keep it small enough to mutate quickly while still covering
# interior pages, overflow chains, an explicit index and an automatic one.
import os, sqlite3, sys

out = os.path.join(sys.argv[1], "small.db")
if os.path.exists(out):
    os.remove(out)
db = sqlite3.connect(out, isolation_level=None)
db.execute("PRAGMA page_size = 512")
db.executescript("""
CREATE TABLE note (id INTEGER PRIMARY KEY, name text NOT NULL, body text, n real);
CREATE INDEX note_name ON note (name);
CREATE TABLE tag (`id` text PRIMARY KEY, "note" integer, [color] blob);
""")
for i in range(40):
    db.execute("INSERT INTO note (name, body, n) VALUES (?, ?, ?)",
               ("n%02d" % (i % 7), "body %d " % i * (1 + i % 5), i / 3))
db.execute("INSERT INTO note (name, body) VALUES ('long', ?)", ("x" * 3000,))
for i in range(12):
    db.execute("INSERT INTO tag VALUES (?, ?, ?)", ("t%d" % i, i, bytes([i, 255 - i])))
db.close()

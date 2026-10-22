package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fuzz tests below run their seeds as ordinary tests under `go test`.
// To search for new crashes, run one at a time, for example:
//
//	make fuzz            # each for a minute, or:
//	go test -run '^$' -fuzz FuzzSQLite -fuzztime 2m -fuzzminimizetime 5s
//
// Inputs that fail are saved under testdata/fuzz/ and replayed from then on.

// FuzzSQLite feeds the SQLite reader a mangled database and write-ahead log.
// Whatever the bytes, it must return an error rather than panic, hang or
// allocate without bound, since trail reads a database another program is
// writing.
func FuzzSQLite(f *testing.F) {
	f.Add(readFile(f, "testdata/sqlite/small.db"), []byte(nil))
	f.Fuzz(func(t *testing.T, db, wal []byte) {
		path := writeDB(t, db, wal)
		if d, err := openSQLite(path); err == nil {
			readEverything(d)
			d.Close()
		}
	})
}

// FuzzOpenCode runs the OpenCode provider on a mangled copy of its fixture,
// covering the reader's index lookups and the parsing of rows. The seed is
// large, so pass -fuzzminimizetime 5s or so to keep the fuzzer from spending
// minutes shrinking each new input.
func FuzzOpenCode(f *testing.F) {
	f.Add(readFile(f, "testdata/opencode/opencode.db"), readFile(f, "testdata/opencode/opencode.db-wal"))
	f.Fuzz(func(t *testing.T, db, wal []byte) {
		path := writeDB(t, db, wal)
		p := &opencodeProvider{dir: filepath.Dir(path)}
		paths, _ := p.Discover()
		for _, s := range paths {
			p.Stamp(s)
			p.Parse(s, true)
		}
	})
}

func readFile(f *testing.F, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	return b
}

// writeDB writes a database, and its WAL if there is one, as opencode.db in
// a fresh directory.
func writeDB(t *testing.T, db, wal []byte) string {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(wal) > 0 {
		if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// readEverything scans every table and index in the schema, and looks a few
// keys up through each index, ignoring the errors.
func readEverything(d *sqliteDB) {
	for _, s := range d.schema {
		switch s.typ {
		case "table":
			t := d.Table(s.name)
			if t == nil {
				continue
			}
			t.Scan(func(r sqliteRow) error {
				for _, c := range t.cols {
					r.Str(c)
					r.Int(c)
					r.Float(c)
					r.Null(c)
				}
				return nil
			})
			for _, c := range t.cols {
				t.Lookup(c, "n03")
			}
			t.Get(1)
			t.Get(-1)
		case "index":
			d.walkIndex(s.root, func([]any) {})
			d.indexRowids(s.root, "t1")
		}
	}
}

// FuzzParsePatch checks the apply_patch parser and the helpers that find a
// patch inside a shell command.
func FuzzParsePatch(f *testing.F) {
	f.Add("*** Begin Patch\n*** Update File: a.go\n@@ func a\n-x\n+y\n context\n\n*** Add File: b.txt\n+hi\n*** Delete File: c\n*** Update File: d\n*** Move to: e\n@@\n-1\n+2\n*** End of File\n*** End Patch", "/work")
	f.Add("*** Begin Patch\n*** Add File: /abs\n+\n+\n\n*** End Patch\n", "")
	f.Add("*** Begin Patch\n*** Update File: \n@@\n", "rel")
	f.Add("bash -lc apply_patch <<'EOF'\n*** Begin Patch\n*** Delete File: x\n*** End Patch\nEOF", ".")
	f.Fuzz(func(t *testing.T, patch, cwd string) {
		for _, c := range parsePatch(patch, cwd) {
			switch c.Op {
			case "update", "add", "delete", "move":
			default:
				t.Fatalf("unknown op %q", c.Op)
			}
			if c.Path == "" {
				t.Fatalf("change without a path: %+v", c)
			}
			if c.Op == "move" && c.MoveTo == "" {
				t.Fatalf("move without a destination: %+v", c)
			}
		}
		patchSummary(patch)
		shellSummary([]string{"apply_patch", patch})
		shellSummary([]string{"bash", "-lc", patch})
		shellSummary(strings.Fields(patch))
	})
}

// FuzzTranscripts runs each line-based parser on arbitrary input, seeded with
// the fixtures. A session file can be cut off mid-write or come from a newer
// agent version, so nothing in it may crash the parser.
func FuzzTranscripts(f *testing.F) {
	for _, g := range []string{
		"testdata/claude/projects/*/*.jsonl",
		"testdata/codex/sessions/*/*/*/*.jsonl",
		"testdata/gemini/tmp/*/chats/*.json*",
		"testdata/gemini/tmp/*/chats/*/*.jsonl",
	} {
		files, _ := filepath.Glob(g)
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(b, strings.HasSuffix(file, ".json"))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, whole bool) {
		dir := t.TempDir()
		name := "s.jsonl"
		if whole {
			name = "s.json"
		}
		path := filepath.Join(dir, name)
		for _, parse := range []func() (*Transcript, error){
			func() (*Transcript, error) {
				return (&claudeProvider{dir: dir}).ParseLines(bytes.NewReader(b), path, true)
			},
			func() (*Transcript, error) {
				return (&codexProvider{dir: dir}).ParseLines(bytes.NewReader(b), path, true)
			},
			func() (*Transcript, error) {
				return (&geminiProvider{dir: dir}).ParseLines(bytes.NewReader(b), path, true)
			},
		} {
			tr, err := parse()
			if err != nil || tr == nil {
				continue
			}
			if tr.Session == nil {
				t.Fatal("transcript without a session")
			}
			for _, ev := range tr.Events {
				for _, ci := range ev.Changes {
					if ci < 0 || ci >= len(tr.Changes) {
						t.Fatalf("event points at change %d of %d", ci, len(tr.Changes))
					}
				}
			}
		}
	})
}

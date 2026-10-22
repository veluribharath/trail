package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture is built by testdata/opencode/gen.py: two sessions and a
// subagent, a second turn and a rename left in the WAL, and a torn frame
// after the last commit.

func TestSQLiteReader(t *testing.T) {
	db, err := openSQLite("testdata/opencode/opencode.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	titles := map[string]string{}
	err = db.Table("session").Scan(func(r sqliteRow) error {
		titles[r.Str("id")] = r.Str("title")
		if r.Str("version") != "1.19.3" || r.Int("tokens_output") != 20 {
			t.Errorf("columns added by ALTER TABLE: %+v", r.vals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 3 || titles["ses_o1"] != "Honour ?next on login" {
		t.Errorf("the committed WAL rename should win: %v", titles)
	}

	parts := db.Table("part")
	n := 0
	if err := parts.Scan(func(sqliteRow) error { n++; return nil }); err != nil || n != 220 {
		t.Fatalf("part scan: %d rows, %v", n, err)
	}

	// Through the index (which has interior pages) and by scanning must agree.
	viaIndex, err := parts.Lookup("session_id", "ses_o2")
	if err != nil || len(viaIndex) != 203 {
		t.Fatalf("index lookup: %d rows, %v", len(viaIndex), err)
	}
	if db.indexOn("part", "session_id") == 0 || db.indexOn("session", "id") == 0 {
		t.Error("indexes not found")
	}
	big := ""
	for _, r := range viaIndex {
		if d := r.Str("data"); strings.Contains(d, `"find ."`) {
			big = d
		}
	}
	if !strings.Contains(big, "./file00399.txt") {
		t.Errorf("a row on overflow pages came back short: %d bytes", len(big))
	}
	if rows, _ := db.Table("message").Lookup("session_id", "ses_o1"); len(rows) != 4 {
		t.Errorf("message lookup through a three-column index: %d rows", len(rows))
	}
	if rows, _ := parts.Lookup("session_id", "ses_nope"); len(rows) != 0 {
		t.Errorf("missing key: %d rows", len(rows))
	}

	// Without the WAL only checkpointed data is there.
	dir := t.TempDir()
	copyFile(t, "testdata/opencode/opencode.db", filepath.Join(dir, "opencode.db"))
	old, err := openSQLite(filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	rows, err := old.Table("session").Lookup("id", "ses_o1")
	if err != nil || len(rows) != 1 || rows[0].Str("title") != "Fix the login redirect" {
		t.Errorf("checkpointed title: %v %v", rows, err)
	}
}

func TestOpenCodeParse(t *testing.T) {
	p := &opencodeProvider{dir: "testdata/opencode"}
	paths, err := p.Discover()
	if err != nil || len(paths) != 2 {
		t.Fatalf("discover should list the two top-level sessions: %v %v", paths, err)
	}
	o1 := p.dbPath() + "#ses_o1"
	stamp, ok := p.Stamp(o1)
	if !ok {
		t.Fatal("no stamp for o1")
	}
	// o1 has a subagent; its stamp must not depend on map order.
	for i := 0; i < 20; i++ {
		p.Discover()
		if again, _ := p.Stamp(o1); again != stamp {
			t.Fatalf("stamp changed without the session changing:\n%s\n%s", stamp, again)
		}
	}
	tr, err := p.Parse(o1, true)
	if err != nil {
		t.Fatal(err)
	}
	s := tr.Session
	if s.Title != "Honour ?next on login" || s.TitleKind != "generated" || s.Cwd != "/work/shop" || s.Model != "claude-sonnet-4-5" {
		t.Errorf("summary fields: %+v", s)
	}
	if s.Prompt != "The login page redirects to /home even when ?next is set. Fix it." {
		t.Errorf("prompt should skip synthetic text: %q", s.Prompt)
	}
	if s.Resume != "cd /work/shop && opencode --session ses_o1" {
		t.Errorf("resume: %q", s.Resume)
	}
	// edit, write, apply_patch, and the subagent's write; the failed edit is left out.
	var rels []string
	for _, c := range tr.Changes {
		rels = append(rels, c.Op+" "+c.Rel)
	}
	if got := strings.Join(rels, ", "); got != "edit src/login.ts, write src/next.ts, update src/app.ts, write test/login.test.ts" {
		t.Errorf("changes: %s", got)
	}
	if c := tr.Changes[0]; c.Hunks[0].Old != "redirect('/home')" || c.Hunks[0].New != "redirect(next ?? '/home')" {
		t.Errorf("edit hunk: %+v", c.Hunks)
	}
	var bash, failed, task *Event
	side, thinking := 0, 0
	for i := range tr.Events {
		e := &tr.Events[i]
		switch {
		case e.Tool == "bash":
			bash = e
		case e.Tool == "edit" && e.Error:
			failed = e
		case e.Tool == "task":
			task = e
		case e.Kind == "thinking":
			thinking++
		}
		if e.Sidechain {
			side++
		}
	}
	if bash == nil || bash.Summary != "grep -rn redirect src/" || bash.Input != "$ grep -rn redirect src/" || !strings.Contains(bash.Output, "login.ts:12") {
		t.Errorf("bash event: %+v", bash)
	}
	if failed == nil || failed.Output != "File not found" {
		t.Errorf("failed edit: %+v", failed)
	}
	if task == nil || task.Summary != "Add a test" {
		t.Errorf("task event: %+v", task)
	}
	if side != 3 || thinking != 1 {
		t.Errorf("sidechain=%d thinking=%d", side, thinking)
	}
	if last := tr.Events[len(tr.Events)-1]; last.Kind != "assistant" || last.Text != "Empty next now falls back to /home." {
		t.Errorf("the turn in the WAL should come last: %+v", last)
	}
	if s.Messages != 6 {
		t.Errorf("messages: %d", s.Messages)
	}

	sum, err := p.Parse(o1, false)
	if err != nil || sum.Session.Edits != s.Edits || sum.Session.Messages != s.Messages || len(sum.Events) != 0 {
		t.Errorf("summary parse disagrees: %+v %v", sum.Session, err)
	}

	o2, err := p.Parse(p.dbPath()+"#ses_o2", false)
	if err != nil || o2.Session.Title != "List every file" || o2.Session.TitleKind != "prompt" || o2.Session.Cwd != "/work/other" {
		t.Errorf("placeholder title should fall back to the prompt: %+v %v", o2.Session, err)
	}
}

func TestOpenCodeIndexRefresh(t *testing.T) {
	// Start from the checkpointed database alone, then let the WAL appear the
	// way it does while OpenCode runs: the index must notice and re-parse.
	dir := t.TempDir()
	copyFile(t, "testdata/opencode/opencode.db", filepath.Join(dir, "opencode.db"))
	idx := NewIndex(&opencodeProvider{dir: dir})
	st := idx.Scan(0)
	if len(st) != 1 || st[0].Sessions != 2 || st[0].Error != "" {
		t.Fatalf("status: %+v", st)
	}
	if s := idx.Get("opencode:ses_o1"); s == nil || s.Title != "Fix the login redirect" || s.Messages != 4 {
		t.Fatalf("before: %+v", s)
	}
	copyFile(t, "testdata/opencode/opencode.db-wal", filepath.Join(dir, "opencode.db-wal"))
	idx.Scan(0)
	if s := idx.Get("opencode:ses_o1"); s == nil || s.Title != "Honour ?next on login" || s.Messages != 6 {
		t.Errorf("after: %+v", s)
	}
	tr, err := idx.Load("opencode:ses_o1")
	if err != nil || tr.Session.Key != "opencode:ses_o1" || len(tr.Changes) != 4 {
		t.Errorf("load: %v", err)
	}

	// No database at all is not an error.
	empty := NewIndex(&opencodeProvider{dir: filepath.Join(dir, "missing")})
	if st := empty.Scan(0); st[0].Found || st[0].Error != "" {
		t.Errorf("missing dir: %+v", st)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeStampBetweenScans(t *testing.T) {
	// Between scans, Stamp works a session's stamp out on its own; it must
	// match what Discover computed for the same data.
	p := &opencodeProvider{dir: "testdata/opencode"}
	paths, _ := p.Discover()
	for _, path := range paths {
		want, _ := p.Stamp(path)
		p.mu.Lock()
		p.stampsDB = nil
		p.mu.Unlock()
		if got, ok := p.Stamp(path); !ok || got != want {
			t.Errorf("%s: fresh stamp %q, Discover's %q", path, got, want)
		}
		p.Discover()
	}
	p.stampsDB = nil
	if _, ok := p.Stamp(p.dbPath() + "#ses_nope"); ok {
		t.Error("a missing session has no stamp")
	}
}

func TestOpenCodeLive(t *testing.T) {
	defer func(p, i time.Duration) { livePoll, liveIdle = p, i }(livePoll, liveIdle)
	livePoll, liveIdle = 10*time.Millisecond, time.Minute

	dir := t.TempDir()
	copyFile(t, "testdata/opencode/opencode.db", filepath.Join(dir, "opencode.db"))
	idx := NewIndex(&opencodeProvider{dir: dir})
	idx.Scan(0)
	before, err := idx.Load("opencode:ses_o1")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{index: idx}).Handler())
	defer srv.Close()
	resp, err := http.Get(fmt.Sprintf("%s/api/live?id=opencode:ses_o1&size=%d", srv.URL, before.Size))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	next := func() (string, liveUpdate) {
		var event string
		var u liveUpdate
		for {
			ln, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
			switch {
			case strings.HasPrefix(ln, "event: "):
				event = strings.TrimSpace(ln[7:])
			case strings.HasPrefix(ln, "data: "):
				json.Unmarshal([]byte(ln[6:]), &u)
			case ln == "\n":
				return event, u
			}
		}
	}
	if ev, _ := next(); ev != "ready" {
		t.Fatalf("an up-to-date viewer should get ready first, got %q", ev)
	}
	// OpenCode commits the next turn: it shows up in the WAL.
	copyFile(t, "testdata/opencode/opencode.db-wal", filepath.Join(dir, "opencode.db-wal"))
	ev, u := next()
	if ev != "update" || u.From != len(before.Events) || len(u.Events) != 2 || u.Session.Title != "Honour ?next on login" {
		t.Fatalf("update: %q from=%d events=%d session=%+v", ev, u.From, len(u.Events), u.Session)
	}
	os.Remove(filepath.Join(dir, "opencode.db"))
	os.Remove(filepath.Join(dir, "opencode.db-wal"))
	if ev, _ := next(); ev != "gone" {
		t.Errorf("deleted database: got %q, want gone", ev)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLineCounts(t *testing.T) {
	for _, c := range []struct {
		old, new string
		add, del int
	}{
		{"", "", 0, 0},
		{"", "a\nb\n", 2, 0},
		{"a\nb\n", "", 0, 2},
		{"a\nb\nc", "a\nx\nc", 1, 1},
		{"a\nb", "a\nb\nc\n", 1, 0},
	} {
		add, del := lineCounts(c.old, c.new)
		if add != c.add || del != c.del {
			t.Errorf("lineCounts(%q, %q) = +%d -%d, want +%d -%d", c.old, c.new, add, del, c.add, c.del)
		}
	}
}

// The fixtures have Claude Code and Gemini CLI fixing the same cart bug in
// /work/shop, which is the case compare is for.
func TestCompareTwoAgents(t *testing.T) {
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &geminiProvider{dir: "testdata/gemini"})
	idx.Scan(0)
	a, err := idx.Load("claude:c1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := idx.Load("gemini:1a2b3c4d-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	c := Compare(a, b)
	if c.A.Key != "claude:c1" || c.B.Key != b.Session.Key {
		t.Fatalf("keys %q %q", c.A.Key, c.B.Key)
	}
	if c.A.Prompts != 2 || c.A.ToolCalls != 4 || c.A.Failed != 1 || c.A.Edits != 3 || c.A.Files != 3 {
		t.Errorf("A = %+v", c.A)
	}
	if c.B.ToolCalls != 5 || c.B.Failed != 1 || c.B.Steps != len(b.Events) {
		t.Errorf("B = %+v", c.B)
	}
	if c.A.Wall != 119_000 || c.A.Active != c.A.Wall {
		t.Errorf("A wall %d active %d", c.A.Wall, c.A.Active)
	}
	if c.Shared != 3 || len(c.Files) != 3 {
		t.Fatalf("files %+v shared %d", c.Files, c.Shared)
	}
	var cart *FileCompare
	for i := range c.Files {
		if c.Files[i].Path == "cart.go" {
			cart = &c.Files[i]
		}
	}
	if cart == nil || cart.A.Added != 1 || cart.A.Removed != 1 || len(cart.B.Changes) != 1 {
		t.Errorf("cart.go %+v", cart)
	}
	if len(c.A.Tools) == 0 || c.A.Tools[0].Calls < c.A.Tools[len(c.A.Tools)-1].Calls {
		t.Errorf("tools not most used first: %+v", c.A.Tools)
	}
	bash := c.A.Tools[0]
	for _, tt := range c.A.Tools {
		if tt.Name == "Bash" {
			bash = tt
		}
	}
	if bash.Name != "Bash" || bash.Failed != 1 {
		t.Errorf("Bash tally %+v", bash)
	}
}

func TestCompareActiveTimeSkipsIdle(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	tr := &Transcript{Session: &Session{Key: "x:1", Started: t0, Ended: t0.Add(2*time.Hour + 2*time.Minute)}, Events: []Event{
		{Kind: "user", Time: t0},
		{Kind: "tool", Tool: "Bash", Time: t0.Add(time.Minute)},
		{Kind: "user", Time: t0.Add(2*time.Hour + time.Minute)}, // back from lunch
		{Kind: "assistant", Time: t0.Add(2*time.Hour + 2*time.Minute)},
	}}
	c := Compare(tr, tr)
	if c.A.Active != (2 * time.Minute).Milliseconds() {
		t.Errorf("active = %v", time.Duration(c.A.Active)*time.Millisecond)
	}
	if c.A.Wall != (2*time.Hour + 2*time.Minute).Milliseconds() {
		t.Errorf("wall = %d", c.A.Wall)
	}
}

// Two agents in separate worktrees of one repository edit the same file at
// different absolute paths; it must line up as one file.
func TestCompareWorktreesLineUp(t *testing.T) {
	side := func(key, worktree, cwd, path string) *Transcript {
		tr := &Transcript{Session: &Session{Key: key, Root: "/repo", Worktree: worktree, Cwd: cwd}}
		tr.Events = []Event{{Kind: "tool", Tool: "Edit"}}
		tr.addChange(FileChange{Path: path, Op: "edit", Hunks: []Hunk{{Old: "a", New: "b"}}}, 0)
		tr.finish()
		return tr
	}
	a := side("claude:a", "/wt/one", "/wt/one/pkg", "/wt/one/pkg/x.go")
	b := side("codex:b", "/wt/two", "/wt/two", "pkg/x.go")
	c := Compare(a, b)
	if len(c.Files) != 1 || c.Files[0].Path != "pkg/x.go" || c.Shared != 1 {
		t.Fatalf("files %+v", c.Files)
	}
}

func TestCompareAPIAndCLI(t *testing.T) {
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &geminiProvider{dir: "testdata/gemini"})
	idx.Scan(0)
	srv := httptest.NewServer((&Server{index: idx}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/compare?a=claude:c1&b=claude:c2")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		A, B    *Transcript
		Compare *Comparison
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if body.A == nil || body.B == nil || body.Compare == nil || body.Compare.B.Key != "claude:c2" || len(body.A.Events) == 0 {
		t.Fatalf("compare response %+v", body)
	}

	res, _ = http.Get(srv.URL + "/api/compare?a=claude:c1&b=claude:nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing session: status %d", res.StatusCode)
	}
	res.Body.Close()

	var out bytes.Buffer
	if err := compareSessions(idx, &out, "c1", "1a2b"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"A  Fix cart total rounding", "failed calls", "3 changed by both", "cart.go"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("CLI output lacks %q:\n%s", want, out.String())
		}
	}
	if err := compareSessions(idx, &out, "c1", "claude:c1"); err == nil {
		t.Error("comparing a session with itself should fail")
	}
}

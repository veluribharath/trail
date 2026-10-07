package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeParse(t *testing.T) {
	p := &claudeProvider{dir: "testdata/claude"}
	paths, err := p.Discover()
	if err != nil || len(paths) != 3 {
		t.Fatalf("discover: %v %v", paths, err)
	}
	c1 := filepath.Join("testdata/claude/projects/-work-shop/c1.jsonl")
	paths = []string{c1}
	tr, err := p.Parse(paths[0], true)
	if err != nil {
		t.Fatal(err)
	}
	s := tr.Session
	if s.Title != "Fix cart total rounding" || s.Cwd != "/work/shop" || s.Branch != "feat/cart" || s.Model != "claude-opus-5-5" {
		t.Errorf("summary fields: %+v", s)
	}
	if s.Prompt != "Cart totals are off by a cent, fix it" {
		t.Errorf("prompt should skip meta lines, got %q", s.Prompt)
	}
	if len(tr.Changes) != 3 || s.Edits != 3 {
		t.Fatalf("want 3 changes (Edit, Write, MultiEdit), got %d", len(tr.Changes))
	}
	if c := tr.Changes[0]; c.Rel != "cart.go" || c.Op != "edit" || c.Hunks[0].New != "total := math.Round(sum * 100)" {
		t.Errorf("edit change: %+v", c)
	}
	if c := tr.Changes[2]; len(c.Hunks) != 2 {
		t.Errorf("multiedit should keep both hunks: %+v", c)
	}
	var bash *Event
	for i := range tr.Events {
		if tr.Events[i].Tool == "Bash" {
			bash = &tr.Events[i]
		}
	}
	if bash == nil || !bash.Error || bash.Output != "FAIL cart_test.go" || bash.Summary != "go test ./..." {
		t.Errorf("tool result not attached: %+v", bash)
	}
	last := tr.Events[len(tr.Events)-1]
	if last.Kind != "user" || last.Text != "/compact keep tests" {
		t.Errorf("slash command not cleaned: %+v", last)
	}
	if !strings.Contains(s.Resume, "claude --resume c1") {
		t.Errorf("resume: %q", s.Resume)
	}

	// Summary mode must agree with full mode on the counts the tree shows.
	sum, _ := p.Parse(paths[0], false)
	if sum.Session.Edits != s.Edits || sum.Session.Messages != s.Messages || len(sum.Events) != 0 {
		t.Errorf("summary parse disagrees: %+v", sum.Session)
	}
}

func TestSessionNames(t *testing.T) {
	cl := &claudeProvider{dir: "testdata/claude"}
	cases := map[string][2]string{
		// Old summary line, no newer name.
		"c1": {"Fix cart total rounding", "summary"},
		// /rename beats ai-title even when a later ai-title follows; a stray
		// summary and another session's title line are ignored.
		"c2": {"checkout-perf", "renamed"},
		"c3": {"Add dark mode toggle", "generated"},
	}
	for id, want := range cases {
		tr, err := cl.Parse("testdata/claude/projects/-work-shop/"+id+".jsonl", false)
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]string{tr.Session.Title, tr.Session.TitleKind}; got != want {
			t.Errorf("%s: got %v, want %v", id, got, want)
		}
	}

	// Codex names live in session_index.jsonl; the latest non-empty entry wins,
	// and they reach both the list and an opened transcript.
	idx := NewIndex(cl, &codexProvider{dir: "testdata/codex"})
	idx.Scan(0)
	key := "codex:0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	if s := idx.Get(key); s.Title != "Return ctx from handler" || s.TitleKind != "renamed" || s.Prompt != "Return the context from handler" {
		t.Errorf("codex list name: %q %q", s.Title, s.TitleKind)
	}
	if tr, _ := idx.Load(key); tr.Session.Title != "Return ctx from handler" {
		t.Errorf("codex transcript name: %q", tr.Session.Title)
	}
	if s := idx.Get("codex:11111111-2222-3333-4444-555555555555"); s.Title != "legacy hello" || s.TitleKind != "prompt" {
		t.Errorf("unnamed codex session should fall back to its prompt: %q %q", s.Title, s.TitleKind)
	}
}

func TestCodexRenameWithoutRolloutChange(t *testing.T) {
	// Renaming in Codex only appends to the index; the rollout is untouched,
	// so the cached summary must still pick up the new name.
	dir := t.TempDir()
	src, _ := os.ReadFile("testdata/codex/sessions/2026/10/01/rollout-2025-05-01T09-00-00-11111111-2222-3333-4444-555555555555.jsonl")
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir, "sessions", "rollout-x-11111111-2222-3333-4444-555555555555.jsonl"), src, 0o644)
	idx := NewIndex(&codexProvider{dir: dir})
	idx.Scan(0)
	key := "codex:11111111-2222-3333-4444-555555555555"
	if s := idx.Get(key); s.Title != "legacy hello" {
		t.Fatalf("before rename: %q", s.Title)
	}
	os.WriteFile(filepath.Join(dir, "session_index.jsonl"),
		[]byte(`{"id":"11111111-2222-3333-4444-555555555555","thread_name":"Greeting test","updated_at":"x"}`+"\n"), 0o644)
	idx.Scan(0)
	if s := idx.Get(key); s.Title != "Greeting test" {
		t.Errorf("after rename: %q", s.Title)
	}
}

func TestCodexParse(t *testing.T) {
	p := &codexProvider{dir: "testdata/codex"}
	paths, _ := p.Discover()
	if len(paths) != 2 {
		t.Fatalf("want 2 rollouts, got %v", paths)
	}
	var cur, legacy *Transcript
	for _, path := range paths {
		tr, err := p.Parse(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, "2025-05") {
			legacy = tr
		} else {
			cur = tr
		}
	}

	s := cur.Session
	if s.ID != "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000" || s.Cwd != "/work/api" || s.Model != "gpt-5-codex" || s.Branch != "main" {
		t.Errorf("meta: %+v", s)
	}
	if s.Prompt != "Return the context from handler" {
		t.Errorf("environment_context should be skipped, prompt=%q", s.Prompt)
	}
	if s.Messages != 2 {
		t.Errorf("event_msg duplicates must not be counted: %d", s.Messages)
	}
	// apply_patch via custom tool (update+add+delete) and via shell (update).
	if len(cur.Changes) != 4 {
		t.Fatalf("want 4 changes, got %+v", cur.Changes)
	}
	up := cur.Changes[0]
	if up.Rel != "api/server.py" || up.Op != "update" || up.Hunks[0].Old != "ctx = 1\nreturn None" || up.Hunks[0].New != "ctx = 1\nreturn ctx" {
		t.Errorf("update hunk: %+v", up)
	}
	if cur.Changes[1].Op != "add" || cur.Changes[1].Hunks[0].New != "print('hi')" {
		t.Errorf("add: %+v", cur.Changes[1])
	}
	if cur.Changes[2].Op != "delete" || cur.Changes[2].Rel != "old.py" {
		t.Errorf("delete: %+v", cur.Changes[2])
	}
	for _, e := range cur.Events {
		if e.Tool == "shell" && e.Summary == "rg handler" && e.Output != "api/server.py:3" {
			t.Errorf("wrapped shell output not unwrapped: %q", e.Output)
		}
	}
	if !strings.HasSuffix(s.Resume, "codex resume "+s.ID) {
		t.Errorf("resume: %q", s.Resume)
	}

	if legacy.Session.ID != "11111111-2222-3333-4444-555555555555" || legacy.Session.Prompt != "legacy hello" || legacy.Session.Messages != 2 {
		t.Errorf("legacy rollout: %+v", legacy.Session)
	}
}

func TestRepoRootWorktree(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "repo")
	wt := filepath.Join(dir, "repo-wt", "sub")
	os.MkdirAll(filepath.Join(main, ".git", "worktrees", "wt"), 0o755)
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(dir, "repo-wt", ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n"), 0o644)

	x := NewIndex()
	if root, w := x.repoRoot(filepath.Join(main)); root != main || w != "" {
		t.Errorf("main repo: %s %s", root, w)
	}
	if root, w := x.repoRoot(wt); root != main || w != filepath.Join(dir, "repo-wt") {
		t.Errorf("worktree should file under main repo: root=%s wt=%s", root, w)
	}
	if root, _ := x.repoRoot("/no/such/dir"); root != "/no/such/dir" {
		t.Errorf("missing dir should be its own root: %s", root)
	}
}

func TestServerAPI(t *testing.T) {
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &codexProvider{dir: "testdata/codex"})
	srv := httptest.NewServer((&Server{index: idx, home: "/work"}).Handler())
	defer srv.Close()

	var list struct {
		Sessions  []Session        `json:"sessions"`
		Providers []ProviderStatus `json:"providers"`
	}
	getJSON(t, srv.URL+"/api/sessions", &list)
	if len(list.Sessions) != 5 || len(list.Providers) != 2 {
		t.Fatalf("sessions=%d providers=%+v", len(list.Sessions), list.Providers)
	}
	if !list.Sessions[0].Ended.After(list.Sessions[2].Ended) {
		t.Error("sessions should be newest first")
	}

	var tr Transcript
	getJSON(t, srv.URL+"/api/session?id=claude:c1", &tr)
	if len(tr.Changes) != 3 || tr.Session.Key != "claude:c1" {
		t.Errorf("transcript: %+v", tr.Session)
	}

	// A session with no edits must send empty lists, not null: the UI calls
	// .map on them.
	resp, _ := http.Get(srv.URL + "/api/session?id=codex:11111111-2222-3333-4444-555555555555")
	var raw map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	if string(raw["changes"]) != "[]" || string(raw["events"]) == "null" {
		t.Errorf("no-edit session: changes=%s events=%s", raw["changes"], raw["events"])
	}

	resp, _ = http.Get(srv.URL + "/api/session?id=claude:../../etc/passwd")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown key should 404, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/sessions", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site API read should be refused, got %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/")
	if resp.StatusCode != 200 {
		t.Errorf("index page: %d", resp.StatusCode)
	}
}

func TestSessionCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "touch a and b")

	now := time.Now()
	tr := &Transcript{Session: &Session{Cwd: dir, Started: now.Add(-time.Minute), Ended: now}}
	tr.addChange(FileChange{Path: filepath.Join(dir, "a.go"), Op: "edit"}, -1)
	res := SessionCommits(t.Context(), tr)
	if !res.Available || len(res.Commits) != 1 {
		t.Fatalf("commits: %+v", res)
	}
	if c := res.Commits[0]; len(c.Overlap) != 1 || c.Overlap[0] != "a.go" || len(c.Files) != 2 {
		t.Errorf("overlap: %+v", c)
	}
	patch, err := CommitPatch(t.Context(), dir, res.Commits[0].Short)
	if err != nil || !strings.Contains(patch, "touch a and b") {
		t.Errorf("patch: %v", err)
	}
	if _, err := CommitPatch(t.Context(), dir, "--output=/tmp/x"); err == nil {
		t.Error("non-sha argument must be rejected")
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
}

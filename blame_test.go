package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeSession writes a Claude Code session in which one prompt leads to
// the given tool calls, each a minute after the last.
func claudeSession(t *testing.T, dir, id, cwd, prompt string, at time.Time, calls ...map[string]any) {
	t.Helper()
	proj := filepath.Join(dir, "projects", "-blame")
	os.MkdirAll(proj, 0o755)
	var b strings.Builder
	line := func(v map[string]any) {
		v["cwd"], v["sessionId"] = cwd, id
		j, _ := json.Marshal(v)
		b.Write(j)
		b.WriteByte('\n')
	}
	stamp := func(d int) string { return at.Add(time.Duration(d) * time.Minute).Format(time.RFC3339) }
	line(map[string]any{"type": "user", "timestamp": stamp(0), "message": map[string]any{"role": "user", "content": prompt}})
	for i, c := range calls {
		name := c["name"].(string)
		delete(c, "name")
		line(map[string]any{"type": "assistant", "timestamp": stamp(i + 1), "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": id + "-" + itoa(i), "name": name, "input": c}}}})
	}
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBlame(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := realPath(filepath.Join(t.TempDir(), "shop"))
	os.MkdirAll(repo, 0o755)
	repo, _ = filepath.EvalSymlinks(repo)
	claude := t.TempDir()
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	git := func(at time.Time, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		d := at.Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Tony", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=Tony", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	write := func(name, s string) { os.WriteFile(filepath.Join(repo, name), []byte(s), 0o644) }

	agentA := "package cart\n\nfunc Total(xs []int) int {\n\tt := 0\n\tfor _, x := range xs {\n\t\tt += x\n\t}\n\treturn t\n}\n"
	// a1 writes the file; a person adds a function before committing both.
	claudeSession(t, claude, "a1", repo, "add a Total function", base,
		map[string]any{"name": "Write", "file_path": filepath.Join(repo, "cart.go"), "content": agentA})
	write("cart.go", agentA+"\n// Hand written: keep totals in cents.\nfunc Cents(t int) int {\n\treturn t * 100\n}\n")
	git(base, "init", "-q")
	git(base.Add(30*time.Minute), "add", ".")
	git(base.Add(30*time.Minute), "commit", "-qm", "cart totals")

	// b1 edits one line; the change is committed together with a rename.
	claudeSession(t, claude, "b1", repo, "return totals in cents", base.Add(time.Hour),
		map[string]any{"name": "Edit", "file_path": filepath.Join(repo, "cart.go"), "old_string": "\treturn t\n}", "new_string": "\treturn Cents(t) // in cents\n}"})
	b, _ := os.ReadFile(filepath.Join(repo, "cart.go"))
	write("cart.go", strings.Replace(string(b), "\treturn t\n}", "\treturn Cents(t) // in cents\n}", 1))
	git(base.Add(2*time.Hour), "mv", "cart.go", "total.go")
	git(base.Add(2*time.Hour), "commit", "-qam", "rename to total.go")

	// c1 adds a line nobody has committed yet.
	claudeSession(t, claude, "c1", repo, "note the overflow", base.Add(3*time.Hour),
		map[string]any{"name": "Edit", "file_path": filepath.Join(repo, "total.go"), "old_string": "\tt := 0\n", "new_string": "\tt := 0 // may overflow\n"})
	b, _ = os.ReadFile(filepath.Join(repo, "total.go"))
	write("total.go", strings.Replace(string(b), "\tt := 0\n", "\tt := 0 // may overflow\n", 1))

	// d1, later still, writes lines identical to a1's. They were committed
	// before d1 ran, so they must stay a1's.
	claudeSession(t, claude, "d1", repo, "rewrite the loop", base.Add(4*time.Hour),
		map[string]any{"name": "Write", "file_path": filepath.Join(repo, "other.go"), "content": "x"},
		map[string]any{"name": "Write", "file_path": filepath.Join(repo, "total.go"), "content": "func Total(xs []int) int {\n\tfor _, x := range xs {\n\t\tt += x\n\t}\n"})

	idx := NewIndex(&claudeProvider{dir: claude})
	idx.Scan(0)
	res := Blame(t.Context(), idx, filepath.Join(repo, "total.go"))
	if !res.Available || res.Rel != "total.go" {
		t.Fatalf("blame: %+v", res)
	}
	who := func(n int) string {
		l := res.Lines[n-1]
		if l.Source < 0 {
			return "-"
		}
		return res.Sources[l.Source].Key
	}
	want := map[int]string{
		1:  "claude:a1", // package cart
		3:  "claude:a1", // func Total
		4:  "claude:c1", // t := 0 // may overflow (uncommitted)
		5:  "claude:a1", // for loop: d1 wrote the same text after the commit
		6:  "claude:a1",
		7:  "claude:a1", // "\t}" is short, but its neighbours vouch for it
		8:  "claude:b1", // return Cents(t), committed with the rename
		9:  "claude:a1",
		11: "-", // hand-written comment
		12: "-",
		14: "-", // a person's "}", even though a1 also wrote "}"
	}
	for n, k := range want {
		if got := who(n); got != k {
			t.Errorf("line %d %q (commit %d): got %s, want %s", n, res.Lines[n-1].Text, res.Lines[n-1].Commit, got, k)
		}
	}
	if l := res.Lines[3]; l.Commit != -1 {
		t.Errorf("line 4 should be uncommitted, got commit %d", l.Commit)
	}
	if c := res.Commits[res.Lines[0].Commit]; c.File != "cart.go" || c.Subject != "cart totals" || c.Author != "Tony" {
		t.Errorf("rename: line 1's commit should name the old path: %+v", c)
	}
	s := res.Sources[res.Lines[7].Source]
	if s.Prompt != "return totals in cents" || s.Op != "edit" || s.Event != 1 || !strings.Contains(s.Resume, "claude --resume b1") {
		t.Errorf("source: %+v", s)
	}
	if res.Searched != 4 {
		t.Errorf("sessions read: %d", res.Searched)
	}

	// Outside git, every line is uncommitted and content still matches.
	plain := realPath(filepath.Join(t.TempDir(), "notes.txt"))
	os.WriteFile(plain, []byte("first line of notes\nby hand\n"), 0o644)
	claudeSession(t, claude, "e1", filepath.Dir(plain), "start notes", base,
		map[string]any{"name": "Write", "file_path": plain, "content": "first line of notes\n"})
	idx.Scan(0)
	res = Blame(t.Context(), idx, plain)
	if !res.Available || res.Toplevel != "" || len(res.Lines) != 2 || res.Lines[0].Source < 0 || res.Lines[1].Source >= 0 {
		t.Errorf("no repo: %+v", res)
	}

	if r := Blame(t.Context(), idx, repo); r.Available {
		t.Error("a folder can't be blamed")
	}

	// The API only reads files under folders sessions ran in.
	srv := httptest.NewServer((&Server{index: idx, search: NewSearch(idx)}).Handler())
	defer srv.Close()
	var got BlameResult
	getJSON(t, srv.URL+"/api/blame?path="+filepath.Join(repo, "total.go"), &got)
	if !got.Available || len(got.Sources) == 0 {
		t.Errorf("api: %+v", got)
	}
	getJSON(t, srv.URL+"/api/blame?id=claude:b1&path=total.go", &got)
	if !got.Available || got.Path != filepath.Join(repo, "total.go") {
		t.Errorf("api, relative to a session: %+v", got)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("x\n"), 0o644)
	var refused map[string]string
	getJSON(t, srv.URL+"/api/blame?path="+outside, &refused)
	if refused["error"] == "" {
		t.Errorf("a file outside every session folder must be refused: %+v", refused)
	}
	refused = nil
	if os.Symlink(outside, filepath.Join(repo, "link.txt")) == nil {
		getJSON(t, srv.URL+"/api/blame?path="+filepath.Join(repo, "link.txt"), &refused)
		if refused["error"] == "" {
			t.Errorf("a link out of a session folder must be refused: %+v", refused)
		}
	}
}

func TestParseBlameArg(t *testing.T) {
	cases := []struct {
		in       string
		path     string
		from, to int
		bad      bool
	}{
		{"main.go", "main.go", 0, 0, false},
		{"main.go:42", "main.go", 42, 42, false},
		{"src/a.go:40-48", "src/a.go", 40, 48, false},
		{"a.go:0", "", 0, 0, true},
		{"a.go:9-3", "", 0, 0, true},
		{"a.go:x", "", 0, 0, true},
	}
	for _, c := range cases {
		p, f, to, err := parseBlameArg(c.in)
		if c.bad != (err != nil) || (!c.bad && (p != c.path || f != c.from || to != c.to)) {
			t.Errorf("%s: %s %d %d %v", c.in, p, f, to, err)
		}
	}
}

func TestAddedLines(t *testing.T) {
	got := addedLines([]Hunk{{Old: "a\nb\nc", New: "a\nB\nc\nd"}})
	if len(got) != 2 || !got["B"] || !got["d"] || got["a"] {
		t.Errorf("context lines must not count: %v", got)
	}
}

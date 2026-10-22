package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionCorrections(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	tr := &Transcript{Session: &Session{}, Events: []Event{
		{Kind: "user", Text: "Add a cart page. Always run the linter before you commit.", Time: at},
		{Kind: "assistant", Text: "On it."},
		{Kind: "tool", Tool: "Bash", Input: "$ npm install"},
		{Kind: "user", Text: "no, use pnpm"},
		{Kind: "user", Text: "No. Use pnpm here.\n```\nnpm install\n```"},
		{Kind: "user", Text: "no idea why that broke"},              // a "no" that isn't an instruction
		{Kind: "user", Text: "don't you think we should cache it?"}, // a question
		{Kind: "user", Text: "Stop adding comments to every line"},
		{Kind: "user", Text: "never touch the migrations", Sidechain: true}, // a subagent prompt, written by the agent
		{Kind: "tool", Tool: "Bash", Input: "$ git push --force", Error: true,
			Output: "The user doesn't want to proceed with this tool use. The tool use was rejected. To tell you how to proceed, the user said:\nnever force push to main"},
	}}
	got := sessionCorrections(tr)
	var rules []string
	for _, c := range got {
		rules = append(rules, c.rule)
	}
	want := []string{"Always run the linter before you commit.", "Use pnpm.", "Use pnpm here.", "Stop adding comments to every line.", "Never force push to main."}
	if strings.Join(rules, "|") != strings.Join(want, "|") {
		t.Fatalf("rules:\n got %q\nwant %q", rules, want)
	}
	if got[1].event != 3 || got[1].text != "no, use pnpm" {
		t.Errorf("correction should keep its step and wording: %+v", got[1])
	}
	if strings.Join(got[1].terms, ",") != "pnpm" || strings.Join(got[0].terms, ",") != "linter,commit" {
		t.Errorf("terms: %q %q", got[1].terms, got[0].terms)
	}

	// Before the agent has done anything, "no" isn't a correction.
	first := &Transcript{Session: &Session{}, Events: []Event{{Kind: "user", Text: "No, use the staging database for this"}}}
	if c := sessionCorrections(first); len(c) != 0 {
		t.Errorf("a first prompt can't correct anything: %+v", c)
	}
}

func TestParseShell(t *testing.T) {
	for _, c := range []struct{ in, head, sig, seg string }{
		{"npm install", "npm install", "npm install", "npm install"},
		{"cd web && npm run build -- --prod", "npm run build", "npm run build", "npm run build -- --prod"},
		{"CGO_ENABLED=0 go test ./...", "CGO_ENABLED=0 go test", "go test", "go test ./..."},
		{"uv run pytest tests/test_cart.py -q", "uv run pytest", "uv run pytest", "uv run pytest tests/test_cart.py -q"},
		{"python -m pytest", "python -m pytest", "python -m pytest", "python -m pytest"},
		{`git -C /work/shop commit -m "a && b"`, "git commit", "git commit", `git -C /work/shop commit -m "a && b"`},
		{"/usr/bin/python3 x.py | tee out", "python3", "python3", "/usr/bin/python3 x.py"},
		{"echo hi; timeout 60 make test-unit", "make test-unit", "make test-unit", "make test-unit"},
		{"pytest", "pytest", "pytest", "pytest"},
	} {
		r, ok := parseShell(c.in)
		if !ok || r.head != c.head || r.sig != c.sig || r.seg != c.seg {
			t.Errorf("parseShell(%q) = %+v, want head %q sig %q seg %q", c.in, r, c.head, c.sig, c.seg)
		}
	}
	if _, ok := parseShell("cd /tmp && echo done"); ok {
		t.Error("cd and echo alone aren't a command worth keeping")
	}
	codex := Event{Kind: "tool", Tool: "shell", Input: "bash -lc npm test"}
	if got := shellCommand(codex); got != "npm test" {
		t.Errorf("codex shell command: %q", got)
	}
	if got := shellCommand(Event{Kind: "tool", Tool: "exec_command", Input: `{"cmd":"go vet ./..."}`}); got != "go vet ./..." {
		t.Errorf("exec_command: %q", got)
	}
	if got := shellCommand(Event{Kind: "tool", Tool: "Read", Input: "$ nope"}); got != "" {
		t.Errorf("only shell tools have commands: %q", got)
	}
}

func TestSessionFailures(t *testing.T) {
	sh := func(cmd string, failed bool, out string) Event {
		return Event{Kind: "tool", Tool: "Bash", Input: "$ " + cmd, Error: failed, Output: out}
	}
	tr := &Transcript{Session: &Session{}, Events: []Event{
		sh("npm install", true, "npm ERR! code EUNSUPPORTEDPROTOCOL\nnpm ERR! Unsupported URL Type \"workspace:\""),
		sh("ls", false, ""),
		sh("pnpm install", false, "done"),
		// A test that fails until the code is fixed isn't a command mistake.
		sh("go test ./...", true, "--- FAIL: TestCart"),
		sh("go test ./...", false, "ok"),
		// A missing program, then the one that works.
		sh("python scripts/seed.py", true, "Exit code 127\nbash: python: command not found"),
		sh("python3 scripts/seed.py", false, ""),
		// A wrapper.
		sh("pytest -q", true, "ModuleNotFoundError: No module named 'cart'"),
		sh("uv run pytest -q", false, ""),
		// Refused.
		sh("git push origin main", true, "The user doesn't want to proceed with this tool use. The tool use was rejected"),
		// The same program doing something else isn't a fix.
		sh("git push", true, "remote: Permission denied to bot."),
		sh("git status", false, "clean"),
		// Failing for a reason that isn't about the environment, with no fix.
		sh("go build ./...", true, "./cart.go:3: undefined: Total"),
		// A subcommand that doesn't exist, then one that names it.
		sh("npm test", true, "npm ERR! Missing script: \"test\""),
		sh("npm run test:unit", false, "ok"),
	}}
	got := sessionFailures(tr)
	type row struct{ sig, fix string }
	var rows []row
	for _, f := range got {
		fix := f.fix
		if f.rejected {
			fix = "(refused)"
		}
		rows = append(rows, row{f.sig, fix})
	}
	want := []row{{"npm install", "pnpm install"}, {"python", "python3"}, {"pytest", "uv run pytest"}, {"git push", "(refused)"}, {"git push", ""}, {"npm test", "npm run test:unit"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("failures:\n got %v\nwant %v", rows, want)
	}
	if got[1].errLine != "bash: python: command not found" || !got[1].missing || !got[1].env {
		t.Errorf("missing program: %+v", got[1])
	}
	if got[0].fixEvent != 2 {
		t.Errorf("fix should point at its step: %+v", got[0])
	}
}

// retroSession writes a Claude Code session file for the retro tests.
func retroSession(t *testing.T, dir, id, cwd string, start time.Time, steps ...any) {
	t.Helper()
	var b bytes.Buffer
	ts := start
	line := func(typ string, content any) {
		ts = ts.Add(time.Second)
		json.NewEncoder(&b).Encode(map[string]any{"type": typ, "sessionId": id, "cwd": cwd, "timestamp": ts.Format(time.RFC3339),
			"message": map[string]any{"role": typ, "content": content}})
	}
	n := 0
	for _, st := range steps {
		switch v := st.(type) {
		case string: // something the person typed
			line("user", v)
		case [3]string: // a Bash call: command, output, "err" when it failed
			n++
			id := fmt.Sprintf("t%d", n)
			line("assistant", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"command": v[0]}}})
			line("user", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": v[1], "is_error": v[2] == "err"}})
		}
	}
	p := filepath.Join(dir, "projects", "-work-shop", id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRetroProject(t *testing.T) {
	dir, proj := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	os.WriteFile(filepath.Join(proj, "CLAUDE.md"), []byte("# Shop\n\n- Run tests with `make test`.\n- Keep commits small.\n"), 0o644)
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	fail := func(cmd, out string) [3]string { return [3]string{cmd, out, "err"} }
	ok := func(cmd string) [3]string { return [3]string{cmd, "ok", ""} }

	retroSession(t, dir, "r1", proj, day,
		"Add a coupon field", ok("ls"), "no, use pnpm",
		fail("npm install", "npm ERR! Unsupported URL Type \"workspace:\""), ok("pnpm install"),
		fail("rg coupon", "bash: rg: command not found"))
	retroSession(t, dir, "r2", proj, day.AddDate(0, 0, 1),
		"Fix the cart rounding", ok("ls"), "No, we use pnpm here", "please always keep commits small",
		fail("npm install", "npm ERR! Unsupported URL Type \"workspace:\""), ok("pnpm install"),
		fail("rg total", "bash: rg: command not found"))
	retroSession(t, dir, "r3", proj, day.AddDate(0, 0, 2),
		"Rename the cart service. Always keep commits small.", fail("rg cart", "bash: rg: command not found"),
		fail("make lint", "make: *** No rule to make target 'lint'.  Stop."), fail("make lint", "make: *** No rule to make target 'lint'.  Stop."),
		fail("make lint", "make: *** No rule to make target 'lint'.  Stop."))

	idx := NewIndex(&claudeProvider{dir: dir})
	idx.Scan(0)
	r, found := NewRetroCache(idx).Build(proj)
	if !found {
		t.Fatal("project not found")
	}
	if r.Sessions != 3 || fmt.Sprint(r.Files) != "[CLAUDE.md]" || len(r.Targets) != 1 || r.Targets[0].File != "CLAUDE.md" {
		t.Errorf("summary: %+v", r)
	}
	byRule := map[string]Lesson{}
	for _, l := range r.Lessons {
		byRule[l.Rule] = l
	}
	pnpm, okp := byRule["Use pnpm."]
	if !okp || pnpm.Kind != "correction" || pnpm.Sessions != 2 || pnpm.Count != 2 || pnpm.Covered != "" {
		t.Errorf("pnpm correction: %+v (lessons %v)", pnpm, rulesOf(r))
	}
	if len(pnpm.Seen) != 2 || pnpm.Seen[0].Key != "claude:r2" || pnpm.Seen[0].Text != "No, we use pnpm here" {
		t.Errorf("evidence should be newest first: %+v", pnpm.Seen)
	}
	cmd, okc := byRule["Use `pnpm install`, not `npm install`."]
	if !okc || cmd.Fixes != 2 || cmd.Sessions != 2 || cmd.Seen[0].Fix != "pnpm install" {
		t.Errorf("npm -> pnpm: %+v", cmd)
	}
	if l, ok := byRule["Don't use `rg`: it isn't installed here."]; !ok || l.Sessions != 3 {
		t.Errorf("missing rg: %+v (lessons %v)", l, rulesOf(r))
	}
	if l, ok := byRule["`make lint` isn't defined in this project."]; !ok || l.Count != 3 || l.Sessions != 1 {
		t.Errorf("make lint: %+v", l)
	}
	if l, ok := byRule["Always keep commits small."]; !ok || l.Covered != "CLAUDE.md" {
		t.Errorf("a rule CLAUDE.md already has should say so: %+v (lessons %v)", l, rulesOf(r))
	}

	var out bytes.Buffer
	printRetro(&out, r, "")
	for _, want := range []string{"Lessons from 3 sessions", "- Use pnpm.", "Already in your instructions", "worked right after 2 of them"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("printed retro lacks %q:\n%s", want, out.String())
		}
	}

	// The second build reads nothing new and gives the same answer.
	c := NewRetroCache(idx)
	a, _ := c.Build(proj)
	b, _ := c.Build(proj)
	if fmt.Sprint(rulesOf(a)) != fmt.Sprint(rulesOf(b)) {
		t.Error("cached build differs")
	}

	srv := httptest.NewServer((&Server{index: idx, search: NewSearch(idx), home: "/work"}).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/retro?root=" + url.QueryEscape(proj))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("api: %v %v", res, err)
	}
	var got Retro
	json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if len(got.Lessons) != len(r.Lessons) {
		t.Errorf("api lessons: %d, want %d", len(got.Lessons), len(r.Lessons))
	}
	res, _ = http.Get(srv.URL + "/api/retro?root=" + url.QueryEscape("/etc"))
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a folder with no sessions should be refused, got %d", res.StatusCode)
	}

	if root, err := retroRoot(idx, filepath.Join(proj, "sub", "dir")); err != nil || root != proj {
		t.Errorf("retroRoot from a subfolder: %q %v", root, err)
	}
}

func rulesOf(r *Retro) []string {
	var out []string
	for _, l := range r.Lessons {
		out = append(out, l.Kind+": "+l.Rule+" ["+l.Command+"]")
	}
	return out
}

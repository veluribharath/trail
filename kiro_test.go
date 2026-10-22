package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures are built by testdata/kiro/gen.py: a CLI 2.x session with a
// subagent, a V3 session, and a database holding a headless conversation, a
// copy of the CLI session and a Kiro CLI 1.x conversation.

const (
	kiroCLI      = "0b1c2d3e-0000-4000-8000-000000000001"
	kiroV3       = "sess_00000000-0000-4000-8000-0000000000b3"
	kiroHeadless = "9f8e7d6c-0000-4000-8000-000000000002"
	kiroLegacy   = "5a5a5a5a-0000-4000-8000-000000000003"
)

func kiroFixture() *kiroProvider {
	return &kiroProvider{dir: "testdata/kiro/home", dataDir: "testdata/kiro/data"}
}

func kiroParseAll(t *testing.T, p *kiroProvider) map[string]*Transcript {
	t.Helper()
	paths, err := p.Discover()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*Transcript{}
	for _, path := range paths {
		tr, err := p.Parse(path, true)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if out[tr.Session.ID] != nil {
			t.Errorf("%s listed twice", tr.Session.ID)
		}
		out[tr.Session.ID] = tr

		// Summary mode must agree with full mode on what the tree shows.
		sum, err := p.Parse(path, false)
		if err != nil || sum.Session.Messages != tr.Session.Messages || sum.Session.Edits != tr.Session.Edits || len(sum.Events) != 0 {
			t.Errorf("%s: summary parse disagrees: %+v", path, sum.Session)
		}
	}
	return out
}

func TestKiroCLIParse(t *testing.T) {
	all := kiroParseAll(t, kiroFixture())
	if len(all) != 4 {
		t.Fatalf("want the CLI, V3, headless and 1.x sessions (the subagent inside its parent): %d", len(all))
	}
	tr := all[kiroCLI]
	s := tr.Session
	if s.Title != "Fix the coupon rounding" || s.TitleKind != "generated" || s.Cwd != "/work/shop" || s.Model != "claude-sonnet-4.5" {
		t.Errorf("summary fields: %+v", s)
	}
	if s.Prompt != "Coupons round the wrong way, fix it" || s.Messages != 5 {
		t.Errorf("prompt %q, messages %d", s.Prompt, s.Messages)
	}
	if s.Resume != "cd /work/shop && kiro-cli chat --resume-id "+kiroCLI {
		t.Errorf("resume: %q", s.Resume)
	}
	if !s.Started.Equal(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("started: %v", s.Started)
	}

	var texts []string
	tools := map[string]Event{}
	sidechain := 0
	for _, e := range tr.Events {
		if e.Sidechain {
			sidechain++
			continue
		}
		switch e.Kind {
		case "assistant", "user":
			texts = append(texts, e.Text)
		case "tool":
			tools[e.Tool+":"+e.Summary] = e
		}
	}
	if texts[1] != "Looking at the coupon code." {
		t.Errorf("a streamed reply should be one message: %q", texts)
	}
	if last := tr.Events[len(tr.Events)-1]; last.Text != "thanks" {
		t.Errorf("the half-written last line should be skipped, and the subagent go before the next prompt: %+v", last)
	}
	if sh := tools["shell:go test ./..."]; !sh.Error || sh.Input != "$ go test ./..." || sh.Output != "FAIL coupon_test.go\nexit status 1" {
		t.Errorf("a command that exited 1 failed: %+v", sh)
	}
	if w := tools["write:coupon_test.go"]; !w.Error || len(w.Changes) != 0 {
		t.Errorf("a refused write is an error and changes nothing: %+v", w)
	}
	if sidechain != 4 {
		t.Errorf("the subagent's events should be in the parent: %d", sidechain)
	}

	if len(tr.Changes) != 2 {
		t.Fatalf("want the strReplace and the subagent's append: %+v", tr.Changes)
	}
	if c := tr.Changes[0]; c.Rel != "coupon.go" || c.Op != "edit" || c.Hunks[0].Old != "math.Floor(x)" || c.Hunks[0].New != "math.Round(x)" {
		t.Errorf("edit: %+v", c)
	}
	if c := tr.Changes[1]; c.Rel != "NOTES.md" || !tr.Events[c.Event].Sidechain || tr.Events[c.Event].Tool != "write" {
		t.Errorf("the subagent's change should point at its event: %+v", c)
	}
	for i, c := range tr.Changes {
		if found := false; c.Event >= 0 {
			for _, k := range tr.Events[c.Event].Changes {
				found = found || k == i
			}
			if !found {
				t.Errorf("change %d and event %d don't point at each other", i, c.Event)
			}
		}
	}
}

func TestKiroV3Parse(t *testing.T) {
	tr := kiroParseAll(t, kiroFixture())[kiroV3]
	s := tr.Session
	if s.Title != "Tidy the notes index" || s.Cwd != "/work/notes" || s.Messages != 2 {
		t.Errorf("summary fields: %+v", s)
	}
	if s.Resume != "cd /work/notes && kiro-cli --v3 chat --resume-id "+kiroV3 {
		t.Errorf("a session in the V3 index resumes from the CLI: %q", s.Resume)
	}
	var tools []Event
	for _, e := range tr.Events {
		if e.Kind == "thinking" || e.Text == "..." {
			t.Errorf("encrypted thinking should be left out: %+v", e)
		}
		if e.Kind == "tool" {
			tools = append(tools, e)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("a call written once per state is one call: %+v", tools)
	}
	if tools[0].Output != "Replaced" || tools[1].Output != "Denied" || !tools[1].Error || tools[2].Input != "$ sort -o index.md index.md" {
		t.Errorf("tools: %+v", tools)
	}
	if len(tr.Changes) != 1 || tr.Changes[0].Rel != "index.md" || tr.Changes[0].Hunks[0].Old != "- [dead](x)\n" {
		t.Errorf("only the completed str_replace changed a file: %+v", tr.Changes)
	}

	// Without an entry in the session index it is an IDE session.
	dir := t.TempDir()
	copyTree(t, "testdata/kiro/home", dir)
	os.Remove(filepath.Join(dir, "session-index", "notes-1a2b.jsonl"))
	p := &kiroProvider{dir: dir}
	ide, err := p.Parse(filepath.Join(dir, "sessions", "notes-1a2b", kiroV3, "messages.jsonl"), false)
	if err != nil || ide.Session.Resume != "" {
		t.Errorf("an IDE session has no CLI resume: %q %v", ide.Session.Resume, err)
	}
}

func TestKiroDatabaseParse(t *testing.T) {
	all := kiroParseAll(t, kiroFixture())
	tr := all[kiroHeadless]
	s := tr.Session
	if s.Cwd != "/work/api" || s.Model != "claude-haiku-4.5" || s.Messages != 3 || s.Prompt != "Add a changelog entry for 1.4" {
		t.Errorf("summary fields: %+v", s)
	}
	if !s.Ended.Equal(time.UnixMilli(1791187260000)) {
		t.Errorf("ended should come from updated_at: %v", s.Ended)
	}
	if s.Resume != "cd /work/api && kiro-cli chat --resume-id "+kiroHeadless {
		t.Errorf("resume: %q", s.Resume)
	}
	if len(tr.Changes) != 1 || tr.Changes[0].Rel != "CHANGELOG.md" || tr.Changes[0].Hunks[0].New != "## 1.4\n" {
		t.Errorf("an insert is an edit adding its text: %+v", tr.Changes)
	}
	var bash *Event
	for i := range tr.Events {
		if tr.Events[i].Tool == "execute_bash" {
			bash = &tr.Events[i]
		}
	}
	if bash == nil || bash.Error || bash.Output != " CHANGELOG.md | 1 +" {
		t.Errorf("a result in the next turn attaches to its call: %+v", bash)
	}

	old := all[kiroLegacy]
	if old.Session.Cwd != "/work/ops" || old.Session.Resume != "cd /work/ops && kiro-cli chat --resume" || old.Session.Messages != 2 {
		t.Errorf("Kiro CLI 1.x conversation: %+v", old.Session)
	}
}

func TestKiroMissing(t *testing.T) {
	p := &kiroProvider{dir: filepath.Join(t.TempDir(), "nope"), dataDir: filepath.Join(t.TempDir(), "nope")}
	if paths, err := p.Discover(); err != nil || len(paths) != 0 {
		t.Errorf("a missing Kiro is no sessions and no error: %v %v", paths, err)
	}
}

func TestKiroIndexRefresh(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "testdata/kiro/home", filepath.Join(dir, "home"))
	copyTree(t, "testdata/kiro/data", filepath.Join(dir, "data"))
	p := &kiroProvider{dir: filepath.Join(dir, "home"), dataDir: filepath.Join(dir, "data")}
	idx := NewIndex(p)
	status := idx.Scan(0)
	if status[0].Sessions != 4 || status[0].Error != "" {
		t.Fatalf("status: %+v", status)
	}
	parent := idx.Get("kiro:" + kiroCLI)
	before, _ := idx.Stamp(parent)
	if st, _ := idx.Stamp(idx.Get("kiro:" + kiroHeadless)); st == "" {
		t.Error("a database session has a stamp")
	}

	// The subagent doing more work changes its parent.
	sub := filepath.Join(dir, "home", "sessions", "cli", "0b1c2d3e-0000-4000-8000-0000000000aa.jsonl")
	f, err := os.OpenFile(sub, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"s9","content":[{"kind":"text","data":"One more thing."}]}}` + "\n")
	f.Close()
	if after, _ := idx.Stamp(parent); after == before {
		t.Error("the parent's stamp should change with its subagent")
	}
	tr, err := idx.Load("kiro:" + kiroCLI)
	if err != nil || !strings.Contains(tr.Events[len(tr.Events)-2].Text, "One more thing.") {
		t.Errorf("reload should show the subagent's new reply: %v", err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		copyFile(t, path, filepath.Join(dst, rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

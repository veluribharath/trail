package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func eventsWithUsage(tr *Transcript) []Event {
	var out []Event
	for _, e := range tr.Events {
		if e.Usage != nil {
			out = append(out, e)
		}
	}
	return out
}

func TestClaudeUsage(t *testing.T) {
	p := &claudeProvider{dir: "testdata/usage/claude"}
	tr, err := p.Parse("testdata/usage/claude/projects/-work-usage/u1.jsonl", true)
	if err != nil {
		t.Fatal(err)
	}
	u := tr.Session.Usage
	if u == nil {
		t.Fatal("no usage")
	}
	// Response a is logged twice with the same usage; its last line counts.
	// a: 10×$4 + 1000×$5 (5m write) + 5000×$0.20 + 300×$20
	// b, fast mode: 20×$8 + 2000×$16 (1h write) + 6000×$0.40 + 200×$40
	// subagent on Haiku 4.5: 100×$1 + 50×$5, plus two web searches at $0.01
	// c is a model with no price; the synthetic line isn't a response.
	want := (10*4+1000*5+5000*0.2+300*20)/1e6 + (20*8+2000*16+6000*0.4+200*40)/1e6 + (100*1+50*5)/1e6 + 0.02
	if !near(u.Cost, want) {
		t.Errorf("cost %v, want %v", u.Cost, want)
	}
	if u.Responses != 4 || u.Unpriced != 1 || strings.Join(u.UnpricedModels, ",") != "claude-nova-9" {
		t.Errorf("responses %d unpriced %d %v", u.Responses, u.Unpriced, u.UnpricedModels)
	}
	if u.Input != 10+20+5+100 || u.CacheWrite != 1000 || u.CacheWrite1h != 2000 || u.CacheRead != 11000 || u.Output != 300+200+5+50 || u.Reasoning != 50 {
		t.Errorf("tokens: %+v", u)
	}
	if u.Context != 20+2000+6000 {
		t.Errorf("context should be the largest prompt: %d", u.Context)
	}
	steps := eventsWithUsage(tr)
	if len(steps) != 3 || steps[0].Tool != "Bash" || steps[0].Usage.Output != 300 || steps[1].Text != "Done in fast mode." {
		t.Errorf("per-step usage should sit on each response's last event: %+v", steps)
	}

	// The summary parse, which builds no events, gets the same totals.
	sum, _ := p.Parse("testdata/usage/claude/projects/-work-usage/u1.jsonl", false)
	if sum.Session.Usage == nil || !near(sum.Session.Usage.Cost, want) {
		t.Errorf("summary usage: %+v", sum.Session.Usage)
	}
}

func TestCodexUsage(t *testing.T) {
	p := &codexProvider{dir: "testdata/usage/codex"}
	paths, _ := p.Discover()
	if len(paths) != 1 {
		t.Fatalf("paths: %v", paths)
	}
	tr, err := p.Parse(paths[0], true)
	if err != nil {
		t.Fatal(err)
	}
	u := tr.Session.Usage
	// First request: 1000 input of which 800 cached. The repeated report is
	// skipped. The second is over 272K, so the whole request pays 2× input and
	// 1.5× output.
	want := (200*5+800*0.5+100*30)/1e6 + (300000*10+1000*45)/1e6
	if u == nil || !near(u.Cost, want) || u.Responses != 2 {
		t.Fatalf("usage %+v, want cost %v", u, want)
	}
	if u.Input != 200+300000 || u.CacheRead != 800 || u.Output != 1100 || u.Reasoning != 40 {
		t.Errorf("tokens: %+v", u)
	}
	steps := eventsWithUsage(tr)
	if len(steps) != 2 || steps[0].Tool != "shell" || steps[1].Kind != "assistant" {
		t.Errorf("steps: %+v", steps)
	}
}

func TestGeminiUsage(t *testing.T) {
	p := &geminiProvider{dir: "testdata/usage/gemini"}
	paths, _ := p.Discover()
	if len(paths) != 1 {
		t.Fatalf("paths: %v", paths)
	}
	tr, err := p.Parse(paths[0], true)
	if err != nil {
		t.Fatal(err)
	}
	u := tr.Session.Usage
	// m2 was written twice; its final record counts. Its 250K prompt is over
	// Gemini 2.5 Pro's 200K tier, and thoughts are billed as output.
	want := (200000*2.5+50000*0.25+1500*15)/1e6 + (1000*0.3+100*2.5)/1e6
	if u == nil || !near(u.Cost, want) || u.Responses != 2 || u.Reasoning != 500 {
		t.Fatalf("usage %+v, want cost %v", u, want)
	}
}

func TestOpenCodeUsage(t *testing.T) {
	p := &opencodeProvider{dir: "testdata/opencode"}
	tr, err := p.Parse(p.dbPath()+"#ses_o1", true)
	if err != nil {
		t.Fatal(err)
	}
	u := tr.Session.Usage
	// One step-finish: 1 input and 2 output tokens on Claude Sonnet 4.5,
	// priced from the table rather than OpenCode's own figure.
	if u == nil || u.Responses != 1 || u.Input != 1 || u.Output != 2 || !near(u.Cost, (1*3+2*15)/1e6) {
		t.Fatalf("usage: %+v", u)
	}
	steps := eventsWithUsage(tr)
	if len(steps) != 1 || steps[0].Text != "Fixed: the redirect now honours ?next." {
		t.Errorf("usage should sit on the turn's own last event, not the subagent's: %+v", steps)
	}
}

func TestPriceLookup(t *testing.T) {
	for _, m := range []string{"claude-opus-4-5-20251101", "claude-sonnet-4-5@20250929", "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"anthropic/claude-sonnet-4.5", "claude-opus-4-6[1m]", "openai/gpt-5.5-2026-04-23", "GPT-5-Codex"} {
		if prices.lookup(m) == nil {
			t.Errorf("%s should be priced", m)
		}
	}
	// A variant trail doesn't know is not priced as its nearest neighbour.
	for _, m := range []string{"gpt-5.5-pro", "claude-opus-4-9", "llama3.1:8b", ""} {
		if prices.lookup(m) != nil {
			t.Errorf("%s should not be priced", m)
		}
	}
	// Haiku 5.5 charges more once the prompt, cache included, is over 100K.
	small := prices.price(step{model: "claude-haiku-5-5", Usage: Usage{Input: 1000, CacheRead: 99000, Output: 10}})
	big := prices.price(step{model: "claude-haiku-5-5", Usage: Usage{Input: 1000, CacheRead: 99001, Output: 10}})
	if !near(small.Cost, (1000*0.1+99000*0.01+10*0.5)/1e6) || !near(big.Cost, (1000*0.5+99001*0.05+10*2.5)/1e6) {
		t.Errorf("haiku tier: %v %v", small.Cost, big.Cost)
	}
	// US-only inference costs 1.1×.
	us := prices.price(step{model: "claude-opus-5-5", usOnly: true, Usage: Usage{Input: 1e6}})
	if !near(us.Cost, 4.4) {
		t.Errorf("us-only: %v", us.Cost)
	}
	// An unknown model keeps the agent's own figure when it has one.
	rec := prices.price(step{model: "local/qwen", recorded: 0.5, Usage: Usage{Input: 10}})
	if rec.Cost != 0.5 || rec.Unpriced != 0 {
		t.Errorf("recorded cost: %+v", rec)
	}
}

func TestPriceOverrides(t *testing.T) {
	saved := prices
	t.Cleanup(func() { prices = saved })
	path := filepath.Join(t.TempDir(), "prices.json")
	os.WriteFile(path, []byte(`{"prices": [
		{"models": ["my-local-model"], "input": 1, "output": 2},
		{"models": ["claude-opus-5-5"], "input": 1, "cacheRead": 0.1, "output": 1}
	]}`), 0o644)
	if err := loadPriceOverrides(path); err != nil {
		t.Fatal(err)
	}
	if u := prices.price(step{model: "my-local-model", Usage: Usage{Input: 1e6, CacheWrite: 1e6, Output: 1e6}}); !near(u.Cost, 4) {
		t.Errorf("an override with no cache write rate should charge writes as input: %v", u.Cost)
	}
	if u := prices.price(step{model: "claude-opus-5-5", Usage: Usage{Input: 1e6}}); !near(u.Cost, 1) {
		t.Errorf("an override should replace the bundled rate: %v", u.Cost)
	}
	if prices.lookup("claude-sonnet-5-5") == nil {
		t.Error("bundled prices should still be there")
	}
	if err := loadPriceOverrides(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Errorf("a missing override file is fine: %v", err)
	}
}

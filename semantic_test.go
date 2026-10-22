package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// conceptEmbedder stands in for a real model: each word votes for a concept,
// so "pricing" lands near "cart" without sharing a word with it.
type conceptEmbedder struct {
	calls atomic.Int32
	fail  bool
}

var concepts = map[string]int{
	"cart": 0, "totals": 0, "cent": 0, "pricing": 0, "money": 0,
	"dark": 1, "mode": 1, "theme": 1, "colors": 1,
	"context": 2, "handler": 2, "request": 2, "ctx": 2,
}

func (e *conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	if e.fail {
		return nil, errors.New("can't reach the embedding server")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 4)
		v[3] = 0.05 // so no vector is all zeros
		tokenize(t, false, func(w string) {
			if c, ok := concepts[w]; ok {
				v[c]++
			}
		})
		out[i] = v
	}
	return out, nil
}

func newSemanticFixture(t *testing.T, emb Embedder, model, dir string) (*Index, *Search, *Semantic) {
	t.Helper()
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &codexProvider{dir: "testdata/codex"})
	idx.Scan(0)
	srch := NewSearch(idx)
	srch.Sync()
	return idx, srch, NewSemantic(idx, srch, emb, model, dir)
}

func TestSemanticBlend(t *testing.T) {
	dir := t.TempDir()
	emb := &conceptEmbedder{}
	_, srch, sem := newSemanticFixture(t, emb, "fake", dir)
	if err := sem.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := sem.Status(); st.Embedded != st.Total || st.Total != 5 || st.Error != "" {
		t.Fatalf("status: %+v", st)
	}

	run := func(raw string) SearchResult {
		q := ParseQuery(raw)
		return sem.Blend(context.Background(), q, srch.Run(q, "", 30), "", 30)
	}
	res := run("pricing money ")
	if len(res.Sessions) == 0 || res.Sessions[0].Key != "claude:c1" {
		t.Fatalf("meaning search should find the cart session: %+v", res.Sessions)
	}
	if h := res.Sessions[0].Hits[0]; h.Field != "related" || h.Event != 0 {
		t.Errorf("related hit should point at the turn's prompt: %+v", h)
	}
	for _, sh := range res.Sessions {
		if sh.Key == "claude:c3" {
			t.Errorf("dark mode session is unrelated to pricing")
		}
	}

	// Word matches still come first when both agree, and nothing is lost.
	res = run("cart ")
	if len(res.Sessions) != 1 || res.Sessions[0].Key != "claude:c1" || res.Sessions[0].Hits[0].Field == "related" {
		t.Errorf("word match should lead: %+v", res.Sessions)
	}

	for _, raw := range []string{"mode:words pricing money ", "tool:bash pricing ", "in:prompt pricing "} {
		if res := run(raw); len(res.Sessions) != 0 {
			t.Errorf("%q should stay a word search, got %d sessions", raw, len(res.Sessions))
		}
	}
	if res := run("agent:codex pricing "); len(res.Sessions) != 0 {
		t.Errorf("agent filter should apply to meaning search too")
	}

	// The cache is owner-only and lets a restart skip the model entirely.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 5 {
		t.Fatalf("want 5 cached sessions, got %d", len(ents))
	}
	if fi, _ := ents[0].Info(); fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode %v, want 0600", fi.Mode().Perm())
	}
	_, _, again := newSemanticFixture(t, &conceptEmbedder{fail: true}, "fake", dir)
	if err := again.Sync(context.Background()); err != nil {
		t.Errorf("restart should load from cache without the model: %v", err)
	}

	// A different model invalidates the cache, and a dead server is reported.
	_, _, other := newSemanticFixture(t, &conceptEmbedder{fail: true}, "other-model", dir)
	if err := other.Sync(context.Background()); err == nil || other.Status().Error == "" {
		t.Errorf("changing model should re-embed, and the failure should show")
	}
}

func TestSemanticPrune(t *testing.T) {
	src := t.TempDir()
	proj := filepath.Join(src, "projects", "-tmp")
	os.MkdirAll(proj, 0o755)
	file := filepath.Join(proj, "x1.jsonl")
	os.WriteFile(file, []byte(`{"cwd":"/tmp","sessionId":"x1","type":"user","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"cart totals"}}`+"\n"), 0o644)
	idx := NewIndex(&claudeProvider{dir: src})
	idx.Scan(0)
	dir := filepath.Join(t.TempDir(), "vectors")
	sem := NewSemantic(idx, NewSearch(idx), &conceptEmbedder{}, "fake", dir)
	if err := sem.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir should be created owner-only: %v %v", fi, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("want one cached session, got %d", len(ents))
	}
	os.Remove(file)
	idx.Scan(0)
	sem.Sync(context.Background())
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("vectors of a deleted session should be pruned, %d left", len(ents))
	}
}

func TestChunkTranscript(t *testing.T) {
	tr := &Transcript{Events: []Event{
		{Kind: "user", Text: "first prompt"},
		{Kind: "assistant", Text: "a reply"},
		{Kind: "tool", Tool: "Bash", Summary: "go test ./...", Input: "ignored"},
		{Kind: "thinking", Text: "ignored too"},
		{Kind: "user", Text: "second " + strings.Repeat("x ", chunkChars)},
		{Kind: "assistant", Text: "after a long prompt"},
	}}
	events, texts := chunkTranscript(tr)
	if len(texts) != 3 || events[0] != 0 || events[1] != 4 || events[2] != 5 {
		t.Fatalf("chunks start at %v: %q", events, texts)
	}
	if texts[0] != "first prompt\na reply\nBash go test ./..." {
		t.Errorf("first chunk: %q", texts[0])
	}
	if len(texts[1]) > chunkChars {
		t.Errorf("chunk over the limit: %d", len(texts[1]))
	}
}

func TestHTTPEmbedder(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		switch r.URL.Path {
		case "/api/embed":
			if got["model"] == "missing" {
				w.WriteHeader(404)
				w.Write([]byte(`{"error":"model \"missing\" not found, try pulling it first"}`))
				return
			}
			w.Write([]byte(`{"model":"m","embeddings":[[1,2],[3,4]]}`))
		case "/v1/embeddings":
			w.Write([]byte(`{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/v1/"} {
		vecs, err := newHTTPEmbedder(base, "m").Embed(context.Background(), []string{"a", "b"})
		if err != nil || len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 4 {
			t.Errorf("%s: %v %v", base, vecs, err)
		}
		if in, _ := got["input"].([]any); len(in) != 2 || got["model"] != "m" {
			t.Errorf("%s: request body %v", base, got)
		}
	}
	if _, err := newHTTPEmbedder(srv.URL, "missing").Embed(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "try pulling") {
		t.Errorf("server's error should come through: %v", err)
	}
	srv.Close()
	if _, err := newHTTPEmbedder(srv.URL, "m").Embed(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "is it running") {
		t.Errorf("down server: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:11434": true, "http://localhost:11434": true, "http://[::1]:8080/v1": true,
		"http://192.168.1.5:11434": false, "https://api.example.com/v1": false, "http://localhost.example.com": false,
	} {
		if isLoopback(raw) != want {
			t.Errorf("%s: want %v", raw, want)
		}
	}
}

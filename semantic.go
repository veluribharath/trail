package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Semantic finds steps by meaning rather than by words. It is off unless trail
// runs with -semantic. Each turn of a transcript (a prompt, the replies and
// the tool calls after it) is turned into a vector by an embedding model the
// person runs themselves, Ollama by default. Vectors are kept in memory and
// cached on disk under the user cache directory, so only new or changed
// sessions are sent to the model again. Agent storage is never written.
type Semantic struct {
	idx    *Index
	search *Search
	embed  Embedder
	model  string
	dir    string // vector cache; "" keeps vectors in memory only

	syncing sync.Mutex

	mu      sync.RWMutex
	byPath  map[string]*semSession
	total   int
	pending int
	err     string    // why the last sync stopped early, if it did
	failed  time.Time // when; Kick waits a while before trying again
}

type semSession struct {
	size   int64
	mod    time.Time
	key    string
	dims   int
	events []int32 // first step of each chunk
	vecs   []int8  // unit vectors scaled to ±127, dims per chunk
}

// Embedder turns texts into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	chunkChars    = 1500 // per chunk sent to the model
	embedBatch    = 16
	retryAfter    = 30 * time.Second
	semFloor      = 0.5  // weakest similarity shown as related
	semBand       = 0.15 // and no further than this below the best match
	semPerSession = 3
	rrfK          = 60
)

func NewSemantic(idx *Index, search *Search, embed Embedder, model, dir string) *Semantic {
	return &Semantic{idx: idx, search: search, embed: embed, model: model, dir: dir, byPath: map[string]*semSession{}}
}

// Kick starts a sync in the background unless one is running, or the last
// one failed moments ago (the model server is likely down).
func (s *Semantic) Kick() {
	s.mu.RLock()
	wait := !s.failed.IsZero() && time.Since(s.failed) < retryAfter
	s.mu.RUnlock()
	if wait || !s.syncing.TryLock() {
		return
	}
	go func() {
		defer s.syncing.Unlock()
		s.sync(context.Background())
	}()
}

// Sync embeds whatever is missing and waits for it to finish.
func (s *Semantic) Sync(ctx context.Context) error {
	s.syncing.Lock()
	defer s.syncing.Unlock()
	return s.sync(ctx)
}

func (s *Semantic) sync(ctx context.Context) error {
	files := s.idx.files()
	// Newest first: recent sessions are the likeliest to be searched.
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	live := map[string]bool{}
	var todo []indexedFile
	s.mu.Lock()
	for _, f := range files {
		live[f.path] = true
		if ss := s.byPath[f.path]; ss != nil && ss.size == f.size && ss.mod.Equal(f.mod) {
			ss.key = f.key
			continue
		}
		todo = append(todo, f)
	}
	for path := range s.byPath {
		if !live[path] {
			delete(s.byPath, path)
		}
	}
	s.total, s.pending = len(files), len(todo)
	s.mu.Unlock()

	var failure error
	for _, f := range todo {
		ss, err := s.load(ctx, f)
		if err != nil {
			failure = err
			break
		}
		s.mu.Lock()
		if ss != nil {
			s.byPath[f.path] = ss
		} else {
			delete(s.byPath, f.path)
		}
		s.pending--
		s.mu.Unlock()
	}

	s.mu.Lock()
	if failure != nil {
		s.err, s.failed = failure.Error(), time.Now()
	} else {
		s.err, s.failed = "", time.Time{}
	}
	s.mu.Unlock()
	if failure == nil {
		s.prune(live)
	}
	return failure
}

// load returns a session's vectors from the disk cache when they are current,
// and otherwise embeds the session and caches the result. A nil session with
// no error means there was nothing to embed.
func (s *Semantic) load(ctx context.Context, f indexedFile) (*semSession, error) {
	if ss := s.readCache(f); ss != nil {
		return ss, nil
	}
	t, err := s.idx.parseFull(f.provider, f.path)
	if t == nil || (err != nil && len(t.Events) == 0) {
		return nil, nil
	}
	events, texts := chunkTranscript(t)
	ss := &semSession{size: f.size, mod: f.mod, key: f.key}
	for i := 0; i < len(texts); i += embedBatch {
		batch := texts[i:min(len(texts), i+embedBatch)]
		docs := make([]string, len(batch))
		for k, b := range batch {
			docs[k] = s.docPrefix() + b
		}
		vecs, err := s.embed.Embed(ctx, docs)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("embedding server returned %d vectors for %d texts", len(vecs), len(batch))
		}
		for k, v := range vecs {
			if ss.dims == 0 {
				ss.dims = len(v)
			}
			if len(v) != ss.dims || ss.dims == 0 {
				return nil, fmt.Errorf("embedding server returned vectors of different sizes")
			}
			ss.events = append(ss.events, int32(events[i+k]))
			ss.vecs = append(ss.vecs, quantize(v)...)
		}
	}
	s.writeCache(f, ss)
	return ss, nil
}

// chunkTranscript splits a transcript into turns: a prompt and everything up
// to the next prompt. A long turn becomes several chunks. Each chunk points
// at the step it starts with.
func chunkTranscript(t *Transcript) (events []int, texts []string) {
	var b strings.Builder
	start := -1
	flush := func() {
		if start >= 0 && strings.TrimSpace(b.String()) != "" {
			events = append(events, start)
			texts = append(texts, b.String())
		}
		b.Reset()
		start = -1
	}
	add := func(i int, s string) {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			return
		}
		if b.Len() > 0 && b.Len()+1+len(s) > chunkChars {
			flush()
		}
		if start < 0 {
			start = i
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// A very long reply keeps its start, which usually says what it is.
		b.WriteString(clipBytes(s, chunkChars))
	}
	for i, e := range t.Events {
		switch e.Kind {
		case "user":
			flush()
			add(i, e.Text)
		case "assistant":
			add(i, e.Text)
		case "tool":
			// The one-line summary says what was done without the bulk.
			add(i, strings.TrimSpace(e.Tool+" "+e.Summary))
		}
	}
	flush()
	return events, texts
}

// quantize scales a vector to unit length and stores it in 8 bits per
// dimension, a quarter of float32's size. Ranking barely notices.
func quantize(v []float32) []int8 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	out := make([]int8, len(v))
	if n == 0 {
		return out
	}
	for i, x := range v {
		out[i] = int8(math.Round(float64(x) / n * 127))
	}
	return out
}

func dot(a, b []int8) float64 {
	var s int32
	for i := range a {
		s += int32(a[i]) * int32(b[i])
	}
	return float64(s) / (127 * 127)
}

// nomic-embed-text was trained with task prefixes and ranks better with them.
func (s *Semantic) docPrefix() string {
	if strings.Contains(s.model, "nomic") {
		return "search_document: "
	}
	return ""
}

func (s *Semantic) queryPrefix() string {
	if strings.Contains(s.model, "nomic") {
		return "search_query: "
	}
	return ""
}

/* ---------------- queries ---------------- */

// SemanticStatus tells the page how far embedding has got.
type SemanticStatus struct {
	Model    string `json:"model"`
	Embedded int    `json:"embedded"`
	Total    int    `json:"total"`
	Error    string `json:"error,omitempty"`
}

func (s *Semantic) Status() *SemanticStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &SemanticStatus{Model: s.model, Embedded: s.total - s.pending, Total: s.total, Error: s.err}
}

type semMatch struct {
	path, key string
	size      int64
	mod       time.Time
	best      float64
	events    []int // matching chunk starts, best first
}

// match returns the sessions whose turns are closest in meaning to text, best
// first.
func (s *Semantic) match(ctx context.Context, q Query, only string, limit int) ([]semMatch, error) {
	vecs, err := s.embed.Embed(ctx, []string{s.queryPrefix() + q.text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embedding server returned no vector for the query")
	}
	qv := quantize(vecs[0])

	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []semMatch
	best := 0.0
	for path, ss := range s.byPath {
		if ss.dims != len(qv) || (only != "" && ss.key != only) || (q.agent != "" && !strings.HasPrefix(ss.key, q.agent)) {
			continue
		}
		m := semMatch{path: path, key: ss.key, size: ss.size, mod: ss.mod}
		type hit struct {
			ev  int
			sim float64
		}
		var hits []hit
		for c := range ss.events {
			sim := dot(qv, ss.vecs[c*ss.dims:(c+1)*ss.dims])
			if sim >= semFloor {
				hits = append(hits, hit{int(ss.events[c]), sim})
			}
		}
		if len(hits) == 0 {
			continue
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].sim > hits[j].sim })
		m.best = hits[0].sim
		best = max(best, m.best)
		for _, h := range hits {
			m.events = append(m.events, h.ev)
		}
		out = append(out, m)
	}
	// Keep only what is close to the best match: the floor alone lets in
	// sessions that share little more than the topic of programming.
	kept := out[:0]
	for _, m := range out {
		if m.best >= best-semBand {
			kept = append(kept, m)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].best != kept[j].best {
			return kept[i].best > kept[j].best
		}
		return kept[i].key < kept[j].key
	})
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// Blend adds sessions found by meaning to a word search, ranking the two
// lists together by reciprocal rank fusion. A query that names fields, tools
// or files, or asks for mode:words, stays a word search.
func (s *Semantic) Blend(ctx context.Context, q Query, res SearchResult, only string, limit int) SearchResult {
	res.Semantic = s.Status()
	if q.wordsOnly || q.narrowed || strings.TrimSpace(q.text) == "" {
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	matches, err := s.match(ctx, q, only, limit)
	if err != nil {
		res.Semantic.Error = err.Error()
		return res
	}
	if len(matches) == 0 {
		return res
	}

	score := map[string]float64{}
	byKey := map[string]*SessionHits{}
	var order []string
	for i := range res.Sessions {
		sh := &res.Sessions[i]
		score[sh.Key] += 1.0 / float64(rrfK+i+1)
		byKey[sh.Key] = sh
		order = append(order, sh.Key)
	}
	for i, m := range matches {
		score[m.key] += 1.0 / float64(rrfK+i+1)
		sh := byKey[m.key]
		if sh == nil {
			sh = &SessionHits{Key: m.key}
			byKey[m.key] = sh
			order = append(order, m.key)
		}
		s.addRelated(sh, m, q)
	}
	merged := make([]SessionHits, 0, len(order))
	for _, k := range order {
		sh := byKey[k]
		if len(sh.Hits) == 0 {
			continue
		}
		sh.Score = score[k]
		merged = append(merged, *sh)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > limit {
		merged, res.More = merged[:limit], true
	}
	res.Sessions = merged
	return res
}

// addRelated adds a session's closest turns to its hits, after any word
// matches, and to its matching steps.
func (s *Semantic) addRelated(sh *SessionHits, m semMatch, q Query) {
	t := s.search.transcriptAt(m.path, m.key, m.size, m.mod)
	if t == nil {
		return
	}
	have := map[int]bool{}
	for _, h := range sh.Hits {
		have[h.Event] = true
	}
	added := 0
	for _, ev := range m.events {
		if ev < 0 || ev >= len(t.Events) {
			continue
		}
		if !have[ev] {
			have[ev] = true
			if len(sh.Hits) < hitsPerSession && added < semPerSession {
				e := t.Events[ev]
				text := e.Text
				if e.Kind == "tool" {
					text = strings.TrimSpace(e.Tool + " " + e.Summary)
				}
				sh.Hits = append(sh.Hits, Hit{Event: ev, Field: "related", Snippet: snippet(text, q.words)})
				added++
			}
		}
		if !containsInt(sh.Events, ev) {
			sh.Events = append(sh.Events, ev)
		}
	}
	sort.Slice(sh.Hits, func(i, j int) bool { return sh.Hits[i].Event < sh.Hits[j].Event })
	sort.Ints(sh.Events)
	sh.Count = len(sh.Events)
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

/* ---------------- disk cache ---------------- */

const cacheMagic = "trailvec1"

// cacheFile names a session's vectors by a hash of its path, so the cache
// directory doesn't spell out project folders.
func (s *Semantic) cacheFile(path string) string {
	h := sha256.Sum256([]byte(path))
	return filepath.Join(s.dir, hex.EncodeToString(h[:12])+".vec")
}

func (s *Semantic) readCache(f indexedFile) *semSession {
	if s.dir == "" {
		return nil
	}
	fh, err := os.Open(s.cacheFile(f.path))
	if err != nil {
		return nil
	}
	defer fh.Close()
	r := bufio.NewReader(fh)
	var hdr struct {
		Magic string `json:"magic"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Mod   int64  `json:"mod"`
		Model string `json:"model"`
		Dims  int    `json:"dims"`
		N     int    `json:"n"`
	}
	line, err := r.ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &hdr) != nil {
		return nil
	}
	if hdr.Magic != cacheMagic || hdr.Path != f.path || hdr.Size != f.size || hdr.Mod != f.mod.UnixNano() ||
		hdr.Model != s.model || hdr.Dims <= 0 || hdr.N < 0 || hdr.N > 1<<24 || hdr.Dims > 1<<14 {
		return nil
	}
	ss := &semSession{size: f.size, mod: f.mod, key: f.key, dims: hdr.Dims, events: make([]int32, hdr.N), vecs: make([]int8, hdr.N*hdr.Dims)}
	if binary.Read(r, binary.LittleEndian, ss.events) != nil || binary.Read(r, binary.LittleEndian, ss.vecs) != nil {
		return nil
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return nil // trailing bytes: not a file this version wrote
	}
	return ss
}

// writeCache stores a session's vectors. The cache is derived from
// transcripts, so it is readable only by its owner. A failed write only
// costs a re-embed next time.
func (s *Semantic) writeCache(f indexedFile, ss *semSession) {
	if s.dir == "" {
		return
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return
	}
	var buf bytes.Buffer
	hdr, _ := json.Marshal(map[string]any{"magic": cacheMagic, "path": f.path, "size": f.size, "mod": f.mod.UnixNano(),
		"model": s.model, "dims": ss.dims, "n": len(ss.events)})
	buf.Write(hdr)
	buf.WriteByte('\n')
	binary.Write(&buf, binary.LittleEndian, ss.events)
	binary.Write(&buf, binary.LittleEndian, ss.vecs)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return
	}
	tmp.Chmod(0o600)
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), s.cacheFile(f.path)) != nil {
		os.Remove(tmp.Name())
	}
}

// prune deletes cached vectors of sessions that are gone from disk.
func (s *Semantic) prune(live map[string]bool) {
	if s.dir == "" {
		return
	}
	keep := map[string]bool{}
	for path := range live {
		keep[filepath.Base(s.cacheFile(path))] = true
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".vec") && !keep[e.Name()] {
			os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// cacheDir is where trail keeps the vectors it computes: its own directory
// under the user cache directory, never inside agent storage.
func cacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "trail", "vectors")
}

/* ---------------- embedding servers ---------------- */

// httpEmbedder talks to Ollama's /api/embed, or to an OpenAI-compatible
// /v1/embeddings when the URL ends in /v1 (llama.cpp, LM Studio, vLLM).
type httpEmbedder struct {
	base   string
	model  string
	openai bool
	client *http.Client
}

func newHTTPEmbedder(base, model string) *httpEmbedder {
	base = strings.TrimRight(base, "/")
	return &httpEmbedder{base: base, model: model, openai: strings.HasSuffix(base, "/v1"), client: &http.Client{Timeout: 2 * time.Minute}}
}

func (e *httpEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	path := "/api/embed"
	if e.openai {
		path = "/embeddings"
	}
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, "POST", e.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach the embedding server at %s; is it running?", e.base)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Error any `json:"error"`
		}
		json.Unmarshal(raw, &msg)
		if msg.Error != nil {
			return nil, fmt.Errorf("embedding server: %v", msg.Error)
		}
		return nil, fmt.Errorf("embedding server answered %s", resp.Status)
	}
	if e.openai {
		var out struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("embedding server: %w", err)
		}
		vecs := make([][]float32, len(out.Data))
		for i, d := range out.Data {
			if d.Index >= 0 && d.Index < len(vecs) {
				vecs[d.Index] = d.Embedding
			} else {
				vecs[i] = d.Embedding
			}
		}
		return vecs, nil
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding server: %w", err)
	}
	return out.Embeddings, nil
}

// isLoopback reports whether an embedding URL stays on this machine.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

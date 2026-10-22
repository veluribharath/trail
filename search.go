package main

import (
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Search is an in-memory full-text index over every transcript. It follows
// the session index: a session file is re-read only when the session index
// saw its size or mtime change. Nothing is written to disk.
//
// The index holds postings, not text. Snippets and phrase checks re-read the
// few sessions a result shows, through a small cache of parsed transcripts.
type Search struct {
	idx *Index

	syncing sync.Mutex // one sync at a time

	mu       sync.RWMutex
	byPath   map[string]*searchSession
	termID   map[string]uint32
	df       []int32 // by term id: docs containing it, across sessions
	docs     int
	totalLen int
	vocab    []string // sorted terms for prefix matching; may lag a sync
	total    int      // session files known to the session index at the last sync
	pending  int      // of those, still being read

	cacheMu sync.Mutex
	cache   []*cachedTranscript // most recently used last
}

type searchSession struct {
	size  int64
	mod   time.Time
	key   string
	docs  []docMeta
	post  []uint64           // term id<<32 | doc<<8 | tf, sorted
	edits map[int32][]string // event -> files it edited
}

// docMeta is one searchable field of one event.
type docMeta struct {
	event int32
	n     uint16 // length in tokens, capped
	field field
	tool  string
}

type field uint8

const (
	fPrompt field = 1 << iota
	fReply
	fTool
	fOutput
	fThinking

	fDefault = fPrompt | fReply | fTool | fOutput
	fAll     = fPrompt | fReply | fTool | fOutput | fThinking
)

var fieldNames = map[field]string{fPrompt: "prompt", fReply: "reply", fTool: "tool", fOutput: "output", fThinking: "thinking"}

// Prompts weigh most: they say what the session was for.
var fieldBoost = map[field]float64{fPrompt: 1.5, fReply: 1, fTool: 0.8, fThinking: 0.6, fOutput: 0.5}

const (
	maxTokenLen  = 40      // longer runs are hashes, base64 and minified code
	maxOutputLen = 4 << 10 // only the start of a failed step's output is indexed
	maxDocs      = 1 << 24 // per session, from the posting layout
	cacheSize    = 24      // parsed transcripts kept for snippets
	phraseBudget = 3 * time.Second
)

func NewSearch(idx *Index) *Search {
	return &Search{idx: idx, byPath: map[string]*searchSession{}, termID: map[string]uint32{}}
}

// Kick starts a sync in the background unless one is already running.
func (s *Search) Kick() {
	if !s.syncing.TryLock() {
		return
	}
	go func() {
		defer s.syncing.Unlock()
		s.sync()
	}()
}

// Sync brings the search index up to date with the session index and waits
// for it to finish.
func (s *Search) Sync() {
	s.syncing.Lock()
	defer s.syncing.Unlock()
	s.sync()
}

func (s *Search) sync() {
	files := s.idx.files()
	live := map[string]bool{}
	var todo []indexedFile
	changed := false
	s.mu.Lock()
	for _, f := range files {
		live[f.path] = true
		if ss := s.byPath[f.path]; ss != nil && ss.size == f.size && ss.mod.Equal(f.mod) {
			ss.key = f.key
			continue
		}
		todo = append(todo, f)
	}
	for path, ss := range s.byPath {
		if !live[path] {
			s.remove(path, ss)
			changed = true
		}
	}
	s.total, s.pending = len(files), len(todo)
	s.mu.Unlock()

	ch := make(chan indexedFile)
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), 8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				s.build(f)
			}
		}()
	}
	for _, f := range todo {
		ch <- f
	}
	close(ch)
	wg.Wait()

	if changed || len(todo) > 0 {
		s.mu.RLock()
		vocab := make([]string, 0, len(s.termID))
		for t, id := range s.termID {
			if s.df[id] > 0 {
				vocab = append(vocab, t)
			}
		}
		s.mu.RUnlock()
		sort.Strings(vocab)
		s.mu.Lock()
		s.vocab = vocab
		s.mu.Unlock()
	}
}

// eachDoc lists the searchable fields of a transcript. Indexing and snippets
// both go through it, so they always agree on what a step's text is.
func eachDoc(t *Transcript, emit func(ev int, f field, tool, text string)) {
	for i, e := range t.Events {
		switch e.Kind {
		case "user":
			emit(i, fPrompt, "", e.Text)
		case "assistant":
			emit(i, fReply, "", e.Text)
		case "thinking":
			emit(i, fThinking, "", e.Text)
		case "tool":
			emit(i, fTool, e.Tool, strings.Join(nonEmpty(e.Summary, e.Input), "\n"))
			// Output is mostly file contents and logs. Only a failure's output
			// is indexed: that is where the error messages people look for are.
			if e.Error {
				emit(i, fOutput, e.Tool, clipBytes(e.Output, maxOutputLen))
			}
		}
	}
}

func (s *Search) build(f indexedFile) {
	var ss *searchSession
	t, err := s.idx.parseFull(f.provider, f.path)
	if t != nil && (err == nil || len(t.Events) > 0) {
		ss = &searchSession{size: f.size, mod: f.mod, key: f.key, edits: map[int32][]string{}}
	}
	type pair struct {
		term string
		doc  int32
		tf   int32
	}
	var pairs []pair
	if ss != nil {
		for _, c := range t.Changes {
			ss.edits[int32(c.Event)] = append(ss.edits[int32(c.Event)], strings.ToLower(c.Rel))
		}
		tf := map[string]int32{}
		eachDoc(t, func(ev int, fl field, tool, text string) {
			if len(ss.docs) >= maxDocs {
				return
			}
			clear(tf)
			n := 0
			tokenize(text, true, func(tok string) { tf[tok]++; n++ })
			if n == 0 {
				return
			}
			d := int32(len(ss.docs))
			for tok, c := range tf {
				pairs = append(pairs, pair{tok, d, min(c, 255)})
			}
			ss.docs = append(ss.docs, docMeta{event: int32(ev), field: fl, tool: tool, n: uint16(min(n, math.MaxUint16))})
		})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.byPath[f.path]; old != nil {
		s.remove(f.path, old)
	}
	s.pending--
	if ss == nil {
		return
	}
	ss.post = make([]uint64, len(pairs))
	for i, p := range pairs {
		id, ok := s.termID[p.term]
		if !ok {
			id = uint32(len(s.df))
			s.termID[p.term] = id
			s.df = append(s.df, 0)
		}
		s.df[id]++
		ss.post[i] = uint64(id)<<32 | uint64(p.doc)<<8 | uint64(p.tf)
	}
	slices.Sort(ss.post)
	for _, d := range ss.docs {
		s.docs++
		s.totalLen += int(d.n)
	}
	s.byPath[f.path] = ss
}

// remove takes a session's postings out of the corpus statistics. Term ids
// are never reused, so the dictionary only grows. Callers hold s.mu.
func (s *Search) remove(path string, ss *searchSession) {
	delete(s.byPath, path)
	for _, p := range ss.post {
		s.df[p>>32]--
	}
	for _, d := range ss.docs {
		s.docs--
		s.totalLen -= int(d.n)
	}
}

// postings returns the slice of a session's postings for one term.
func (ss *searchSession) postings(id uint32) []uint64 {
	lo := uint64(id) << 32
	i := sort.Search(len(ss.post), func(i int) bool { return ss.post[i] >= lo })
	j := i
	for j < len(ss.post) && ss.post[j]>>32 == uint64(id) {
		j++
	}
	return ss.post[i:j]
}

func nonEmpty(ss ...string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// junk is a token nobody searches for: long hex (hashes, ids) and long
// numbers. Short numbers stay, for ports, status codes and line numbers.
func junk(tok string) bool {
	if len(tok) < 12 {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// tokenize lowercases runs of letters, digits and underscores. With parts, an
// identifier also yields its pieces, so handleSession is found by "session"
// and snake_case by "case".
func tokenize(s string, parts bool, emit func(string)) {
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		run := s[start:end]
		start = -1
		if len(run) > maxTokenLen || junk(run) {
			return
		}
		whole := strings.ToLower(run)
		emit(whole)
		if parts {
			splitIdent(run, whole, emit)
		}
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(s))
}

// splitIdent emits the pieces of snake_case and camelCase identifiers.
// HTTPServer splits into http and server.
func splitIdent(run, whole string, emit func(string)) {
	var cuts []int
	rs := []rune(run)
	off := make([]int, len(rs)+1)
	for i, r := range rs {
		off[i+1] = off[i] + utf8.RuneLen(r)
	}
	pieceStart := 0
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '_' {
			cuts = append(cuts, pieceStart, i)
			pieceStart = i + 1
			continue
		}
		if i > pieceStart && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				cuts = append(cuts, pieceStart, i)
				pieceStart = i
			}
		}
	}
	if pieceStart == 0 {
		return // one piece: the whole token already covers it
	}
	cuts = append(cuts, pieceStart, len(rs))
	for i := 0; i < len(cuts); i += 2 {
		a, b := cuts[i], cuts[i+1]
		if b-a < 2 {
			continue
		}
		if p := strings.ToLower(run[off[a]:off[b]]); p != whole {
			emit(p)
		}
	}
}

/* ---------------- queries ---------------- */

// Query is a parsed search. Each slot must match; a slot lists the terms that
// satisfy it (more than one when the last word is matched as a prefix).
type Query struct {
	slots   [][]string
	words   []string // what to highlight
	phrases []string
	fields  field
	tool    string
	file    string
	agent   string
}

// ParseQuery reads plain words, "quoted phrases" and the filters in:, tool:,
// file: and agent:. The last word is matched as a prefix while the person is
// still typing it (no trailing space).
func ParseQuery(raw string) Query {
	q := Query{}
	var prefix string
	parts := splitQuery(raw)
	for i, p := range parts {
		if p.quoted {
			ph := strings.ToLower(strings.Join(strings.Fields(p.text), " "))
			if ph == "" {
				continue
			}
			q.phrases = append(q.phrases, ph)
			tokenize(ph, false, func(t string) { q.slots = append(q.slots, []string{t}) })
			q.words = append(q.words, ph)
			continue
		}
		if k, v, ok := strings.Cut(p.text, ":"); ok && v != "" {
			switch strings.ToLower(k) {
			case "in":
				for _, f := range strings.Split(strings.ToLower(v), ",") {
					q.fields |= fieldNamed(f)
				}
				continue
			case "tool":
				q.tool = strings.ToLower(v)
				continue
			case "file":
				q.file = strings.ToLower(v)
				continue
			case "agent":
				q.agent = strings.ToLower(v)
				continue
			}
		}
		var toks []string
		tokenize(p.text, false, func(t string) { toks = append(toks, t) })
		last := i == len(parts)-1 && !strings.HasSuffix(raw, " ") && !p.quoted
		for j, t := range toks {
			q.words = append(q.words, t)
			if last && j == len(toks)-1 && utf8.RuneCountInString(t) >= 2 {
				prefix = t
				continue
			}
			q.slots = append(q.slots, []string{t})
		}
	}
	if prefix != "" {
		q.slots = append(q.slots, []string{prefix, "\x00prefix"})
	}
	if q.fields == 0 {
		q.fields = fDefault
	}
	return q
}

func fieldNamed(s string) field {
	switch strings.TrimSuffix(s, "s") {
	case "prompt", "user":
		return fPrompt
	case "reply", "replie", "assistant":
		return fReply
	case "tool", "input", "command":
		return fTool
	case "output", "result":
		return fOutput
	case "thinking":
		return fThinking
	case "all", "everything":
		return fAll
	}
	return 0
}

type queryPart struct {
	text   string
	quoted bool
}

func splitQuery(s string) []queryPart {
	var out []queryPart
	for s != "" {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				out = append(out, queryPart{s[1:], true})
				break
			}
			out = append(out, queryPart{s[1 : end+1], true})
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		out = append(out, queryPart{s[:end], false})
		s = s[end:]
	}
	return out
}

// SearchResult is the answer to one query, grouped by session.
type SearchResult struct {
	Sessions []SessionHits `json:"sessions"`
	More     bool          `json:"more"`     // more sessions matched than were returned
	Indexed  int           `json:"indexed"`  // session files searched
	Total    int           `json:"total"`    // session files known
	Building bool          `json:"building"` // still reading transcripts
}

type SessionHits struct {
	Key    string  `json:"key"`
	Score  float64 `json:"score"`
	Count  int     `json:"count"`  // matching steps
	Hits   []Hit   `json:"hits"`   // the best few, in transcript order
	Events []int   `json:"events"` // every matching step, in order
}

type Hit struct {
	Event   int       `json:"event"`
	Field   string    `json:"field"`
	Tool    string    `json:"tool,omitempty"`
	Snippet []Segment `json:"snippet"`
}

// Segment is a piece of a snippet; Hit marks the words that matched.
type Segment struct {
	Text string `json:"t"`
	Hit  bool   `json:"m,omitempty"`
}

const (
	hitsPerSession = 3
	snippetChars   = 180
	maxPrefixTerms = 64
)

type scored struct {
	doc   int32
	score float64
}

type candidate struct {
	path, key string
	ss        *searchSession
	hits      []scored
	best      float64
}

// Run searches every indexed session, or only the one with key only when it
// is set. At most limit sessions are returned, best first.
func (s *Search) Run(q Query, only string, limit int) SearchResult {
	s.mu.RLock()
	res := SearchResult{Sessions: []SessionHits{}, Total: s.total, Indexed: s.total - s.pending, Building: s.pending > 0}
	slots, words := s.resolve(q)
	if len(slots) == 0 {
		s.mu.RUnlock()
		return res
	}
	avg := 1.0
	if s.docs > 0 {
		avg = float64(s.totalLen) / float64(s.docs)
	}
	idf := map[uint32]float64{}
	for _, slot := range slots {
		for _, id := range slot {
			df := float64(s.df[id])
			idf[id] = math.Log(1 + (float64(s.docs)-df+0.5)/(df+0.5))
		}
	}
	var cands []candidate
	for path, ss := range s.byPath {
		if only != "" && ss.key != only {
			continue
		}
		if q.agent != "" && !strings.HasPrefix(ss.key, q.agent) {
			continue
		}
		hits := matchSession(ss, q, slots, idf, avg)
		if len(hits) == 0 {
			continue
		}
		c := candidate{path: path, key: ss.key, ss: ss, hits: hits}
		for _, h := range hits {
			c.best = max(c.best, h.score)
		}
		cands = append(cands, c)
	}
	s.mu.RUnlock()

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].best != cands[j].best {
			return cands[i].best > cands[j].best
		}
		return cands[i].key < cands[j].key
	})

	// Snippets (and phrase checks) need the text, so re-read the sessions
	// that make the cut, a few at a time.
	deadline := time.Now().Add(phraseBudget)
	i := 0
	for i < len(cands) && len(res.Sessions) < limit {
		if len(q.phrases) > 0 && time.Now().After(deadline) {
			break
		}
		batch := cands[i:min(len(cands), i+8)]
		out := make([]*SessionHits, len(batch))
		var wg sync.WaitGroup
		for k := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[k] = s.finish(batch[k], q, words)
			}()
		}
		wg.Wait()
		for _, sh := range out {
			if sh != nil && len(res.Sessions) < limit {
				res.Sessions = append(res.Sessions, *sh)
			}
		}
		i += len(batch)
	}
	res.More = i < len(cands)
	sort.SliceStable(res.Sessions, func(i, j int) bool { return res.Sessions[i].Score > res.Sessions[j].Score })
	return res
}

// resolve turns the query's terms into term ids, expanding a prefix into the
// indexed terms that start with it, most common first. A term nobody has
// used means nothing can match. Callers hold s.mu for reading.
func (s *Search) resolve(q Query) (slots [][]uint32, words []string) {
	words = q.words
	for _, slot := range q.slots {
		if len(slot) == 2 && slot[1] == "\x00prefix" {
			p := slot[0]
			var terms []string
			for i := sort.SearchStrings(s.vocab, p); i < len(s.vocab) && strings.HasPrefix(s.vocab[i], p); i++ {
				terms = append(terms, s.vocab[i])
			}
			if _, ok := s.termID[p]; ok && !slices.Contains(terms, p) {
				terms = append(terms, p) // indexed since the vocabulary was last sorted
			}
			if len(terms) > maxPrefixTerms {
				sort.SliceStable(terms, func(a, b int) bool { return s.df[s.termID[terms[a]]] > s.df[s.termID[terms[b]]] })
				terms = terms[:maxPrefixTerms]
			}
			var ids []uint32
			for _, t := range terms {
				if id, ok := s.termID[t]; ok && s.df[id] > 0 {
					ids = append(ids, id)
				}
			}
			if len(ids) == 0 {
				return nil, nil
			}
			slots = append(slots, ids)
			words = append(words, terms...)
			continue
		}
		id, ok := s.termID[slot[0]]
		if !ok || s.df[id] <= 0 {
			return nil, nil
		}
		slots = append(slots, []uint32{id})
	}
	return slots, words
}

// matchSession scores every step field that has a term from each slot, with
// BM25. Callers hold s.mu for reading.
func matchSession(ss *searchSession, q Query, slots [][]uint32, idf map[uint32]float64, avg float64) []scored {
	const k1, b = 1.2, 0.75
	var acc map[int32]float64
	for _, slot := range slots {
		cur := map[int32]float64{}
		for _, id := range slot {
			for _, p := range ss.postings(id) {
				doc, tf := int32(p>>8&0xFFFFFF), float64(p&0xFF)
				if acc != nil {
					if _, ok := acc[doc]; !ok {
						continue
					}
				}
				d := &ss.docs[doc]
				if d.field&q.fields == 0 {
					continue
				}
				sc := idf[id] * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(d.n)/avg))
				cur[doc] = max(cur[doc], sc)
			}
		}
		if acc != nil {
			for d, v := range cur {
				cur[d] = v + acc[d]
			}
		}
		acc = cur
		if len(acc) == 0 {
			return nil
		}
	}
	var out []scored
	for di, sc := range acc {
		d := &ss.docs[di]
		if q.tool != "" && strings.ToLower(d.tool) != q.tool {
			continue
		}
		if q.file != "" && !slices.ContainsFunc(ss.edits[d.event], func(f string) bool { return strings.Contains(f, q.file) }) {
			continue
		}
		out = append(out, scored{di, sc * fieldBoost[d.field]})
	}
	return out
}

// finish re-reads a matching session for its snippets, drops steps that
// don't hold a quoted phrase, and summarizes the rest.
func (s *Search) finish(c candidate, q Query, words []string) *SessionHits {
	t := s.transcript(c)
	if t == nil {
		return nil
	}
	type at struct {
		ev int
		f  field
	}
	text := map[at]string{}
	eachDoc(t, func(ev int, f field, _, s string) { text[at{ev, f}] = s })

	hits := c.hits[:0:0]
	for _, h := range c.hits {
		d := c.ss.docs[h.doc]
		if len(q.phrases) > 0 {
			low := strings.ToLower(strings.Join(strings.Fields(text[at{int(d.event), d.field}]), " "))
			if !allIn(low, q.phrases) {
				continue
			}
		}
		hits = append(hits, h)
	}
	if len(hits) == 0 {
		return nil
	}
	sh := &SessionHits{Key: c.key}
	seen := map[int32]bool{}
	best := 0.0
	for _, h := range hits {
		best = max(best, h.score)
		if ev := c.ss.docs[h.doc].event; !seen[ev] {
			seen[ev] = true
			sh.Events = append(sh.Events, int(ev))
		}
	}
	sort.Ints(sh.Events)
	sh.Count = len(sh.Events)
	// The best match, nudged up by how much of the session is about it.
	sh.Score = best * (1 + 0.1*math.Log(float64(sh.Count)))

	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	clear(seen)
	for _, h := range hits {
		d := c.ss.docs[h.doc]
		if seen[d.event] {
			continue
		}
		seen[d.event] = true
		sh.Hits = append(sh.Hits, Hit{Event: int(d.event), Field: fieldNames[d.field], Tool: d.tool, Snippet: snippet(text[at{int(d.event), d.field}], words)})
		if len(sh.Hits) == hitsPerSession {
			break
		}
	}
	sort.Slice(sh.Hits, func(i, j int) bool { return sh.Hits[i].Event < sh.Hits[j].Event })
	return sh
}

func allIn(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

type cachedTranscript struct {
	path string
	size int64
	mod  time.Time
	t    *Transcript
}

// transcript returns the parsed session a candidate was indexed from, or nil
// when the file has changed since (the next sync will catch up).
func (s *Search) transcript(c candidate) *Transcript {
	s.cacheMu.Lock()
	for i, ct := range s.cache {
		if ct.path == c.path && ct.size == c.ss.size && ct.mod.Equal(c.ss.mod) {
			s.cache = append(append(s.cache[:i:i], s.cache[i+1:]...), ct)
			s.cacheMu.Unlock()
			return ct.t
		}
	}
	s.cacheMu.Unlock()

	provider, _, _ := strings.Cut(c.key, ":")
	t, err := s.idx.parseFull(provider, c.path)
	if t == nil || (err != nil && len(t.Events) == 0) {
		return nil
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache = append(s.cache, &cachedTranscript{c.path, c.ss.size, c.ss.mod, t})
	if len(s.cache) > cacheSize {
		s.cache = s.cache[1:]
	}
	return t
}

// snippet cuts a window of text around the first matched word and marks every
// match inside it. Matching folds ASCII case only, so offsets stay put.
func snippet(text string, words []string) []Segment {
	text = strings.Join(strings.Fields(text), " ")
	low := asciiLower(text)
	first := -1
	for _, w := range words {
		if i := strings.Index(low, asciiLower(w)); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	start := 0
	if first > snippetChars/3 {
		start = first - snippetChars/3
		if sp := strings.IndexByte(text[start:first], ' '); sp >= 0 {
			start += sp + 1
		}
		for !utf8.RuneStart(text[start]) {
			start++
		}
	}
	end := min(len(text), start+snippetChars)
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	if end < len(text) {
		if sp := strings.LastIndexByte(text[start:end], ' '); sp > snippetChars/2 {
			end = start + sp
		}
	}
	win, lwin := text[start:end], low[start:end]

	// Mark the longest match at each position.
	marked := make([]bool, len(win))
	for _, w := range words {
		lw := asciiLower(w)
		if lw == "" {
			continue
		}
		for i := 0; ; {
			j := strings.Index(lwin[i:], lw)
			if j < 0 {
				break
			}
			for k := i + j; k < i+j+len(lw); k++ {
				marked[k] = true
			}
			i += j + len(lw)
		}
	}
	var segs []Segment
	if start > 0 {
		segs = append(segs, Segment{Text: "…"})
	}
	for i := 0; i < len(win); {
		j := i
		for j < len(win) && marked[j] == marked[i] {
			j++
		}
		segs = append(segs, Segment{Text: win[i:j], Hit: marked[i]})
		i = j
	}
	if end < len(text) {
		segs = append(segs, Segment{Text: "…"})
	}
	return segs
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// opencodeProvider reads OpenCode sessions. Since 1.2 OpenCode keeps them in
// one SQLite database, <dir>/opencode.db, in WAL mode: a session table, a
// message table and a part table whose data columns hold the JSON OpenCode
// used to write as separate files. Subagent runs are sessions of their own
// with a parent_id; they are shown inside their parent as sidechain events.
//
// Each session is addressed as "<db path>#<session id>", so the index can
// track them one by one.
type opencodeProvider struct {
	dir string

	mu       sync.Mutex
	snap     *opencodeSnap
	stamps   map[string]string // path -> stamp, from the last Discover
	stampsDB *sqliteDB         // the snapshot stamps were read from
}

// opencodeSnap is one open, consistent view of the database. A new one is
// opened when the database or its WAL changes; the old one is closed once
// nothing is reading it.
type opencodeSnap struct {
	db    *sqliteDB
	key   string
	refs  int
	stale bool
}

func (p *opencodeProvider) Name() string { return "opencode" }
func (p *opencodeProvider) Root() string { return p.dir }
func (p *opencodeProvider) dbPath() string {
	return filepath.Join(p.dir, "opencode.db")
}

// acquire returns the current snapshot, opening a new one if the database
// changed. Call the returned func when done reading.
func (p *opencodeProvider) acquire() (*sqliteDB, func(), error) {
	key := ""
	for _, f := range []string{p.dbPath(), p.dbPath() + "-wal"} {
		if fi, err := os.Stat(f); err == nil {
			key += fmt.Sprintf("%d@%d;", fi.Size(), fi.ModTime().UnixNano())
		} else {
			key += "-;"
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snap == nil || p.snap.key != key {
		db, err := openSQLite(p.dbPath())
		if err != nil {
			return nil, nil, err
		}
		if old := p.snap; old != nil {
			old.stale = true
			if old.refs == 0 {
				old.db.Close()
			}
		}
		p.snap = &opencodeSnap{db: db, key: key}
	}
	s := p.snap
	s.refs++
	return s.db, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		s.refs--
		if s.stale && s.refs == 0 {
			s.db.Close()
		}
	}, nil
}

func (p *opencodeProvider) Discover() ([]string, error) {
	if _, err := os.Stat(p.dbPath()); os.IsNotExist(err) {
		p.mu.Lock()
		p.stamps = nil
		p.mu.Unlock()
		return nil, nil
	}
	db, release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	sessions := db.Table("session")
	if sessions == nil || db.Table("part") == nil {
		return nil, fmt.Errorf("%s: no session table; this OpenCode version's storage isn't supported yet", p.dbPath())
	}

	// A session changes when its row does (OpenCode adds every step's cost
	// and tokens to it) or when it gains parts.
	parts := map[string]int{}
	if root := db.indexOn("part", "session_id"); root != 0 {
		err = db.walkIndex(root, func(vals []any) {
			if s, ok := vals[0].(string); ok {
				parts[s]++
			}
		})
		if err != nil {
			return nil, err
		}
	}
	own := map[string]string{}
	parent := map[string]string{}
	err = sessions.Scan(func(r sqliteRow) error {
		id := r.Str("id")
		own[id] = opencodeOwnStamp(r, parts[id])
		if pid := r.Str("parent_id"); pid != "" {
			parent[id] = pid
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Fold each subagent's stamp into its top-level session.
	// Sorted, so an unchanged session always gets the same stamp.
	ids := make([]string, 0, len(own))
	for id := range own {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stamps := map[string]string{}
	for _, id := range ids {
		top := id
		for i := 0; i < 16 && parent[top] != ""; i++ {
			top = parent[top]
		}
		path := p.dbPath() + "#" + top
		stamps[path] += own[id] + ";"
	}
	out := make([]string, 0, len(stamps))
	for _, id := range ids {
		if parent[id] == "" {
			out = append(out, p.dbPath()+"#"+id)
		}
	}
	p.mu.Lock()
	p.stamps, p.stampsDB = stamps, db
	p.mu.Unlock()
	return out, nil
}

// opencodeOwnStamp is one session row's part of a stamp.
func opencodeOwnStamp(r sqliteRow, parts int) string {
	return fmt.Sprintf("%d/%d/%g/%d/%d/%s", r.Int("time_updated"), parts, r.Float("cost"),
		r.Int("tokens_input"), r.Int("tokens_output"), r.Str("title"))
}

// Stamp returns the stamp Discover computed, while the database hasn't
// changed since. Live updates ask between scans, so after a change it is
// worked out for this one session through the indexes, the same way.
func (p *opencodeProvider) Stamp(path string) (string, bool) {
	db, release, err := p.acquire()
	if err != nil {
		return "", false
	}
	defer release()
	p.mu.Lock()
	if db == p.stampsDB {
		s, ok := p.stamps[path]
		p.mu.Unlock()
		return s, ok
	}
	p.mu.Unlock()

	i := strings.LastIndex(path, "#")
	sessions, partRoot := db.Table("session"), db.indexOn("part", "session_id")
	if i < 0 || sessions == nil || partRoot == 0 {
		return "", false
	}
	// The session and its subagents, in the same sorted order as Discover.
	rows := map[string]sqliteRow{}
	queue := []string{path[i+1:]}
	for depth := 0; len(queue) > 0 && depth < 16; depth++ {
		var next []string
		for _, id := range queue {
			found, err := sessions.Lookup("id", id)
			if err != nil || len(found) == 0 {
				if depth == 0 {
					return "", false
				}
				continue
			}
			rows[id] = found[0]
			children, err := sessions.Lookup("parent_id", id)
			if err != nil {
				return "", false
			}
			for _, c := range children {
				if _, seen := rows[c.Str("id")]; !seen {
					next = append(next, c.Str("id"))
				}
			}
		}
		queue = next
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stamp := ""
	for _, id := range ids {
		n, err := db.indexRowids(partRoot, id)
		if err != nil {
			return "", false
		}
		stamp += opencodeOwnStamp(rows[id], len(n)) + ";"
	}
	return stamp, true
}

type opencodeMessage struct {
	id         string
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Cost    float64         `json:"cost"`
	Tokens  *opencodeTokens `json:"tokens"`
	created int64
	parts   []opencodePart
}

// opencodeTokens is how OpenCode records a step's usage: input excludes cache
// reads and writes, and reasoning is counted apart from output.
type opencodeTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

func (o *opencodeTokens) step(model string, cost float64) step {
	st := step{model: model, recorded: cost}
	st.Input, st.CacheRead, st.CacheWrite = o.Input, o.Cache.Read, o.Cache.Write
	st.Output, st.Reasoning = o.Output+o.Reasoning, o.Reasoning
	return st
}

type opencodePart struct {
	id        string
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	Tool      string `json:"tool"`
	// step-finish
	Cost   float64         `json:"cost"`
	Tokens *opencodeTokens `json:"tokens"`
	Time   struct {
		Start int64 `json:"start"`
	} `json:"time"`
	State struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Error    string          `json:"error"`
		Metadata struct {
			SessionID string `json:"sessionId"`
		} `json:"metadata"`
		Time struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
	} `json:"state"`
}

// opencodeDefaultTitle matches the placeholder title a session has until
// OpenCode names it.
var opencodeDefaultTitle = regexp.MustCompile(`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func (p *opencodeProvider) Parse(path string, full bool) (*Transcript, error) {
	i := strings.LastIndex(path, "#")
	if i < 0 {
		return nil, os.ErrNotExist
	}
	id := path[i+1:]
	db, release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer release()

	rows, err := db.Table("session").Lookup("id", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, os.ErrNotExist
	}
	row := rows[0]
	t := &Transcript{Session: &Session{ID: id, Provider: "opencode", Path: path, Cwd: row.Str("directory")}}
	s := t.Session
	t.touch(msTime(row.Int("time_created")))
	t.touch(msTime(row.Int("time_updated")))
	if title := strings.TrimSpace(row.Str("title")); title != "" && !opencodeDefaultTitle.MatchString(title) {
		s.Title, s.TitleKind = oneLine(title, 90), "generated"
	}
	if err := p.addSession(db, t, id, full, false, 0); err != nil {
		return nil, err
	}
	for i := range t.Changes {
		t.Changes[i].Rel = relTo(s.Cwd, t.Changes[i].Path)
	}
	s.Resume = "opencode --session " + id
	if s.Cwd != "" {
		s.Resume = "cd " + shellQuote(s.Cwd) + " && " + s.Resume
	}
	t.finish()
	return t, nil
}

// addSession appends one session's messages to t. Subagent sessions started
// by the task tool are added right after the call, marked as sidechain.
func (p *opencodeProvider) addSession(db *sqliteDB, t *Transcript, id string, full, side bool, depth int) error {
	msgs, err := opencodeMessages(db, id)
	if err != nil {
		return err
	}
	s := t.Session
	for _, m := range msgs {
		ts := msTime(m.created)
		if !side {
			t.touch(ts)
			t.touch(msTime(m.Time.Completed))
		}
		if m.Role == "assistant" && m.ModelID != "" && !side {
			s.Model = m.ModelID
		}
		if m.Role == "user" {
			var texts []string
			for _, pt := range m.parts {
				if pt.Type == "text" && !pt.Synthetic && !pt.Ignored && strings.TrimSpace(pt.Text) != "" {
					texts = append(texts, strings.TrimSpace(pt.Text))
				}
			}
			if len(texts) == 0 {
				continue
			}
			text := strings.Join(texts, "\n\n")
			s.Messages++
			if s.Prompt == "" && !side {
				s.Prompt = oneLine(text, promptChars)
			}
			if full {
				t.Events = append(t.Events, Event{Kind: "user", Time: ts, Text: text, Sidechain: side})
			}
			continue
		}
		if m.Role != "assistant" {
			continue
		}
		// Usage goes on the turn's last own event, not on the steps of a
		// subagent the turn started.
		steps, mine := 0, -1
		if full {
			mine = len(t.Events) - 1
		}
		for _, pt := range m.parts {
			pts := ts
			if pt.Time.Start > 0 {
				pts = msTime(pt.Time.Start)
			}
			switch pt.Type {
			case "text":
				text := strings.TrimSpace(pt.Text)
				if text == "" || pt.Synthetic {
					continue
				}
				s.Messages++
				if full {
					t.Events = append(t.Events, Event{Kind: "assistant", Time: pts, Text: text, Sidechain: side})
					mine = len(t.Events) - 1
				}
			case "reasoning":
				if full && strings.TrimSpace(pt.Text) != "" {
					t.Events = append(t.Events, Event{Kind: "thinking", Time: pts, Text: pt.Text, Sidechain: side})
					mine = len(t.Events) - 1
				}
			case "step-finish":
				// Each model request in a turn ends with one, carrying its
				// usage and the cost OpenCode worked out for it.
				if pt.Tokens != nil {
					steps++
					t.addUsage(pt.Tokens.step(m.ModelID, pt.Cost), mine)
				}
			case "tool":
				st := pt.State
				if st.Time.Start > 0 {
					pts = msTime(st.Time.Start)
				}
				// Only a completed call changed the files; a pending or
				// running one may still fail or be cancelled.
				var changes []FileChange
				if st.Status == "completed" {
					changes = opencodeChanges(pt.Tool, st.Input, s.Cwd)
				}
				idx := -1
				if full {
					ev := Event{Kind: "tool", Time: pts, Tool: pt.Tool, Sidechain: side,
						Summary: opencodeToolSummary(pt.Tool, st.Input, s.Cwd),
						Input:   clip(opencodeToolInput(pt.Tool, st.Input), maxToolText)}
					switch st.Status {
					case "completed":
						ev.Output = clip(st.Output, maxToolText)
					case "error":
						ev.Output, ev.Error = clip(st.Error, maxToolText), true
					}
					t.Events = append(t.Events, ev)
					idx = len(t.Events) - 1
					mine = idx
				}
				for _, c := range changes {
					c.Time = pts
					t.addChange(c, idx)
				}
				if sub := st.Metadata.SessionID; pt.Tool == "task" && sub != "" && sub != id && depth < 4 {
					if err := p.addSession(db, t, sub, full, true, depth+1); err != nil {
						return err
					}
				}
			}
		}
		// Messages written before step parts existed carry the turn's total.
		if steps == 0 && m.Tokens != nil {
			t.addUsage(m.Tokens.step(m.ModelID, m.Cost), mine)
		}
	}
	return nil
}

// opencodeMessages loads a session's messages with their parts, in the order
// OpenCode shows them.
func opencodeMessages(db *sqliteDB, sessionID string) ([]*opencodeMessage, error) {
	mt, pt := db.Table("message"), db.Table("part")
	if mt == nil || pt == nil {
		return nil, nil
	}
	rows, err := mt.Lookup("session_id", sessionID)
	if err != nil {
		return nil, err
	}
	byID := map[string]*opencodeMessage{}
	msgs := make([]*opencodeMessage, 0, len(rows))
	for _, r := range rows {
		m := &opencodeMessage{id: r.Str("id"), created: r.Int("time_created")}
		if json.Unmarshal([]byte(r.Str("data")), m) != nil {
			continue
		}
		if m.Time.Created > 0 {
			m.created = m.Time.Created
		}
		byID[m.id] = m
		msgs = append(msgs, m)
	}
	rows, err = pt.Lookup("session_id", sessionID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		m := byID[r.Str("message_id")]
		if m == nil {
			continue
		}
		part := opencodePart{id: r.Str("id")}
		if json.Unmarshal([]byte(r.Str("data")), &part) != nil {
			continue
		}
		m.parts = append(m.parts, part)
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].created != msgs[j].created {
			return msgs[i].created < msgs[j].created
		}
		return msgs[i].id < msgs[j].id
	})
	for _, m := range msgs {
		sort.SliceStable(m.parts, func(i, j int) bool { return m.parts[i].id < m.parts[j].id })
	}
	return msgs, nil
}

// opencodeChanges turns an editing tool call into file changes. OpenCode's
// edit and write tools take absolute paths; apply_patch (used with OpenAI
// models) takes the Codex patch format with paths relative to the session.
func opencodeChanges(tool string, raw json.RawMessage, cwd string) []FileChange {
	var in struct {
		FilePath  string `json:"filePath"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
		Content   string `json:"content"`
		PatchText string `json:"patchText"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) || cwd == "" {
			return p
		}
		return filepath.Join(cwd, p)
	}
	switch tool {
	case "edit":
		if in.FilePath != "" {
			return []FileChange{{Path: abs(in.FilePath), Op: "edit", Hunks: []Hunk{{Old: in.OldString, New: in.NewString}}}}
		}
	case "write":
		if in.FilePath != "" {
			return []FileChange{{Path: abs(in.FilePath), Op: "write", Hunks: []Hunk{{New: in.Content}}}}
		}
	case "apply_patch", "patch":
		return parsePatch(in.PatchText, cwd)
	}
	return nil
}

func opencodeToolSummary(tool string, raw json.RawMessage, cwd string) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch {
	case tool == "apply_patch" || tool == "patch":
		return patchSummary(str("patchText"))
	case str("filePath") != "":
		return relTo(cwd, str("filePath"))
	case str("command") != "":
		return oneLine(str("command"), 120)
	case str("pattern") != "":
		return str("pattern")
	case str("url") != "":
		return str("url")
	case str("query") != "":
		return oneLine(str("query"), 120)
	case str("description") != "":
		return oneLine(str("description"), 120)
	case str("prompt") != "":
		return oneLine(str("prompt"), 120)
	case str("name") != "":
		return str("name")
	}
	return ""
}

func opencodeToolInput(tool string, raw json.RawMessage) string {
	var in struct {
		Command   string `json:"command"`
		PatchText string `json:"patchText"`
	}
	json.Unmarshal(raw, &in)
	switch {
	case tool == "bash" && in.Command != "":
		return "$ " + in.Command
	case in.PatchText != "":
		return in.PatchText
	}
	if len(raw) == 0 {
		return ""
	}
	return prettyJSON(raw)
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kiroProvider reads Kiro CLI sessions (formerly Amazon Q Developer CLI).
// Kiro has kept them in three places over its versions, and trail reads all
// of them:
//
//   - <dir>/sessions/cli/<id>.jsonl, with a <id>.json header beside it: the
//     interactive CLI 2.x. Each line is {"kind","data"}: a Prompt, an
//     AssistantMessage (several can share a message_id while a reply
//     streams in), or ToolResults. The header has the folder, the title, the
//     model, and parent_session_id for a subagent's session.
//   - <dir>/sessions/<workspace>/sess_<id>/messages.jsonl, with session.json
//     beside it: the V3 engine (kiro-cli --v3, the default from CLI 3.0) and
//     the Kiro IDE. Each line is {"timestamp","payload"}; a tool call is
//     written again each time its state changes.
//   - <data dir>/data.sqlite3: headless runs (kiro-cli chat --no-interactive)
//     in the conversations_v2 table, and the conversations table of Kiro
//     CLI 1.x, one conversation per folder. A row's value is the whole
//     conversation as JSON. It is read with trail's own SQLite reader, and
//     each conversation is addressed as "<db path>#<conversation id>".
//
// A conversation found in a JSONL file and in the database is read from the
// file. Subagent sessions are shown inside their parent as sidechain events.
type kiroProvider struct {
	dir     string // ~/.kiro
	dataDir string // kiro-cli's data directory, which holds data.sqlite3

	once  sync.Once
	snaps *sqliteSnapshots

	mu       sync.Mutex
	heads    map[string]kiroHeadEntry // header path -> header
	children map[string][]string      // parent session id -> subagent transcripts
	rows     map[string]kiroRow       // db path#id -> row, as of rowsKey
	rowsKey  string
}

type kiroHeadEntry struct {
	stamp string
	head  kiroCLIHeader
}

// kiroRow is where a database conversation is, and its stamp.
type kiroRow struct {
	id, table, cwd string
	rowid          int64
	stamp          string
}

type kiroCLIHeader struct {
	SessionID       string `json:"session_id"`
	Cwd             string `json:"cwd"`
	Title           string `json:"title"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	ParentSessionID string `json:"parent_session_id"`
	SessionState    struct {
		RTSModelState struct {
			ModelInfo struct {
				ModelID string `json:"model_id"`
			} `json:"model_info"`
		} `json:"rts_model_state"`
	} `json:"session_state"`
}

func (p *kiroProvider) Name() string { return "kiro" }
func (p *kiroProvider) Root() string { return p.dir }

func (p *kiroProvider) Sources() []string {
	return []string{filepath.Join(p.dir, "sessions"), p.dbPath()}
}

func (p *kiroProvider) dbPath() string { return filepath.Join(p.dataDir, "data.sqlite3") }
func (p *kiroProvider) cliDir() string { return filepath.Join(p.dir, "sessions", "cli") }

func (p *kiroProvider) acquire() (*sqliteDB, func(), error) {
	p.once.Do(func() { p.snaps = &sqliteSnapshots{path: p.dbPath()} })
	return p.snaps.acquire()
}

func (p *kiroProvider) isDB(path string) bool {
	return strings.HasPrefix(path, p.dbPath()+"#")
}

func (p *kiroProvider) Discover() ([]string, error) {
	var out []string
	seen := map[string]bool{} // conversation ids found in files
	top := map[string]bool{}  // ids of the CLI sessions listed on their own
	children := map[string][]string{}

	files, _ := os.ReadDir(p.cliDir())
	heads := map[string]kiroHeadEntry{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(p.cliDir(), f.Name())
		h := p.head(path, heads)
		id := kiroCLIID(path, h)
		seen[id] = true
		if par := h.ParentSessionID; par != "" && par != id {
			children[par] = append(children[par], path)
			continue
		}
		top[id] = true
		out = append(out, path)
	}
	// A subagent whose parent is gone is listed on its own.
	for par, paths := range children {
		if !top[par] {
			out = append(out, paths...)
			delete(children, par)
		}
	}
	for _, paths := range children {
		sort.Strings(paths)
	}

	workspaces, _ := os.ReadDir(filepath.Join(p.dir, "sessions"))
	for _, ws := range workspaces {
		if !ws.IsDir() || ws.Name() == "cli" {
			continue
		}
		sessions, _ := os.ReadDir(filepath.Join(p.dir, "sessions", ws.Name()))
		for _, s := range sessions {
			if !s.IsDir() || !strings.HasPrefix(s.Name(), "sess_") {
				continue
			}
			path := filepath.Join(p.dir, "sessions", ws.Name(), s.Name(), "messages.jsonl")
			if _, err := os.Stat(path); err == nil {
				out = append(out, path)
				seen[s.Name()] = true
			}
		}
	}

	p.mu.Lock()
	p.heads, p.children = heads, children
	p.mu.Unlock()

	rows, err := p.dbRows()
	paths := make([]string, 0, len(rows))
	for path, r := range rows {
		if !seen[r.id] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return append(out, paths...), err
}

// head reads a CLI session's header, reusing the last read while the file
// is unchanged. Discover passes the map it is building; other callers nil.
func (p *kiroProvider) head(jsonl string, into map[string]kiroHeadEntry) kiroCLIHeader {
	path := strings.TrimSuffix(jsonl, ".jsonl") + ".json"
	stamp := kiroFileStamp(path)
	p.mu.Lock()
	e, ok := p.heads[path]
	p.mu.Unlock()
	if !ok || e.stamp != stamp {
		e = kiroHeadEntry{stamp: stamp}
		if b, err := os.ReadFile(path); err == nil {
			json.Unmarshal(b, &e.head)
		}
	}
	if into != nil {
		into[path] = e
	}
	return e.head
}

func kiroCLIID(path string, h kiroCLIHeader) string {
	if h.SessionID != "" {
		return h.SessionID
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func kiroFileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return strconv.FormatInt(fi.Size(), 10) + "@" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

// dbRows lists the database's conversations, reading the tables again only
// when the database or its WAL changed.
func (p *kiroProvider) dbRows() (map[string]kiroRow, error) {
	// kiro-cli creates an empty file on first launch.
	if fi, err := os.Stat(p.dbPath()); err != nil || fi.Size() == 0 {
		p.mu.Lock()
		p.rows, p.rowsKey = nil, ""
		p.mu.Unlock()
		return nil, nil
	}
	key := sqliteStatKey(p.dbPath())
	p.mu.Lock()
	if p.rows != nil && p.rowsKey == key {
		rows := p.rows
		p.mu.Unlock()
		return rows, nil
	}
	p.mu.Unlock()

	db, release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	rows := map[string]kiroRow{}
	if t := db.Table("conversations_v2"); t != nil {
		err = t.Scan(func(r sqliteRow) error {
			id := r.Str("conversation_id")
			if id != "" {
				rows[p.dbPath()+"#"+id] = kiroRow{id: id, table: "conversations_v2", cwd: r.Str("key"), rowid: r.Rowid(),
					stamp: fmt.Sprintf("%d/%d", r.Int("updated_at"), len(r.Str("value")))}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Kiro CLI 1.x: one row per folder, keyed by the folder, no timestamps.
	if t := db.Table("conversations"); t != nil {
		err = t.Scan(func(r sqliteRow) error {
			v := r.Str("value")
			var c struct {
				ConversationID string `json:"conversation_id"`
			}
			if json.Unmarshal([]byte(v), &c) != nil || c.ConversationID == "" {
				return nil
			}
			path := p.dbPath() + "#" + c.ConversationID
			if _, ok := rows[path]; ok {
				return nil
			}
			h := fnv.New64a()
			h.Write([]byte(v))
			rows[path] = kiroRow{id: c.ConversationID, table: "conversations", cwd: r.Str("key"), rowid: r.Rowid(),
				stamp: strconv.FormatUint(h.Sum64(), 36)}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	p.rows, p.rowsKey = rows, sqliteStatKey(p.dbPath())
	if p.rowsKey != key {
		p.rowsKey = "" // changed while being read; read it again next time
	}
	p.mu.Unlock()
	return rows, nil
}

// Stamp changes whenever a session does: its row in the database, or its
// transcript, its header, and its subagents' transcripts.
func (p *kiroProvider) Stamp(path string) (string, bool) {
	if p.isDB(path) {
		rows, err := p.dbRows()
		if err != nil {
			return "", false
		}
		r, ok := rows[path]
		return r.stamp, ok
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	stamp := kiroFileStamp(path)
	if filepath.Base(path) == "messages.jsonl" {
		return stamp + ";" + kiroFileStamp(filepath.Join(filepath.Dir(path), "session.json")), true
	}
	stamp += ";" + kiroFileStamp(strings.TrimSuffix(path, ".jsonl")+".json")
	p.mu.Lock()
	kids := p.children[kiroCLIID(path, p.heads[strings.TrimSuffix(path, ".jsonl")+".json"].head)]
	p.mu.Unlock()
	for _, k := range kids {
		stamp += ";" + kiroFileStamp(k) + "," + kiroFileStamp(strings.TrimSuffix(k, ".jsonl")+".json")
	}
	return stamp, true
}

func (p *kiroProvider) Parse(path string, full bool) (*Transcript, error) {
	switch {
	case p.isDB(path):
		return p.parseDB(path, full)
	case filepath.Base(path) == "messages.jsonl":
		return p.parseV3(path, full)
	}
	t, err := p.parseCLI(path, full)
	if err != nil {
		return nil, err
	}
	kiroSetResume(t.Session, "kiro-cli chat --resume-id "+t.Session.ID)
	p.mu.Lock()
	kids := p.children[t.Session.ID]
	p.mu.Unlock()
	for _, k := range kids {
		if c, err := p.parseCLI(k, full); err == nil {
			kiroAddSidechain(t, c)
		}
	}
	t.finish()
	return t, nil
}

// kiroBuilder turns turns, replies and tool calls into a transcript. All
// three stores have the same pieces; only the JSON around them differs.
type kiroBuilder struct {
	t    *Transcript
	full bool

	calls    map[string]*kiroCall
	lastID   string // message id of the reply being streamed
	lastText int    // its event, or -1
}

type kiroCall struct {
	event int
	name  string
	args  json.RawMessage
	time  time.Time
	done  bool
}

func newKiroBuilder(t *Transcript, full bool) *kiroBuilder {
	return &kiroBuilder{t: t, full: full, calls: map[string]*kiroCall{}, lastText: -1}
}

func (b *kiroBuilder) user(text string, ts time.Time) {
	text = strings.TrimSpace(text)
	b.lastID, b.lastText = "", -1
	if text == "" {
		return
	}
	s := b.t.Session
	s.Messages++
	if s.Prompt == "" && !strings.HasPrefix(text, "/") {
		s.Prompt = oneLine(text, promptChars)
	}
	if b.full {
		b.t.Events = append(b.t.Events, Event{Kind: "user", Time: ts, Text: text})
	}
}

// assistant adds reply text. Pieces under the message id of the reply
// before it are the same reply, streamed.
func (b *kiroBuilder) assistant(id, text string, ts time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if id != "" && id == b.lastID {
		if b.lastText >= 0 {
			b.t.Events[b.lastText].Text += text
		}
		return
	}
	b.lastID, b.lastText = id, -1
	b.t.Session.Messages++
	if b.full {
		b.t.Events = append(b.t.Events, Event{Kind: "assistant", Time: ts, Text: strings.TrimLeft(text, "\n")})
		b.lastText = len(b.t.Events) - 1
	}
}

// toolUse adds a call. Its file changes wait for a result that says it ran.
func (b *kiroBuilder) toolUse(id, name string, args json.RawMessage, ts time.Time) *kiroCall {
	if c := b.calls[id]; c != nil && id != "" {
		return c
	}
	c := &kiroCall{event: -1, name: name, args: args, time: ts}
	if b.full {
		cwd := b.t.Session.Cwd
		b.t.Events = append(b.t.Events, Event{Kind: "tool", Time: ts, Tool: name,
			Summary: kiroToolSummary(args, cwd), Input: clip(kiroToolInput(name, args), maxToolText), callID: id})
		c.event = len(b.t.Events) - 1
	}
	if id != "" {
		b.calls[id] = c
	}
	return c
}

func (b *kiroBuilder) toolResult(id, out string, failed bool) {
	c := b.calls[id]
	if c == nil {
		return
	}
	if c.event >= 0 {
		e := &b.t.Events[c.event]
		e.Output, e.Error = clip(out, maxToolText), failed
	}
	if !failed {
		b.ran(c)
	}
}

// ran records a call's file changes once.
func (b *kiroBuilder) ran(c *kiroCall) {
	if c.done {
		return
	}
	c.done = true
	for _, ch := range kiroChanges(c.name, c.args, b.t.Session.Cwd) {
		ch.Time = c.time
		b.t.addChange(ch, c.event)
	}
}

// kiroPart is one part of a CLI record's content: text, toolUse, toolResult,
// or (inside a result) json.
type kiroPart struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// parseCLI reads a CLI 2.x session. The caller finishes the transcript, once
// its subagents are in.
func (p *kiroProvider) parseCLI(path string, full bool) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := p.head(path, nil)
	id := kiroCLIID(path, h)
	t := &Transcript{Session: &Session{ID: id, Provider: "kiro", Path: path, Cwd: h.Cwd,
		Model: h.SessionState.RTSModelState.ModelInfo.ModelID}}
	s := t.Session
	t.touch(kiroTime(h.CreatedAt))
	t.touch(kiroTime(h.UpdatedAt))
	if title := strings.TrimSpace(h.Title); title != "" {
		s.Title, s.TitleKind = oneLine(title, 90), "generated"
	}
	b := newKiroBuilder(t, full)

	var ts time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			Kind string `json:"kind"`
			Data struct {
				MessageID string     `json:"message_id"`
				Content   []kiroPart `json:"content"`
				Meta      struct {
					Timestamp json.RawMessage `json:"timestamp"`
				} `json:"meta"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue // also a line still being written
		}
		// Older versions stamp only the prompt; the rest take its time.
		if at := kiroRawTime(rec.Data.Meta.Timestamp); !at.IsZero() {
			ts = at
		}
		t.touch(ts)
		switch rec.Kind {
		case "Prompt":
			var texts []string
			for _, pt := range rec.Data.Content {
				if s := kiroPartText(pt); s != "" {
					texts = append(texts, s)
				}
			}
			b.user(strings.Join(texts, "\n"), ts)
		case "AssistantMessage":
			for _, pt := range rec.Data.Content {
				switch pt.Kind {
				case "text", "":
					b.assistant(rec.Data.MessageID, kiroPartText(pt), ts)
				case "toolUse":
					var u struct {
						ToolUseID string          `json:"toolUseId"`
						Name      string          `json:"name"`
						Input     json.RawMessage `json:"input"`
					}
					if json.Unmarshal(pt.Data, &u) == nil && u.Name != "" {
						b.toolUse(u.ToolUseID, u.Name, u.Input, ts)
					}
				}
			}
		case "ToolResults":
			for _, pt := range rec.Data.Content {
				var r struct {
					ToolUseID string     `json:"toolUseId"`
					Content   []kiroPart `json:"content"`
					Status    string     `json:"status"`
				}
				if pt.Kind != "toolResult" || json.Unmarshal(pt.Data, &r) != nil {
					continue
				}
				var blocks []kiroResultBlock
				for _, c := range r.Content {
					switch c.Kind {
					case "json":
						blocks = append(blocks, kiroResultBlock{json: c.Data})
					default:
						var s string
						json.Unmarshal(c.Data, &s)
						blocks = append(blocks, kiroResultBlock{text: s})
					}
				}
				out, failed := kiroResultOutput(blocks)
				b.toolResult(r.ToolUseID, out, failed || strings.EqualFold(r.Status, "error"))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

func kiroPartText(pt kiroPart) string {
	if pt.Kind != "text" && pt.Kind != "" {
		return ""
	}
	var s string
	json.Unmarshal(pt.Data, &s)
	return s
}

// kiroResultBlock is one block of a tool result: text, or JSON (a shell
// command's exit status and output).
type kiroResultBlock struct {
	text string
	json json.RawMessage
}

// kiroResultOutput is what a result shows, and whether it failed: a shell
// command that exited non-zero failed even when the call succeeded.
func kiroResultOutput(blocks []kiroResultBlock) (string, bool) {
	var out []string
	failed := false
	for _, b := range blocks {
		if len(b.json) == 0 {
			if strings.TrimSpace(b.text) != "" {
				out = append(out, b.text)
			}
			continue
		}
		var sh struct {
			ExitStatus *string `json:"exit_status"`
			Stdout     *string `json:"stdout"`
			Stderr     *string `json:"stderr"`
		}
		json.Unmarshal(b.json, &sh)
		if sh.Stdout == nil && sh.Stderr == nil {
			var s string
			if json.Unmarshal(b.json, &s) == nil {
				out = append(out, s)
			} else {
				out = append(out, prettyJSON(b.json))
			}
			continue
		}
		for _, v := range []*string{sh.Stdout, sh.Stderr} {
			if v != nil && strings.TrimSpace(*v) != "" {
				out = append(out, strings.TrimRight(*v, "\n"))
			}
		}
		if sh.ExitStatus != nil {
			// "exit status: 1" in the CLI's files, "1" in the database.
			code := strings.TrimSpace(strings.TrimPrefix(*sh.ExitStatus, "exit status:"))
			if code != "0" && code != "" {
				failed = true
				out = append(out, "exit status "+code)
			}
		}
	}
	return strings.Join(out, "\n"), failed
}

// parseV3 reads a sess_ session of the V3 engine or the IDE.
func (p *kiroProvider) parseV3(path string, full bool) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dir := filepath.Dir(path)
	var meta struct {
		ID             string   `json:"id"`
		Title          string   `json:"title"`
		ModelID        string   `json:"modelId"`
		WorkspacePaths []string `json:"workspacePaths"`
		CreatedAt      string   `json:"createdAt"`
		LastModifiedAt string   `json:"lastModifiedAt"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "session.json")); err == nil {
		json.Unmarshal(b, &meta)
	}
	id := meta.ID
	if id == "" {
		id = filepath.Base(dir)
	}
	t := &Transcript{Session: &Session{ID: id, Provider: "kiro", Path: path, Model: meta.ModelID}}
	s := t.Session
	if len(meta.WorkspacePaths) > 0 {
		s.Cwd = meta.WorkspacePaths[0]
	}
	t.touch(kiroTime(meta.CreatedAt))
	t.touch(kiroTime(meta.LastModifiedAt))
	if title := strings.TrimSpace(meta.Title); title != "" {
		s.Title, s.TitleKind = oneLine(title, 90), "generated"
	}
	b := newKiroBuilder(t, full)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			Payload   *struct {
				Type          string          `json:"type"`
				Content       json.RawMessage `json:"content"`
				OperationType string          `json:"operationType"`
				ToolCallID    string          `json:"toolCallId"`
				ToolName      string          `json:"toolName"`
				Args          json.RawMessage `json:"args"`
				Status        string          `json:"status"`
				Success       *bool           `json:"success"`
			} `json:"payload"`
			// Older builds wrote flat lines.
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		ts := kiroTime(rec.Timestamp)
		pl := rec.Payload
		if pl == nil {
			if rec.Role == "" {
				continue
			}
			t.touch(ts)
			switch rec.Role {
			case "user":
				b.user(kiroV3Text(rec.Content), ts)
			case "assistant":
				b.assistant("", kiroV3Text(rec.Content), ts)
			}
			continue
		}
		switch pl.Type {
		case "user":
			t.touch(ts)
			b.user(kiroV3Text(pl.Content), ts)
		case "assistant":
			// Thinking is encrypted; its content is a placeholder.
			if pl.OperationType == "Reasoning" {
				continue
			}
			t.touch(ts)
			b.assistant(rec.ID, kiroV3Text(pl.Content), ts)
		case "tool_call":
			// Written again at each step: awaiting approval, executing, then
			// completed, failed or denied.
			t.touch(ts)
			c := b.toolUse(pl.ToolCallID, pl.ToolName, pl.Args, ts)
			if len(pl.Args) > 0 {
				c.args = pl.Args
			}
			switch pl.Status {
			case "completed":
				b.ran(c)
			case "failed", "denied", "cancelled":
				if c.event >= 0 {
					e := &t.Events[c.event]
					e.Error = true
					if e.Output == "" && pl.Status != "failed" {
						e.Output = strings.ToUpper(pl.Status[:1]) + pl.Status[1:]
					}
				}
			}
		case "tool_result":
			t.touch(ts)
			ok := pl.Success == nil || *pl.Success
			b.toolResult(pl.ToolCallID, kiroV3Text(pl.Content), !ok)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// The V3 CLI lists its sessions in session-index/<workspace>.jsonl; the
	// IDE's sessions aren't there, and open from the IDE instead.
	if kiroV3Listed(p.dir, dir) {
		kiroSetResume(s, "kiro-cli --v3 chat --resume-id "+id)
	}
	t.finish()
	return t, nil
}

func kiroV3Text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// A list of content blocks.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var out []string
	for _, pt := range parts {
		if strings.TrimSpace(pt.Text) != "" {
			out = append(out, pt.Text)
		}
	}
	return strings.Join(out, "\n")
}

func kiroV3Listed(dir, sessDir string) bool {
	ws := filepath.Base(filepath.Dir(sessDir))
	b, err := os.ReadFile(filepath.Join(dir, "session-index", ws+".jsonl"))
	if err != nil {
		return false
	}
	want := ws + "/" + filepath.Base(sessDir)
	listed := false
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e struct {
			SessionPath string `json:"sessionPath"`
			Op          string `json:"op"`
		}
		if json.Unmarshal(line, &e) == nil && filepath.ToSlash(e.SessionPath) == want {
			listed = e.Op != "remove" && e.Op != "delete"
		}
	}
	return listed
}

// kiroConversation is a database row's value: Kiro CLI's ConversationState.
type kiroConversation struct {
	ConversationID string            `json:"conversation_id"`
	History        []json.RawMessage `json:"history"`
	ModelInfo      *struct {
		ModelID string `json:"model_id"`
	} `json:"model_info"`
	Model string `json:"model"`
}

type kiroUserMessage struct {
	Content map[string]struct {
		Prompt  *string `json:"prompt"`
		Results []struct {
			ToolUseID string                       `json:"tool_use_id"`
			Content   []map[string]json.RawMessage `json:"content"`
			Status    string                       `json:"status"`
		} `json:"tool_use_results"`
	} `json:"content"`
	Timestamp  string `json:"timestamp"`
	EnvContext struct {
		EnvState struct {
			Cwd string `json:"current_working_directory"`
		} `json:"env_state"`
	} `json:"env_context"`
}

// kiroAssistantMessage is {"Response":{…}} or {"ToolUse":{…}}.
type kiroAssistantMessage map[string]struct {
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
	ToolUses  []struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_uses"`
}

func (p *kiroProvider) parseDB(path string, full bool) (*Transcript, error) {
	rows, err := p.dbRows()
	if err != nil {
		return nil, err
	}
	r, ok := rows[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	db, release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	tbl := db.Table(r.table)
	if tbl == nil {
		return nil, os.ErrNotExist
	}
	var conv kiroConversation
	var created, updated int64
	row, found, err := tbl.Get(r.rowid)
	if err != nil {
		return nil, err
	}
	match := func(row sqliteRow) bool {
		if r.table == "conversations_v2" {
			return row.Str("conversation_id") == r.id
		}
		return row.Str("key") == r.cwd
	}
	if !found || !match(row) {
		// The table changed since it was listed.
		found = false
		err = tbl.Scan(func(x sqliteRow) error {
			if !found && match(x) {
				row, found = x, true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, os.ErrNotExist
		}
	}
	if err := json.Unmarshal([]byte(row.Str("value")), &conv); err != nil {
		return nil, err
	}
	created, updated = row.Int("created_at"), row.Int("updated_at")

	t := &Transcript{Session: &Session{ID: r.id, Provider: "kiro", Path: path, Cwd: row.Str("key")}}
	s := t.Session
	if conv.ModelInfo != nil && conv.ModelInfo.ModelID != "" {
		s.Model = conv.ModelInfo.ModelID
	} else {
		s.Model = conv.Model
	}
	t.touch(msTime(created))
	t.touch(msTime(updated))
	b := newKiroBuilder(t, full)
	var ts time.Time
	for _, raw := range conv.History {
		var user kiroUserMessage
		var asst kiroAssistantMessage
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && raw[0] == '[' {
			// Older versions kept each turn as a [user, assistant] pair.
			var pair []json.RawMessage
			json.Unmarshal(raw, &pair)
			if len(pair) > 0 {
				json.Unmarshal(pair[0], &user)
			}
			if len(pair) > 1 {
				json.Unmarshal(pair[1], &asst)
			}
		} else {
			var e struct {
				User      kiroUserMessage      `json:"user"`
				Assistant kiroAssistantMessage `json:"assistant"`
			}
			json.Unmarshal(raw, &e)
			user, asst = e.User, e.Assistant
		}
		if at := kiroTime(user.Timestamp); !at.IsZero() {
			ts = at
		}
		t.touch(ts)
		if s.Cwd == "" {
			s.Cwd = user.EnvContext.EnvState.Cwd
		}
		// Prompt, ToolUseResults, or CancelledToolUses, which has both.
		for _, c := range user.Content {
			for _, res := range c.Results {
				var blocks []kiroResultBlock
				for _, blk := range res.Content {
					if j, ok := blk["Json"]; ok {
						blocks = append(blocks, kiroResultBlock{json: j})
					} else if tx, ok := blk["Text"]; ok {
						var s string
						json.Unmarshal(tx, &s)
						blocks = append(blocks, kiroResultBlock{text: s})
					}
				}
				out, failed := kiroResultOutput(blocks)
				b.toolResult(res.ToolUseID, out, failed || strings.EqualFold(res.Status, "error"))
			}
			if c.Prompt != nil {
				b.user(*c.Prompt, ts)
			}
		}
		for _, a := range asst {
			b.assistant(a.MessageID, a.Content, ts)
			for _, u := range a.ToolUses {
				b.toolUse(u.ID, u.Name, u.Args, ts)
			}
		}
	}
	if r.table == "conversations_v2" {
		kiroSetResume(s, "kiro-cli chat --resume-id "+r.id)
	} else {
		// One conversation per folder; --resume picks it up.
		kiroSetResume(s, "kiro-cli chat --resume")
	}
	t.finish()
	return t, nil
}

func kiroSetResume(s *Session, cmd string) {
	s.Resume = cmd
	if s.Cwd != "" {
		s.Resume = "cd " + shellQuote(s.Cwd) + " && " + cmd
	}
}

// kiroAddSidechain puts a subagent's events into its parent, after the
// parent's events up to the time the subagent started.
func kiroAddSidechain(t, c *Transcript) {
	for i := range c.Changes {
		c.Changes[i].Rel = relTo(t.Session.Cwd, c.Changes[i].Path)
	}
	start := c.Session.Started
	pos := len(t.Events)
	for i, e := range t.Events {
		if !start.IsZero() && e.Time.After(start) {
			pos = i
			break
		}
	}
	n, base := len(c.Events), len(t.Changes)
	for i := range t.Changes {
		if t.Changes[i].Event >= pos {
			t.Changes[i].Event += n
		}
	}
	for i := range c.Events {
		c.Events[i].Sidechain = true
		for k := range c.Events[i].Changes {
			c.Events[i].Changes[k] += base
		}
	}
	for _, ch := range c.Changes {
		if ch.Event >= 0 {
			ch.Event += pos
		}
		t.Changes = append(t.Changes, ch)
	}
	events := make([]Event, 0, len(t.Events)+n)
	events = append(events, t.Events[:pos]...)
	events = append(events, c.Events...)
	t.Events = append(events, t.Events[pos:]...)
	t.touch(c.Session.Started)
	t.touch(c.Session.Ended)
}

// kiroChanges turns a write tool's arguments into file changes. The CLI's
// tools are write (content, oldStr, newStr) and, headless, fs_write
// (file_text, old_str, new_str), each with a command: create, str_replace,
// insert or append. The V3 engine has fs_write and fs_append (text),
// str_replace (oldStr, newStr) and delete_file (targetFile).
func kiroChanges(tool string, raw json.RawMessage, cwd string) []FileChange {
	var in struct {
		Command    string  `json:"command"`
		Path       string  `json:"path"`
		TargetFile string  `json:"targetFile"`
		FileText   *string `json:"file_text"`
		Content    *string `json:"content"`
		Text       *string `json:"text"`
		OldStr     *string `json:"old_str"`
		NewStr     *string `json:"new_str"`
		OldStrC    *string `json:"oldStr"`
		NewStrC    *string `json:"newStr"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	path := in.Path
	if path == "" {
		path = in.TargetFile
	}
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	first := func(vs ...*string) string {
		for _, v := range vs {
			if v != nil {
				return *v
			}
		}
		return ""
	}
	body := first(in.FileText, in.Content, in.Text)
	oldS, newS := first(in.OldStr, in.OldStrC), first(in.NewStr, in.NewStrC)
	edit := func(o, n string) []FileChange {
		return []FileChange{{Path: path, Op: "edit", Hunks: []Hunk{{Old: o, New: n}}}}
	}
	switch tool {
	case "write", "fs_write":
		switch strings.ToLower(strings.ReplaceAll(in.Command, "_", "")) {
		case "create", "":
			if in.Command == "" && body == "" {
				return nil
			}
			return []FileChange{{Path: path, Op: "write", Hunks: []Hunk{{New: body}}}}
		case "strreplace":
			return edit(oldS, newS)
		case "insert", "append":
			// Only the added text is known.
			if newS == "" {
				newS = body
			}
			return edit("", newS)
		}
	case "fs_append":
		return edit("", body)
	case "str_replace":
		return edit(oldS, newS)
	case "delete_file":
		return []FileChange{{Path: path, Op: "delete", Hunks: []Hunk{}}}
	}
	return nil
}

func kiroToolSummary(raw json.RawMessage, cwd string) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch {
	case str("command") != "" && str("path") == "":
		return oneLine(str("command"), 120)
	case str("path") != "":
		return relTo(cwd, str("path"))
	case str("targetFile") != "":
		return relTo(cwd, str("targetFile"))
	}
	if ops, ok := in["operations"].([]any); ok && len(ops) > 0 {
		var paths []string
		for _, op := range ops {
			if m, ok := op.(map[string]any); ok {
				if s, _ := m["path"].(string); s != "" {
					paths = append(paths, relTo(cwd, s))
				}
			}
		}
		return oneLine(strings.Join(paths, ", "), 120)
	}
	for _, k := range []string{"pattern", "query", "url", "prompt", "task", "description", "service_name"} {
		if s := str(k); s != "" {
			return oneLine(s, 120)
		}
	}
	return ""
}

func kiroToolInput(tool string, raw json.RawMessage) string {
	switch tool {
	case "shell", "execute_bash", "execute_cmd", "execute_pwsh":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &in) == nil && in.Command != "" {
			return "$ " + in.Command
		}
	}
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return prettyJSON(raw)
}

// kiroTime parses the CLI's timestamps, which can come without a zone.
func kiroTime(v string) time.Time {
	if t := parseTime(v); !t.IsZero() {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", v); err == nil {
		return t
	}
	return time.Time{}
}

// kiroRawTime reads a timestamp in seconds since the epoch, or a string.
func kiroRawTime(raw json.RawMessage) time.Time {
	var sec float64
	if json.Unmarshal(raw, &sec) == nil && sec > 0 {
		return time.Unix(0, int64(sec*1e9)).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return kiroTime(s)
	}
	return time.Time{}
}

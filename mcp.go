package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// trail -mcp is a Model Context Protocol server on standard input and output,
// so a coding agent can search its own past sessions. The agent starts it
// when it starts and stops it when it exits; nothing runs in the background.
//
// Nothing is read until the first tool call. Then, if a trail window is
// already open on this machine over the same agent folders, its index answers
// and nothing is rebuilt; otherwise this process builds its own.
//
// Every result is redacted, because whatever the agent reads goes to its
// model provider.

const mcpServerName = "trail"

var mcpProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// mcpSource is where sessions come from: this process's own index, or a
// running trail window's.
type mcpSource interface {
	Sessions() ([]*Session, error)
	Load(key string) (*Transcript, error)
	Search(q string, limit int) (SearchResult, error)
}

type mcpServer struct {
	version string
	sources []string // name=root for each provider, to match a running window
	local   *localSource
	uiPath  string // where a running window announces itself
	client  *http.Client

	mu       sync.Mutex
	useLocal bool // the local index is in use; stop looking for a window

	outMu sync.Mutex
	out   io.Writer
}

func newMCPServer(idx *Index, sem func(*Search) *Semantic, version string) *mcpServer {
	return &mcpServer{
		version: version,
		sources: idx.Sources(),
		local:   &localSource{idx: idx, newSem: sem},
		uiPath:  uiFile(),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Serve answers requests until in is closed.
func (m *mcpServer) Serve(in io.Reader, out io.Writer) error {
	m.out = out
	r := bufio.NewReaderSize(in, 1<<16)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if line[0] == '[' {
				// A batch (2025-03-26): answer it as one array.
				var batch []json.RawMessage
				if json.Unmarshal(line, &batch) != nil {
					m.send(rpcError(nil, -32700, "parse error"))
				} else {
					wg.Add(1)
					go func() {
						defer wg.Done()
						var replies []any
						for _, raw := range batch {
							if rep := m.handle(raw); rep != nil {
								replies = append(replies, rep)
							}
						}
						if len(replies) > 0 {
							m.send(replies)
						}
					}()
				}
			} else {
				msg := slices.Clone(line)
				wg.Add(1)
				go func() {
					defer wg.Done()
					if rep := m.handle(msg); rep != nil {
						m.send(rep)
					}
				}()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (m *mcpServer) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	m.outMu.Lock()
	defer m.outMu.Unlock()
	m.out.Write(append(b, '\n'))
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcError(id json.RawMessage, code int, msg string) *rpcReply {
	if id == nil {
		id = json.RawMessage("null")
	}
	return &rpcReply{JSONRPC: "2.0", ID: id, Error: &rpcErr{code, msg}}
}

// handle answers one message; notifications and replies get nil.
func (m *mcpServer) handle(raw []byte) *rpcReply {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcError(nil, -32700, "parse error")
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // a notification, or a reply to a request we never send
	}
	if req.Method == "" {
		return rpcError(req.ID, -32600, "invalid request")
	}
	result, err := m.call(req.Method, req.Params)
	if err != nil {
		var re *rpcErr
		if errors.As(err, &re) {
			return rpcError(req.ID, re.Code, re.Message)
		}
		return rpcError(req.ID, -32603, err.Error())
	}
	b, err := json.Marshal(result)
	if err != nil {
		return rpcError(req.ID, -32603, err.Error())
	}
	return &rpcReply{JSONRPC: "2.0", ID: req.ID, Result: b}
}

func (e *rpcErr) Error() string { return e.Message }

func (m *mcpServer) call(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		v := mcpProtocols[0]
		if slices.Contains(mcpProtocols, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "title": "trail", "version": m.version},
			"instructions": "trail reads the session logs that Claude Code, Codex, OpenCode, Gemini CLI and Kiro CLI keep on this machine. " +
				"Use search_sessions to find past work on a problem, list_sessions to see recent sessions in a project, and get_session to read one. " +
				"Secrets are redacted from everything trail returns.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcErr{-32602, "invalid params"}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := m.runTool(p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcErr{-32602, "unknown tool " + p.Name}
		}
		if err != nil {
			return toolResult(Redact(err.Error()), true), nil
		}
		return toolResult(Redact(text), false), nil
	}
	return nil, &rpcErr{-32601, "method not found: " + method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

/* ---------------- tools ---------------- */

var errUnknownTool = errors.New("unknown tool")

var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

var mcpTools = []map[string]any{
	{
		"name":  "search_sessions",
		"title": "Search past agent sessions",
		"description": "Full-text search over past coding-agent sessions on this machine (Claude Code, Codex, OpenCode, Gemini CLI, Kiro CLI): prompts, replies, tool calls and failed tool output. " +
			"Use it to find how a problem was solved before, why a change was made, or what was tried and failed. " +
			`Query syntax: plain words must all match; "quoted phrases"; in:prompt|reply|tool|output|thinking; tool:Bash; file:main.go; agent:claude. ` +
			"Results list sessions with their best matching steps; read more with get_session(id, from=step).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "What to look for"},
				"project": map[string]any{"type": "string", "description": `Only sessions in this project: an absolute path, "." for the current directory, or part of a folder name`},
				"limit":   map[string]any{"type": "integer", "description": "Sessions to return (default 10, at most 50)", "minimum": 1, "maximum": 50},
			},
			"required": []string{"query"},
		},
		"annotations": readOnly,
	},
	{
		"name":        "list_sessions",
		"title":       "List recent agent sessions",
		"description": "List past coding-agent sessions on this machine, most recent first, with their ids, projects, titles and edit counts.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": `Only sessions in this project: an absolute path, "." for the current directory, or part of a folder name`},
				"agent":   map[string]any{"type": "string", "description": "Only this agent: claude, codex, opencode, gemini or kiro"},
				"limit":   map[string]any{"type": "integer", "description": "Sessions to return (default 20, at most 100)", "minimum": 1, "maximum": 100},
			},
		},
		"annotations": readOnly,
	},
	{
		"name":        "get_session",
		"title":       "Read an agent session",
		"description": "Read one past session's transcript: prompts, replies and tool calls with their output, a page of steps at a time. Long text is shortened unless full is true.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":               map[string]any{"type": "string", "description": "Session id from search_sessions or list_sessions (provider:id, an id, or the start of one)"},
				"from":             map[string]any{"type": "integer", "description": "First step to show, counting from 1 (default 1)", "minimum": 1},
				"steps":            map[string]any{"type": "integer", "description": "Steps to show (default 40, at most 200)", "minimum": 1, "maximum": 200},
				"include_thinking": map[string]any{"type": "boolean", "description": "Also show the agent's reasoning steps"},
				"full":             map[string]any{"type": "boolean", "description": "Don't shorten long prompts, replies and tool output"},
			},
			"required": []string{"id"},
		},
		"annotations": readOnly,
	},
}

func (m *mcpServer) runTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case "search_sessions":
		var a struct {
			Query   string `json:"query"`
			Project string `json:"project"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
		return m.searchSessions(a.Query, a.Project, a.Limit)
	case "list_sessions":
		var a struct {
			Project string `json:"project"`
			Agent   string `json:"agent"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
		return m.listSessions(a.Project, a.Agent, a.Limit)
	case "get_session":
		var a struct {
			ID       string `json:"id"`
			From     int    `json:"from"`
			Steps    int    `json:"steps"`
			Thinking bool   `json:"include_thinking"`
			Full     bool   `json:"full"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
		return m.getSession(a.ID, a.From, a.Steps, a.Thinking, a.Full)
	}
	return "", errUnknownTool
}

func (m *mcpServer) searchSessions(query, project string, limit int) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", errors.New("query is empty")
	}
	limit = clampInt(limit, 10, 50)
	project = m.projectArg(project)
	src := m.source()
	want := limit
	if project != "" {
		want = 200 // filtered below
	}
	res, err := src.Search(query, want)
	if err != nil {
		return "", err
	}
	sessions, err := src.Sessions()
	if err != nil {
		return "", err
	}
	byKey := map[string]*Session{}
	for _, s := range sessions {
		byKey[s.Key] = s
	}
	var b strings.Builder
	n := 0
	more := res.More
	for _, sh := range res.Sessions {
		s := byKey[sh.Key]
		if s == nil || !inProject(s, project) {
			continue
		}
		if n == limit {
			more = true
			break
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n   id %s · %s · %s · %d matching %s\n", n, oneLine(s.Title, 100), s.Key, s.Root,
			s.Ended.Local().Format("2006-01-02 15:04"), sh.Count, plural(sh.Count, "step", "steps"))
		for _, h := range sh.Hits {
			var t strings.Builder
			for _, seg := range h.Snippet {
				t.WriteString(seg.Text)
			}
			where := h.Field
			if h.Tool != "" {
				where += " " + h.Tool
			}
			fmt.Fprintf(&b, "   step %d (%s): %s\n", h.Event+1, where, oneLine(Redact(t.String()), 220))
		}
	}
	var head strings.Builder
	if n == 0 {
		fmt.Fprintf(&head, "No sessions matched %q", query)
		if project != "" {
			fmt.Fprintf(&head, " in %s", project)
		}
		head.WriteString(".")
	} else {
		fmt.Fprintf(&head, "%d %s matched %q", n, plural(n, "session", "sessions"), query)
		if project != "" {
			fmt.Fprintf(&head, " in %s", project)
		}
		if more {
			head.WriteString(" (more matched; narrow the query or raise limit)")
		}
		head.WriteString(".")
	}
	if res.Building {
		fmt.Fprintf(&head, " The index is still being built: %d of %d sessions searched so far.", res.Indexed, res.Total)
	}
	head.WriteString("\n")
	if n > 0 {
		head.WriteString("\n")
		b.WriteString("\nRead a session with get_session(id, from=step).\n")
	}
	return head.String() + b.String(), nil
}

func (m *mcpServer) listSessions(project, agent string, limit int) (string, error) {
	limit = clampInt(limit, 20, 100)
	project = m.projectArg(project)
	sessions, err := m.source().Sessions()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n, total := 0, 0
	for _, s := range sessions {
		if !inProject(s, project) || (agent != "" && !strings.EqualFold(s.Provider, agent)) {
			continue
		}
		total++
		if n == limit {
			continue
		}
		n++
		fmt.Fprintf(&b, "%s  %s  %s\n    id %s · %s · %d %s, %d %s\n", s.Ended.Local().Format("2006-01-02 15:04"), s.Provider,
			oneLine(s.Title, 100), s.Key, s.Root, s.Messages, plural(s.Messages, "message", "messages"), s.Edits, plural(s.Edits, "edit", "edits"))
	}
	if total == 0 {
		return "No sessions found.", nil
	}
	head := fmt.Sprintf("%d of %d sessions, most recent first:\n\n", n, total)
	return head + b.String(), nil
}

const (
	mcpTextChars   = 4000
	mcpInputChars  = 1500
	mcpOutputChars = 2000
	mcpPageChars   = 60000 // a page stops early past this, so one call can't flood the context
)

func (m *mcpServer) getSession(id string, from, steps int, thinking, full bool) (string, error) {
	src := m.source()
	sessions, err := src.Sessions()
	if err != nil {
		return "", err
	}
	s, err := pickSession(sessions, id)
	if err != nil {
		return "", errors.New(strings.Replace(err.Error(), "trail -list prints session ids", "list_sessions or search_sessions give ids", 1))
	}
	t, err := src.Load(s.Key)
	if err != nil {
		return "", fmt.Errorf("session %s is no longer on disk", s.Key)
	}
	RedactTranscript(t)
	s = t.Session
	if from < 1 {
		from = 1
	}
	steps = clampInt(steps, 40, 200)
	fit := func(v string, n int) string {
		if full {
			return v
		}
		return clip(v, n)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", oneLine(s.Title, 120))
	fmt.Fprintf(&b, "id %s · agent %s", s.Key, s.Provider)
	if s.Model != "" {
		fmt.Fprintf(&b, " · model %s", s.Model)
	}
	fmt.Fprintf(&b, "\nproject %s", s.Root)
	if s.Cwd != "" && s.Cwd != s.Root {
		fmt.Fprintf(&b, " (ran in %s)", s.Cwd)
	}
	if s.Branch != "" {
		fmt.Fprintf(&b, " · branch %s", s.Branch)
	}
	fmt.Fprintf(&b, "\n%s to %s · %d steps · %d %s\n", s.Started.Local().Format("2006-01-02 15:04"), s.Ended.Local().Format("2006-01-02 15:04"),
		len(t.Events), s.Edits, plural(s.Edits, "edit", "edits"))
	if len(s.Files) > 0 {
		files := s.Files
		if len(files) > 30 {
			files = append(slices.Clone(files[:30]), fmt.Sprintf("and %d more", len(s.Files)-30))
		}
		fmt.Fprintf(&b, "files edited: %s\n", strings.Join(files, ", "))
	}
	if from > len(t.Events) {
		fmt.Fprintf(&b, "\nThe session has %d steps; from=%d is past the end.\n", len(t.Events), from)
		return b.String(), nil
	}

	end := min(from-1+steps, len(t.Events))
	shown, hidden := 0, 0
	var body strings.Builder
	last := from - 1
	for i := from - 1; i < end; i++ {
		if body.Len() > mcpPageChars && shown > 0 {
			break
		}
		last = i + 1
		e := t.Events[i]
		if e.Kind == "thinking" && !thinking {
			hidden++
			continue
		}
		shown++
		fmt.Fprintf(&body, "\n[%d] %s", i+1, e.Kind)
		if e.Tool != "" {
			fmt.Fprintf(&body, " %s", e.Tool)
		}
		if e.Summary != "" {
			fmt.Fprintf(&body, " · %s", oneLine(e.Summary, 160))
		}
		if e.Sidechain {
			body.WriteString(" (subagent)")
		}
		if e.Error {
			body.WriteString(" (failed)")
		}
		if !e.Time.IsZero() {
			fmt.Fprintf(&body, " · %s", e.Time.Local().Format("15:04:05"))
		}
		body.WriteString("\n")
		if e.Text != "" {
			body.WriteString(fit(e.Text, mcpTextChars) + "\n")
		}
		if e.Input != "" && e.Input != e.Summary {
			body.WriteString("input:\n" + fit(e.Input, mcpInputChars) + "\n")
		}
		if e.Output != "" {
			body.WriteString("output:\n" + fit(e.Output, mcpOutputChars) + "\n")
		}
		for _, c := range e.Changes {
			if c >= 0 && c < len(t.Changes) {
				fmt.Fprintf(&body, "%s %s\n", t.Changes[c].Op, t.Changes[c].Rel)
			}
		}
	}
	fmt.Fprintf(&b, "\nSteps %d to %d of %d", from, last, len(t.Events))
	if hidden > 0 {
		fmt.Fprintf(&b, " (%d reasoning %s hidden; pass include_thinking to see them)", hidden, plural(hidden, "step", "steps"))
	}
	b.WriteString(":\n")
	b.WriteString(body.String())
	if last < len(t.Events) {
		fmt.Fprintf(&b, "\nMore steps follow: get_session(id=%q, from=%d).\n", s.Key, last+1)
	}
	return b.String(), nil
}

func clampInt(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// projectArg turns "." into the directory the agent started trail in.
func (m *mcpServer) projectArg(p string) string {
	p = strings.TrimSpace(p)
	if p == "." || p == "./" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
	}
	return p
}

// inProject reports whether a session belongs to a project the agent named:
// an absolute path to the project or a folder inside it, or part of its name.
func inProject(s *Session, p string) bool {
	if p == "" {
		return true
	}
	if filepath.IsAbs(p) {
		p = filepath.Clean(p)
		for _, d := range []string{s.Root, s.Cwd} {
			if d != "" && (d == p || strings.HasPrefix(d, p+string(filepath.Separator)) || strings.HasPrefix(p, d+string(filepath.Separator))) {
				return true
			}
		}
		return false
	}
	return strings.Contains(strings.ToLower(s.Root), strings.ToLower(p))
}

/* ---------------- sources ---------------- */

// source picks where to read from. A running trail window is preferred until
// this process has built its own index; after that the local index is kept,
// since it's already paid for.
func (m *mcpServer) source() mcpSource {
	m.mu.Lock()
	local := m.useLocal
	m.mu.Unlock()
	if !local {
		if ui := m.findUI(); ui != nil {
			return &fallbackSource{ui: ui, m: m}
		}
		m.mu.Lock()
		m.useLocal = true
		m.mu.Unlock()
	}
	return m.local
}

// fallbackSource reads from a running window and switches to the local index
// for good if the window goes away mid-session.
type fallbackSource struct {
	ui *uiSource
	m  *mcpServer
}

func (f *fallbackSource) fall() mcpSource {
	f.m.mu.Lock()
	f.m.useLocal = true
	f.m.mu.Unlock()
	return f.m.local
}

func (f *fallbackSource) Sessions() ([]*Session, error) {
	if s, err := f.ui.Sessions(); err == nil {
		return s, nil
	}
	return f.fall().Sessions()
}

func (f *fallbackSource) Load(key string) (*Transcript, error) {
	t, err := f.ui.Load(key)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return t, err
	}
	return f.fall().Load(key)
}

func (f *fallbackSource) Search(q string, limit int) (SearchResult, error) {
	if r, err := f.ui.Search(q, limit); err == nil {
		return r, nil
	}
	return f.fall().Search(q, limit)
}

// localSource is this process's own index, built on first use.
type localSource struct {
	idx    *Index
	newSem func(*Search) *Semantic

	once sync.Once
	srch *Search
	sem  *Semantic
}

func (l *localSource) init() {
	l.once.Do(func() {
		l.srch = NewSearch(l.idx)
		if l.newSem != nil {
			l.sem = l.newSem(l.srch)
		}
	})
}

func (l *localSource) Sessions() ([]*Session, error) {
	l.idx.Scan(5 * time.Second)
	return l.idx.Sessions(), nil
}

func (l *localSource) Load(key string) (*Transcript, error) {
	l.idx.Scan(5 * time.Second)
	return l.idx.Load(key)
}

func (l *localSource) Search(q string, limit int) (SearchResult, error) {
	l.init()
	l.idx.Scan(5 * time.Second)
	l.srch.Sync()
	pq := ParseQuery(q + " ")
	res := l.srch.Run(pq, "", limit)
	if l.sem != nil {
		l.sem.Kick()
		res = l.sem.Blend(context.Background(), pq, res, "", limit)
	}
	return res, nil
}

// uiSource is a trail window running on this machine, reached over its API.
type uiSource struct {
	base   string
	client *http.Client
}

// uiAnnounce is what a running window writes so trail -mcp can find it.
type uiAnnounce struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

// uiFile is where a running window announces itself: beside the vector
// cache, outside any agent's storage.
func uiFile() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "trail", "ui.json")
}

// findUI returns a running window that reads the same agent folders as this
// process, or nil.
func (m *mcpServer) findUI() *uiSource {
	if m.uiPath == "" {
		return nil
	}
	b, err := os.ReadFile(m.uiPath)
	if err != nil {
		return nil
	}
	var a uiAnnounce
	if json.Unmarshal(b, &a) != nil || !isLoopback(a.URL) {
		return nil
	}
	ui := &uiSource{base: strings.TrimRight(a.URL, "/"), client: m.client}
	var ping struct {
		App     string   `json:"app"`
		Sources []string `json:"sources"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ui.get(ctx, "/api/ping", nil, &ping) != nil || ping.App != "trail" || !slices.Equal(ping.Sources, m.sources) {
		return nil
	}
	return ui
}

func (u *uiSource) get(ctx context.Context, path string, q url.Values, v any) error {
	target := u.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && path == "/api/session" {
		return os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("trail window: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (u *uiSource) Sessions() ([]*Session, error) {
	var r struct {
		Sessions []*Session `json:"sessions"`
	}
	err := u.get(context.Background(), "/api/sessions", nil, &r)
	return r.Sessions, err
}

func (u *uiSource) Load(key string) (*Transcript, error) {
	var t Transcript
	if err := u.get(context.Background(), "/api/session", url.Values{"id": {key}}, &t); err != nil {
		return nil, err
	}
	if t.Session == nil {
		return nil, os.ErrNotExist
	}
	return &t, nil
}

func (u *uiSource) Search(q string, limit int) (SearchResult, error) {
	// The trailing space: every word is a whole word, none a prefix still being typed.
	args := url.Values{"q": {q + " "}, "limit": {fmt.Sprint(limit)}}
	// A window answers from what it has read so far. Give it a while to
	// finish, since an agent wants every match, not the first few.
	deadline := time.Now().Add(uiSearchWait)
	for {
		var r SearchResult
		err := u.get(context.Background(), "/api/search", args, &r)
		if err != nil || !r.Building || time.Now().After(deadline) {
			return r, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const uiSearchWait = 20 * time.Second

// announceUI records a running window's address for trail -mcp, and returns
// a function that removes the record if it is still this window's.
func announceUI(addr string) func() {
	path := uiFile()
	if path == "" || !isLoopback(addr) {
		return func() {}
	}
	b, _ := json.Marshal(uiAnnounce{URL: addr, PID: os.Getpid()})
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, b, 0o600) != nil {
		return func() {}
	}
	return func() {
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b) {
			os.Remove(path)
		}
	}
}

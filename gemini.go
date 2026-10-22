package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// geminiProvider reads Gemini CLI chat recordings:
// <dir>/tmp/<project>/chats/session-<time>-<id>.jsonl
//
// <project> is a short name for the project folder; the folder itself is in
// <project>/.project_root, and in <dir>/projects.json. A session file is a
// metadata line followed by message records. A record is appended again
// whenever it changes (a tool call gaining its result), and later lines can
// patch, reorder or rewind earlier ones, so the file is replayed the way
// Gemini CLI replays it. Older versions wrote one JSON document per session
// (.json); resuming one copies it to a .jsonl next to it.
//
// Subagent runs are recorded under chats/<parent session id>/ and aren't
// listed on their own.
type geminiProvider struct{ dir string }

func (p *geminiProvider) Name() string { return "gemini" }
func (p *geminiProvider) Root() string { return filepath.Join(p.dir, "tmp") }

func (p *geminiProvider) Discover() ([]string, error) {
	projects, err := os.ReadDir(p.Root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		chats := filepath.Join(p.Root(), proj.Name(), "chats")
		files, err := os.ReadDir(chats)
		if err != nil {
			continue
		}
		have := map[string]bool{}
		for _, f := range files {
			have[f.Name()] = true
		}
		for _, f := range files {
			n := f.Name()
			if f.IsDir() || !strings.HasPrefix(n, "session-") {
				continue
			}
			// A legacy .json that was resumed lives on as the .jsonl.
			if strings.HasSuffix(n, ".jsonl") || (strings.HasSuffix(n, ".json") && !have[n+"l"]) {
				out = append(out, filepath.Join(chats, n))
			}
		}
	}
	return out, nil
}

// geminiMessage is one MessageRecord. Content is a string, a part, or a list
// of parts (the Gemini API's PartListUnion).
type geminiMessage struct {
	ID             string          `json:"id"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"` // user, gemini, info, error, warning
	Content        json.RawMessage `json:"content"`
	DisplayContent json.RawMessage `json:"displayContent"`
	Model          string          `json:"model"`
	Thoughts       []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
		Timestamp   string `json:"timestamp"`
	} `json:"thoughts"`
	ToolCalls []geminiToolCall `json:"toolCalls"`
	Tokens    *geminiTokens    `json:"tokens"`
}

// geminiTokens is Gemini CLI's TokensSummary of the API's usage metadata.
// input is the whole prompt, cached tokens included; thoughts and tool-use
// prompt tokens are counted apart from output and input.
type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
}

func (g *geminiTokens) step(model string) step {
	st := step{model: model}
	st.CacheRead = min(g.Cached, g.Input)
	st.Input = g.Input - st.CacheRead + g.Tool
	st.Output, st.Reasoning = g.Output+g.Thoughts, g.Thoughts
	return st
}

type geminiToolCall struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Args          json.RawMessage `json:"args"`
	Result        json.RawMessage `json:"result"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
	ResultDisplay json.RawMessage `json:"resultDisplay"`
}

type geminiMeta struct {
	SessionID   string          `json:"sessionId"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Summary     string          `json:"summary"`
	Messages    json.RawMessage `json:"messages"`
}

// geminiLog replays a session file into its current list of messages.
// Insertion order is kept the way a JavaScript Map keeps it: a message
// written again stays where it first appeared.
type geminiLog struct {
	meta  geminiMeta
	order []string
	msgs  map[string]*geminiMessage
}

func (l *geminiLog) put(m *geminiMessage) {
	if _, ok := l.msgs[m.ID]; !ok {
		l.order = append(l.order, m.ID)
	}
	l.msgs[m.ID] = m
}

func (l *geminiLog) reset() {
	l.order, l.msgs = nil, map[string]*geminiMessage{}
}

func (l *geminiLog) remove(ids map[string]bool) {
	kept := l.order[:0]
	for _, id := range l.order {
		if ids[id] {
			delete(l.msgs, id)
		} else {
			kept = append(kept, id)
		}
	}
	l.order = kept
}

func (l *geminiLog) putAll(raw json.RawMessage) {
	var msgs []*geminiMessage
	json.Unmarshal(raw, &msgs)
	for _, m := range msgs {
		if m != nil && m.ID != "" {
			l.put(m)
		}
	}
}

// line applies one JSONL record.
func (l *geminiLog) line(b []byte) {
	var rec struct {
		ID       *string         `json:"id"`
		RewindTo *string         `json:"$rewindTo"`
		Set      json.RawMessage `json:"$set"`
		Patch    *struct {
			geminiPatch
			Updates   []geminiPatch `json:"updates"`
			RemoveIDs []string      `json:"removeIds"`
			OrderIDs  []string      `json:"orderIds"`
		} `json:"$patch"`
		SessionID   string `json:"sessionId"`
		ProjectHash string `json:"projectHash"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return
	}
	switch {
	case rec.RewindTo != nil:
		// Drop the message and everything after it; an unknown id drops all.
		for i, id := range l.order {
			if id == *rec.RewindTo {
				drop := map[string]bool{}
				for _, d := range l.order[i:] {
					drop[d] = true
				}
				l.remove(drop)
				return
			}
		}
		l.reset()
	case rec.Patch != nil:
		pt := rec.Patch
		if pt.ID != "" {
			l.patch(pt.geminiPatch)
		}
		for _, u := range pt.Updates {
			l.patch(u)
		}
		if len(pt.RemoveIDs) > 0 {
			drop := map[string]bool{}
			for _, id := range pt.RemoveIDs {
				drop[id] = true
			}
			l.remove(drop)
		}
		if len(pt.OrderIDs) > 0 {
			// Listed messages move to the end, in the listed order.
			listed := map[string]bool{}
			var tail []string
			for _, id := range pt.OrderIDs {
				if _, ok := l.msgs[id]; ok && !listed[id] {
					listed[id] = true
					tail = append(tail, id)
				}
			}
			var head []string
			for _, id := range l.order {
				if !listed[id] {
					head = append(head, id)
				}
			}
			l.order = append(head, tail...)
		}
	case rec.ID != nil:
		var m geminiMessage
		if json.Unmarshal(b, &m) == nil {
			l.put(&m)
		}
	case len(rec.Set) > 0:
		var set geminiMeta
		json.Unmarshal(rec.Set, &set)
		if len(set.Messages) > 0 && set.Messages[0] == '[' {
			l.reset()
			l.putAll(set.Messages)
		}
		l.meta.merge(set)
	case rec.SessionID != "" && rec.ProjectHash != "":
		var meta geminiMeta
		json.Unmarshal(b, &meta)
		l.meta.merge(meta)
		if len(meta.Messages) > 0 {
			l.putAll(meta.Messages)
		}
	}
}

type geminiPatch struct {
	ID        string          `json:"id"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	} `json:"toolCalls"`
}

func (l *geminiLog) patch(pt geminiPatch) {
	m := l.msgs[pt.ID]
	if m == nil {
		return
	}
	if len(pt.Content) > 0 {
		m.Content = pt.Content
	}
	for _, tp := range pt.ToolCalls {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].ID == tp.ID && len(tp.Result) > 0 {
				m.ToolCalls[i].Result = tp.Result
			}
		}
	}
}

func (m *geminiMeta) merge(o geminiMeta) {
	if o.SessionID != "" {
		m.SessionID = o.SessionID
	}
	if o.StartTime != "" {
		m.StartTime = o.StartTime
	}
	if o.LastUpdated != "" {
		m.LastUpdated = o.LastUpdated
	}
	if o.Summary != "" {
		m.Summary = o.Summary
	}
}

func (p *geminiProvider) Parse(path string, full bool) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return p.ParseLines(f, path, full)
}

func (p *geminiProvider) ParseLines(r io.Reader, path string, full bool) (*Transcript, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	log := &geminiLog{msgs: map[string]*geminiMessage{}}
	if strings.HasSuffix(path, ".json") {
		// One JSON document: the whole ConversationRecord.
		var meta geminiMeta
		if err := json.Unmarshal(b, &meta); err != nil {
			return nil, err
		}
		log.meta = meta
		log.putAll(meta.Messages)
	} else {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) > 0 {
				log.line(sc.Bytes())
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}

	id := log.meta.SessionID
	if id == "" {
		// session-2026-10-22T14-05-1a2b3c4d.jsonl: only the id's first 8 characters.
		id = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".jsonl"), ".json")
	}
	t := &Transcript{Session: &Session{ID: id, Provider: "gemini", Path: path}}
	s := t.Session
	s.Cwd = geminiProjectRoot(p.dir, filepath.Dir(filepath.Dir(path)))
	t.touch(parseTime(log.meta.StartTime))
	t.touch(parseTime(log.meta.LastUpdated))
	if sum := strings.TrimSpace(log.meta.Summary); sum != "" {
		s.Title, s.TitleKind = oneLine(sum, 90), "generated"
	}

	for _, mid := range log.order {
		m := log.msgs[mid]
		ts := parseTime(m.Timestamp)
		t.touch(ts)
		switch m.Type {
		case "user":
			content := m.DisplayContent
			if len(content) == 0 || string(content) == "null" {
				content = m.Content
			}
			text := strings.TrimSpace(geminiText(content))
			if text == "" || strings.HasPrefix(text, "<session_context>") || strings.HasPrefix(text, "<hook_context>") {
				continue
			}
			s.Messages++
			if s.Prompt == "" && !strings.HasPrefix(text, "/") {
				s.Prompt = oneLine(text, promptChars)
			}
			if full {
				t.Events = append(t.Events, Event{Kind: "user", Time: ts, Text: text})
			}
		case "gemini":
			if m.Model != "" {
				s.Model = m.Model
			}
			if full {
				for _, th := range m.Thoughts {
					text := strings.TrimSpace(th.Subject)
					if d := strings.TrimSpace(th.Description); d != "" {
						if text != "" {
							text = "**" + text + "**\n\n" + d
						} else {
							text = d
						}
					}
					if text != "" {
						t.Events = append(t.Events, Event{Kind: "thinking", Time: parseTime(th.Timestamp), Text: text})
					}
				}
			}
			if text := strings.TrimSpace(geminiText(m.Content)); text != "" {
				s.Messages++
				if full {
					t.Events = append(t.Events, Event{Kind: "assistant", Time: ts, Text: text})
				}
			}
			for _, tc := range m.ToolCalls {
				cts := parseTime(tc.Timestamp)
				if cts.IsZero() {
					cts = ts
				}
				var changes []FileChange
				if tc.Status == "success" {
					changes = geminiChanges(tc, s.Cwd)
				}
				idx := -1
				if full {
					out, failed := geminiToolOutput(tc)
					t.Events = append(t.Events, Event{Kind: "tool", Time: cts, Tool: tc.Name,
						Summary: geminiToolSummary(tc.Name, tc.Args, s.Cwd),
						Input:   clip(geminiToolInput(tc.Name, tc.Args), maxToolText),
						Output:  clip(out, maxToolText), Error: failed})
					idx = len(t.Events) - 1
				}
				for _, c := range changes {
					c.Time = cts
					t.addChange(c, idx)
				}
			}
			if m.Tokens != nil {
				ev := -1
				if full {
					ev = len(t.Events) - 1
				}
				t.addUsage(m.Tokens.step(m.Model), ev)
			}
		}
	}
	for i := range t.Changes {
		t.Changes[i].Rel = relTo(s.Cwd, t.Changes[i].Path)
	}
	s.Resume = "gemini --resume " + id
	if s.Cwd != "" {
		s.Resume = "cd " + shellQuote(s.Cwd) + " && " + s.Resume
	}
	t.finish()
	return t, nil
}

// geminiProjectRoot finds the folder a project directory belongs to: the
// .project_root marker inside it, or its entry in projects.json. Older,
// hash-named project directories can't be traced back and return "".
func geminiProjectRoot(dir, projDir string) string {
	if b, err := os.ReadFile(filepath.Join(projDir, ".project_root")); err == nil {
		if root := strings.TrimSpace(string(b)); root != "" {
			return root
		}
	}
	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "projects.json")); err == nil && json.Unmarshal(b, &reg) == nil {
		slug := filepath.Base(projDir)
		for root, s := range reg.Projects {
			if s == slug {
				return root
			}
		}
	}
	return ""
}

// geminiPart is one part of Gemini API content.
type geminiPart struct {
	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	FunctionResponse *struct {
		Response map[string]any `json:"response"`
	} `json:"functionResponse"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
	} `json:"inlineData"`
}

func geminiParts(raw json.RawMessage) []geminiPart {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		json.Unmarshal(raw, &s)
		return []geminiPart{{Text: s}}
	case '{':
		var p geminiPart
		json.Unmarshal(raw, &p)
		return []geminiPart{p}
	case '[':
		// A list can mix strings and parts.
		var items []json.RawMessage
		json.Unmarshal(raw, &items)
		var out []geminiPart
		for _, it := range items {
			out = append(out, geminiParts(it)...)
		}
		return out
	}
	return nil
}

// geminiText joins the text parts of a message, leaving out thoughts.
func geminiText(raw json.RawMessage) string {
	var out []string
	for _, p := range geminiParts(raw) {
		switch {
		case p.Thought:
		case p.InlineData != nil:
			out = append(out, "["+p.InlineData.MimeType+"]")
		case strings.TrimSpace(p.Text) != "":
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// geminiToolOutput prefers what Gemini CLI showed the person (resultDisplay,
// when it is text) over what it sent back to the model.
func geminiToolOutput(tc geminiToolCall) (string, bool) {
	failed := tc.Status == "error" || tc.Status == "cancelled"
	var display string
	if json.Unmarshal(tc.ResultDisplay, &display) == nil && strings.TrimSpace(display) != "" {
		return display, failed
	}
	var out []string
	for _, p := range geminiParts(tc.Result) {
		if fr := p.FunctionResponse; fr != nil {
			for _, k := range []string{"output", "error"} {
				if v, ok := fr.Response[k].(string); ok && v != "" {
					out = append(out, v)
				}
			}
		} else if strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	if len(out) == 0 && tc.Status == "cancelled" {
		return "Cancelled", true
	}
	return strings.Join(out, "\n"), failed
}

// geminiChanges turns a completed replace or write_file call into file
// changes. write_file's result carries the file as it was before, so its diff
// is a real one, and a new file shows as added.
func geminiChanges(tc geminiToolCall, cwd string) []FileChange {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(tc.Args, &in) != nil || in.FilePath == "" {
		return nil
	}
	path := in.FilePath
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	switch tc.Name {
	case "replace":
		return []FileChange{{Path: path, Op: "edit", Hunks: []Hunk{{Old: in.OldString, New: in.NewString}}}}
	case "write_file":
		// originalContent is null for a file that didn't exist.
		var d struct {
			OriginalContent *string `json:"originalContent"`
			NewContent      *string `json:"newContent"`
		}
		var known map[string]json.RawMessage
		json.Unmarshal(tc.ResultDisplay, &d)
		json.Unmarshal(tc.ResultDisplay, &known)
		h := Hunk{New: in.Content}
		if d.NewContent != nil {
			h.New = *d.NewContent
		}
		switch _, recorded := known["originalContent"]; {
		case d.OriginalContent != nil:
			h.Old = *d.OriginalContent
			return []FileChange{{Path: path, Op: "edit", Hunks: []Hunk{h}}}
		case recorded:
			return []FileChange{{Path: path, Op: "add", Hunks: []Hunk{h}}}
		}
		return []FileChange{{Path: path, Op: "write", Hunks: []Hunk{h}}}
	}
	return nil
}

func geminiToolSummary(tool string, raw json.RawMessage, cwd string) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch {
	case str("file_path") != "":
		return relTo(cwd, str("file_path"))
	case str("command") != "":
		return oneLine(str("command"), 120)
	case str("dir_path") != "":
		return relTo(cwd, str("dir_path"))
	case str("pattern") != "":
		return str("pattern")
	case str("query") != "":
		return oneLine(str("query"), 120)
	case str("prompt") != "":
		return oneLine(str("prompt"), 120)
	case str("description") != "":
		return oneLine(str("description"), 120)
	case str("name") != "":
		return str("name")
	}
	return ""
}

func geminiToolInput(tool string, raw json.RawMessage) string {
	if tool == "run_shell_command" {
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &in) == nil && in.Command != "" {
			return "$ " + in.Command
		}
	}
	if len(raw) == 0 {
		return ""
	}
	return prettyJSON(raw)
}

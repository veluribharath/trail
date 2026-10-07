package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// claudeProvider reads Claude Code transcripts:
// <dir>/projects/<encoded-cwd>/<session-id>.jsonl
type claudeProvider struct{ dir string }

func (p *claudeProvider) Name() string { return "claude" }
func (p *claudeProvider) Root() string { return filepath.Join(p.dir, "projects") }

func (p *claudeProvider) Discover() ([]string, error) {
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
		files, err := os.ReadDir(filepath.Join(p.Root(), proj.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			n := f.Name()
			// agent-*.jsonl are subagent transcripts from older versions; they
			// are shown inside their parent session as sidechain events.
			if f.IsDir() || !strings.HasSuffix(n, ".jsonl") || strings.HasPrefix(n, "agent-") {
				continue
			}
			out = append(out, filepath.Join(p.Root(), proj.Name(), n))
		}
	}
	return out, nil
}

type claudeLine struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	UUID        string          `json:"uuid"`
	Message     json.RawMessage `json:"message"`

	// Session names. /rename writes custom-title; Claude Code names sessions
	// on its own with ai-title; versions before that wrote summary lines.
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	Summary     string `json:"summary"`
	LeafUUID    string `json:"leafUuid"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (p *claudeProvider) Parse(path string, full bool) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	t := &Transcript{Session: &Session{ID: id, Provider: "claude", Path: path}}
	s := t.Session
	calls := map[string]int{} // tool_use id -> event index
	var custom, generated string
	uuids := map[string]bool{}
	type summary struct{ leaf, text string }
	var summaries []summary

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ln claudeLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if ln.Cwd != "" && s.Cwd == "" {
			s.Cwd = ln.Cwd
		}
		if ln.GitBranch != "" && ln.GitBranch != "HEAD" {
			s.Branch = ln.GitBranch
		}
		if ln.UUID != "" {
			uuids[ln.UUID] = true
		}
		// A title line belongs to the session it names; skip any that name
		// another session. The latest line of each kind wins.
		ownLine := ln.SessionID == "" || ln.SessionID == id
		switch ln.Type {
		case "custom-title":
			if ownLine && strings.TrimSpace(ln.CustomTitle) != "" {
				custom = ln.CustomTitle
			}
			continue
		case "ai-title":
			if ownLine && strings.TrimSpace(ln.AITitle) != "" {
				generated = ln.AITitle
			}
			continue
		case "summary":
			if strings.TrimSpace(ln.Summary) != "" {
				summaries = append(summaries, summary{ln.LeafUUID, ln.Summary})
			}
			continue
		case "user", "assistant":
		default:
			continue
		}
		if ln.IsMeta || len(ln.Message) == 0 {
			continue
		}
		ts := parseTime(ln.Timestamp)
		t.touch(ts)

		var msg claudeMessage
		if json.Unmarshal(ln.Message, &msg) != nil {
			continue
		}
		if msg.Model != "" && !strings.HasPrefix(msg.Model, "<") {
			s.Model = msg.Model
		}

		// Content is either a plain string or a list of blocks.
		var blocks []claudeBlock
		if len(msg.Content) > 0 && msg.Content[0] == '"' {
			var text string
			json.Unmarshal(msg.Content, &text)
			blocks = []claudeBlock{{Type: "text", Text: text}}
		} else {
			json.Unmarshal(msg.Content, &blocks)
		}

		for _, b := range blocks {
			switch b.Type {
			case "text":
				text := strings.TrimSpace(b.Text)
				if text == "" {
					continue
				}
				if ln.Type == "user" {
					if isClaudeNoise(text) {
						continue
					}
					text = cleanClaudeCommand(text)
					if s.Prompt == "" && !ln.IsSidechain {
						s.Prompt = oneLine(text, promptChars)
					}
				}
				s.Messages++
				if full {
					t.Events = append(t.Events, Event{Kind: ln.Type, Time: ts, Text: text, Sidechain: ln.IsSidechain})
				}
			case "thinking":
				if full && strings.TrimSpace(b.Thinking) != "" {
					t.Events = append(t.Events, Event{Kind: "thinking", Time: ts, Text: b.Thinking, Sidechain: ln.IsSidechain})
				}
			case "tool_use":
				changes := claudeChanges(b.Name, b.Input)
				if !full {
					// Summary only needs the change count and file list.
					for _, c := range changes {
						t.addChange(c, -1)
					}
					continue
				}
				ev := Event{Kind: "tool", Time: ts, Tool: b.Name, Sidechain: ln.IsSidechain,
					Summary: claudeToolSummary(b.Name, b.Input, s.Cwd), Input: clip(claudeToolInput(b.Name, b.Input), maxToolText)}
				t.Events = append(t.Events, ev)
				idx := len(t.Events) - 1
				calls[b.ID] = idx
				for _, c := range changes {
					c.Time = ts
					t.addChange(c, idx)
				}
			case "tool_result":
				if !full {
					continue
				}
				if idx, ok := calls[b.ToolUseID]; ok {
					t.Events[idx].Output = clip(blockText(b.Content), maxToolText)
					t.Events[idx].Error = b.IsError
				}
			}
		}
	}
	if s.Cwd == "" {
		s.Cwd = decodeClaudeDir(filepath.Base(filepath.Dir(path)))
	}
	// Changes recorded during the scan used the cwd known at that moment;
	// recompute now that it is settled.
	for i := range t.Changes {
		t.Changes[i].Rel = relTo(s.Cwd, t.Changes[i].Path)
	}
	s.Resume = "cd " + shellQuote(s.Cwd) + " && claude --resume " + id

	// A name you gave beats one Claude Code generated, which beats an old
	// summary. Older versions sometimes wrote summaries of one conversation
	// into another's file, so a summary only counts if its leaf message is in
	// this file.
	switch {
	case custom != "":
		s.Title, s.TitleKind = oneLine(custom, 90), "renamed"
	case generated != "":
		s.Title, s.TitleKind = oneLine(generated, 90), "generated"
	default:
		for i := len(summaries) - 1; i >= 0; i-- {
			if summaries[i].leaf == "" || uuids[summaries[i].leaf] {
				s.Title, s.TitleKind = oneLine(summaries[i].text, 90), "summary"
				break
			}
		}
	}
	t.finish()
	return t, sc.Err()
}

// claudeChanges turns an editing tool call into file changes.
func claudeChanges(tool string, raw json.RawMessage) []FileChange {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(raw, &in) != nil || in.FilePath == "" {
		return nil
	}
	switch tool {
	case "Edit":
		return []FileChange{{Path: in.FilePath, Op: "edit", Hunks: []Hunk{{Old: in.OldString, New: in.NewString}}}}
	case "MultiEdit":
		c := FileChange{Path: in.FilePath, Op: "edit"}
		for _, e := range in.Edits {
			c.Hunks = append(c.Hunks, Hunk{Old: e.OldString, New: e.NewString})
		}
		return []FileChange{c}
	case "Write":
		return []FileChange{{Path: in.FilePath, Op: "write", Hunks: []Hunk{{New: in.Content}}}}
	}
	return nil
}

func claudeToolSummary(tool string, raw json.RawMessage, cwd string) string {
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
	case str("subject") != "":
		return oneLine(str("subject"), 120)
	case str("skill") != "":
		return str("skill")
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		if v, ok := in[keys[0]].(string); ok {
			return oneLine(v, 120)
		}
	}
	return ""
}

// claudeToolInput shows a shell command as the command itself; everything
// else as indented JSON.
func claudeToolInput(tool string, raw json.RawMessage) string {
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &in) == nil && in.Command != "" {
			return "$ " + in.Command
		}
	}
	return prettyJSON(raw)
}

// isClaudeNoise reports user-role text that the harness injected rather than
// the person typing it.
func isClaudeNoise(text string) bool {
	for _, p := range []string{"<system-reminder>", "<local-command-stdout>", "<local-command-stderr>",
		"Caveat: The messages below", "[Request interrupted", "<command-message>"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// cleanClaudeCommand turns "<command-name>/foo</command-name><command-args>x</command-args>"
// into "/foo x".
func cleanClaudeCommand(text string) string {
	if !strings.Contains(text, "<command-name>") {
		return text
	}
	name := between(text, "<command-name>", "</command-name>")
	args := between(text, "<command-args>", "</command-args>")
	if name == "" {
		return text
	}
	return strings.TrimSpace(name + " " + args)
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

// blockText flattens a tool_result content field (string or text blocks).
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "image" {
			b.WriteString("[image]\n")
			continue
		}
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// decodeClaudeDir is a lossy fallback for sessions with no cwd recorded:
// Claude Code names project folders by replacing path separators with "-".
func decodeClaudeDir(name string) string {
	if strings.HasPrefix(name, "-") {
		return "/" + strings.ReplaceAll(name[1:], "-", "/")
	}
	return name
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !(r == '/' || r == '.' || r == '_' || r == '-' || r == '~' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

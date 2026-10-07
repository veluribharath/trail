package main

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// codexProvider reads Codex CLI rollouts:
// <dir>/sessions/YYYY/MM/DD/rollout-<timestamp>-<id>.jsonl
type codexProvider struct {
	dir string

	namesMu   sync.Mutex
	namesSize int64
	namesMod  time.Time
	names     map[string]string
}

// SessionNames reads <dir>/session_index.jsonl, where Codex records thread
// names: one {"id", "thread_name", "updated_at"} line per rename, appended,
// with the latest non-empty name for an id winning. The rollout itself never
// carries the name. The file is re-read only when it changes.
func (p *codexProvider) SessionNames() map[string]string {
	p.namesMu.Lock()
	defer p.namesMu.Unlock()
	path := filepath.Join(p.dir, "session_index.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		p.names, p.namesSize, p.namesMod = nil, 0, time.Time{}
		return nil
	}
	if p.names != nil && fi.Size() == p.namesSize && fi.ModTime().Equal(p.namesMod) {
		return p.names
	}
	f, err := os.Open(path)
	if err != nil {
		return p.names
	}
	defer f.Close()
	names := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		if name := strings.TrimSpace(e.ThreadName); name != "" {
			names[e.ID] = name
		}
	}
	p.names, p.namesSize, p.namesMod = names, fi.Size(), fi.ModTime()
	return names
}

func (p *codexProvider) Name() string { return "codex" }
func (p *codexProvider) Root() string { return filepath.Join(p.dir, "sessions") }

func (p *codexProvider) Discover() ([]string, error) {
	var out []string
	err := filepath.WalkDir(p.Root(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	// Legacy rollouts (before the typed envelope) put the session meta on the
	// first line and response items directly on later lines.
	ID string `json:"id"`
}

type codexItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Message   string          `json:"message"` // event_msg user_message / agent_message
	// session_meta / turn_context
	ID    string `json:"id"`
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
	Git   struct {
		Branch string `json:"branch"`
	} `json:"git"`
	Timestamp string `json:"timestamp"`
}

func (p *codexProvider) Parse(path string, full bool) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := &Transcript{Session: &Session{Provider: "codex", Path: path}}
	s := t.Session
	calls := map[string]int{}
	var fallback []Event // event_msg messages, used only if no response items carry text
	fallbackPrompt := ""
	sawItems := false

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	first := true
	for sc.Scan() {
		var ln codexLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		var it codexItem
		kind := ln.Type
		if len(ln.Payload) > 0 {
			json.Unmarshal(ln.Payload, &it)
		} else {
			json.Unmarshal(sc.Bytes(), &it)
			switch {
			case first && ln.ID != "":
				kind = "session_meta"
			case it.Type != "":
				kind = "response_item"
			}
		}
		first = false
		ts := parseTime(ln.Timestamp)
		if ts.IsZero() {
			ts = parseTime(it.Timestamp)
		}

		switch kind {
		case "session_meta":
			if it.ID != "" {
				s.ID = it.ID
			}
			if it.Cwd != "" {
				s.Cwd = it.Cwd
			}
			if it.Git.Branch != "" {
				s.Branch = it.Git.Branch
			}
			t.touch(ts)
			continue
		case "turn_context":
			if it.Model != "" {
				s.Model = it.Model
			}
			if it.Cwd != "" && s.Cwd == "" {
				s.Cwd = it.Cwd
			}
			continue
		case "event_msg":
			if it.Type == "user_message" || it.Type == "agent_message" {
				role := "user"
				if it.Type == "agent_message" {
					role = "assistant"
				}
				text := strings.TrimSpace(it.Message)
				if text != "" && !isCodexNoise(text) {
					if role == "user" && fallbackPrompt == "" {
						fallbackPrompt = oneLine(text, promptChars)
					}
					fallback = append(fallback, Event{Kind: role, Time: ts, Text: text})
				}
			}
			continue
		case "response_item":
		default:
			continue
		}
		t.touch(ts)

		switch it.Type {
		case "message":
			if it.Role != "user" && it.Role != "assistant" {
				continue
			}
			text := strings.TrimSpace(codexText(it.Content))
			if text == "" || (it.Role == "user" && isCodexNoise(text)) {
				continue
			}
			sawItems = true
			s.Messages++
			if it.Role == "user" && s.Prompt == "" {
				s.Prompt = oneLine(text, promptChars)
			}
			if full {
				t.Events = append(t.Events, Event{Kind: it.Role, Time: ts, Text: text})
			}
		case "reasoning":
			if full {
				if text := strings.TrimSpace(codexText(it.Summary)); text != "" {
					t.Events = append(t.Events, Event{Kind: "thinking", Time: ts, Text: text})
				}
			}
		case "function_call", "custom_tool_call", "local_shell_call":
			name, input, summary, patch := codexCall(it)
			changes := parsePatch(patch, s.Cwd)
			if !full {
				for _, c := range changes {
					t.addChange(c, -1)
				}
				continue
			}
			t.Events = append(t.Events, Event{Kind: "tool", Time: ts, Tool: name, Summary: summary,
				Input: clip(input, maxToolText), callID: it.CallID})
			idx := len(t.Events) - 1
			if it.CallID != "" {
				calls[it.CallID] = idx
			}
			for _, c := range changes {
				c.Time = ts
				t.addChange(c, idx)
			}
		case "function_call_output", "custom_tool_call_output":
			if !full {
				continue
			}
			if idx, ok := calls[it.CallID]; ok {
				out, failed := codexOutput(it.Output)
				t.Events[idx].Output = clip(out, maxToolText)
				t.Events[idx].Error = failed
			}
		}
	}

	if !sawItems {
		s.Prompt = fallbackPrompt
		for _, e := range fallback {
			s.Messages++
			if full {
				t.Events = append(t.Events, e)
			}
		}
	}
	if s.ID == "" {
		// rollout-2025-01-02T03-04-05-<uuid>.jsonl
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if len(base) >= 36 {
			s.ID = base[len(base)-36:]
		} else {
			s.ID = base
		}
	}
	for i := range t.Changes {
		t.Changes[i].Rel = relTo(s.Cwd, t.Changes[i].Path)
	}
	s.Resume = "codex resume " + s.ID
	if s.Cwd != "" {
		s.Resume = "cd " + shellQuote(s.Cwd) + " && " + s.Resume
	}
	t.finish()
	return t, sc.Err()
}

func isCodexNoise(text string) bool {
	for _, p := range []string{"<environment_context>", "<user_instructions>", "# AGENTS.md", "<permissions",
		"<user_shell_command>", "<turn_aborted>"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// codexText flattens message content or reasoning summary parts.
func codexText(raw json.RawMessage) string {
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
	json.Unmarshal(raw, &parts)
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n\n")
}

// codexCall extracts a display name, raw input, one-line summary and any
// apply_patch body from a tool call item.
func codexCall(it codexItem) (name, input, summary, patch string) {
	name = it.Name
	switch it.Type {
	case "custom_tool_call":
		input = it.Input
		if name == "apply_patch" {
			patch = it.Input
			summary = patchSummary(patch)
		} else {
			summary = oneLine(it.Input, 120)
		}
		return
	case "local_shell_call":
		name = "shell"
		var a struct {
			Command []string `json:"command"`
		}
		json.Unmarshal(it.Action, &a)
		input = strings.Join(a.Command, " ")
		summary, patch = shellSummary(a.Command)
		return
	}
	input = it.Arguments
	var args struct {
		Command json.RawMessage `json:"command"`
		Cmd     string          `json:"cmd"`
		Input   string          `json:"input"`
	}
	if json.Unmarshal([]byte(it.Arguments), &args) == nil {
		var argv []string
		if len(args.Command) > 0 {
			if args.Command[0] == '[' {
				json.Unmarshal(args.Command, &argv)
			} else {
				var one string
				json.Unmarshal(args.Command, &one)
				argv = []string{one}
			}
		} else if args.Cmd != "" {
			argv = []string{args.Cmd}
		}
		if len(argv) > 0 {
			input = strings.Join(argv, " ")
			summary, patch = shellSummary(argv)
			return
		}
		if name == "apply_patch" && args.Input != "" {
			patch = args.Input
			input = patch
			summary = patchSummary(patch)
			return
		}
	}
	summary = oneLine(it.Arguments, 120)
	return
}

// shellSummary recognises apply_patch invoked through the shell tool, either
// as ["apply_patch", "<patch>"] or a heredoc inside "bash -lc".
func shellSummary(argv []string) (summary, patch string) {
	if len(argv) >= 2 && argv[0] == "apply_patch" {
		return patchSummary(argv[1]), argv[1]
	}
	joined := strings.Join(argv, " ")
	if i := strings.Index(joined, "*** Begin Patch"); i >= 0 && strings.Contains(joined, "apply_patch") {
		body := joined[i:]
		if j := strings.Index(body, "*** End Patch"); j >= 0 {
			body = body[:j+len("*** End Patch")]
		}
		return patchSummary(body), body
	}
	cmd := joined
	if len(argv) == 3 && (argv[0] == "bash" || argv[0] == "zsh" || argv[0] == "sh") && strings.HasPrefix(argv[1], "-") {
		cmd = argv[2]
	}
	return oneLine(cmd, 120), ""
}

func patchSummary(patch string) string {
	var files []string
	for _, l := range strings.Split(patch, "\n") {
		for _, p := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if strings.HasPrefix(l, p) {
				files = append(files, strings.TrimSpace(l[len(p):]))
			}
		}
	}
	return oneLine(strings.Join(files, ", "), 120)
}

func codexOutput(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		json.Unmarshal(raw, &text)
	} else {
		text = codexText(raw)
		if text == "" {
			text = string(raw)
		}
	}
	// Older rollouts wrap shell output as {"output": "...", "metadata": {"exit_code": N}}.
	var wrapped struct {
		Output   string `json:"output"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal([]byte(text), &wrapped) == nil && wrapped.Metadata.ExitCode != nil {
		return wrapped.Output, *wrapped.Metadata.ExitCode != 0
	}
	return text, false
}

// parsePatch reads the Codex apply_patch format into file changes. Each "@@"
// section becomes one hunk: context and removed lines form Old, context and
// added lines form New.
func parsePatch(patch, cwd string) []FileChange {
	if !strings.Contains(patch, "*** Begin Patch") {
		return nil
	}
	var out []FileChange
	var cur *FileChange
	var oldB, newB strings.Builder
	flushHunk := func() {
		if cur != nil && (oldB.Len() > 0 || newB.Len() > 0) {
			cur.Hunks = append(cur.Hunks, Hunk{Old: strings.TrimSuffix(oldB.String(), "\n"), New: strings.TrimSuffix(newB.String(), "\n")})
		}
		oldB.Reset()
		newB.Reset()
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	abs := func(p string) string {
		p = strings.TrimSpace(p)
		if filepath.IsAbs(p) || cwd == "" {
			return p
		}
		return filepath.Join(cwd, p)
	}
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "*** Update File: "):
			flushFile()
			cur = &FileChange{Path: abs(l[len("*** Update File: "):]), Op: "update"}
		case strings.HasPrefix(l, "*** Add File: "):
			flushFile()
			cur = &FileChange{Path: abs(l[len("*** Add File: "):]), Op: "add"}
		case strings.HasPrefix(l, "*** Delete File: "):
			flushFile()
			out = append(out, FileChange{Path: abs(l[len("*** Delete File: "):]), Op: "delete"})
		case strings.HasPrefix(l, "*** Move to: "):
			if cur != nil {
				cur.Op = "move"
				cur.MoveTo = abs(l[len("*** Move to: "):])
			}
		case strings.HasPrefix(l, "*** End Patch"):
			flushFile()
		case strings.HasPrefix(l, "***"):
			// Begin Patch, End of File
		case strings.HasPrefix(l, "@@"):
			flushHunk()
		case cur == nil:
		case strings.HasPrefix(l, "+"):
			newB.WriteString(l[1:] + "\n")
		case strings.HasPrefix(l, "-"):
			oldB.WriteString(l[1:] + "\n")
		case strings.HasPrefix(l, " "):
			oldB.WriteString(l[1:] + "\n")
			newB.WriteString(l[1:] + "\n")
		case l == "":
			if cur.Op != "add" {
				oldB.WriteString("\n")
				newB.WriteString("\n")
			}
		}
	}
	flushFile()
	return out
}

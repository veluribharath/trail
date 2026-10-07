package main

import (
	"path/filepath"
	"strings"
	"time"
)

// Provider is one coding agent whose session logs trail can read. Adapters are
// read-only: they never write to the agent's storage.
type Provider interface {
	// Name is the stable id used in session ids and the UI ("claude", "codex").
	Name() string
	// Root is the directory the provider reads from, shown in the UI.
	Root() string
	// Discover lists session files. A missing root is not an error.
	Discover() ([]string, error)
	// Parse reads one session file. When full is false only the summary
	// fields are needed, so adapters may skip building events.
	Parse(path string, full bool) (*Transcript, error)
}

// sessionNamer is implemented by providers that keep session names outside
// the session files, keyed by session id. It is called on every scan, so it
// should be cheap when nothing changed.
type sessionNamer interface {
	SessionNames() map[string]string
}

// Session is the summary row shown in the folder tree.
type Session struct {
	Key      string `json:"key"` // provider:id, unique across providers
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Path     string `json:"-"`
	Cwd      string `json:"cwd"`
	Root     string `json:"root"`               // repo root (or cwd) the session is filed under
	Worktree string `json:"worktree,omitempty"` // set when cwd is a linked git worktree
	Branch   string `json:"branch,omitempty"`
	Model    string `json:"model,omitempty"`
	Title    string `json:"title"`
	// TitleKind says where Title came from: "renamed" (the person named it),
	// "generated" (the agent named it), "summary" (an older Claude Code
	// summary line), or "prompt" (no name; Title is the first prompt).
	TitleKind string `json:"titleKind"`
	Prompt    string `json:"prompt,omitempty"` // first real user message, trimmed

	// The name found in the session file itself. Names kept outside the file
	// (Codex's session index) are layered on top by applyName, so a rename
	// shows up without re-parsing the transcript.
	fileTitle, fileKind string
	Started             time.Time `json:"started"`
	Ended               time.Time `json:"ended"`
	Messages            int       `json:"messages"` // user + assistant text turns
	Edits               int       `json:"edits"`    // file changes recorded in the transcript
	Files               []string  `json:"files,omitempty"`
	Resume              string    `json:"resume,omitempty"`
}

// Transcript is a session with its full event stream.
type Transcript struct {
	Session *Session     `json:"session"`
	Events  []Event      `json:"events"`
	Changes []FileChange `json:"changes"`
}

// Event is one entry in the transcript.
type Event struct {
	Kind      string    `json:"kind"` // user, assistant, thinking, tool
	Time      time.Time `json:"time,omitempty"`
	Text      string    `json:"text,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Summary   string    `json:"summary,omitempty"` // one-line description of a tool call
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     bool      `json:"error,omitempty"`
	Sidechain bool      `json:"sidechain,omitempty"` // subagent work inside the session
	Changes   []int     `json:"changes,omitempty"`   // indices into Transcript.Changes
	callID    string
}

// FileChange is an edit the agent made, as recorded in its own transcript.
type FileChange struct {
	Path   string    `json:"path"`
	Rel    string    `json:"rel"`
	Op     string    `json:"op"` // edit, write, add, delete, update, move
	MoveTo string    `json:"moveTo,omitempty"`
	Time   time.Time `json:"time,omitempty"`
	Event  int       `json:"event"`
	Hunks  []Hunk    `json:"hunks"`
}

// Hunk is a before/after pair. The UI computes the line diff.
type Hunk struct {
	Old string `json:"old"`
	New string `json:"new"`
}

const (
	maxToolText = 24 << 10 // cap on stored tool input/output per event
	promptChars = 280
)

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (" + itoa(len(s)-cut) + " more bytes)"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// oneLine collapses whitespace and trims to n runes, for titles and summaries.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

func relTo(base, p string) string {
	if base == "" || !filepath.IsAbs(p) {
		return p
	}
	if r, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}

// finish fills the derived summary fields once events and changes are known.
func (t *Transcript) finish() {
	// Always send lists, never null: a session with no edits is common.
	if t.Events == nil {
		t.Events = []Event{}
	}
	if t.Changes == nil {
		t.Changes = []FileChange{}
	}
	s := t.Session
	s.Edits = len(t.Changes)
	seen := map[string]bool{}
	s.Files = s.Files[:0]
	for _, c := range t.Changes {
		if !seen[c.Rel] {
			seen[c.Rel] = true
			s.Files = append(s.Files, c.Rel)
		}
	}
	s.fileTitle, s.fileKind = s.Title, s.TitleKind
	s.applyName("")
}

// applyName sets the displayed title. An external name (from an index kept
// outside the session file) wins; then the name in the file; then the first
// prompt.
func (s *Session) applyName(external string) {
	switch {
	case strings.TrimSpace(external) != "":
		s.Title, s.TitleKind = oneLine(external, 90), "renamed"
	case s.fileTitle != "":
		s.Title, s.TitleKind = s.fileTitle, s.fileKind
	case s.Prompt != "":
		s.Title, s.TitleKind = oneLine(s.Prompt, 90), "prompt"
	default:
		s.Title, s.TitleKind = "Untitled session", "prompt"
	}
}

func (t *Transcript) addChange(c FileChange, ev int) {
	c.Event = ev
	c.Rel = relTo(t.Session.Cwd, c.Path)
	if c.Time.IsZero() && ev >= 0 && ev < len(t.Events) {
		c.Time = t.Events[ev].Time
	}
	t.Changes = append(t.Changes, c)
	if ev >= 0 && ev < len(t.Events) {
		t.Events[ev].Changes = append(t.Events[ev].Changes, len(t.Changes)-1)
	}
}

func (t *Transcript) touch(ts time.Time) {
	if ts.IsZero() {
		return
	}
	s := t.Session
	if s.Started.IsZero() || ts.Before(s.Started) {
		s.Started = ts
	}
	if ts.After(s.Ended) {
		s.Ended = ts
	}
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

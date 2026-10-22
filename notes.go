package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Notes are what the person writes about sessions: a bookmark on a session or
// on one of its steps, and a note on either. They live in one file of trail's
// own, next to prices.json in the user config directory, never in agent
// storage. They are the person's writing rather than something trail can
// rebuild, so they sit outside the cache directory and -forget leaves them.

// SessionNotes is what the person kept about one session, keyed by the
// session's provider:id, which doesn't change when the agent appends to it.
type SessionNotes struct {
	Bookmarked bool       `json:"bookmarked,omitempty"`
	Note       string     `json:"note,omitempty"`
	Steps      []StepMark `json:"steps,omitempty"` // sorted by Event
	Updated    time.Time  `json:"updated"`
}

// StepMark is a bookmarked step. Label is a line of the step taken when it
// was marked, so a list of bookmarks reads without loading the transcript.
type StepMark struct {
	Event int    `json:"event"`
	Label string `json:"label,omitempty"`
	Note  string `json:"note,omitempty"`
}

const (
	maxNoteLen  = 20000 // characters in one note
	maxLabelLen = 200
	maxSteps    = 1000 // marked steps in one session
)

func (n *SessionNotes) empty() bool {
	return !n.Bookmarked && strings.TrimSpace(n.Note) == "" && len(n.Steps) == 0
}

// clean checks what the page sent and puts it in the stored form.
func (n *SessionNotes) clean() error {
	if utf8.RuneCountInString(n.Note) > maxNoteLen {
		return fmt.Errorf("a note can be at most %d characters", maxNoteLen)
	}
	if strings.TrimSpace(n.Note) == "" {
		n.Note = ""
	}
	if len(n.Steps) > maxSteps {
		return fmt.Errorf("at most %d steps can be bookmarked in one session", maxSteps)
	}
	seen := map[int]bool{}
	steps := n.Steps[:0]
	for _, m := range n.Steps {
		if m.Event < 0 {
			return errors.New("a bookmarked step has a negative position")
		}
		if seen[m.Event] {
			continue
		}
		seen[m.Event] = true
		if utf8.RuneCountInString(m.Note) > maxNoteLen {
			return fmt.Errorf("a note can be at most %d characters", maxNoteLen)
		}
		if strings.TrimSpace(m.Note) == "" {
			m.Note = ""
		}
		m.Label = oneLine(m.Label, maxLabelLen)
		steps = append(steps, m)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Event < steps[j].Event })
	n.Steps = steps
	if len(n.Steps) == 0 {
		n.Steps = nil
	}
	return nil
}

type notesFile struct {
	Version  int                      `json:"version"`
	Sessions map[string]*SessionNotes `json:"sessions"`
}

// NoteStore reads and writes the notes file. Every write reads the file
// again first, so two trail processes don't undo each other's notes.
type NoteStore struct {
	path string // "" keeps notes in memory only

	mu    sync.Mutex
	all   map[string]*SessionNotes
	stamp string // size and mtime of the file when it was last read
}

func NewNoteStore(path string) *NoteStore {
	return &NoteStore{path: path, all: map[string]*SessionNotes{}}
}

// notesPath is where notes are kept: trail's own directory under the user
// config directory, next to prices.json.
func notesPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "trail", "notes.json")
}

// refresh rereads the file if it changed since the last read. Callers hold mu.
func (s *NoteStore) refresh() error {
	if s.path == "" {
		return nil
	}
	fi, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		s.all, s.stamp = map[string]*SessionNotes{}, ""
		return nil
	}
	if err != nil {
		return err
	}
	stamp := fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	if stamp == s.stamp {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f notesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s isn't valid JSON: %w", s.path, err)
	}
	if f.Sessions == nil {
		f.Sessions = map[string]*SessionNotes{}
	}
	s.all, s.stamp = f.Sessions, stamp
	return nil
}

// All returns every session's notes. The map and its values are copies. A
// nil store has none.
func (s *NoteStore) All() (map[string]*SessionNotes, error) {
	if s == nil {
		return map[string]*SessionNotes{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.refresh()
	out := make(map[string]*SessionNotes, len(s.all))
	for k, n := range s.all {
		c := *n
		c.Steps = append([]StepMark(nil), n.Steps...)
		out[k] = &c
	}
	return out, err
}

// Get returns one session's notes, or nil when there are none.
func (s *NoteStore) Get(key string) *SessionNotes {
	all, _ := s.All()
	return all[key]
}

// Set replaces one session's notes; empty notes delete the entry. It returns
// what was stored.
func (s *NoteStore) Set(key string, n SessionNotes) (*SessionNotes, error) {
	if s == nil {
		return nil, errors.New("this trail keeps no notes")
	}
	if err := n.clean(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		// A file we can't read is the person's notes, maybe hand-edited.
		// Writing over it would lose them.
		return nil, err
	}
	var stored *SessionNotes
	if n.empty() {
		delete(s.all, key)
	} else {
		n.Updated = time.Now().UTC().Truncate(time.Second)
		stored = &n
		s.all[key] = stored
	}
	return stored, s.write()
}

// write saves every note atomically, readable only by the person. Callers
// hold mu.
func (s *NoteStore) write() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(notesFile{Version: 1, Sessions: s.all}); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".notes-*")
	if err != nil {
		return err
	}
	tmp.Chmod(0o600)
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		werr = os.Rename(tmp.Name(), s.path)
	}
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.stamp = fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}
	return nil
}

// printNotes lists bookmarked and annotated sessions, newest note first, for
// trail -notes.
func printNotes(idx *Index, store *NoteStore) error {
	all, err := store.All()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := all[keys[i]], all[keys[j]]
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		fmt.Println("no notes or bookmarks yet; add them from a session's page in trail")
		return nil
	}
	for _, k := range keys {
		n := all[k]
		title, when := "(no longer on disk)", ""
		if s := idx.Get(k); s != nil {
			title, when = oneLine(s.Title, 60), s.Ended.Local().Format("2006-01-02 15:04")
		}
		star := " "
		if n.Bookmarked {
			star = "*"
		}
		fmt.Printf("%s %s  %-16s  %s\n", star, k, when, title)
		if n.Note != "" {
			for _, line := range strings.Split(strings.TrimSpace(n.Note), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		for _, m := range n.Steps {
			fmt.Printf("    #%-4d %s\n", m.Event+1, oneLine(m.Label, 80))
			if m.Note != "" {
				fmt.Printf("          %s\n", oneLine(m.Note, 100))
			}
		}
	}
	return nil
}

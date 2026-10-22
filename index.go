package main

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Index keeps one summary per session file, re-parsing a file only when its
// size or mtime changes.
type Index struct {
	providers []Provider

	mu       sync.Mutex
	entries  map[string]*entry // path -> entry
	byKey    map[string]*Session
	scanned  time.Time
	scanning sync.Mutex

	rootsMu sync.Mutex
	roots   map[string][2]string // cwd -> {repo root, worktree}
}

type entry struct {
	stamp string
	sess  *Session
}

type ProviderStatus struct {
	Name     string `json:"name"`
	Root     string `json:"root"`
	Found    bool   `json:"found"`
	Sessions int    `json:"sessions"`
	Error    string `json:"error,omitempty"`
}

func NewIndex(providers ...Provider) *Index {
	return &Index{
		providers: providers,
		entries:   map[string]*entry{},
		byKey:     map[string]*Session{},
		roots:     map[string][2]string{},
	}
}

// Scan refreshes the index. Calls within maxAge of the last scan return
// immediately, so the UI can poll freely.
func (x *Index) Scan(maxAge time.Duration) []ProviderStatus {
	x.scanning.Lock()
	defer x.scanning.Unlock()

	x.mu.Lock()
	fresh := time.Since(x.scanned) < maxAge
	x.mu.Unlock()

	type job struct {
		p     Provider
		path  string
		stamp string
	}
	var jobs []job
	status := make([]ProviderStatus, 0, len(x.providers))
	live := map[string]bool{}

	for _, p := range x.providers {
		st := ProviderStatus{Name: p.Name(), Root: p.Root()}
		if _, err := os.Stat(p.Root()); err == nil {
			st.Found = true
		}
		if fresh {
			status = append(status, st)
			continue
		}
		paths, err := p.Discover()
		if err != nil {
			st.Error = err.Error()
		}
		for _, path := range paths {
			stamp, ok := sessionStamp(p, path)
			if !ok {
				continue
			}
			live[path] = true
			x.mu.Lock()
			e := x.entries[path]
			x.mu.Unlock()
			if e != nil && e.stamp == stamp {
				continue
			}
			jobs = append(jobs, job{p, path, stamp})
		}
		status = append(status, st)
	}

	if !fresh {
		var wg sync.WaitGroup
		ch := make(chan job)
		workers := runtime.NumCPU()
		if workers > 8 {
			workers = 8
		}
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					t, err := j.p.Parse(j.path, false)
					if err != nil && (t == nil || t.Session.Messages == 0) {
						continue
					}
					s := t.Session
					if s.Messages == 0 {
						continue // empty or aborted session
					}
					s.Key = s.Provider + ":" + s.ID
					s.Root, s.Worktree = x.repoRoot(s.Cwd)
					x.mu.Lock()
					x.entries[j.path] = &entry{stamp: j.stamp, sess: s}
					x.mu.Unlock()
				}
			}()
		}
		for _, j := range jobs {
			ch <- j
		}
		close(ch)
		wg.Wait()

		x.mu.Lock()
		for path := range x.entries {
			if !live[path] {
				delete(x.entries, path)
			}
		}
		x.byKey = map[string]*Session{}
		for _, e := range x.entries {
			x.byKey[e.sess.Key] = e.sess
		}
		x.scanned = time.Now()
		x.mu.Unlock()

		// Names some agents keep outside the session file. Applied on every
		// scan because a rename doesn't touch the session file.
		for _, p := range x.providers {
			n, ok := p.(sessionNamer)
			if !ok {
				continue
			}
			names := n.SessionNames()
			x.mu.Lock()
			for _, s := range x.byKey {
				if s.Provider == p.Name() {
					s.applyName(names[s.ID])
				}
			}
			x.mu.Unlock()
		}
	}

	x.mu.Lock()
	for i := range status {
		for _, s := range x.byKey {
			if s.Provider == status[i].Name {
				status[i].Sessions++
			}
		}
	}
	x.mu.Unlock()
	return status
}

// sessionStamp returns a value that changes whenever the session at path
// does: the file's size and mtime, or the provider's own stamp for sessions
// that aren't files.
func sessionStamp(p Provider, path string) (string, bool) {
	if st, ok := p.(sessionStamper); ok {
		return st.Stamp(path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(fi.Size(), 10) + "@" + strconv.FormatInt(fi.ModTime().UnixNano(), 10), true
}

// Sessions returns a snapshot of every indexed session, newest first. They are
// copies, so a later scan renaming a session can't race with the caller.
func (x *Index) Sessions() []*Session {
	x.mu.Lock()
	out := make([]*Session, 0, len(x.byKey))
	for _, s := range x.byKey {
		c := *s
		out = append(out, &c)
	}
	x.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Ended.After(out[j].Ended) })
	return out
}

// Get returns a copy of one indexed session, or nil.
func (x *Index) Get(key string) *Session {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.byKey[key]
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// Load parses the full transcript for an indexed session. Only paths found by
// discovery can be loaded; the key never maps to an arbitrary file.
func (x *Index) Load(key string) (*Transcript, error) {
	s := x.Get(key)
	if s == nil {
		return nil, os.ErrNotExist
	}
	for _, p := range x.providers {
		if p.Name() == s.Provider {
			var t *Transcript
			var err error
			if lp, ok := p.(lineParser); ok {
				var b []byte
				if b, err = readComplete(s.Path); err != nil {
					return nil, err
				}
				t, err = lp.ParseLines(bytes.NewReader(b), s.Path, true)
				if t != nil {
					t.Size = int64(len(b))
				}
			} else {
				// Read the stamp first: if the session changes during the
				// parse, the next live poll sees a newer stamp and reloads.
				stamp, _ := sessionStamp(p, s.Path)
				t, err = p.Parse(s.Path, true)
				if t != nil {
					t.Size = stampToken(stamp)
				}
			}
			if t != nil {
				t.Session.Key, t.Session.Root, t.Session.Worktree = s.Key, s.Root, s.Worktree
				if n, ok := p.(sessionNamer); ok {
					t.Session.applyName(n.SessionNames()[t.Session.ID])
				}
			}
			return t, err
		}
	}
	return nil, os.ErrNotExist
}

// Stamp returns the current stamp of an indexed session, read now rather
// than at the last scan. ok is false once the session is gone.
func (x *Index) Stamp(s *Session) (string, bool) {
	for _, p := range x.providers {
		if p.Name() == s.Provider {
			return sessionStamp(p, s.Path)
		}
	}
	return "", false
}

// stampToken folds a stamp into Transcript.Size for sessions that aren't
// line files, so live updates can tell versions apart the same way.
func stampToken(stamp string) int64 {
	h := fnv.New64a()
	h.Write([]byte(stamp))
	return int64(h.Sum64() >> 1)
}

// repoRoot files a cwd under its git repository. A linked worktree is filed
// under the main repository so all of a project's sessions sit together. A cwd
// that is not a repository (or no longer exists) is its own root.
func (x *Index) repoRoot(cwd string) (root, worktree string) {
	if cwd == "" {
		return "(unknown)", ""
	}
	x.rootsMu.Lock()
	if r, ok := x.roots[cwd]; ok {
		x.rootsMu.Unlock()
		return r[0], r[1]
	}
	x.rootsMu.Unlock()

	root = cwd
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		fi, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			root = dir
			if !fi.IsDir() {
				// "gitdir: /main/repo/.git/worktrees/<name>"
				if b, err := os.ReadFile(filepath.Join(dir, ".git")); err == nil {
					gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
					if i := strings.Index(gitdir, string(filepath.Separator)+".git"+string(filepath.Separator)+"worktrees"+string(filepath.Separator)); i > 0 {
						root, worktree = gitdir[:i], dir
					}
				}
			}
			break
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	x.rootsMu.Lock()
	x.roots[cwd] = [2]string{root, worktree}
	x.rootsMu.Unlock()
	return root, worktree
}

// readComplete reads a session file's complete lines. The agents append one
// JSON line per write, so text after the last newline that isn't valid JSON
// yet is a line still being written; it is left for the next read. The file is
// closed before returning, so trail never holds it open while the agent works.
func readComplete(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// A file that is one whole JSON document (Gemini CLI's older .json
	// sessions) is complete even though its last line isn't valid alone.
	if end := bytes.LastIndexByte(b, '\n') + 1; end < len(b) && !json.Valid(b[end:]) && !json.Valid(b) {
		b = b[:end]
	}
	return b, nil
}

// indexedFile is a session file the index holds, for the search index to
// follow without re-statting anything.
type indexedFile struct {
	path, key, provider string
	stamp               string
}

func (x *Index) files() []indexedFile {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]indexedFile, 0, len(x.entries))
	for path, e := range x.entries {
		out = append(out, indexedFile{path: path, key: e.sess.Key, provider: e.sess.Provider, stamp: e.stamp})
	}
	return out
}

// parseFull reads a whole transcript by provider name.
func (x *Index) parseFull(provider, path string) (*Transcript, error) {
	for _, p := range x.providers {
		if p.Name() == provider {
			return p.Parse(path, true)
		}
	}
	return nil, os.ErrNotExist
}

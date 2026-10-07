package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	size int64
	mod  time.Time
	sess *Session
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
		p    Provider
		path string
		fi   os.FileInfo
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
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			live[path] = true
			x.mu.Lock()
			e := x.entries[path]
			x.mu.Unlock()
			if e != nil && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
				continue
			}
			jobs = append(jobs, job{p, path, fi})
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
					x.entries[j.path] = &entry{size: j.fi.Size(), mod: j.fi.ModTime(), sess: s}
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
			t, err := p.Parse(s.Path, true)
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

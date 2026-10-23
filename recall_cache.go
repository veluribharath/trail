package main

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// trail recall runs once per prompt, so it can't read every session log each
// time. It keeps what it read in its own cache, beside the vectors: the
// session list for every project, and the word index for the project it ran
// in. A later prompt re-reads only the logs that changed. The cache holds
// words from transcripts, so it is readable only by you, and -forget
// deletes it.

const recallCacheVersion = 1

func recallCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "trail", "recall")
}

// recallCacheName is a file name for a set of agent folders and, optionally,
// a project in them.
func recallCacheName(sources []string, root string) string {
	h := sha256.Sum256([]byte(strings.Join(sources, "\x00") + "\x00" + root))
	return hex.EncodeToString(h[:12]) + ".gob"
}

func readGob(path string, v any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return gob.NewDecoder(f).Decode(v) == nil
}

// writeGob replaces path whole, so a hook running beside this one never reads
// half a file.
func writeGob(path string, v any) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	err = gob.NewEncoder(f).Encode(v)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		os.Chmod(f.Name(), 0o600)
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
}

/* ---------------- the session list ---------------- */

type cachedSessions struct {
	Version int
	Entries map[string]cachedEntry // by path
}

type cachedEntry struct {
	Stamp               string
	Sess                Session
	FileTitle, FileKind string
}

// loadEntries seeds the index with sessions read on an earlier run; a scan
// then re-reads only the files whose stamp changed.
func (x *Index) loadEntries(path string) {
	var c cachedSessions
	if !readGob(path, &c) || c.Version != recallCacheVersion {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for p, ce := range c.Entries {
		s := ce.Sess
		s.fileTitle, s.fileKind = ce.FileTitle, ce.FileKind
		x.entries[p] = &entry{stamp: ce.Stamp, sess: &s}
	}
}

func (x *Index) saveEntries(path string) {
	c := cachedSessions{Version: recallCacheVersion, Entries: map[string]cachedEntry{}}
	x.mu.Lock()
	for p, e := range x.entries {
		c.Entries[p] = cachedEntry{Stamp: e.stamp, Sess: *e.sess, FileTitle: e.sess.fileTitle, FileKind: e.sess.fileKind}
	}
	x.mu.Unlock()
	writeGob(path, c)
}

/* ---------------- the word index ---------------- */

type cachedSearch struct {
	Version  int
	Sessions map[string]cachedSearchSession // by path
}

// cachedSearchSession is a searchSession with its own small vocabulary, since
// term ids are only good for one process.
type cachedSearchSession struct {
	Stamp, Key string
	Docs       []cachedDoc
	Terms      []string
	Post       []uint64 // local term id<<32 | doc<<8 | tf
	Edits      map[int32][]string
}

type cachedDoc struct {
	Event int32
	N     uint16
	Field uint8
	Tool  string
}

// load seeds an empty search index with sessions indexed on an earlier run.
// Sync then rebuilds only the ones whose stamp changed.
func (s *Search) load(path string) {
	var c cachedSearch
	if !readGob(path, &c) || c.Version != recallCacheVersion {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, cs := range c.Sessions {
		if s.keep != nil && !s.keep(cs.Key) {
			continue
		}
		ss := &searchSession{stamp: cs.Stamp, key: cs.Key, edits: cs.Edits, docs: make([]docMeta, len(cs.Docs)), post: make([]uint64, len(cs.Post))}
		ids := make([]uint32, len(cs.Terms))
		for i, t := range cs.Terms {
			id, ok := s.termID[t]
			if !ok {
				id = uint32(len(s.df))
				s.termID[t] = id
				s.df = append(s.df, 0)
			}
			ids[i] = id
		}
		bad := false
		for i, p := range cs.Post {
			local := p >> 32
			if local >= uint64(len(ids)) || int(p>>8&0xFFFFFF) >= len(cs.Docs) {
				bad = true
				break
			}
			ss.post[i] = uint64(ids[local])<<32 | p&0xFFFFFFFF
		}
		if bad {
			continue // the next sync reads it again
		}
		for i, d := range cs.Docs {
			ss.docs[i] = docMeta{event: d.Event, n: d.N, field: field(d.Field), tool: d.Tool}
			s.docs++
			s.totalLen += int(d.N)
		}
		slices.Sort(ss.post)
		for _, p := range ss.post {
			s.df[p>>32]++
		}
		s.byPath[p] = ss
	}
}

func (s *Search) save(path string) {
	c := cachedSearch{Version: recallCacheVersion, Sessions: map[string]cachedSearchSession{}}
	s.mu.RLock()
	terms := make([]string, len(s.df))
	for t, id := range s.termID {
		terms[id] = t
	}
	for p, ss := range s.byPath {
		cs := cachedSearchSession{Stamp: ss.stamp, Key: ss.key, Edits: ss.edits, Docs: make([]cachedDoc, len(ss.docs)), Post: make([]uint64, len(ss.post))}
		local := map[uint64]uint64{}
		for i, p := range ss.post {
			id, ok := local[p>>32]
			if !ok {
				id = uint64(len(cs.Terms))
				local[p>>32] = id
				cs.Terms = append(cs.Terms, terms[p>>32])
			}
			cs.Post[i] = id<<32 | p&0xFFFFFFFF
		}
		for i, d := range ss.docs {
			cs.Docs[i] = cachedDoc{Event: d.event, N: d.n, Field: uint8(d.field), Tool: d.tool}
		}
		c.Sessions[p] = cs
	}
	s.mu.RUnlock()
	writeGob(path, c)
}

// recallCacheRoot is where the cache lives; tests point it elsewhere.
var recallCacheRoot = recallCacheDir

// recallWarm fills the cache for a project in a process of its own, so the
// hook can return without waiting for it; nil turns it off.
var recallWarm = func(cache, cwd string) {
	lock := warmLock(cache, cwd)
	if os.MkdirAll(cache, 0o700) != nil {
		return
	}
	// One warmer at a time per project; a lock left by a crash expires.
	if fi, err := os.Stat(lock); err == nil && time.Since(fi.ModTime()) > 10*time.Minute {
		os.Remove(lock)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Close()
	exe, err := os.Executable()
	if err != nil {
		os.Remove(lock)
		return
	}
	// The same agent folders as this run: every flag before "recall".
	var args []string
	for i, a := range os.Args[1:] {
		if a == "recall" {
			args = append(slices.Clone(os.Args[1:i+1]), "recall", "-warm", "-project", cwd)
			break
		}
	}
	if args == nil {
		os.Remove(lock)
		return
	}
	cmd := exec.Command(exe, args...)
	detach(cmd)
	if cmd.Start() != nil {
		os.Remove(lock)
		return
	}
	cmd.Process.Release()
}

func warmLock(cache, cwd string) string {
	h := sha256.Sum256([]byte(cwd))
	return filepath.Join(cache, "warm-"+hex.EncodeToString(h[:8])+".lock")
}

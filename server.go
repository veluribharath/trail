package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	index    *Index
	search   *Search
	semantic *Semantic // nil unless trail runs with -semantic
	home     string
	version  string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/commits", s.handleCommits)
	mux.HandleFunc("GET /api/commit", s.handleCommit)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/export", s.handleExport)
	mux.HandleFunc("GET /api/blame", s.handleBlame)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/ping", s.handlePing)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", noCache(http.FileServer(http.FS(sub))))
	return securityHeaders(mux)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	maxAge := 5 * time.Second
	if r.URL.Query().Get("refresh") == "1" {
		maxAge = 0
	}
	status := s.index.Scan(maxAge)
	s.search.Kick()
	if s.semantic != nil {
		s.semantic.Kick()
	}
	writeJSON(w, map[string]any{
		"home":      s.home,
		"version":   s.version,
		"providers": status,
		"sessions":  s.index.Sessions(),
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	t, err := s.index.Load(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "That session is no longer on disk. Refresh the list.")
		return
	}
	writeJSON(w, t)
}

// handleSearch answers from whatever is indexed so far; while transcripts are
// still being read the result says so, and the page asks again.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	s.index.Scan(5 * time.Second)
	s.search.Kick()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	q, only := ParseQuery(r.URL.Query().Get("q")), r.URL.Query().Get("id")
	res := s.search.Run(q, only, limit)
	if s.semantic != nil {
		s.semantic.Kick()
		res = s.semantic.Blend(r.Context(), q, res, only, limit)
	}
	writeJSON(w, res)
}

// handleExport sends one session as an offline HTML file. It's a download,
// so the browser asks where to save it; trail itself writes nothing.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	t, err := s.index.Load(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "That session is no longer on disk. Refresh the list.")
		return
	}
	var b bytes.Buffer
	if err := ExportHTML(r.Context(), &b, t, s.home, s.version, false); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportName(t.Session)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// handlePing lets trail -mcp check that a window it found is trail, reading
// the same agent folders, before it searches through it.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"app": "trail", "version": s.version, "sources": s.index.Sources()})
}

// handleStats aggregates sessions by project, file, tool and agent. Tool and
// file numbers come from the search index, so while it is still reading
// transcripts the result says how far it got, and the page asks again.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.index.Scan(5 * time.Second)
	s.search.Kick()
	writeJSON(w, s.stats(r.URL.Query().Get("days"), r.URL.Query().Get("root"), r.URL.Query().Get("agents")))
}

// stats answers for the last days days ("" or "all" for everything).
func (s *Server) stats(days, root, agents string) *Stats {
	q := StatsQuery{Root: root, Agents: parseAgents(agents), To: time.Now()}
	if n, err := strconv.Atoi(days); err == nil && n > 0 {
		q.From = bucketStart(q.To.AddDate(0, 0, -n+1), "day")
	}
	per, total, pending := s.search.sessionStats()
	st := BuildStats(s.index.Sessions(), per, q)
	st.Total, st.Read = total, total-pending
	if total == 0 {
		// The first sync hasn't started counting yet.
		st.Total, st.Read = len(s.index.files()), 0
	}
	return st
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	t, err := s.index.Load(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "That session is no longer on disk. Refresh the list.")
		return
	}
	writeJSON(w, SessionCommits(r.Context(), t))
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	sess := s.index.Get(r.URL.Query().Get("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "That session is no longer on disk. Refresh the list.")
		return
	}
	patch, err := CommitPatch(r.Context(), sess.Cwd, r.URL.Query().Get("sha"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"patch": patch})
}

// handleBlame attributes a file's lines to sessions. A relative path is read
// against the session named by id. Only files under a folder some session
// ran in can be read, so the page can't be used to read anything else.
func (s *Server) handleBlame(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(s.home, p[1:])
	}
	if p == "" {
		fail(w, http.StatusBadRequest, "Name a file.")
		return
	}
	if !filepath.IsAbs(p) {
		sess := s.index.Get(r.URL.Query().Get("id"))
		if sess == nil {
			fail(w, http.StatusBadRequest, "Give the file's full path, or a path inside a session's folder.")
			return
		}
		p = filepath.Join(sess.Cwd, p)
	}
	// Resolve the file itself too: a link inside a session folder must not
	// lead to a file outside every one.
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	} else {
		p = realPath(filepath.Clean(p))
	}
	if !s.underSession(p) {
		fail(w, http.StatusForbidden, "trail only reads files inside folders your sessions ran in.")
		return
	}
	writeJSON(w, Blame(r.Context(), s.index, p))
}

func (s *Server) underSession(p string) bool {
	for _, sess := range s.index.Sessions() {
		for _, dir := range []string{sess.Root, sess.Worktree, sess.Cwd} {
			if dir == "" || !filepath.IsAbs(dir) {
				continue
			}
			if d, err := filepath.EvalSymlinks(dir); err == nil {
				dir = d
			}
			if rel, err := filepath.Rel(dir, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				return true
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// securityHeaders keeps the page from loading anything off-box and refuses
// cross-site reads of the API, since transcripts can contain secrets.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				fail(w, http.StatusForbidden, "cross-site request refused")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

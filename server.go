package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
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
	mux.HandleFunc("GET /api/stats", s.handleStats)
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
	if err := ExportHTML(r.Context(), &b, t, s.home, s.version); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportName(t.Session)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
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

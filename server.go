package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	index   *Index
	home    string
	version string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/commits", s.handleCommits)
	mux.HandleFunc("GET /api/commit", s.handleCommit)
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

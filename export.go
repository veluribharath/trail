package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// An export is one session as a single HTML file: the same page the server
// shows, with the stylesheet, fonts, script and transcript inlined. It opens
// from disk with no server, and its content security policy forbids every
// network request, so a shared copy can't load or send anything.

// exportData is the JSON the exported page boots from instead of the API.
type exportData struct {
	Version    string            `json:"version"`
	Exported   time.Time         `json:"exported"`
	Home       string            `json:"home"`
	Transcript *Transcript       `json:"transcript"`
	Commits    CommitsResult     `json:"commits"`
	Patches    map[string]string `json:"patches"` // sha -> git show output
}

const maxExportCommits = 30

var fontURL = regexp.MustCompile(`url\("fonts/([a-z0-9.-]+\.woff2)"\)`)

// ExportHTML writes t as a self-contained HTML page. Commits are included
// only when they touch a file the session edited, each with its patch, so
// the file carries what the session did and not the rest of the repository.
// With redact, credentials are hidden in the transcript, commit messages and
// patches; t itself is redacted in place.
func ExportHTML(ctx context.Context, w io.Writer, t *Transcript, home, version string, redact bool) error {
	data := exportData{Version: version, Exported: time.Now().UTC(), Home: home, Transcript: t, Patches: map[string]string{}}
	data.Commits = SessionCommits(ctx, t)
	kept := data.Commits.Commits[:0]
	for _, c := range data.Commits.Commits {
		if len(c.Overlap) > 0 && len(kept) < maxExportCommits {
			kept = append(kept, c)
		}
	}
	data.Commits.Commits = kept
	for _, c := range kept {
		if p, err := CommitPatch(ctx, t.Session.Cwd, c.SHA); err == nil {
			data.Patches[c.SHA] = p
		}
	}
	if redact {
		RedactTranscript(t)
		for i := range data.Commits.Commits {
			c := &data.Commits.Commits[i]
			c.Subject = Redact(c.Subject)
		}
		for sha, p := range data.Patches {
			data.Patches[sha] = Redact(p)
		}
	}
	return writeExport(w, &data)
}

func writeExport(w io.Writer, data *exportData) error {
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		return err
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		return err
	}
	var fontErr error
	css = fontURL.ReplaceAllFunc(css, func(m []byte) []byte {
		name := fontURL.FindSubmatch(m)[1]
		b, err := webFS.ReadFile("web/fonts/" + string(name))
		if err != nil {
			fontErr = err
			return m
		}
		return []byte(`url("data:font/woff2;base64,` + base64.StdEncoding.EncodeToString(b) + `")`)
	})
	if fontErr != nil {
		return fontErr
	}
	// The stylesheet and script are inlined as is, so they must not be able
	// to close their own element.
	if bytes.Contains(bytes.ToLower(css), []byte("</style")) || bytes.Contains(bytes.ToLower(js), []byte("</script")) {
		return errors.New("web assets can't be inlined")
	}
	// json.Marshal escapes <, > and &, so the transcript can't close its
	// script element either.
	blob, err := json.Marshal(data)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(js)
	csp := "default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:; script-src 'sha256-" +
		base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'"

	s := data.Transcript.Session
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<meta http-equiv=\"Content-Security-Policy\" content=\"%s\">\n", csp)
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<meta name=\"referrer\" content=\"no-referrer\">\n")
	fmt.Fprintf(&b, "<meta name=\"generator\" content=\"trail %s\">\n", htmlEscape(data.Version))
	fmt.Fprintf(&b, "<title>%s · trail</title>\n", htmlEscape(oneLine(s.Title, 80)))
	b.WriteString(`<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' rx='4' fill='%230E1320'/%3E%3Cpath d='M3.5 12c2.5 0 2-4 4.5-4s2-4 4.5-4' fill='none' stroke='%23F2B544' stroke-width='1.8' stroke-linecap='round' stroke-dasharray='0.1 3'/%3E%3Ccircle cx='12.5' cy='4' r='1.6' fill='%23F2B544'/%3E%3C/svg%3E">` + "\n")
	b.WriteString("<style>\n")
	b.Write(css)
	b.WriteString("</style>\n</head>\n<body>\n")
	b.WriteString(`<div class="app exported" id="app"><main class="main" id="main"><div id="view" class="view"></div></main></div>` + "\n")
	b.WriteString(`<div class="tip" id="tip" role="tooltip" hidden></div>` + "\n")
	b.WriteString(`<script type="application/json" id="trail-export">`)
	b.Write(blob)
	b.WriteString("</script>\n<script>")
	b.Write(js)
	b.WriteString("</script>\n</body>\n</html>\n")
	_, err = io.WriteString(w, b.String())
	return err
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// exportName is the file name offered for a session's export.
func exportName(s *Session) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s.Title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = s.Provider + "-" + s.ID
		if len(slug) > 48 {
			slug = slug[:48]
		}
	}
	return "trail-" + slug + ".html"
}

// findSession resolves what someone typed for -export: a session key
// (provider:id), a session id, or a unique prefix of one.
func findSession(idx *Index, arg string) (*Session, error) {
	arg = strings.TrimSpace(arg)
	if s := idx.Get(arg); s != nil {
		return s, nil
	}
	return pickSession(idx.Sessions(), arg)
}

// pickSession is findSession over a list of sessions.
func pickSession(sessions []*Session, arg string) (*Session, error) {
	arg = strings.TrimSpace(arg)
	var exact, prefix []*Session
	for _, s := range sessions {
		if s.Key == arg {
			return s, nil
		}
		if s.ID == arg {
			exact = append(exact, s)
		} else if arg != "" && strings.HasPrefix(s.ID, arg) {
			prefix = append(prefix, s)
		}
	}
	for _, list := range [][]*Session{exact, prefix} {
		if len(list) == 1 {
			return list[0], nil
		}
		if len(list) > 1 {
			keys := make([]string, len(list))
			for i, s := range list {
				keys[i] = s.Key
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("%q matches %d sessions: %s", arg, len(list), strings.Join(keys, ", "))
		}
	}
	return nil, fmt.Errorf("no session %q; trail -list prints session ids", arg)
}

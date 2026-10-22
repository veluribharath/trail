package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func exportIndex() *Index {
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &codexProvider{dir: "testdata/codex"}, &opencodeProvider{dir: "testdata/opencode"})
	idx.Scan(0)
	return idx
}

var (
	cspRe    = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]+)">`)
	blobRe   = regexp.MustCompile(`(?s)<script type="application/json" id="trail-export">(.*?)</script>`)
	scriptRe = regexp.MustCompile(`(?s)</script>\n<script>(.*?)</script>`)
	remoteRe = regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*(https?:)?//`)
)

func TestExportEveryProvider(t *testing.T) {
	idx := exportIndex()
	for _, key := range []string{"claude:c1", "codex:0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "opencode:ses_o1"} {
		tr, err := idx.Load(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		var b bytes.Buffer
		if err := ExportHTML(context.Background(), &b, tr, "/work", "1.2.3"); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		page := b.String()

		m := blobRe.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("%s: no embedded session", key)
		}
		var data exportData
		if err := json.Unmarshal([]byte(m[1]), &data); err != nil {
			t.Fatalf("%s: embedded session isn't JSON: %v", key, err)
		}
		if data.Transcript.Session.Key != key || len(data.Transcript.Events) != len(tr.Events) || len(data.Transcript.Changes) != len(tr.Changes) {
			t.Errorf("%s: embedded transcript differs: %+v", key, data.Transcript.Session)
		}
		if data.Home != "/work" || data.Version != "1.2.3" {
			t.Errorf("%s: home=%q version=%q", key, data.Home, data.Version)
		}

		// The page may run its own script and nothing else, and may not
		// fetch anything.
		csp := cspRe.FindStringSubmatch(page)
		js := scriptRe.FindStringSubmatch(page)
		if csp == nil || js == nil {
			t.Fatalf("%s: missing CSP or script", key)
		}
		sum := sha256.Sum256([]byte(js[1]))
		if !strings.Contains(csp[1], "default-src 'none'") || !strings.Contains(csp[1], "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
			t.Errorf("%s: CSP doesn't pin the script: %s", key, csp[1])
		}
		if strings.Contains(csp[1], "script-src 'unsafe-inline'") || strings.Contains(csp[1], "connect-src") {
			t.Errorf("%s: CSP too loose: %s", key, csp[1])
		}
		if loc := remoteRe.FindStringIndex(page); loc != nil {
			t.Errorf("%s: page references something off-box: %q", key, page[loc[0]:min(len(page), loc[1]+40)])
		}
		if strings.Contains(page, `url("fonts/`) || !strings.Contains(page, "data:font/woff2;base64,") {
			t.Errorf("%s: fonts should be inlined", key)
		}
		if strings.Count(page, "</script>") != 2 {
			t.Errorf("%s: want exactly two script elements, got %d closings", key, strings.Count(page, "</script>"))
		}
	}
}

// A transcript that contains markup must stay data: it can't end the script
// element it is embedded in.
func TestExportEscapesTranscript(t *testing.T) {
	tr := &Transcript{
		Session: &Session{Key: "claude:x", ID: "x", Provider: "claude", Title: `</title><script>alert(1)</script>`},
		Events:  []Event{{Kind: "user", Text: "</script><script>alert(1)</script><!--"}},
		Changes: []FileChange{},
	}
	var b bytes.Buffer
	if err := writeExport(&b, &exportData{Transcript: tr, Patches: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	if strings.Count(page, "</script>") != 2 || strings.Contains(page, "<script>alert") || strings.Contains(page, "<!--") {
		t.Errorf("transcript text escaped its element")
	}
	if !strings.Contains(page, "<title>&lt;/title&gt;&lt;script&gt;") {
		t.Errorf("title not escaped")
	}
}

func TestExportName(t *testing.T) {
	for _, c := range []struct {
		s    Session
		want string
	}{
		{Session{Title: "Fix cart total rounding!", Provider: "claude", ID: "c1"}, "trail-fix-cart-total-rounding.html"},
		{Session{Title: "  ¿Qué?  ", Provider: "codex", ID: "abc"}, "trail-qu.html"},
		{Session{Title: "日本語", Provider: "opencode", ID: "ses_o1"}, "trail-opencode-ses_o1.html"},
	} {
		if got := exportName(&c.s); got != c.want {
			t.Errorf("%q: got %q, want %q", c.s.Title, got, c.want)
		}
	}
}

func TestFindSession(t *testing.T) {
	idx := exportIndex()
	for arg, want := range map[string]string{
		"claude:c1": "claude:c1",
		"c1":        "claude:c1",
		"0199aaaa":  "codex:0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000",
		"ses_o1":    "opencode:ses_o1",
	} {
		s, err := findSession(idx, arg)
		if err != nil || s.Key != want {
			t.Errorf("%q: got %v %v, want %s", arg, s, err, want)
		}
	}
	if _, err := findSession(idx, "c"); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Errorf("ambiguous prefix should say so: %v", err)
	}
	if _, err := findSession(idx, "nope"); err == nil {
		t.Error("unknown id should fail")
	}
}

func TestExportEndpoint(t *testing.T) {
	idx := exportIndex()
	srv := httptest.NewServer((&Server{index: idx, search: NewSearch(idx), home: "/work"}).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/export?id=claude:c1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="trail-fix-cart-total-rounding.html"` {
		t.Errorf("disposition: %q", cd)
	}
	if !bytes.Contains(body, []byte(`id="trail-export"`)) {
		t.Error("body isn't an export")
	}
	resp, _ = http.Get(srv.URL + "/api/export?id=claude:missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/export?id=claude:c1", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site export should be refused, got %d", resp.StatusCode)
	}
}

// Only commits that touch the session's files go into the export, each with
// its patch.
func TestExportCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "edit a")
	os.WriteFile(filepath.Join(dir, "other.go"), []byte("package secret\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "unrelated work")

	now := time.Now()
	tr := &Transcript{Session: &Session{Key: "claude:x", Cwd: dir, Title: "t", Started: now.Add(-time.Minute), Ended: now}, Events: []Event{}}
	tr.addChange(FileChange{Path: filepath.Join(dir, "a.go"), Op: "edit"}, -1)
	var b bytes.Buffer
	if err := ExportHTML(t.Context(), &b, tr, "", "dev"); err != nil {
		t.Fatal(err)
	}
	var data exportData
	json.Unmarshal([]byte(blobRe.FindStringSubmatch(b.String())[1]), &data)
	if len(data.Commits.Commits) != 1 || data.Commits.Commits[0].Subject != "edit a" {
		t.Fatalf("commits: %+v", data.Commits.Commits)
	}
	if p := data.Patches[data.Commits.Commits[0].SHA]; !strings.Contains(p, "package a") {
		t.Errorf("patch missing: %q", p)
	}
	if strings.Contains(b.String(), "unrelated work") || strings.Contains(b.String(), "package secret") {
		t.Error("a commit that doesn't touch the session's files leaked into the export")
	}
}

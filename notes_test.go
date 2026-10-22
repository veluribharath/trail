package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNoteStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trail", "notes.json")
	s := NewNoteStore(path)
	got, err := s.Set("claude:c1", SessionNotes{Bookmarked: true, Note: "  why the cart rounds  ",
		Steps: []StepMark{{Event: 7, Label: "Bash:\n go   test", Note: " "}, {Event: 2}, {Event: 7, Note: "dup"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "  why the cart rounds  " || got.Updated.IsZero() {
		t.Errorf("stored: %+v", got)
	}
	if len(got.Steps) != 2 || got.Steps[0].Event != 2 || got.Steps[1].Event != 7 || got.Steps[1].Label != "Bash: go test" || got.Steps[1].Note != "" {
		t.Errorf("steps should be sorted, deduped and trimmed: %+v", got.Steps)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("notes file mode %v, want 0600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("notes dir mode %v, want 0700", di.Mode().Perm())
		}
	}

	// A second process sees the note, and its own write keeps the first's.
	other := NewNoteStore(path)
	if n := other.Get("claude:c1"); n == nil || !n.Bookmarked {
		t.Fatalf("second store: %+v", n)
	}
	if _, err := other.Set("codex:x", SessionNotes{Note: "later"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.All()
	if len(all) != 2 || all["codex:x"].Note != "later" {
		t.Errorf("first store didn't pick up the other write: %+v", all)
	}

	// Copies, not the store's own values.
	all["claude:c1"].Steps[0].Note = "mutated"
	if s.Get("claude:c1").Steps[0].Note == "mutated" {
		t.Error("All leaked the store's own value")
	}

	// Clearing everything removes the entry.
	if n, err := s.Set("claude:c1", SessionNotes{Note: "   "}); err != nil || n != nil {
		t.Errorf("empty notes: %+v %v", n, err)
	}
	if s.Get("claude:c1") != nil {
		t.Error("cleared notes still stored")
	}
}

func TestNoteStoreLimits(t *testing.T) {
	s := NewNoteStore("")
	if _, err := s.Set("k", SessionNotes{Note: strings.Repeat("x", maxNoteLen+1)}); err == nil {
		t.Error("an over-long note should be refused")
	}
	if _, err := s.Set("k", SessionNotes{Steps: []StepMark{{Event: -1}}}); err == nil {
		t.Error("a negative step should be refused")
	}
	if _, err := s.Set("k", SessionNotes{Steps: make([]StepMark, maxSteps+1)}); err == nil {
		t.Error("too many steps should be refused")
	}
	var nilStore *NoteStore
	if all, err := nilStore.All(); err != nil || len(all) != 0 {
		t.Error("a nil store has no notes")
	}
	if _, err := nilStore.Set("k", SessionNotes{Bookmarked: true}); err == nil {
		t.Error("a nil store can't save")
	}
}

// A notes file trail can't read is the person's, maybe edited by hand; a
// save must not write over it.
func TestNoteStoreKeepsUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	os.WriteFile(path, []byte(`{"sessions": {"claude:c1": {"note": "precious"`), 0o600)
	s := NewNoteStore(path)
	if _, err := s.Set("claude:c2", SessionNotes{Bookmarked: true}); err == nil {
		t.Fatal("saving over a broken notes file should fail")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "precious") {
		t.Errorf("notes file was overwritten: %s", b)
	}
}

func TestNotesAPI(t *testing.T) {
	idx := exportIndex()
	store := NewNoteStore(filepath.Join(t.TempDir(), "notes.json"))
	srv := httptest.NewServer((&Server{index: idx, search: NewSearch(idx), notes: store, home: "/work"}).Handler())
	defer srv.Close()

	put := func(id, ctype, body string, hdr ...string) (int, string) {
		req, _ := http.NewRequest("PUT", srv.URL+"/api/notes?id="+id, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	code, body := put("claude:c1", "application/json", `{"bookmarked":true,"note":"check rounding","steps":[{"event":3,"label":"Edit cart.go","note":"here"}]}`)
	if code != 200 || !strings.Contains(body, `"bookmarked":true`) {
		t.Fatalf("save: %d %s", code, body)
	}
	if n := store.Get("claude:c1"); n == nil || n.Note != "check rounding" || len(n.Steps) != 1 {
		t.Errorf("stored: %+v", n)
	}

	var list struct {
		Notes map[string]SessionNotes `json:"notes"`
	}
	getJSON(t, srv.URL+"/api/sessions", &list)
	if !list.Notes["claude:c1"].Bookmarked {
		t.Errorf("session list should carry notes: %+v", list.Notes)
	}

	for _, c := range []struct {
		name, id, ctype, body string
		hdr                   []string
		want                  int
	}{
		{"form post", "claude:c1", "text/plain", `{"note":"x"}`, nil, http.StatusUnsupportedMediaType},
		{"unknown session", "claude:missing", "application/json", `{"note":"x"}`, nil, http.StatusNotFound},
		{"unknown field", "claude:c1", "application/json", `{"nope":1}`, nil, http.StatusBadRequest},
		{"bad step", "claude:c1", "application/json", `{"steps":[{"event":-4}]}`, nil, http.StatusBadRequest},
		{"cross-site", "claude:c1", "application/json", `{"note":"x"}`, []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
	} {
		if code, body := put(c.id, c.ctype, c.body, c.hdr...); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
	if n := store.Get("claude:c1"); n == nil || n.Note != "check rounding" {
		t.Errorf("a refused request changed the notes: %+v", n)
	}

	// GET can't write.
	resp, _ := http.Get(srv.URL + "/api/notes?id=claude:c1")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /api/notes: %d", resp.StatusCode)
	}

	// The export carries the notes unless asked not to.
	exportNotes := func(q string) *SessionNotes {
		resp, err := http.Get(srv.URL + "/api/export?id=claude:c1" + q)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var data exportData
		if err := json.Unmarshal([]byte(blobRe.FindStringSubmatch(string(b))[1]), &data); err != nil {
			t.Fatal(err)
		}
		return data.Notes
	}
	if n := exportNotes(""); n == nil || n.Note != "check rounding" || n.Steps[0].Note != "here" {
		t.Errorf("export notes: %+v", n)
	}
	if n := exportNotes("&notes=0"); n != nil {
		t.Errorf("notes=0 should leave notes out: %+v", n)
	}

	// Clearing deletes.
	if code, _ := put("claude:c1", "application/json", `{}`); code != 200 || store.Get("claude:c1") != nil {
		t.Errorf("clear: %d %+v", code, store.Get("claude:c1"))
	}
}

func TestExportSessionNotes(t *testing.T) {
	idx := exportIndex()
	dir := t.TempDir()
	store := NewNoteStore(filepath.Join(dir, "notes.json"))
	store.Set("claude:c1", SessionNotes{Note: "</script><script>alert(1)</script>"})
	out := filepath.Join(dir, "s.html")
	if err := exportSession(idx, store, "c1", out, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if bytes.Contains(b, []byte("<script>alert(1)")) {
		t.Error("a note closed the export's script element")
	}
	var data exportData
	json.Unmarshal([]byte(blobRe.FindStringSubmatch(string(b))[1]), &data)
	if data.Notes == nil || !strings.Contains(data.Notes.Note, "alert(1)") {
		t.Errorf("export notes: %+v", data.Notes)
	}
	if err := exportSession(idx, nil, "c1", out, false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(out)
	if bytes.Contains(b, []byte("alert(1)")) {
		t.Error("-no-notes export still has the note")
	}

	// -redact covers what the person wrote, too.
	store.Set("claude:c1", SessionNotes{Note: "db password=hunter2hunter2",
		Steps: []StepMark{{Event: 1, Label: "export API_KEY=Xk9fQ2pLm7Zr4T", Note: "token: s3cr3tvalue99"}}})
	if err := exportSession(idx, store, "c1", out, true); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(out)
	for _, secret := range []string{"hunter2hunter2", "Xk9fQ2pLm7Zr4T", "s3cr3tvalue99"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("-redact export still has %q from the notes", secret)
		}
	}
	if s := store.Get("claude:c1"); s.Note != "db password=hunter2hunter2" {
		t.Errorf("redacting the export changed the stored note: %q", s.Note)
	}
}

// -forget deletes the vector cache; notes live elsewhere and survive it.
func TestNotesOutsideCache(t *testing.T) {
	n, c := notesPath(), cacheDir()
	if n == "" || c == "" {
		t.Skip("no config or cache directory")
	}
	if strings.HasPrefix(n, c+string(filepath.Separator)) {
		t.Errorf("notes %s are inside the cache %s that -forget deletes", n, c)
	}
}

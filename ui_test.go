package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUISmoke loads the page in headless Chrome against the fixtures, opens
// every session, uses the palette and the keyboard, and opens an exported
// page, failing on any uncaught exception or console error. It talks to
// Chrome over --remote-debugging-pipe, so it needs no driver or module.
//
// It skips when no Chrome or Chromium is found; set TRAIL_CHROME to the
// binary to pick one, or TRAIL_UI_TEST=1 to fail instead of skipping (CI does).
func TestUISmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a browser")
	}
	bin := findChrome()
	if bin == "" {
		if os.Getenv("TRAIL_UI_TEST") != "" {
			t.Fatal("TRAIL_UI_TEST is set but no Chrome or Chromium was found; set TRAIL_CHROME")
		}
		t.Skip("no Chrome or Chromium found; set TRAIL_CHROME to run the UI smoke test")
	}

	idx := exportIndex() // every provider's fixtures
	sessions := idx.Sessions()
	mux := http.NewServeMux()
	mux.Handle("/", (&Server{index: idx, search: NewSearch(idx), home: "/work", version: "test"}).Handler())
	// The export, served as a page rather than a download.
	mux.HandleFunc("GET /exported", func(w http.ResponseWriter, r *http.Request) {
		tr, err := idx.Load(r.URL.Query().Get("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		ExportHTML(r.Context(), w, tr, "/work", "test")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := startChrome(t, bin)
	b.navigate(srv.URL + "/")

	// Home: the sidebar lists every session and the page counts them.
	b.waitFor(fmt.Sprintf(`document.querySelectorAll(".tree .sess").length === %d`, len(sessions)))
	b.waitFor(`document.querySelector(".home h1")?.textContent.includes("sessions across")`)

	// Every session renders its transcript, changes and trace.
	for _, s := range sessions {
		b.eval(fmt.Sprintf(`location.hash = "s=" + encodeURIComponent(%q)`, s.Key))
		b.waitFor(fmt.Sprintf(`document.querySelector(".tree .sess[aria-current=true]")?.dataset.key === %q && !!document.querySelector(".session .log-inner > *")`, s.Key))
		b.eval(`document.querySelector('.pane-tabs [data-pane="ch"]').click()`)
		b.eval(`document.querySelectorAll(".log details.tool").forEach((d) => (d.open = true))`)
		b.eval(`document.querySelectorAll('[data-f]').forEach((f) => f.click())`)
	}
	b.check()

	// j and k move between sessions in the sidebar.
	first := b.evalString(`document.querySelector(".tree .sess").dataset.key`)
	b.eval(fmt.Sprintf(`location.hash = "s=" + encodeURIComponent(%q)`, first))
	b.waitFor(fmt.Sprintf(`document.querySelector(".tree .sess[aria-current=true]")?.dataset.key === %q`, first))
	b.key("j", 0)
	b.waitFor(fmt.Sprintf(`document.querySelector(".tree .sess[aria-current=true]")?.dataset.key !== %q`, first))
	b.key("k", 0)
	b.waitFor(fmt.Sprintf(`document.querySelector(".tree .sess[aria-current=true]")?.dataset.key === %q`, first))

	// The palette opens on Ctrl+K, finds a session by name and by transcript
	// text, and Enter opens the selection.
	b.key("k", 2)
	b.waitFor(`!document.querySelector("#palette").hidden && document.activeElement?.id === "palette-q"`)
	b.call("Input.insertText", map[string]any{"text": "rounding"})
	b.waitFor(`[...document.querySelectorAll("#palette-list .prow .t")].some((t) => t.textContent.includes("Fix cart total rounding"))`)
	b.waitFor(`!!document.querySelector("#palette-list .hrow")`)
	b.key("ArrowDown", 0)
	picked := b.evalString(`document.querySelector("#palette-list .prow[aria-selected=true] .t").textContent`)
	b.key("Enter", 0)
	b.waitFor(fmt.Sprintf(`document.querySelector("#palette").hidden && document.querySelector(".tree .sess[aria-current=true] .t")?.textContent === %q`, picked))

	// The sidebar filter narrows the list.
	b.key("/", 0)
	b.call("Input.insertText", map[string]any{"text": "rounding"})
	b.waitFor(fmt.Sprintf(`(n => n > 0 && n < %d)(document.querySelectorAll(".tree .sess").length)`, len(sessions)))
	b.key("Escape", 0)
	b.waitFor(fmt.Sprintf(`document.querySelectorAll(".tree .sess").length === %d`, len(sessions)))
	b.check()

	// An exported page renders on its own, with no server behind it.
	b.navigate(srv.URL + "/exported?id=" + sessions[0].Key)
	b.waitFor(`!!document.getElementById("trail-export") && !!document.querySelector(".session .log-inner .ev")`)
	b.check()
}

// findChrome returns a Chrome or Chromium binary, or "".
func findChrome() string {
	if p := os.Getenv("TRAIL_CHROME"); p != "" {
		return p
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	case "windows":
		paths = []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`}
	}
	// Playwright's download, when it's there.
	if dir := os.Getenv("PLAYWRIGHT_BROWSERS_PATH"); dir != "" {
		m, _ := filepath.Glob(filepath.Join(dir, "chromium-*", "chrome-linux", "chrome"))
		paths = append(paths, m...)
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// browser is one headless Chrome page driven over the DevTools protocol.
type browser struct {
	t       *testing.T
	in      *os.File // commands to Chrome
	session string
	mu      sync.Mutex
	nextID  int
	waiting map[int]chan cdpMessage
	errs    []string // uncaught exceptions and console errors
}

type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startChrome(t *testing.T, bin string) *browser {
	t.Helper()
	// Chrome reads commands from fd 3 and writes to fd 4, each message ending
	// in a NUL byte.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--headless=new", "--remote-debugging-pipe", "--user-data-dir=" + t.TempDir(),
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--disable-extensions",
		"--window-size=1280,900", "about:blank",
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		args = append([]string{"--no-sandbox"}, args...) // Chrome refuses to sandbox as root
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.ExtraFiles = []*os.File{cmdR, outW}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cmdR.Close()
	outW.Close()
	b := &browser{t: t, in: cmdW, waiting: map[int]chan cdpMessage{}}
	done := make(chan struct{})
	t.Cleanup(func() {
		b.send("Browser.close", nil, "")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		cancel()
		cmd.Wait()
		cmdW.Close()
		if t.Failed() {
			var keep []string
			for _, l := range strings.Split(stderr.String(), "\n") {
				if l != "" && !strings.Contains(l, "dbus") {
					keep = append(keep, l)
				}
			}
			if len(keep) > 0 {
				t.Logf("chrome stderr:\n%s", clip(strings.Join(keep, "\n"), 4000))
			}
		}
	})
	go func() {
		defer close(done)
		r := bufio.NewReader(outR)
		for {
			line, err := r.ReadBytes(0)
			if err != nil {
				return
			}
			var m cdpMessage
			if json.Unmarshal(line[:len(line)-1], &m) != nil {
				continue
			}
			b.mu.Lock()
			if ch, ok := b.waiting[m.ID]; ok && m.ID != 0 {
				delete(b.waiting, m.ID)
				ch <- m
			} else {
				b.event(m)
			}
			b.mu.Unlock()
		}
	}()

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	b.decode(b.callIn("", "Target.getTargets", nil), &targets)
	for _, ti := range targets.TargetInfos {
		if ti.Type == "page" {
			var att struct {
				SessionID string `json:"sessionId"`
			}
			b.decode(b.callIn("", "Target.attachToTarget", map[string]any{"targetId": ti.TargetID, "flatten": true}), &att)
			b.session = att.SessionID
			break
		}
	}
	if b.session == "" {
		t.Fatal("chrome opened no page")
	}
	b.call("Runtime.enable", nil)
	b.call("Log.enable", nil)
	b.call("Page.enable", nil)
	return b
}

// event records anything that means the page broke. Called with mu held.
func (b *browser) event(m cdpMessage) {
	switch m.Method {
	case "Runtime.exceptionThrown":
		var p struct {
			ExceptionDetails struct {
				Text      string `json:"text"`
				URL       string `json:"url"`
				Line      int    `json:"lineNumber"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		json.Unmarshal(m.Params, &p)
		d := p.ExceptionDetails
		b.errs = append(b.errs, fmt.Sprintf("uncaught at %s:%d: %s %s", d.URL, d.Line+1, d.Text, d.Exception.Description))
	case "Runtime.consoleAPICalled":
		var p struct {
			Type string `json:"type"`
			Args []struct {
				Value       any    `json:"value"`
				Description string `json:"description"`
			} `json:"args"`
		}
		json.Unmarshal(m.Params, &p)
		if p.Type == "error" || p.Type == "assert" {
			var parts []string
			for _, a := range p.Args {
				if a.Description != "" {
					parts = append(parts, a.Description)
				} else {
					parts = append(parts, fmt.Sprint(a.Value))
				}
			}
			b.errs = append(b.errs, "console."+p.Type+": "+strings.Join(parts, " "))
		}
	case "Log.entryAdded":
		// Failed loads and Content-Security-Policy violations land here.
		var p struct {
			Entry struct {
				Level, Source, Text, URL string
			} `json:"entry"`
		}
		json.Unmarshal(m.Params, &p)
		if p.Entry.Level == "error" {
			b.errs = append(b.errs, fmt.Sprintf("%s error: %s %s", p.Entry.Source, p.Entry.Text, p.Entry.URL))
		}
	}
}

// check fails the test if the page has logged an error so far.
func (b *browser) check() {
	b.t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.errs {
		b.t.Error(e)
	}
	if len(b.errs) > 0 {
		b.t.FailNow()
	}
}

func (b *browser) send(method string, params any, session string) chan cdpMessage {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan cdpMessage, 1)
	b.waiting[id] = ch
	b.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params, "sessionId": session})
	if params == nil {
		msg, _ = json.Marshal(map[string]any{"id": id, "method": method, "sessionId": session})
	}
	b.in.Write(append(msg, 0))
	return ch
}

func (b *browser) callIn(session, method string, params any) json.RawMessage {
	b.t.Helper()
	select {
	case m := <-b.send(method, params, session):
		if m.Error != nil {
			b.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		return m.Result
	case <-time.After(15 * time.Second):
		b.t.Fatalf("%s: chrome didn't answer", method)
	}
	return nil
}

func (b *browser) call(method string, params any) json.RawMessage {
	b.t.Helper()
	return b.callIn(b.session, method, params)
}

func (b *browser) decode(raw json.RawMessage, v any) {
	b.t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		b.t.Fatal(err)
	}
}

func (b *browser) navigate(url string) {
	b.t.Helper()
	b.call("Page.navigate", map[string]any{"url": url})
	b.waitFor(`document.readyState === "complete"`)
}

// eval runs a script in the page and returns its value, failing on an
// exception.
func (b *browser) eval(expr string) any {
	b.t.Helper()
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	b.decode(b.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}), &r)
	if e := r.ExceptionDetails; e != nil {
		b.t.Fatalf("%s\n%s %s", expr, e.Text, e.Exception.Description)
	}
	return r.Result.Value
}

func (b *browser) evalString(expr string) string {
	b.t.Helper()
	s, _ := b.eval(expr).(string)
	return s
}

// waitFor polls a script until it returns true.
func (b *browser) waitFor(expr string) {
	b.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, _ := b.eval(expr).(bool); v {
			return
		}
		b.check() // a broken page won't get there; say why now
		if time.Now().After(deadline) {
			b.mu.Lock()
			errs := strings.Join(b.errs, "\n")
			b.mu.Unlock()
			b.t.Fatalf("timed out waiting for %s\npage errors: %s\nbody: %s", expr, errs, clip(b.evalString(`document.body.innerText`), 1500))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// key presses a key, with modifiers as the protocol numbers them (2 is Ctrl).
func (b *browser) key(key string, modifiers int) {
	b.t.Helper()
	code := map[string]string{"Enter": "Enter", "Escape": "Escape", "ArrowDown": "ArrowDown", "/": "Slash"}[key]
	vk := map[string]int{"Enter": 13, "Escape": 27, "ArrowDown": 40, "/": 191}[key]
	if code == "" && len(key) == 1 {
		code = "Key" + strings.ToUpper(key)
		vk = int(strings.ToUpper(key)[0])
	}
	p := map[string]any{"key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers}
	down := map[string]any{"type": "keyDown"}
	if len(key) == 1 && modifiers == 0 {
		down["text"] = key
	}
	if key == "Enter" {
		down["text"] = "\r"
	}
	for k, v := range p {
		down[k] = v
	}
	b.call("Input.dispatchKeyEvent", down)
	up := map[string]any{"type": "keyUp"}
	for k, v := range p {
		up[k] = v
	}
	b.call("Input.dispatchKeyEvent", up)
}

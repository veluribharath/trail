package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpIndex() *Index {
	return NewIndex(&claudeProvider{dir: "testdata/claude"}, &codexProvider{dir: "testdata/codex"}, &opencodeProvider{dir: "testdata/opencode"}, &geminiProvider{dir: "testdata/gemini"},
		&kiroProvider{dir: "testdata/kiro/home", dataDir: "testdata/kiro/data"})
}

// mcpSession sends lines to a server and returns its replies by id.
func mcpSession(t *testing.T, m *mcpServer, lines ...string) map[string]rpcReply {
	t.Helper()
	var out bytes.Buffer
	if err := m.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]rpcReply{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		if l[0] == '[' {
			var batch []rpcReply
			if err := json.Unmarshal([]byte(l), &batch); err != nil {
				t.Fatalf("bad batch %q: %v", l, err)
			}
			for _, r := range batch {
				got[string(r.ID)] = r
			}
			continue
		}
		var r rpcReply
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("bad reply %q: %v", l, err)
		}
		got[string(r.ID)] = r
	}
	return got
}

func toolText(t *testing.T, r rpcReply) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("rpc error: %v", r.Error.Message)
	}
	var res struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		t.Fatalf("bad tool result %s", r.Result)
	}
	return res.Content[0].Text, res.IsError
}

func call(id int, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

func TestMCPProtocol(t *testing.T) {
	m := newMCPServer(mcpIndex(), nil, "1.2.3")
	m.uiPath = ""
	got := mcpSession(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`not json`,
		`[{"jsonrpc":"2.0","id":6,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/cancelled"}]`,
	)
	var init struct {
		ProtocolVersion string
		ServerInfo      struct{ Name, Version string }
	}
	json.Unmarshal(got["1"].Result, &init)
	if init.ProtocolVersion != "2025-03-26" || init.ServerInfo.Name != "trail" || init.ServerInfo.Version != "1.2.3" {
		t.Errorf("initialize: %s", got["1"].Result)
	}
	var list struct{ Tools []struct{ Name string } }
	json.Unmarshal(got["2"].Result, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "search_sessions,list_sessions,get_session" {
		t.Errorf("tools: %v", names)
	}
	if string(got["3"].Result) != "{}" {
		t.Errorf("ping: %s", got["3"].Result)
	}
	if got["4"].Error == nil || got["4"].Error.Code != -32601 {
		t.Errorf("unknown method: %+v", got["4"])
	}
	if got["5"].Error == nil || got["5"].Error.Code != -32602 {
		t.Errorf("unknown tool: %+v", got["5"])
	}
	if got["null"].Error == nil || got["null"].Error.Code != -32700 {
		t.Errorf("parse error: %+v", got["null"])
	}
	if len(got) != 7 {
		t.Errorf("notifications must not be answered; got %d replies", len(got))
	}
}

func TestMCPTools(t *testing.T) {
	m := newMCPServer(mcpIndex(), nil, "dev")
	m.uiPath = ""
	got := mcpSession(t, m,
		call(1, "search_sessions", `{"query":"rounding"}`),
		call(2, "search_sessions", `{"query":"rounding","project":"/work/api"}`),
		call(3, "list_sessions", `{"project":"shop","agent":"claude"}`),
		call(4, "get_session", `{"id":"c1","steps":2}`),
		call(5, "get_session", `{"id":"c"}`),
		call(6, "search_sessions", `{"query":""}`),
	)
	if s, _ := toolText(t, got["1"]); !strings.Contains(s, "claude:c1") || !strings.Contains(s, "get_session(id, from=step)") {
		t.Errorf("search:\n%s", s)
	}
	if s, _ := toolText(t, got["2"]); !strings.HasPrefix(s, "No sessions matched") {
		t.Errorf("search outside the project:\n%s", s)
	}
	s, _ := toolText(t, got["3"])
	if !strings.Contains(s, "claude:c1") || strings.Contains(s, "gemini") || strings.Contains(s, "/work/api") {
		t.Errorf("list:\n%s", s)
	}
	s, isErr := toolText(t, got["4"])
	if isErr || !strings.Contains(s, "# Fix cart total rounding") || !strings.Contains(s, "Steps 1 to 2 of") || !strings.Contains(s, `from=3`) {
		t.Errorf("get_session:\n%s", s)
	}
	if s, isErr := toolText(t, got["5"]); !isErr || !strings.Contains(s, "matches 3 sessions") {
		t.Errorf("ambiguous id: %v %s", isErr, s)
	}
	if _, isErr := toolText(t, got["6"]); !isErr {
		t.Error("empty query should be a tool error")
	}
}

func TestMCPRedacts(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-work-keys")
	os.MkdirAll(proj, 0o755)
	key := "sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	line := `{"type":"user","sessionId":"k1","cwd":"/work/keys","timestamp":"2026-10-01T10:00:00Z","uuid":"u1","message":{"role":"user","content":"deploy with ` + key + ` please"}}`
	os.WriteFile(filepath.Join(proj, "k1.jsonl"), []byte(line+"\n"), 0o644)
	m := newMCPServer(NewIndex(&claudeProvider{dir: dir}), nil, "dev")
	m.uiPath = ""
	got := mcpSession(t, m, call(1, "search_sessions", `{"query":"deploy"}`), call(2, "get_session", `{"id":"k1"}`), call(3, "list_sessions", `{}`))
	for id, r := range got {
		s, _ := toolText(t, r)
		if strings.Contains(s, "sk-ant") {
			t.Errorf("reply %s leaks the key:\n%s", id, s)
		}
		if id == "2" && !strings.Contains(s, redacted) {
			t.Errorf("get_session should show where the key was:\n%s", s)
		}
	}
}

func TestMCPUsesRunningWindow(t *testing.T) {
	ui := mcpIndex()
	ui.Scan(0)
	srch := NewSearch(ui)
	srch.Sync() // a window starts reading transcripts as it opens
	srv := httptest.NewServer((&Server{index: ui, search: srch, home: "/work", version: "dev"}).Handler())
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "ui.json")
	b, _ := json.Marshal(uiAnnounce{URL: srv.URL})
	os.WriteFile(path, b, 0o600)

	// The same folders: the window answers, and this process reads nothing.
	idx := mcpIndex()
	m := newMCPServer(idx, nil, "dev")
	m.uiPath = path
	got := mcpSession(t, m, call(1, "search_sessions", `{"query":"rounding"}`), call(2, "get_session", `{"id":"claude:c1","steps":1}`))
	if s, _ := toolText(t, got["1"]); !strings.Contains(s, "claude:c1") {
		t.Errorf("search through the window:\n%s", s)
	}
	if s, _ := toolText(t, got["2"]); !strings.Contains(s, "Fix cart total rounding") {
		t.Errorf("get_session through the window:\n%s", s)
	}
	if len(idx.Sessions()) != 0 || m.useLocal {
		t.Error("the local index should not have been built")
	}

	// Different folders: the window is ignored.
	other := NewIndex(&claudeProvider{dir: "testdata/claude"})
	m2 := newMCPServer(other, nil, "dev")
	m2.uiPath = path
	if m2.findUI() != nil {
		t.Error("a window over other folders must not be used")
	}

	// The window went away: fall back to the local index.
	srv.Close()
	m3 := newMCPServer(mcpIndex(), nil, "dev")
	m3.uiPath = path
	got = mcpSession(t, m3, call(1, "list_sessions", `{"agent":"claude"}`))
	if s, _ := toolText(t, got["1"]); !strings.Contains(s, "claude:c1") {
		t.Errorf("fallback:\n%s", s)
	}
}

func TestAnnounceUI(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	remove := announceUI("http://127.0.0.1:7878")
	b, err := os.ReadFile(uiFile())
	if err != nil || !strings.Contains(string(b), "127.0.0.1:7878") {
		t.Fatalf("announce: %s %v", b, err)
	}
	remove()
	if _, err := os.Stat(uiFile()); !os.IsNotExist(err) {
		t.Error("remove should delete its own record")
	}
	announceUI("http://192.168.1.5:7878")
	if _, err := os.Stat(uiFile()); !os.IsNotExist(err) {
		t.Error("a window off loopback must not be announced")
	}
}

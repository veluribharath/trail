// trail is a read-only browser for coding-agent sessions. It finds the session
// logs that Claude Code and Codex leave on disk, files them under the project
// folders they ran in, and shows each transcript next to the changes it made.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// version is set by -ldflags in release builds. `go install module@vX.Y.Z`
// leaves it unset, so fall back to the module version Go recorded.
var version = ""

func currentVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

func main() {
	host := flag.String("host", "127.0.0.1", "address to listen on; use 0.0.0.0 to browse from another machine")
	port := flag.Int("port", 7878, "port to listen on (0 picks a free one)")
	claudeDir := flag.String("claude-dir", envOr("CLAUDE_CONFIG_DIR", filepath.Join(homeDir(), ".claude")), "Claude Code config directory")
	codexDir := flag.String("codex-dir", envOr("CODEX_HOME", filepath.Join(homeDir(), ".codex")), "Codex home directory")
	open := flag.Bool("open", false, "open the browser after starting")
	list := flag.Bool("list", false, "print sessions grouped by folder and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("trail", currentVersion())
		return
	}

	idx := NewIndex(&claudeProvider{dir: *claudeDir}, &codexProvider{dir: *codexDir})
	start := time.Now()
	status := idx.Scan(0)

	if *list {
		printList(idx)
		return
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "trail:", err)
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String()
	if *host == "0.0.0.0" || *host == "::" {
		url = "http://localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	}

	fmt.Printf("trail %s\n", currentVersion())
	for _, s := range status {
		state := "not found"
		if s.Found {
			state = fmt.Sprintf("%d sessions", s.Sessions)
		}
		fmt.Printf("  %-7s %-40s %s\n", s.Name, s.Root, state)
	}
	fmt.Printf("  indexed in %s\n  open %s\n", time.Since(start).Round(time.Millisecond), url)
	if *host != "127.0.0.1" && *host != "localhost" {
		fmt.Println("  warning: transcripts can contain secrets, and anyone who can reach this port can read them")
	}
	if *open {
		openBrowser(url)
	}

	srv := &Server{index: idx, home: homeDir(), version: currentVersion()}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := httpSrv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "trail:", err)
		os.Exit(1)
	}
}

func printList(idx *Index) {
	byRoot := map[string][]*Session{}
	var order []string
	for _, s := range idx.Sessions() {
		if _, ok := byRoot[s.Root]; !ok {
			order = append(order, s.Root)
		}
		byRoot[s.Root] = append(byRoot[s.Root], s)
	}
	for _, root := range order {
		fmt.Println(root)
		for _, s := range byRoot[root] {
			fmt.Printf("  %-6s %s  %-60s %3d edits  %s\n", s.Provider, s.Ended.Local().Format("2006-01-02 15:04"),
				oneLine(s.Title, 60), s.Edits, s.ID)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// trail is a read-only browser for coding-agent sessions. It finds the session
// logs that Claude Code, Codex, OpenCode, Gemini CLI and Kiro CLI leave on disk, files them under the project
// folders they ran in, and shows each transcript next to the changes it made.
package main

import (
	"bytes"
	"context"
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
	geminiDir := flag.String("gemini-dir", filepath.Join(envOr("GEMINI_CLI_HOME", homeDir()), ".gemini"), "Gemini CLI directory")
	opencodeDir := flag.String("opencode-dir", filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(homeDir(), ".local", "share")), "opencode"), "OpenCode data directory (holds opencode.db)")
	kiroDir := flag.String("kiro-dir", filepath.Join(homeDir(), ".kiro"), "Kiro CLI directory (holds sessions/)")
	kiroDataDir := flag.String("kiro-data-dir", kiroDefaultDataDir(), "Kiro CLI data directory (holds data.sqlite3)")
	open := flag.Bool("open", false, "open the browser after starting")
	list := flag.Bool("list", false, "print sessions grouped by folder and exit")
	search := flag.String("search", "", "print sessions whose transcripts match a query and exit")
	semantic := flag.Bool("semantic", false, "also find steps by meaning, using a local embedding model (see -embed-url)")
	embedURL := flag.String("embed-url", "http://127.0.0.1:11434", "embedding server for -semantic: Ollama, or an OpenAI-compatible URL ending in /v1")
	embedModel := flag.String("embed-model", "nomic-embed-text", "embedding model for -semantic")
	forget := flag.Bool("forget", false, "delete the vectors -semantic cached and exit")
	showPrices := flag.Bool("prices", false, "print the token prices used for API-equivalent costs and exit")
	stats := flag.String("stats", "", "print activity for the last N days (or all) and exit")
	export := flag.String("export", "", "write one session (id, provider:id, or id prefix) as a single offline HTML file and exit")
	out := flag.String("o", "", "file for -export to write; without it the page goes to standard output")
	retro := flag.String("retro", "", "print suggested CLAUDE.md or AGENTS.md rules from a project's sessions (a folder, or . for this one) and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if err := loadPriceOverrides(pricesPath()); err != nil {
		fmt.Fprintln(os.Stderr, "trail: ignoring price overrides:", err)
	}
	if *showPrices {
		printPrices(os.Stdout)
		return
	}

	if *showVersion {
		fmt.Println("trail", currentVersion())
		return
	}

	if *forget {
		dir := cacheDir()
		if dir == "" {
			fmt.Fprintln(os.Stderr, "trail: no cache directory on this system")
			os.Exit(1)
		}
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(os.Stderr, "trail:", err)
			os.Exit(1)
		}
		fmt.Println("deleted", dir)
		return
	}

	idx := NewIndex(&claudeProvider{dir: *claudeDir}, &codexProvider{dir: *codexDir}, &opencodeProvider{dir: *opencodeDir}, &geminiProvider{dir: *geminiDir},
		&kiroProvider{dir: *kiroDir, dataDir: *kiroDataDir})
	start := time.Now()
	status := idx.Scan(0)

	if *list {
		printList(idx)
		return
	}
	if *export != "" {
		if err := exportSession(idx, *export, *out); err != nil {
			fmt.Fprintln(os.Stderr, "trail:", err)
			os.Exit(1)
		}
		return
	}
	if *retro != "" {
		root, err := retroRoot(idx, *retro)
		if err != nil {
			fmt.Fprintln(os.Stderr, "trail:", err)
			os.Exit(1)
		}
		r, _ := NewRetroCache(idx).Build(root)
		printRetro(os.Stdout, r, homeDir())
		return
	}
	srch := NewSearch(idx)
	var sem *Semantic
	if *semantic {
		sem = NewSemantic(idx, srch, newHTTPEmbedder(*embedURL, *embedModel), *embedModel, cacheDir())
	}
	if *search != "" {
		srch.Sync()
		if sem != nil {
			if err := sem.Sync(context.Background()); err != nil {
				fmt.Fprintln(os.Stderr, "trail: meaning search is off for this run:", err)
				sem = nil
			}
		}
		printSearch(idx, srch, sem, *search)
		return
	}
	if *stats != "" {
		if *stats != "all" {
			if n, err := strconv.Atoi(*stats); err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, "trail: -stats takes a number of days, or all")
				os.Exit(2)
			}
		}
		srch.Sync()
		srv := &Server{index: idx, search: srch, home: homeDir()}
		printStats(srv.stats(*stats, "", ""), *stats, homeDir())
		return
	}
	srch.Kick()
	if sem != nil {
		sem.Kick()
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
		fmt.Printf("  %-8s %-40s %s\n", s.Name, s.Root, state)
	}
	fmt.Printf("  indexed in %s\n  open %s\n", time.Since(start).Round(time.Millisecond), url)
	if sem != nil {
		fmt.Printf("  semantic %s at %s, vectors cached in %s\n", *embedModel, *embedURL, cacheDir())
		if !isLoopback(*embedURL) {
			fmt.Println("  warning: -embed-url is not on this machine, so transcript text will be sent to it")
		}
	}
	if *host != "127.0.0.1" && *host != "localhost" {
		fmt.Println("  warning: transcripts can contain secrets, and anyone who can reach this port can read them")
	}
	if *open {
		openBrowser(url)
	}

	srv := &Server{index: idx, search: srch, semantic: sem, retro: NewRetroCache(idx), home: homeDir(), version: currentVersion()}
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
			fmt.Printf("  %-8s %s  %-60s %3d edits  %s\n", s.Provider, s.Ended.Local().Format("2006-01-02 15:04"),
				oneLine(s.Title, 60), s.Edits, s.ID)
		}
	}
}

// exportSession writes one session's page to the file the person named, or
// to standard output when it isn't a terminal. Nothing else is written.
func exportSession(idx *Index, arg, path string) error {
	s, err := findSession(idx, arg)
	if err != nil {
		return err
	}
	t, err := idx.Load(s.Key)
	if err != nil {
		return err
	}
	if path == "" || path == "-" {
		if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("-export writes HTML; name a file with -o %s, or redirect the output", exportName(s))
		}
		return ExportHTML(context.Background(), os.Stdout, t, homeDir(), currentVersion())
	}
	var b bytes.Buffer
	if err := ExportHTML(context.Background(), &b, t, homeDir(), currentVersion()); err != nil {
		return err
	}
	// Readable only by you: the file holds the whole transcript.
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%s, %d steps)\n", path, byteSize(b.Len()), len(t.Events))
	fmt.Fprintln(os.Stderr, "transcripts can contain secrets (keys, tokens, file contents, command output); read it before you share it")
	return nil
}

func byteSize(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func printSearch(idx *Index, srch *Search, sem *Semantic, q string) {
	pq := ParseQuery(q + " ")
	res := srch.Run(pq, "", 50)
	if sem != nil {
		res = sem.Blend(context.Background(), pq, res, "", 50)
		if res.Semantic.Error != "" {
			fmt.Fprintln(os.Stderr, "trail: meaning search failed:", res.Semantic.Error)
		}
	}
	if len(res.Sessions) == 0 {
		fmt.Println("no matches")
		return
	}
	for _, sh := range res.Sessions {
		s := idx.Get(sh.Key)
		if s == nil {
			continue
		}
		fmt.Printf("%s  %s  %s  (%d %s)\n", s.Ended.Local().Format("2006-01-02 15:04"), oneLine(s.Title, 60), s.ID, sh.Count, plural(sh.Count, "step", "steps"))
		for _, h := range sh.Hits {
			var b strings.Builder
			for _, seg := range h.Snippet {
				b.WriteString(seg.Text)
			}
			fmt.Printf("    #%-4d %-8s %s\n", h.Event+1, h.Field, oneLine(b.String(), 100))
		}
	}
	if res.More {
		fmt.Println("(more sessions matched; narrow the query)")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// kiroDefaultDataDir is where kiro-cli keeps data.sqlite3: the platform's
// local data directory.
func kiroDefaultDataDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Application Support", "kiro-cli")
	case "windows":
		return filepath.Join(envOr("LOCALAPPDATA", filepath.Join(homeDir(), "AppData", "Local")), "kiro-cli")
	}
	return filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(homeDir(), ".local", "share")), "kiro-cli")
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

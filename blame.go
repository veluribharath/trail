package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

// Blame answers "which session wrote this line?" from what is already on
// disk: git blame says which commit last brought each line in, and the
// sessions' own transcripts say which edits put that text there. Nothing has
// to be installed before the agent works, and nothing leaves the machine.
//
// Commits rarely map one to one onto sessions: a person may commit the work of
// several sessions at once, squash it, or commit hours later. So a line is
// matched by content rather than by time alone. Its source is the latest
// recorded edit to the same file that introduced that exact text and was made
// before the commit (committer time, so amends and rebases still fit). Lines
// that aren't committed yet can match any edit. Short lines such as "}" only
// count when a neighbouring line from the same commit came from the same edit.
//
// Agents also write files from the shell (a heredoc, sed -i, a script). Those
// steps record no edit, so when no edit matches, a shell command that names
// the file and contains the line's text counts, marked as a weaker match.

// BlameResult is one file, line by line.
type BlameResult struct {
	Available bool          `json:"available"`
	Reason    string        `json:"reason,omitempty"`
	Path      string        `json:"path"`               // absolute
	Rel       string        `json:"rel"`                // relative to the repository, or the file name
	Toplevel  string        `json:"toplevel,omitempty"` // empty when the file isn't in a git repository
	Lines     []BlameLine   `json:"lines"`
	Commits   []BlameCommit `json:"commits"`
	Sources   []BlameSource `json:"sources"`
	Truncated bool          `json:"truncated,omitempty"`
	Searched  int           `json:"searched"` // sessions whose edits were read
}

// BlameLine is one line of the file. Commit and Source index into the
// result's lists; -1 means not committed yet, and no session's edit.
type BlameLine struct {
	Text   string `json:"text"`
	Commit int    `json:"commit"`
	Source int    `json:"source"`
}

type BlameCommit struct {
	SHA     string    `json:"sha"`
	Short   string    `json:"short"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"` // author time, as git log shows it
	Subject string    `json:"subject"`
	File    string    `json:"file"` // the file's path in that commit, which differs after a rename
	commit  time.Time // committer time: edits after it can't be in the commit
}

// BlameSource is the step of a session that wrote some lines.
type BlameSource struct {
	Key      string    `json:"key"`
	Provider string    `json:"provider"`
	Title    string    `json:"title"`
	Resume   string    `json:"resume,omitempty"`
	Change   int       `json:"change"` // index into the transcript's changes; -1 for a shell command
	Event    int       `json:"event"`  // index into its events: the step
	Steps    int       `json:"steps"`
	Time     time.Time `json:"time"`
	Op       string    `json:"op"`                // the change's op, or "shell"
	Summary  string    `json:"summary,omitempty"` // the tool call, in one line
	Prompt   string    `json:"prompt,omitempty"`  // the request the step answered
	Lines    int       `json:"lines"`             // lines of the file it wrote
}

const (
	blameSlack      = 10 * time.Minute // clocks and timestamps disagree a little
	blameMaxBytes   = 8 << 20
	blameMaxLines   = 50000
	blameMaxSession = 200 // sessions read per file, newest first
	blameNear       = 3   // lines either side that can vouch for a short line
)

// edit is one recorded change to the blamed file, with the lines it added.
type edit struct {
	t     *Transcript
	ci    int // change index, -1 for a shell command
	ev    int
	when  time.Time
	added map[string]bool
	shell bool
	src   int // index into Sources once used, else -1
}

// Blame attributes each line of the file at path.
func Blame(ctx context.Context, idx *Index, path string) BlameResult {
	res := BlameResult{Lines: []BlameLine{}, Commits: []BlameCommit{}, Sources: []BlameSource{}}
	abs, err := filepath.Abs(path)
	if err != nil {
		res.Reason = err.Error()
		return res
	}
	abs = realPath(abs)
	res.Path, res.Rel = abs, filepath.Base(abs)
	fi, err := os.Stat(abs)
	switch {
	case err != nil:
		res.Reason = "There's no file at " + abs + "."
		return res
	case fi.IsDir():
		res.Reason = abs + " is a folder. Name a file in it."
		return res
	case !fi.Mode().IsRegular():
		res.Reason = abs + " isn't a regular file."
		return res
	case fi.Size() > blameMaxBytes:
		res.Reason = "That file is larger than 8 MB."
		return res
	}

	commitAt := map[string]int{}
	lineSHA := []string{}
	if _, err := exec.LookPath("git"); err == nil {
		if top, err := git(ctx, filepath.Dir(abs), "rev-parse", "--show-toplevel"); err == nil {
			res.Toplevel = strings.TrimSpace(top)
			if r, err := filepath.Rel(res.Toplevel, abs); err == nil && !strings.HasPrefix(r, "..") {
				res.Rel = filepath.ToSlash(r)
			}
			if out, err := git(ctx, res.Toplevel, "blame", "--line-porcelain", "--", res.Rel); err == nil {
				lineSHA = parseBlame(out, &res, commitAt)
			}
			// Otherwise the file isn't tracked yet (or the repository has no
			// commits): every line is uncommitted, read below.
		}
	}
	if len(res.Lines) == 0 {
		b, err := os.ReadFile(abs)
		if err != nil {
			res.Reason = err.Error()
			return res
		}
		text := strings.TrimSuffix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		if text != "" {
			for _, l := range strings.Split(text, "\n") {
				if len(res.Lines) == blameMaxLines {
					res.Truncated = true
					break
				}
				res.Lines = append(res.Lines, BlameLine{Text: l, Commit: -1, Source: -1})
			}
		}
		lineSHA = make([]string, len(res.Lines))
	}
	res.Available = true

	// The file under every name it had: renames show up as a commit's file.
	names := map[string]bool{fileKey(idx, abs): true}
	bases := map[string]bool{filepath.Base(abs): true}
	if res.Toplevel != "" {
		for _, c := range res.Commits {
			p := filepath.Join(res.Toplevel, filepath.FromSlash(c.File))
			names[fileKey(idx, p)] = true
			bases[filepath.Base(p)] = true
		}
	}

	root, _ := idx.repoRoot(filepath.Dir(abs))
	edits := blameEdits(idx, root, names, bases, &res)
	if len(edits) == 0 {
		return res
	}
	byText := map[string][]int{}
	for i, e := range edits {
		for l := range e.added {
			byText[l] = append(byText[l], i)
		}
	}

	norms := make([]string, len(res.Lines))
	for i, l := range res.Lines {
		norms[i] = strings.TrimSpace(l.Text)
	}
	for i := range res.Lines {
		cands := byText[norms[i]]
		if len(cands) == 0 {
			continue
		}
		limit := time.Time{}
		if c := res.Lines[i].Commit; c >= 0 {
			limit = res.Commits[c].commit.Add(blameSlack)
		}
		best, bestScore := -1, 0
		for _, ei := range cands {
			e := edits[ei]
			if !limit.IsZero() && e.when.After(limit) {
				continue
			}
			// An edit tool's record always beats a shell command's.
			if best >= 0 && e.shell != edits[best].shell {
				if e.shell {
					continue
				}
				best, bestScore = -1, 0
			}
			// Neighbours from the same commit that this edit also added. Blank
			// lines are everywhere, so they never vouch.
			score := 1
			for j := max(0, i-blameNear); j <= min(len(res.Lines)-1, i+blameNear); j++ {
				if j != i && lineSHA[j] == lineSHA[i] && norms[j] != "" && e.added[norms[j]] {
					score++
				}
			}
			if score > bestScore || (score == bestScore && e.when.After(edits[best].when)) {
				best, bestScore = ei, score
			}
		}
		if best < 0 || (trivial(norms[i]) && bestScore < 2) {
			continue
		}
		e := edits[best]
		if e.src < 0 {
			e.src = len(res.Sources)
			res.Sources = append(res.Sources, blameSource(e))
		}
		res.Lines[i].Source = e.src
	}
	// A blank line between two lines from the same step is that step's too.
	for i := range res.Lines {
		if res.Lines[i].Source >= 0 || norms[i] != "" {
			continue
		}
		j := i
		for j < len(res.Lines) && norms[j] == "" {
			j++
		}
		if i > 0 && j < len(res.Lines) && res.Lines[i-1].Source >= 0 && res.Lines[i-1].Source == res.Lines[j].Source {
			for k := i; k < j; k++ {
				res.Lines[k].Source = res.Lines[i-1].Source
			}
		}
	}
	for _, l := range res.Lines {
		if l.Source >= 0 {
			res.Sources[l.Source].Lines++
		}
	}
	return res
}

// parseBlame reads `git blame --line-porcelain` into res, returning each
// line's commit id.
func parseBlame(out string, res *BlameResult, commitAt map[string]int) []string {
	var shas []string
	var sha string
	info := map[string]*BlameCommit{}
	var cur *BlameCommit
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "\t") {
			if len(res.Lines) == blameMaxLines {
				res.Truncated = true
				break
			}
			ci := -1
			if strings.Trim(sha, "0") != "" {
				var ok bool
				if ci, ok = commitAt[sha]; !ok {
					ci = len(res.Commits)
					commitAt[sha] = ci
					res.Commits = append(res.Commits, *cur)
				}
			}
			res.Lines = append(res.Lines, BlameLine{Text: l[1:], Commit: ci, Source: -1})
			shas = append(shas, sha)
			continue
		}
		if len(l) > 41 && l[40] == ' ' && shaRe.MatchString(l[:40]) {
			sha = l[:40]
			if cur = info[sha]; cur == nil {
				cur = &BlameCommit{SHA: sha, Short: sha[:7]}
				info[sha] = cur
			}
			continue
		}
		if cur == nil {
			continue
		}
		k, v, _ := strings.Cut(l, " ")
		switch k {
		case "author":
			cur.Author = v
		case "author-time":
			cur.When = unixTime(v)
		case "committer-time":
			cur.commit = unixTime(v)
		case "summary":
			cur.Subject = v
		case "filename":
			cur.File = v
		}
	}
	return shas
}

func unixTime(v string) time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// blameEdits reads the transcripts of sessions that edited the file under any
// of its names, then of other sessions in the same repository, and returns
// their edits to it (and shell commands that name it), oldest first.
func blameEdits(idx *Index, root string, names, bases map[string]bool, res *BlameResult) []*edit {
	hit := func(cwd, p string) bool {
		if p == "" || !bases[filepath.Base(p)] {
			return false
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return names[fileKey(idx, p)]
	}
	var keys, others []string
	for _, s := range idx.Sessions() {
		edited := false
		for _, f := range s.Files {
			if hit(s.Cwd, f) {
				edited = true
				break
			}
		}
		if edited {
			keys = append(keys, s.Key)
		} else if s.Root == root {
			others = append(others, s.Key)
		}
	}
	keys = append(keys, others...)
	if len(keys) > blameMaxSession {
		keys = keys[:blameMaxSession]
	}
	var edits []*edit
	for _, k := range keys {
		t, err := idx.Load(k)
		if err != nil || t == nil {
			continue
		}
		res.Searched++
		for ci, c := range t.Changes {
			if c.Op == "delete" || !(hit(t.Session.Cwd, c.Path) || hit(t.Session.Cwd, c.MoveTo)) {
				continue
			}
			e := &edit{t: t, ci: ci, ev: c.Event, when: c.Time, added: addedLines(c.Hunks), src: -1}
			if e.when.IsZero() {
				e.when = t.Session.Ended
			}
			if len(e.added) > 0 {
				edits = append(edits, e)
			}
		}
		for i, ev := range t.Events {
			if ev.Kind != "tool" || len(ev.Changes) > 0 || !shellWrites(ev.Input, bases) {
				continue
			}
			e := &edit{t: t, ci: -1, ev: i, when: ev.Time, added: map[string]bool{}, shell: true, src: -1}
			if e.when.IsZero() {
				e.when = t.Session.Ended
			}
			for _, l := range strings.Split(ev.Input, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					e.added[l] = true
				}
			}
			edits = append(edits, e)
		}
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].when.Before(edits[j].when) })
	return edits
}

// shellWrites says whether a command names the file and looks like it
// writes one: a redirect, a heredoc, an in-place edit or a script. Reading
// commands (grep, cat) quote lines too, so they must not count.
func shellWrites(cmd string, bases map[string]bool) bool {
	named := false
	for b := range bases {
		if strings.Contains(cmd, b) {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	for _, w := range []string{">", "<<", "sed -i", "perl -i", "perl -pi", "tee ", "python", "node -e", "ruby -e", "write"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}

// addedLines is the set of lines (trimmed) an edit brought in: those it has
// more of afterwards than before. Context lines a patch repeats don't count.
func addedLines(hunks []Hunk) map[string]bool {
	out := map[string]bool{}
	for _, h := range hunks {
		n := map[string]int{}
		for _, l := range strings.Split(h.New, "\n") {
			n[strings.TrimSpace(l)]++
		}
		if h.Old != "" {
			for _, l := range strings.Split(h.Old, "\n") {
				n[strings.TrimSpace(l)]--
			}
		}
		for l, c := range n {
			if c > 0 && (l != "" || h.New != "") {
				out[l] = true
			}
		}
	}
	return out
}

// trivial lines ("}", "", "end") are too common to identify an edit alone.
func trivial(s string) bool {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n < 4
}

func blameSource(e *edit) BlameSource {
	t, s := e.t, e.t.Session
	src := BlameSource{Key: s.Key, Provider: s.Provider, Title: s.Title, Resume: s.Resume,
		Change: e.ci, Event: e.ev, Steps: len(t.Events), Time: e.when, Op: "shell"}
	if e.ci >= 0 {
		src.Op = t.Changes[e.ci].Op
	}
	if e.ev >= 0 && e.ev < len(t.Events) {
		ev := t.Events[e.ev]
		src.Summary = oneLine(firstNonEmpty(ev.Summary, ev.Tool), 120)
		for i := e.ev; i >= 0; i-- {
			if u := t.Events[i]; u.Kind == "user" && !u.Sidechain && strings.TrimSpace(u.Text) != "" {
				src.Prompt = oneLine(u.Text, promptChars)
				break
			}
		}
	}
	return src
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// fileKey names a file the same way whichever checkout it was seen in: its
// repository plus its path inside it, so an edit made in a linked worktree
// matches the same file in the main checkout. Outside a repository it is the
// path itself.
func fileKey(idx *Index, p string) string {
	p = realPath(filepath.Clean(p))
	root, wt := idx.repoRoot(filepath.Dir(p))
	top := root
	if wt != "" {
		top = wt
	}
	if r, err := filepath.Rel(top, p); err == nil && !strings.HasPrefix(r, "..") {
		return root + "\x00" + filepath.ToSlash(r)
	}
	return p
}

// realPath resolves symlinks in a path's folder (macOS /var is /private/var),
// leaving it unchanged when the folder no longer exists.
func realPath(p string) string {
	if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(d, filepath.Base(p))
	}
	return p
}

// parseBlameArg splits "file", "file:42" or "file:40-48". A file whose own
// name ends in ":digits" wins over the line suffix.
func parseBlameArg(arg string) (path string, from, to int, err error) {
	if _, err := os.Stat(arg); err == nil {
		return arg, 0, 0, nil
	}
	i := strings.LastIndexByte(arg, ':')
	if i <= 0 {
		return arg, 0, 0, nil
	}
	a, b, isRange := strings.Cut(arg[i+1:], "-")
	from, err1 := strconv.Atoi(a)
	to, err2 := from, error(nil)
	if isRange {
		to, err2 = strconv.Atoi(b)
	}
	if err1 != nil || err2 != nil || from < 1 || to < from {
		return arg, 0, 0, fmt.Errorf("%q isn't a line or a range like 40-48", arg[i+1:])
	}
	return arg[:i], from, to, nil
}

// printBlame is `trail blame`: one line in detail, or every run of lines
// in a range or file with the session (or commit) behind it.
func printBlame(idx *Index, arg string) error {
	path, from, to, err := parseBlameArg(arg)
	if err != nil {
		return err
	}
	res := Blame(context.Background(), idx, path)
	if !res.Available {
		return fmt.Errorf("%s", res.Reason)
	}
	n := len(res.Lines)
	if n == 0 {
		fmt.Printf("%s is empty\n", res.Rel)
		return nil
	}
	if from == 0 {
		from, to = 1, n
	}
	if from > n {
		return fmt.Errorf("%s has %d %s", res.Rel, n, plural(n, "line", "lines"))
	}
	to = min(to, n)
	if from == to {
		printBlameLine(res, from)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "LINES\tWRITTEN BY\tSTEP\tCOMMIT\n")
	agent, used := 0, map[string]bool{}
	for i := from - 1; i < to; {
		l := res.Lines[i]
		j := i + 1
		for j < to && res.Lines[j].Source == l.Source && res.Lines[j].Commit == l.Commit {
			j++
		}
		span := strconv.Itoa(i + 1)
		if j-i > 1 {
			span += "-" + strconv.Itoa(j)
		}
		commit := "not committed"
		if l.Commit >= 0 {
			c := res.Commits[l.Commit]
			commit = c.Short + " " + oneLine(c.Author, 20)
		}
		who, step := "no session", ""
		if l.Source >= 0 {
			s := res.Sources[l.Source]
			who = providerName(s.Provider) + ": " + oneLine(s.Title, 50)
			step = "#" + strconv.Itoa(s.Event+1)
			if s.Op == "shell" {
				step += " (shell, probably)"
			}
			agent += j - i
			used[s.Key] = true
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", span, who, step, commit)
		i = j
	}
	w.Flush()
	fmt.Printf("\n%d of %d %s came from %d %s (read %d %s from this folder).\n",
		agent, to-from+1, plural(to-from+1, "line", "lines"), len(used), plural(len(used), "session", "sessions"),
		res.Searched, plural(res.Searched, "session", "sessions"))
	if len(used) > 0 {
		fmt.Printf("Run trail blame %s:<line> for the session's id and the request behind a line.\n", res.Rel)
	}
	if res.Truncated {
		fmt.Printf("Only the first %d lines were read.\n", blameMaxLines)
	}
	return nil
}

func printBlameLine(res BlameResult, n int) {
	l := res.Lines[n-1]
	local := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	fmt.Printf("%s:%d\n    %s\n\n", res.Rel, n, strings.TrimSpace(l.Text))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if l.Commit >= 0 {
		c := res.Commits[l.Commit]
		fmt.Fprintf(w, "commit\t%s  %s, %s  %s\n", c.Short, c.Author, local(c.When), oneLine(c.Subject, 60))
		if c.File != "" && c.File != res.Rel {
			fmt.Fprintf(w, "\t(the file was %s then)\n", c.File)
		}
	} else {
		fmt.Fprintf(w, "commit\tnot committed yet\n")
	}
	if l.Source < 0 {
		w.Flush()
		fmt.Printf("\nNo recorded agent edit contains this line (read %d %s from this folder).\n",
			res.Searched, plural(res.Searched, "session", "sessions"))
		fmt.Println("A person wrote it, or an agent did through a shell command rather than an edit tool.")
		return
	}
	s := res.Sources[l.Source]
	fmt.Fprintf(w, "session\t%s: %s\n", providerName(s.Provider), s.Title)
	fmt.Fprintf(w, "id\t%s\n", s.Key)
	step := fmt.Sprintf("#%d of %d", s.Event+1, s.Steps)
	if s.Summary != "" {
		step += ", " + s.Summary
	}
	fmt.Fprintf(w, "step\t%s, %s\n", step, local(s.Time))
	if s.Op == "shell" {
		fmt.Fprintf(w, "\tprobably: no edit records this line, but this shell command contains it and names the file\n")
	}
	if s.Prompt != "" {
		fmt.Fprintf(w, "asked\t%s\n", s.Prompt)
	}
	if s.Resume != "" {
		fmt.Fprintf(w, "resume\t%s\n", s.Resume)
	}
	w.Flush()
}

func providerName(p string) string {
	switch p {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "gemini":
		return "Gemini CLI"
	case "kiro":
		return "Kiro"
	}
	return p
}

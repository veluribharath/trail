package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Retro reads a project's sessions for things the person keeps having to say
// ("no, use pnpm") and commands that keep failing the same way, and suggests
// lines for CLAUDE.md or AGENTS.md. It only suggests: trail never writes to
// the project. Everything here is heuristics over the transcripts, with no
// model involved, so a suggestion always points at the steps behind it.

type Retro struct {
	Root     string        `json:"root"`
	Sessions int           `json:"sessions"` // sessions read
	Files    []string      `json:"files"`    // instruction files found in the project, relative to it
	Targets  []RetroTarget `json:"targets"`  // which file each agent used here reads
	Lessons  []Lesson      `json:"lessons"`
}

// RetroTarget is an instruction file and the agents that read it.
type RetroTarget struct {
	File   string   `json:"file"`
	Agents []string `json:"agents"`
}

// Lesson is one suggestion and the evidence for it.
type Lesson struct {
	// Kind is "correction" (the person said it), "command" (a command kept
	// failing) or "rejected" (the person kept refusing a command).
	Kind string `json:"kind"`
	// Rule is the suggested line. It is empty when trail saw a command keep
	// failing but not what works instead.
	Rule     string    `json:"rule,omitempty"`
	Command  string    `json:"command,omitempty"` // the command that failed or was refused
	Fix      string    `json:"fix,omitempty"`     // the command that worked right after it
	Fixes    int       `json:"fixes,omitempty"`   // how many failures Fix followed
	Error    string    `json:"error,omitempty"`   // the most common error line
	Sessions int       `json:"sessions"`
	Count    int       `json:"count"`
	Last     time.Time `json:"last"`
	// Covered names the instruction file that already seems to say this.
	Covered string          `json:"covered,omitempty"`
	Seen    []LessonExample `json:"seen"` // newest first
	terms   []string
}

// LessonExample is one step behind a lesson.
type LessonExample struct {
	Key   string    `json:"key"`
	Title string    `json:"title"`
	Event int       `json:"event"`
	Time  time.Time `json:"time"`
	Text  string    `json:"text"` // what the person said, or the command
	Error string    `json:"error,omitempty"`
	Fix   string    `json:"fix,omitempty"`
}

const (
	maxLessons     = 40
	maxSeen        = 5
	fixLookahead   = 4 // shell runs after a failure that may count as its fix
	maxSentence    = 220
	maxMessageScan = 6000 // longer user messages are pasted documents or logs
	maxCorrections = 3000 // the newest instructions grouped per project
)

// instructionFiles are the files agents read standing instructions from,
// checked to mark suggestions that are already written down.
var instructionFiles = []string{"CLAUDE.md", "CLAUDE.local.md", ".claude/CLAUDE.md", "AGENTS.md", "GEMINI.md"}

// agentFiles is the instruction file each agent reads.
var agentFiles = map[string]string{"claude": "CLAUDE.md", "codex": "AGENTS.md", "opencode": "AGENTS.md", "kiro": "AGENTS.md", "gemini": "GEMINI.md"}

// RetroCache keeps what each session contributed, so asking again only reads
// sessions that changed.
type RetroCache struct {
	idx   *Index
	mu    sync.Mutex
	notes map[string]*retroNotes // session key -> notes
}

type retroNotes struct {
	stamp       string
	corrections []correction
	runs        []failedRun
}

type correction struct {
	event int
	time  time.Time
	text  string // the sentence as written
	rule  string
	terms []string
}

type failedRun struct {
	event    int
	time     time.Time
	cmd      string // the command as run, on one line
	head     string // env assignments, program and subcommand
	sig      string // head without env assignments
	rejected bool
	env      bool   // the error says something is missing or unknown, not that the code is wrong
	missing  bool   // the program itself isn't installed
	errLine  string // first useful line of the output
	fix      string // head of the command that worked right after
	fixEvent int
}

func NewRetroCache(idx *Index) *RetroCache {
	return &RetroCache{idx: idx, notes: map[string]*retroNotes{}}
}

// Build reads every session filed under root and returns its lessons. root
// must be a folder the index files sessions under.
func (c *RetroCache) Build(root string) (*Retro, bool) {
	var sessions []*Session
	for _, s := range c.idx.Sessions() {
		if s.Root == root {
			sessions = append(sessions, s)
		}
	}
	if len(sessions) == 0 {
		return nil, false
	}
	files := map[string]indexedFile{}
	for _, f := range c.idx.files() {
		files[f.key] = f
	}

	// Drop notes for sessions that are gone, then read the ones that changed.
	c.mu.Lock()
	for k := range c.notes {
		if _, ok := files[k]; !ok {
			delete(c.notes, k)
		}
	}
	var todo []indexedFile
	for _, s := range sessions {
		f, ok := files[s.Key]
		if !ok {
			continue
		}
		if n := c.notes[s.Key]; n == nil || n.stamp != f.stamp {
			todo = append(todo, f)
		}
	}
	c.mu.Unlock()

	var wg sync.WaitGroup
	ch := make(chan indexedFile)
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				n := &retroNotes{stamp: f.stamp}
				if t, err := c.idx.parseFull(f.provider, f.path); t != nil && (err == nil || len(t.Events) > 0) {
					n.corrections, n.runs = sessionCorrections(t), sessionFailures(t)
				}
				c.mu.Lock()
				c.notes[f.key] = n
				c.mu.Unlock()
			}
		}()
	}
	for _, f := range todo {
		ch <- f
	}
	close(ch)
	wg.Wait()

	c.mu.Lock()
	notes := make(map[string]*retroNotes, len(sessions))
	for _, s := range sessions {
		if n := c.notes[s.Key]; n != nil {
			notes[s.Key] = n
		}
	}
	c.mu.Unlock()
	return buildRetro(root, sessions, notes), true
}

func buildRetro(root string, sessions []*Session, notes map[string]*retroNotes) *Retro {
	r := &Retro{Root: root, Files: []string{}, Targets: []RetroTarget{}, Lessons: []Lesson{}}
	byKey := map[string]*Session{}
	agents := map[string]bool{}
	for _, s := range sessions {
		byKey[s.Key] = s
		agents[s.Provider] = true
		if notes[s.Key] != nil {
			r.Sessions++
		}
	}
	for _, p := range []string{"claude", "codex", "opencode", "kiro", "gemini"} {
		if !agents[p] {
			continue
		}
		f := agentFiles[p]
		i := 0
		for i < len(r.Targets) && r.Targets[i].File != f {
			i++
		}
		if i == len(r.Targets) {
			r.Targets = append(r.Targets, RetroTarget{File: f})
		}
		r.Targets[i].Agents = append(r.Targets[i].Agents, p)
	}

	r.Lessons = append(r.Lessons, correctionLessons(byKey, notes)...)
	r.Lessons = append(r.Lessons, commandLessons(byKey, notes)...)

	written := readInstructions(root)
	for _, w := range written {
		r.Files = append(r.Files, w.file)
	}
	for i := range r.Lessons {
		r.Lessons[i].Covered = coveredBy(r.Lessons[i].terms, written)
	}

	sort.SliceStable(r.Lessons, func(i, j int) bool {
		a, b := &r.Lessons[i], &r.Lessons[j]
		if sa, sb := a.Sessions*3+a.Count, b.Sessions*3+b.Count; sa != sb {
			return sa > sb
		}
		return a.Last.After(b.Last)
	})
	if len(r.Lessons) > maxLessons {
		r.Lessons = r.Lessons[:maxLessons]
	}
	return r
}

// repeated is the bar for a lesson: seen in two sessions, or three times in
// one. Once is a one-off, not a habit.
func repeated(sessions, count int) bool { return sessions >= 2 || count >= 3 }

/* ---------------- corrections ---------------- */

var (
	// A correction that only makes sense after the agent did something.
	correctionLead = regexp.MustCompile(`^(?:no+|nope|wrong|actually|i said|i told you|i asked you to|as i said|again|instead|not like that|that'?s wrong|that is wrong|that'?s not right)\b[\s,.:;!—–-]*`)
	// A standing instruction, wherever it appears.
	standingLead = regexp.MustCompile(`^(?:always|never|don'?t|do not|avoid|prefer|we use|we don'?t|we always|we never|make sure (?:to|you)|remember to|only use)\b`)
	// Phrases that start like an instruction but aren't one.
	notInstruction = regexp.MustCompile(`^(?:never ?mind|don'?t (?:worry|know|understand|see|think|get|care|remember|mind)|do not (?:worry|know)|no (?:worries|problem|thanks))\b`)
	useInstead     = regexp.MustCompile(`^use \S.*\b(?:instead|not|rather than)\b`)
	// What a correction's remainder has to start with to be an instruction.
	imperative = regexp.MustCompile(`^(?:use|run|don'?t|do not|never|always|avoid|prefer|stop|keep|put|add|write|call|make|only|leave|ask|check|remove|install|follow|read|test|commit|push|import|name|store|switch|try|skip|set|pass|start|create|edit|change|update|include|exclude|place|move|rename|wrap|return|we)\b`)
	fillerLead = regexp.MustCompile(`^(?:(?:ok|okay|hmm+|um+|so|but|and|also|please|pls|hey)\b[\s,.:;!]*)+`)
	termRe     = regexp.MustCompile(`[a-z0-9][a-z0-9._+/@:#-]*`)
	// Claude Code's text when the person refuses a tool call and says why.
	rejectFeedback = regexp.MustCompile(`(?is)the user said:\s*(.+)$`)
)

// sessionCorrections finds sentences in the person's messages that read as
// instructions to the agent: corrections after it acted ("no, use pnpm") and
// standing rules ("always run the linter").
func sessionCorrections(t *Transcript) []correction {
	var out []correction
	acted := false
	for i, e := range t.Events {
		if e.Sidechain {
			continue // a subagent's prompts are written by the agent
		}
		switch {
		case e.Kind == "assistant":
			acted = true
		case e.Kind == "tool":
			acted = true
			// A refused tool call can carry the reason the person typed.
			if e.Error && rejectedRe.MatchString(e.Output) {
				if m := rejectFeedback.FindStringSubmatch(e.Output); m != nil {
					out = append(out, messageCorrections(m[1], i, e.Time, true, true)...)
				}
			}
		case e.Kind == "user":
			out = append(out, messageCorrections(e.Text, i, e.Time, acted, false)...)
		}
	}
	return out
}

// messageCorrections splits one message into sentences and keeps the ones
// that are instructions. marked says the whole message is a correction.
func messageCorrections(text string, ev int, ts time.Time, acted, marked bool) []correction {
	if len(text) > maxMessageScan || strings.HasPrefix(text, "<") ||
		strings.HasPrefix(text, "This session is being continued from a previous conversation") {
		return nil
	}
	var out []correction
	carry := marked
	for _, sent := range sentences(text) {
		raw := strings.TrimSpace(sent)
		lower := strings.ToLower(raw)
		if len(lower) != len(raw) {
			continue // lowercasing changed the length; offsets below wouldn't line up
		}
		low := fillerLead.ReplaceAllString(lower, "")
		body := raw[len(raw)-len(low):]
		var rule string
		if notInstruction.MatchString(low) {
			carry = marked
			continue
		}
		if m := correctionLead.FindString(low); m != "" && acted {
			rest := strings.TrimSpace(body[len(m):])
			restLow := fillerLead.ReplaceAllString(strings.ToLower(rest), "")
			rest = rest[len(rest)-len(restLow):]
			if rest == "" {
				carry = true // "No." on its own; the next sentence says what instead
				continue
			}
			if imperative.MatchString(restLow) {
				rule = rest
			}
		} else if (acted && (carry || strings.HasPrefix(low, "stop ")) && imperative.MatchString(low)) || standingLead.MatchString(low) || useInstead.MatchString(low) {
			rule = body
		}
		carry = marked
		if rule == "" || strings.HasSuffix(rule, "?") {
			continue
		}
		rule = tidyRule(rule)
		if len(rule) < 8 || len(rule) > maxSentence || len(strings.Fields(rule)) < 2 {
			continue
		}
		terms := ruleTerms(rule)
		if len(terms) == 0 {
			continue
		}
		out = append(out, correction{event: ev, time: ts, text: oneLine(raw, maxSentence), rule: rule, terms: terms})
	}
	return out
}

// sentences splits a message into sentences, skipping code blocks, quotes and
// lines that look like pasted output.
func sentences(text string) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "```") {
			fence = !fence
			continue
		}
		if fence || l == "" || strings.HasPrefix(l, ">") || strings.HasPrefix(l, "$ ") || strings.HasPrefix(l, "    ") {
			continue
		}
		l = strings.TrimLeft(l, "-*• ")
		start := 0
		for i := 0; i < len(l); i++ {
			c := l[i]
			end := c == ';' || ((c == '.' || c == '!' || c == '?') && (i+1 == len(l) || l[i+1] == ' '))
			if !end {
				continue
			}
			if c == '?' {
				i++ // keep the question mark, so questions can be dropped
			}
			if s := strings.TrimSpace(l[start:min(i, len(l))]); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
		if start < len(l) {
			if s := strings.TrimSpace(l[start:]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// tidyRule turns a sentence into a line for an instructions file.
func tidyRule(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	s = strings.TrimRight(s, ".!,;: ")
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but if then so of to in on at by for with from into onto as is are be was
		were it its this that these those there here i you we me my our your us they them he she his her do does did done don t
		not no never always use using used run running please just only also again instead rather than make sure remember
		should must can could would will want need dont don't avoid prefer stop keep any all every some each when while
		before after like via per same other ok okay yes yeah one thing things way what which who how why get got let
		everything anything nothing something repo repository project codebase now`) {
		m[w] = true
	}
	return m
}()

// ruleTerms is what a rule is about: its words without the instruction verbs
// and filler, with plurals folded, so "use pnpm" and "no, we use pnpm here"
// land together.
func ruleTerms(rule string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range termRe.FindAllString(strings.ToLower(rule), -1) {
		w = strings.TrimRight(w, ".:,-")
		if len(w) < 2 || stopwords[w] {
			continue
		}
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.ContainsAny(w, "./") {
			w = w[:len(w)-1]
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := map[string]bool{}
	for _, w := range a {
		in[w] = true
	}
	both := 0
	for _, w := range b {
		if in[w] {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

type corrHit struct {
	key string
	c   correction
}

func correctionLessons(byKey map[string]*Session, notes map[string]*retroNotes) []Lesson {
	// Group sentences about the same thing: two sentences belong together
	// when they share half their terms, and groups join through any pair.
	var all []corrHit
	for _, key := range sortedKeys(notes) {
		for _, c := range notes[key].corrections {
			all = append(all, corrHit{key, c})
		}
	}
	if len(all) > maxCorrections {
		sort.Slice(all, func(i, j int) bool { return all[i].c.time.After(all[j].c.time) })
		all = all[:maxCorrections]
	}
	parent := make([]int, len(all))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if jaccard(all[i].c.terms, all[j].c.terms) >= 0.5 {
				parent[find(j)] = find(i)
			}
		}
	}
	byRoot := map[int]int{}
	var groups [][]corrHit
	for i, h := range all {
		r := find(i)
		g, ok := byRoot[r]
		if !ok {
			g = len(groups)
			byRoot[r] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], h)
	}

	var out []Lesson
	for _, g := range groups {
		sess := map[string]bool{}
		freq := map[string]int{}
		for _, m := range g {
			sess[m.key] = true
			for _, w := range m.c.terms {
				freq[w]++
			}
		}
		if !repeated(len(sess), len(g)) {
			continue
		}
		// The wording that best covers the group: the sentence whose terms
		// come up most across it, and the shorter one on a tie.
		best, bestScore := g[0].c, -1
		for _, m := range g {
			score := 0
			for _, w := range m.c.terms {
				score += freq[w]
			}
			if score > bestScore || (score == bestScore && len(m.c.rule) < len(best.rule)) {
				best, bestScore = m.c, score
			}
		}
		l := Lesson{Kind: "correction", Rule: best.rule, Sessions: len(sess), Count: len(g), terms: best.terms}
		sort.Slice(g, func(i, j int) bool { return g[i].c.time.After(g[j].c.time) })
		for _, m := range g {
			if m.c.time.After(l.Last) {
				l.Last = m.c.time
			}
			if len(l.Seen) < maxSeen {
				l.Seen = append(l.Seen, LessonExample{Key: m.key, Title: titleOf(byKey, m.key), Event: m.c.event, Time: m.c.time, Text: m.c.text})
			}
		}
		out = append(out, l)
	}
	return out
}

/* ---------------- failing commands ---------------- */

var (
	rejectedRe = regexp.MustCompile(`(?i)(user doesn'?t want to proceed|tool use was rejected|rejected by (?:the )?user|user (?:denied|rejected|declined)|denied by (?:the )?user|aborted by (?:the )?user)`)
	// Errors that say something is missing or unknown, as opposed to code
	// that doesn't compile or tests that fail.
	envErrRe  = regexp.MustCompile(`(?i)(command not found|: not found|not found: |is not recognized as|modulenotfounderror|no module named|cannot find module|unknown command|unknown subcommand|unknown flag|unknown option|unknown shorthand flag|unrecognized option|unrecognized arguments|invalid option|illegal option|permission denied|eacces|is not installed|missing script|no rule to make target|no such command|no such option|externally-managed-environment|not a git repository|could not find a version|no matching version|enoent)`)
	missingRe = regexp.MustCompile(`(?i)(command not found|: not found|not found: |is not recognized as)`)
	// Exit status lines that say nothing about why.
	exitLineRe = regexp.MustCompile(`(?i)^(?:error: )?(?:exit code|exit status|command failed|process exited)\b`)
)

// shellTools are the tools that run a shell command, by each agent's name.
var shellTools = map[string]bool{"bash": true, "shell": true, "exec_command": true, "local_shell": true, "shell_command": true,
	"run_shell_command": true, "execute_bash": true, "execute_cmd": true, "execute_pwsh": true, "container.exec": true}

// shellCommand returns the command a shell tool call ran, or "".
func shellCommand(e Event) string {
	if e.Kind != "tool" || !shellTools[strings.ToLower(e.Tool)] {
		return ""
	}
	in := strings.TrimSpace(e.Input)
	if strings.HasPrefix(in, "$ ") {
		return strings.TrimSpace(in[2:])
	}
	if strings.HasPrefix(in, "{") {
		var a struct {
			Command json.RawMessage `json:"command"`
			Cmd     string          `json:"cmd"`
		}
		if json.Unmarshal([]byte(in), &a) == nil {
			var one string
			var argv []string
			switch {
			case json.Unmarshal(a.Command, &one) == nil && one != "":
				in = one
			case json.Unmarshal(a.Command, &argv) == nil && len(argv) > 0:
				in = strings.Join(argv, " ")
			case a.Cmd != "":
				in = a.Cmd
			default:
				return ""
			}
		}
	}
	for _, p := range []string{"bash -lc ", "bash -c ", "zsh -lc ", "zsh -c ", "sh -c ", "/bin/bash -lc ", "/bin/zsh -lc ", "/bin/sh -c "} {
		if strings.HasPrefix(in, p) {
			in = strings.Trim(strings.TrimSpace(in[len(p):]), `'"`)
			break
		}
	}
	return in
}

// shellRun is one command split into the parts retro compares.
type shellRun struct {
	head, sig string // env + program + subcommand; the same without env
	prog      string
	seg       string   // the command from its program on
	args      []string // words after the subcommand
}

// subcommands is how many words after the program name say what it does.
var subcommands = map[string]bool{"npm": true, "pnpm": true, "yarn": true, "bun": true, "npx": true, "pnpx": true, "bunx": true,
	"go": true, "cargo": true, "git": true, "make": true, "docker": true, "docker-compose": true, "podman": true, "kubectl": true,
	"pip": true, "pip3": true, "uv": true, "poetry": true, "pipenv": true, "conda": true, "mvn": true, "gradle": true,
	"gradlew": true, "./gradlew": true, "dotnet": true, "bundle": true, "rails": true, "rake": true, "mix": true, "deno": true,
	"gh": true, "brew": true, "terraform": true, "swift": true, "rustup": true, "flutter": true, "dart": true, "composer": true,
	"just": true, "task": true, "nx": true, "turbo": true, "corepack": true, "hatch": true, "pdm": true, "rye": true, "tox": true, "nox": true}

// runners take a second word after these subcommands ("npm run build").
var runners = map[string]bool{"run": true, "exec": true, "x": true, "dlx": true, "compose": true, "tool": true}

var subRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9:_.-]*$`)

// parseShell finds the command that matters in a shell line: the first one
// that isn't cd, echo or an export, after env assignments and wrappers.
func parseShell(cmd string) (shellRun, bool) {
	for _, seg := range splitShell(cmd) {
		f := strings.Fields(seg)
		var env []string
		for len(f) > 0 {
			w := f[0]
			if eq := strings.IndexByte(w, '='); eq > 0 && subRe.MatchString(w[:eq]) && !strings.HasPrefix(w, "-") {
				env = append(env, w)
				f = f[1:]
				continue
			}
			if w == "sudo" || w == "time" || w == "command" || w == "exec" || w == "nohup" || w == "env" {
				f = f[1:]
				continue
			}
			if w == "timeout" && len(f) > 2 {
				f = f[2:]
				continue
			}
			break
		}
		if len(f) == 0 {
			continue
		}
		prog := f[0]
		if strings.HasPrefix(prog, "/") {
			prog = filepath.Base(prog)
		}
		switch prog {
		case "cd", "pushd", "popd", "export", "set", "source", ".", "echo", "printf", "true", "unset", "alias", "clear":
			continue
		}
		words := []string{prog}
		rest := f[1:]
		switch {
		case subcommands[prog]:
			// Skip global flags before the subcommand (git -C dir status).
			for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
				if (rest[0] == "-C" || rest[0] == "-c" || rest[0] == "--prefix" || rest[0] == "--cwd") && len(rest) > 1 {
					rest = rest[1:]
				}
				rest = rest[1:]
			}
			if len(rest) > 0 && subRe.MatchString(rest[0]) {
				words = append(words, rest[0])
				if runners[rest[0]] && len(rest) > 1 && subRe.MatchString(rest[1]) {
					words = append(words, rest[1])
					rest = rest[1:]
				}
				rest = rest[1:]
			}
		case (prog == "python" || prog == "python3" || prog == "node") && len(rest) > 1 && rest[0] == "-m":
			words = append(words, "-m", rest[1])
			rest = rest[2:]
		}
		sig := strings.Join(words, " ")
		head := sig
		if len(env) > 0 {
			head = strings.Join(env, " ") + " " + sig
		}
		return shellRun{head: head, sig: sig, prog: prog, seg: strings.Join(f, " "), args: rest}, true
	}
	return shellRun{}, false
}

// splitShell splits a command line at &&, ||, ;, | and newlines that aren't
// inside quotes.
func splitShell(cmd string) []string {
	var out []string
	var b strings.Builder
	var quote byte
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			b.WriteByte(c)
		case c == '\n' || c == ';' || c == '|' || (c == '&' && i+1 < len(cmd) && cmd[i+1] == '&'):
			flush()
			if (c == '&' || c == '|') && i+1 < len(cmd) && cmd[i+1] == c {
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

// family groups tools that stand in for each other, so a failed npm install
// followed by a working pnpm install reads as a fix.
var family = map[string]string{"npm": "js", "pnpm": "js", "yarn": "js", "bun": "js", "npx": "jsx", "pnpx": "jsx", "bunx": "jsx",
	"pip": "pip", "pip3": "pip", "pipenv": "py", "poetry": "py", "uv": "py", "pdm": "py", "rye": "py", "hatch": "py",
	"python": "py", "python3": "py", "py": "py", "docker": "docker", "docker-compose": "docker", "podman": "docker",
	"gradle": "gradle", "gradlew": "gradle", "./gradlew": "gradle", "grep": "grep", "rg": "grep", "ag": "grep",
	"find": "find", "fd": "find", "fdfind": "find", "ls": "ls", "eza": "ls", "exa": "ls"}

func kin(a, b string) bool {
	if a == b {
		return true
	}
	if fa, fb := family[a], family[b]; fa != "" && fa == fb {
		return true
	}
	return len(a) >= 2 && len(b) >= 2 && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

// fixes reports whether a command that worked stands in for one that failed.
func fixes(failed, next shellRun, env bool) bool {
	if next.head == failed.head {
		return false
	}
	switch {
	case strings.HasSuffix(next.seg, " "+failed.seg):
		return true // a wrapper: pytest -> uv run pytest
	case next.sig == failed.sig:
		return true // the same command with different env assignments
	case next.prog != failed.prog && kin(next.prog, failed.prog):
		// npm install -> pnpm install, python x.py -> python3 x.py
		return strings.TrimPrefix(next.sig, next.prog) == strings.TrimPrefix(failed.sig, failed.prog) ||
			(len(failed.args) > 0 && jaccard(next.args, failed.args) >= 0.5)
	case env && next.prog == failed.prog:
		return true // npm test -> npm run test:unit after "missing script"
	case env && len(failed.args) > 0:
		return jaccard(next.args, failed.args) >= 0.5 // rg foo . -> grep -r foo .
	}
	return false
}

// sessionFailures lists shell commands that failed or were refused, with what
// ran successfully right after when it looks like the fix.
func sessionFailures(t *Transcript) []failedRun {
	type run struct {
		ev   int
		r    shellRun
		ok   bool
		cmd  string
		fail failedRun
	}
	var runs []run
	for i, e := range t.Events {
		cmd := shellCommand(e)
		if cmd == "" {
			continue
		}
		r, ok := parseShell(cmd)
		if !ok {
			continue
		}
		runs = append(runs, run{ev: i, r: r, ok: !e.Error, cmd: oneLine(cmd, 160)})
	}
	var out []failedRun
	for i, x := range runs {
		if x.ok {
			continue
		}
		e := t.Events[x.ev]
		f := failedRun{event: x.ev, time: e.Time, cmd: x.cmd, head: x.r.head, sig: x.r.sig}
		f.rejected = rejectedRe.MatchString(e.Output)
		if !f.rejected {
			f.errLine = errorLine(e.Output)
			f.missing = programMissing(e.Output, x.r.prog)
			f.env = f.missing || envErrRe.MatchString(e.Output)
			for _, y := range runs[i+1 : min(len(runs), i+1+fixLookahead)] {
				if y.r.head == x.r.head {
					if y.ok {
						break // the same command worked later: the code was fixed, not the command
					}
					continue
				}
				if y.ok && fixes(x.r, y.r, f.env) {
					f.fix, f.fixEvent = y.r.head, y.ev
					break
				}
			}
		}
		if f.rejected || f.env || f.fix != "" {
			out = append(out, f)
		}
	}
	return out
}

// programMissing reports whether the shell said the program itself doesn't
// exist, as opposed to a file the program was given.
func programMissing(out, prog string) bool {
	low, p := strings.ToLower(out), strings.ToLower(prog)
	for _, s := range []string{p + ": command not found", "command not found: " + p, p + ": not found",
		p + ": no such file or directory", "'" + p + "' is not recognized", "command '" + p + "' not found"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// errorLine picks the line of a failure's output that says why.
func errorLine(out string) string {
	var first string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || exitLineRe.MatchString(l) {
			continue
		}
		if envErrRe.MatchString(l) || missingRe.MatchString(l) {
			return oneLine(l, 160)
		}
		if first == "" {
			first = oneLine(l, 160)
		}
	}
	return first
}

// probes are commands agents run to look around. Their failures are how they
// find out, not mistakes, unless something else then worked in their place.
var probes = map[string]bool{"cat": true, "ls": true, "head": true, "tail": true, "grep": true, "rg": true, "find": true,
	"sed": true, "awk": true, "wc": true, "test": true, "[": true, "diff": true, "stat": true, "file": true, "which": true,
	"type": true, "sleep": true, "kill": true, "pkill": true, "ps": true, "lsof": true, "curl": true, "wget": true, "mkdir": true,
	"rm": true, "cp": true, "mv": true, "touch": true, "chmod": true, "tree": true, "pwd": true, "readlink": true, "realpath": true}

type runHit struct {
	key string
	f   failedRun
}

func commandLessons(byKey map[string]*Session, notes map[string]*retroNotes) []Lesson {
	groups := map[string][]runHit{}
	var order []string
	for _, key := range sortedKeys(notes) {
		for _, f := range notes[key].runs {
			g := "fail " + f.sig
			if f.rejected {
				g = "refuse " + f.sig
			}
			if _, ok := groups[g]; !ok {
				order = append(order, g)
			}
			groups[g] = append(groups[g], runHit{key, f})
		}
	}

	var out []Lesson
	for _, g := range order {
		hits := groups[g]
		sort.Slice(hits, func(i, j int) bool { return hits[i].f.time.After(hits[j].f.time) })
		sess := map[string]bool{}
		fixCount, errCount := map[string]int{}, map[string]int{}
		heads := map[string]int{}
		missing := 0
		for _, h := range hits {
			sess[h.key] = true
			heads[h.f.head]++
			if h.f.fix != "" {
				fixCount[h.f.fix]++
			}
			if h.f.errLine != "" {
				errCount[h.f.errLine]++
			}
			if h.f.missing {
				missing++
			}
		}
		if !repeated(len(sess), len(hits)) {
			continue
		}
		l := Lesson{Kind: "command", Command: top(heads), Sessions: len(sess), Count: len(hits), Error: top(errCount)}
		prog := strings.Fields(hits[0].f.sig)[0]
		if fix := top(fixCount); fix != "" {
			l.Fix, l.Fixes = fix, fixCount[fix]
		}
		cmd := strings.TrimPrefix(g, "fail ")
		switch {
		case strings.HasPrefix(g, "refuse "):
			l.Kind, l.Command = "rejected", strings.TrimPrefix(g, "refuse ")
			l.Rule = fmt.Sprintf("Ask before running `%s`.", l.Command)
			l.terms = ruleTerms(l.Command)
		case l.Fix != "":
			l.Rule = fmt.Sprintf("Use `%s`, not `%s`.", l.Fix, l.Command)
			// Already written down if the file mentions what to use.
			l.terms = minus(ruleTerms(l.Fix), ruleTerms(l.Command))
			if len(l.terms) == 0 {
				l.terms = ruleTerms(l.Fix)
			}
		case missing*2 > len(hits):
			l.Rule = fmt.Sprintf("Don't use `%s`: it isn't installed here.", prog)
			l.terms = []string{prog}
		case probes[prog]:
			continue
		case strings.Contains(strings.ToLower(l.Error), "missing script") || strings.Contains(strings.ToLower(l.Error), "no rule to make target") ||
			strings.Contains(strings.ToLower(l.Error), "unknown command") || strings.Contains(strings.ToLower(l.Error), "no such command"):
			l.Rule = fmt.Sprintf("`%s` isn't defined in this project.", cmd)
			l.terms = ruleTerms(cmd)
		default:
			// Seen failing, but not what works instead: worth writing down by
			// hand, so it is listed without a rule.
			l.terms = ruleTerms(cmd)
		}
		for _, h := range hits {
			if h.f.time.After(l.Last) {
				l.Last = h.f.time
			}
			if len(l.Seen) < maxSeen {
				l.Seen = append(l.Seen, LessonExample{Key: h.key, Title: titleOf(byKey, h.key), Event: h.f.event, Time: h.f.time,
					Text: h.f.cmd, Error: h.f.errLine, Fix: h.f.fix})
			}
		}
		out = append(out, l)
	}
	return out
}

/* ---------------- instruction files ---------------- */

type writtenFile struct {
	file  string
	lines [][]string // terms per line
}

// readInstructions reads the instruction files at the project root, to mark
// suggestions they already cover. They are only read, never written.
func readInstructions(root string) []writtenFile {
	if !filepath.IsAbs(root) {
		return nil
	}
	var out []writtenFile
	for _, name := range instructionFiles {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, 512<<10))
		f.Close()
		w := writtenFile{file: name}
		for _, l := range strings.Split(string(b), "\n") {
			if t := ruleTerms(l); len(t) > 0 {
				w.lines = append(w.lines, t)
			}
		}
		out = append(out, w)
	}
	return out
}

// coveredBy names the first file with a line that mentions every term.
func coveredBy(terms []string, files []writtenFile) string {
	if len(terms) == 0 {
		return ""
	}
	for _, f := range files {
		for _, line := range f.lines {
			have := map[string]bool{}
			for _, w := range line {
				have[w] = true
			}
			all := true
			for _, w := range terms {
				if !have[w] {
					all = false
					break
				}
			}
			if all {
				return f.file
			}
		}
	}
	return ""
}

/* ---------------- helpers ---------------- */

func sortedKeys(m map[string]*retroNotes) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func titleOf(byKey map[string]*Session, key string) string {
	if s := byKey[key]; s != nil {
		return s.Title
	}
	return key
}

// top returns the most common key, and the alphabetically first on a tie.
func top(m map[string]int) string {
	best, n := "", 0
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}

func minus(a, b []string) []string {
	drop := map[string]bool{}
	for _, w := range b {
		drop[w] = true
	}
	var out []string
	for _, w := range a {
		if !drop[w] {
			out = append(out, w)
		}
	}
	return out
}

/* ---------------- command line ---------------- */

// retroRoot resolves the folder named on the command line to the project
// root sessions are filed under.
func retroRoot(idx *Index, arg string) (string, error) {
	if arg == "" || arg == "." {
		arg, _ = os.Getwd()
	}
	if strings.HasPrefix(arg, "~/") {
		arg = filepath.Join(homeDir(), arg[2:])
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	roots := map[string]bool{}
	for _, s := range idx.Sessions() {
		roots[s.Root] = true
	}
	if roots[abs] {
		return abs, nil
	}
	if root, _ := idx.repoRoot(abs); roots[root] {
		return root, nil
	}
	return "", fmt.Errorf("no sessions ran in %s; trail -list shows the folders it knows", abs)
}

func printRetro(w io.Writer, r *Retro, home string) {
	where := r.Root
	if home != "" && strings.HasPrefix(where, home+string(filepath.Separator)) {
		where = "~" + where[len(home):]
	}
	fmt.Fprintf(w, "Lessons from %s in %s\n", plural(r.Sessions, "1 session", fmt.Sprintf("%d sessions", r.Sessions)), where)
	if len(r.Lessons) == 0 {
		fmt.Fprintln(w, "\nNothing repeats yet: no instruction you gave twice, and no command that kept failing the same way.")
		return
	}
	var fresh, covered, open []*Lesson
	for i := range r.Lessons {
		l := &r.Lessons[i]
		switch {
		case l.Covered != "":
			covered = append(covered, l)
		case l.Rule == "":
			open = append(open, l)
		default:
			fresh = append(fresh, l)
		}
	}
	if len(fresh) > 0 {
		var files []string
		for _, t := range r.Targets {
			files = append(files, t.File)
		}
		fmt.Fprintf(w, "\nSuggested rules for %s (trail doesn't write them; copy what you agree with):\n\n", strings.Join(files, " or "))
		for _, l := range fresh {
			fmt.Fprintf(w, "- %s\n", l.Rule)
		}
		fmt.Fprintln(w, "\nWhy:")
		for _, l := range fresh {
			printLesson(w, l)
		}
	}
	if len(open) > 0 {
		fmt.Fprintln(w, "\nKept failing, with no fix seen (worth writing down how to run them):")
		for _, l := range open {
			printLesson(w, l)
		}
	}
	if len(covered) > 0 {
		fmt.Fprintln(w, "\nAlready in your instructions, but still came up:")
		for _, l := range covered {
			fmt.Fprintf(w, "  %s  (%s)\n", l.Rule, l.Covered)
			printLesson(w, l)
		}
	}
}

func timesWord(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

func printLesson(w io.Writer, l *Lesson) {
	in := fmt.Sprintf("in %s", plural(l.Sessions, "1 session", fmt.Sprintf("%d sessions", l.Sessions)))
	last := l.Last.Local().Format("Jan 2")
	switch l.Kind {
	case "correction":
		fmt.Fprintf(w, "  %q: you said it %s %s, last %s\n", l.Rule, timesWord(l.Count), in, last)
	case "rejected":
		fmt.Fprintf(w, "  `%s`: you refused it %s %s, last %s\n", l.Command, timesWord(l.Count), in, last)
	default:
		fmt.Fprintf(w, "  `%s` failed %s %s, last %s", l.Command, timesWord(l.Count), in, last)
		if l.Fix != "" {
			fmt.Fprintf(w, "; `%s` worked right after %s", l.Fix, plural(l.Fixes, "one", fmt.Sprintf("%d of them", l.Fixes)))
		}
		fmt.Fprintln(w)
		if l.Error != "" {
			fmt.Fprintf(w, "      %s\n", l.Error)
		}
	}
	for _, e := range l.Seen {
		fmt.Fprintf(w, "      %s  #%-4d %-40s %s\n", e.Time.Local().Format("2006-01-02"), e.Event+1, oneLine(e.Title, 40), oneLine(e.Text, 70))
	}
}

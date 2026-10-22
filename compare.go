package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// A comparison puts two sessions side by side: two agents given the same
// task, or one agent before and after a prompt change. It is worked out from
// the transcripts alone, so it covers every provider the same way.

// idleGap is the longest pause between two steps that still counts as the
// agent working. Longer gaps are someone away from the keyboard.
const idleGap = 5 * time.Minute

type Comparison struct {
	A     *CompareSide  `json:"a"`
	B     *CompareSide  `json:"b"`
	Files []FileCompare `json:"files"` // every file either session changed, shared ones first
	// Shared is how many files both sessions changed.
	Shared int `json:"shared"`
}

// CompareSide is one session's numbers.
type CompareSide struct {
	Key       string      `json:"key"`
	Wall      int64       `json:"wall"`   // ms from first to last step
	Active    int64       `json:"active"` // ms, leaving out gaps over idleGap
	Steps     int         `json:"steps"`
	Prompts   int         `json:"prompts"`
	ToolCalls int         `json:"toolCalls"`
	Failed    int         `json:"failed"`
	Edits     int         `json:"edits"`
	Files     int         `json:"files"`
	Added     int         `json:"added"`
	Removed   int         `json:"removed"`
	Usage     *Usage      `json:"usage,omitempty"`
	Tools     []ToolTally `json:"tools"` // most used first
}

type ToolTally struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed"`
}

// FileCompare is one file and what each session did to it. Paths are
// relative to each session's checkout, so two agents working in separate
// worktrees of one repository still line up.
type FileCompare struct {
	Path string     `json:"path"`
	A    *FileTally `json:"a,omitempty"`
	B    *FileTally `json:"b,omitempty"`
}

type FileTally struct {
	Edits   int   `json:"edits"`
	Added   int   `json:"added"`
	Removed int   `json:"removed"`
	Deleted bool  `json:"deleted,omitempty"`
	Changes []int `json:"changes"` // indices into that transcript's Changes
}

// Compare works out the comparison of two loaded transcripts.
func Compare(a, b *Transcript) *Comparison {
	c := &Comparison{Files: []FileCompare{}}
	var fa, fb map[string]*FileTally
	c.A, fa = compareSide(a)
	c.B, fb = compareSide(b)
	paths := map[string]bool{}
	for p := range fa {
		paths[p] = true
	}
	for p := range fb {
		paths[p] = true
	}
	for p := range paths {
		f := FileCompare{Path: p, A: fa[p], B: fb[p]}
		if f.A != nil && f.B != nil {
			c.Shared++
		}
		c.Files = append(c.Files, f)
	}
	sort.Slice(c.Files, func(i, j int) bool {
		x, y := c.Files[i], c.Files[j]
		bx, by := x.A != nil && x.B != nil, y.A != nil && y.B != nil
		if bx != by {
			return bx
		}
		return x.Path < y.Path
	})
	return c
}

func compareSide(t *Transcript) (*CompareSide, map[string]*FileTally) {
	s := t.Session
	side := &CompareSide{Key: s.Key, Steps: len(t.Events), Edits: len(t.Changes), Usage: s.Usage, Tools: []ToolTally{}}
	if !s.Started.IsZero() && s.Ended.After(s.Started) {
		side.Wall = s.Ended.Sub(s.Started).Milliseconds()
	}
	tools := map[string]*ToolTally{}
	var last time.Time
	for _, e := range t.Events {
		if !e.Time.IsZero() {
			if !last.IsZero() {
				if gap := e.Time.Sub(last); gap > 0 && gap <= idleGap {
					side.Active += gap.Milliseconds()
				}
			}
			if e.Time.After(last) {
				last = e.Time
			}
		}
		switch e.Kind {
		case "user":
			side.Prompts++
		case "tool":
			side.ToolCalls++
			tt := tools[e.Tool]
			if tt == nil {
				tt = &ToolTally{Name: e.Tool}
				tools[e.Tool] = tt
			}
			tt.Calls++
			if e.Error {
				side.Failed++
				tt.Failed++
			}
		}
	}
	for _, tt := range tools {
		side.Tools = append(side.Tools, *tt)
	}
	sort.Slice(side.Tools, func(i, j int) bool {
		if side.Tools[i].Calls != side.Tools[j].Calls {
			return side.Tools[i].Calls > side.Tools[j].Calls
		}
		return side.Tools[i].Name < side.Tools[j].Name
	})

	files := map[string]*FileTally{}
	for i, ch := range t.Changes {
		p := checkoutPath(s, ch)
		f := files[p]
		if f == nil {
			f = &FileTally{}
			files[p] = f
		}
		f.Edits++
		f.Changes = append(f.Changes, i)
		f.Deleted = ch.Op == "delete"
		if ch.Op == "delete" {
			continue
		}
		for _, h := range ch.Hunks {
			add, del := lineCounts(h.Old, h.New)
			f.Added += add
			f.Removed += del
		}
	}
	for _, f := range files {
		side.Added += f.Added
		side.Removed += f.Removed
	}
	side.Files = len(files)
	return side, files
}

// checkoutPath is a changed file's path relative to the checkout the session
// ran in: its worktree, or its repository. Files outside it keep their full
// path.
func checkoutPath(s *Session, c FileChange) string {
	base := s.Worktree
	if base == "" {
		base = s.Root
	}
	p := c.Path
	if !filepath.IsAbs(p) && s.Cwd != "" {
		p = filepath.Join(s.Cwd, p)
	}
	if !filepath.IsAbs(base) || !filepath.IsAbs(p) {
		return c.Rel
	}
	return relTo(base, p)
}

// lineCounts is how many lines a change from old to new adds and removes,
// matching the diffs the page draws: the longest common subsequence of lines
// stays, and anything too big to diff counts as replaced outright.
func lineCounts(old, new string) (add, del int) {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	a, b := split(old), split(new)
	if len(a)*len(b) > 4e6 {
		return len(b), len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				cur[j] = prev[j+1] + 1
			} else {
				cur[j] = max(prev[j], cur[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return len(b) - prev[0], len(a) - prev[0]
}

// compareSessions prints a comparison for -compare.
func compareSessions(idx *Index, w io.Writer, argA, argB string) error {
	var ts [2]*Transcript
	for i, arg := range []string{argA, argB} {
		s, err := findSession(idx, arg)
		if err != nil {
			return err
		}
		if ts[i], err = idx.Load(s.Key); err != nil {
			return err
		}
	}
	if ts[0].Session.Key == ts[1].Session.Key {
		return fmt.Errorf("%q and %q are the same session", argA, argB)
	}
	printCompare(w, ts[0], ts[1], Compare(ts[0], ts[1]))
	return nil
}

func printCompare(w io.Writer, ta, tb *Transcript, c *Comparison) {
	sa, sb := ta.Session, tb.Session
	label := func(s *Session) string {
		p := s.Provider
		if s.Model != "" {
			p += ", " + s.Model
		}
		return fmt.Sprintf("%s (%s)", oneLine(s.Title, 60), p)
	}
	fmt.Fprintf(w, "A  %s  %s\n", label(sa), sa.ID)
	fmt.Fprintf(w, "B  %s  %s\n\n", label(sb), sb.ID)

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', tabwriter.AlignRight)
	row := func(name, a, b, d string) { fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", name, a, b, d) }
	num := func(name string, a, b int) { row(name, itoa(a), itoa(b), signed(b-a)) }
	dur := func(name string, a, b int64) {
		row(name, fmtDur(a), fmtDur(b), sign(b-a)+fmtDur(abs64(b-a)))
	}
	row("", "A", "B", "B − A")
	dur("active time", c.A.Active, c.B.Active)
	dur("wall clock", c.A.Wall, c.B.Wall)
	num("steps", c.A.Steps, c.B.Steps)
	num("prompts", c.A.Prompts, c.B.Prompts)
	num("tool calls", c.A.ToolCalls, c.B.ToolCalls)
	num("failed calls", c.A.Failed, c.B.Failed)
	num("edits", c.A.Edits, c.B.Edits)
	num("files changed", c.A.Files, c.B.Files)
	num("lines added", c.A.Added, c.B.Added)
	num("lines removed", c.A.Removed, c.B.Removed)
	if c.A.Usage != nil || c.B.Usage != nil {
		ua, ub := orZero(c.A.Usage), orZero(c.B.Usage)
		ta, tb := ua.Prompt()+ua.Output, ub.Prompt()+ub.Output
		row("tokens", tokensHuman(ta), tokensHuman(tb), sign(tb-ta)+tokensHuman(abs64(tb-ta)))
		row("API-equivalent cost", costCell(ua), costCell(ub), fmt.Sprintf("%+.2f", ub.Cost-ua.Cost))
	}
	tw.Flush()

	if len(c.Files) > 0 {
		fmt.Fprintf(w, "\nfiles: %d changed by both, %d only by A, %d only by B\n", c.Shared, countSide(c.Files, true), countSide(c.Files, false))
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		cell := func(f *FileTally) string {
			switch {
			case f == nil:
				return "·"
			case f.Deleted:
				return "deleted"
			}
			return fmt.Sprintf("+%d −%d", f.Added, f.Removed)
		}
		for _, f := range c.Files {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.Path, cell(f.A), cell(f.B))
		}
		tw.Flush()
	}
}

func countSide(files []FileCompare, a bool) int {
	n := 0
	for _, f := range files {
		if a && f.A != nil && f.B == nil || !a && f.B != nil && f.A == nil {
			n++
		}
	}
	return n
}

func orZero(u *Usage) Usage {
	if u == nil {
		return Usage{}
	}
	return *u
}

// costCell is a cost short enough for a table cell.
func costCell(u Usage) string {
	if u.Unpriced > 0 && u.Cost == 0 {
		return "—"
	}
	s := fmt.Sprintf("$%.2f", u.Cost)
	if u.Unpriced > 0 {
		s = "≥" + s
	}
	return s
}

func signed(n int) string {
	if n > 0 {
		return "+" + itoa(n)
	}
	if n < 0 {
		return "−" + itoa(-n)
	}
	return "0"
}

// sign prefixes a difference shown as its absolute value.
func sign(n int64) string {
	if n < 0 {
		return "−"
	}
	return "+"
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func fmtDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

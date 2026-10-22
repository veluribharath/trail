package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sessionStats is what the stats view needs from one full transcript, kept
// next to its search postings so it is read once, with them.
type sessionStats struct {
	tools map[string][2]int // tool name -> calls, failed
	files map[string]int    // absolute path -> edits
}

func collectStats(t *Transcript) *sessionStats {
	st := &sessionStats{tools: map[string][2]int{}, files: map[string]int{}}
	for _, e := range t.Events {
		if e.Kind != "tool" || e.Tool == "" {
			continue
		}
		c := st.tools[e.Tool]
		c[0]++
		if e.Error {
			c[1]++
		}
		st.tools[e.Tool] = c
	}
	for _, c := range t.Changes {
		p := c.Path
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) && t.Session.Cwd != "" {
			p = filepath.Join(t.Session.Cwd, p)
		}
		st.files[filepath.Clean(p)]++
	}
	return st
}

// sessionStats returns the stats of every transcript the search index has
// read, by session key, and how far it has got.
func (s *Search) sessionStats() (out map[string]*sessionStats, total, pending int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out = make(map[string]*sessionStats, len(s.byPath))
	for _, ss := range s.byPath {
		if ss.stats != nil {
			out[ss.key] = ss.stats
		}
	}
	return out, s.total, s.pending
}

// StatsQuery picks the sessions the stats cover. A zero From means since the
// first session; empty Root and nil Agents mean all of them.
type StatsQuery struct {
	From, To time.Time
	Root     string
	Agents   map[string]bool
}

type Stats struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Bucket string    `json:"bucket"` // day, week or month
	// Read and Total count transcripts the search index has read. Tool and
	// file numbers only cover read sessions, so the page asks again until
	// Read reaches Total.
	Read  int `json:"read"`
	Total int `json:"total"`

	Totals StatsTotals `json:"totals"`

	Series      []StatsBucket  `json:"series"`
	ProjectRows []StatsProject `json:"projects"`
	TopFiles    []StatsFile    `json:"files"`
	ToolRows    []StatsTool    `json:"tools"`
	AgentRows   []StatsAgent   `json:"agents"`
}

type StatsTotals struct {
	Sessions   int `json:"sessions"`
	Projects   int `json:"projects"`
	ActiveDays int `json:"activeDays"`
	Edits      int `json:"edits"`
	Files      int `json:"files"` // distinct files edited
	Tools      int `json:"tools"`
	Failed     int `json:"failed"`
	// Usage sums the tokens and API-equivalent cost of sessions that
	// recorded any; nil when none did.
	Usage *Usage `json:"usage,omitempty"`
}

// StatsBucket is one day, week or month. ByRoot counts sessions per project;
// CostByRoot and TokensByRoot are their usage.
type StatsBucket struct {
	Start        time.Time          `json:"start"`
	ByRoot       map[string]int     `json:"byRoot"`
	CostByRoot   map[string]float64 `json:"costByRoot,omitempty"`
	TokensByRoot map[string]int64   `json:"tokensByRoot,omitempty"`
	Tools        int                `json:"tools"`
	Failed       int                `json:"failed"`
}

type StatsProject struct {
	Root     string    `json:"root"`
	Sessions int       `json:"sessions"`
	Edits    int       `json:"edits"`
	Tools    int       `json:"tools"`
	Failed   int       `json:"failed"`
	Last     time.Time `json:"last"`
	Usage    *Usage    `json:"usage,omitempty"`
}

type StatsFile struct {
	Root     string `json:"root"`
	Path     string `json:"path"` // relative to the project when inside it
	Edits    int    `json:"edits"`
	Sessions int    `json:"sessions"`
}

type StatsTool struct {
	Provider string `json:"provider"`
	Tool     string `json:"tool"`
	Calls    int    `json:"calls"`
	Failed   int    `json:"failed"`
}

type StatsAgent struct {
	Provider string `json:"provider"`
	Sessions int    `json:"sessions"`
	Edits    int    `json:"edits"`
	Tools    int    `json:"tools"`
	Failed   int    `json:"failed"`
	Usage    *Usage `json:"usage,omitempty"`
}

const (
	statsTopFiles = 12
	statsTopTools = 12
)

// BuildStats aggregates indexed sessions. A session counts in the bucket it
// started in, in local time.
func BuildStats(sessions []*Session, per map[string]*sessionStats, q StatsQuery) *Stats {
	if q.To.IsZero() {
		q.To = time.Now()
	}
	var picked []*Session
	for _, s := range sessions {
		at := sessionStart(s)
		if q.Root != "" && s.Root != q.Root || q.Agents != nil && !q.Agents[s.Provider] {
			continue
		}
		if !q.From.IsZero() && at.Before(q.From) || at.After(q.To) {
			continue
		}
		picked = append(picked, s)
	}
	from := q.From
	if from.IsZero() {
		from = q.To
		for _, s := range picked {
			if at := sessionStart(s); at.Before(from) {
				from = at
			}
		}
	}
	st := &Stats{From: from, To: q.To, Bucket: bucketFor(from, q.To)}

	buckets := map[int64]*StatsBucket{} // by start, in Unix seconds
	for t := bucketStart(from, st.Bucket); !t.After(q.To); t = nextBucket(t, st.Bucket) {
		b := &StatsBucket{Start: t, ByRoot: map[string]int{}}
		buckets[t.Unix()] = b
		st.Series = append(st.Series, *b)
	}
	projects := map[string]*StatsProject{}
	agents := map[string]*StatsAgent{}
	tools := map[[2]string]*StatsTool{}
	type fileKey struct{ root, path string }
	files := map[fileKey]*StatsFile{}
	days := map[string]bool{}

	for _, s := range picked {
		at := sessionStart(s).Local()
		days[at.Format("2006-01-02")] = true
		st.Totals.Sessions++
		st.Totals.Edits += s.Edits

		p := projects[s.Root]
		if p == nil {
			p = &StatsProject{Root: s.Root}
			projects[s.Root] = p
		}
		p.Sessions++
		p.Edits += s.Edits
		if s.Ended.After(p.Last) {
			p.Last = s.Ended
		}
		a := agents[s.Provider]
		if a == nil {
			a = &StatsAgent{Provider: s.Provider}
			agents[s.Provider] = a
		}
		a.Sessions++
		a.Edits += s.Edits

		b := buckets[bucketStart(at, st.Bucket).Unix()]
		if b != nil {
			b.ByRoot[s.Root]++
		}
		if u := s.Usage; u != nil {
			addUsageTo(&st.Totals.Usage, u)
			addUsageTo(&p.Usage, u)
			addUsageTo(&a.Usage, u)
			if b != nil {
				if b.CostByRoot == nil {
					b.CostByRoot, b.TokensByRoot = map[string]float64{}, map[string]int64{}
				}
				b.CostByRoot[s.Root] += u.Cost
				b.TokensByRoot[s.Root] += u.Prompt() + u.Output
			}
		}

		ss := per[s.Key]
		if ss == nil {
			continue
		}
		for name, c := range ss.tools {
			st.Totals.Tools += c[0]
			st.Totals.Failed += c[1]
			p.Tools += c[0]
			p.Failed += c[1]
			a.Tools += c[0]
			a.Failed += c[1]
			if b != nil {
				b.Tools += c[0]
				b.Failed += c[1]
			}
			k := [2]string{s.Provider, name}
			t := tools[k]
			if t == nil {
				t = &StatsTool{Provider: s.Provider, Tool: name}
				tools[k] = t
			}
			t.Calls += c[0]
			t.Failed += c[1]
		}
		base := s.Root
		if s.Worktree != "" {
			base = s.Worktree
		}
		for path, n := range ss.files {
			k := fileKey{s.Root, relTo(base, path)}
			f := files[k]
			if f == nil {
				f = &StatsFile{Root: s.Root, Path: k.path}
				files[k] = f
			}
			f.Edits += n
			f.Sessions++
		}
	}

	for i := range st.Series {
		st.Series[i] = *buckets[st.Series[i].Start.Unix()]
	}
	st.Totals.Projects = len(projects)
	st.Totals.ActiveDays = len(days)
	st.Totals.Files = len(files)

	for _, p := range projects {
		st.ProjectRows = append(st.ProjectRows, *p)
	}
	sort.Slice(st.ProjectRows, func(i, j int) bool {
		a, b := st.ProjectRows[i], st.ProjectRows[j]
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Root < b.Root
	})
	for _, a := range agents {
		st.AgentRows = append(st.AgentRows, *a)
	}
	sort.Slice(st.AgentRows, func(i, j int) bool {
		a, b := st.AgentRows[i], st.AgentRows[j]
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Provider < b.Provider
	})
	for _, t := range tools {
		st.ToolRows = append(st.ToolRows, *t)
	}
	sort.Slice(st.ToolRows, func(i, j int) bool {
		a, b := st.ToolRows[i], st.ToolRows[j]
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return a.Provider+a.Tool < b.Provider+b.Tool
	})
	st.ToolRows = st.ToolRows[:min(len(st.ToolRows), statsTopTools)]
	for _, f := range files {
		st.TopFiles = append(st.TopFiles, *f)
	}
	sort.Slice(st.TopFiles, func(i, j int) bool {
		a, b := st.TopFiles[i], st.TopFiles[j]
		if a.Edits != b.Edits {
			return a.Edits > b.Edits
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Root+a.Path < b.Root+b.Path
	})
	st.TopFiles = st.TopFiles[:min(len(st.TopFiles), statsTopFiles)]

	// Lists, never null, so the page can always iterate.
	if st.ProjectRows == nil {
		st.ProjectRows = []StatsProject{}
	}
	if st.TopFiles == nil {
		st.TopFiles = []StatsFile{}
	}
	if st.ToolRows == nil {
		st.ToolRows = []StatsTool{}
	}
	if st.AgentRows == nil {
		st.AgentRows = []StatsAgent{}
	}
	return st
}

// addUsageTo adds u into *dst, allocating it on first use.
func addUsageTo(dst **Usage, u *Usage) {
	if *dst == nil {
		*dst = &Usage{}
	}
	(*dst).add(*u)
}

func sessionStart(s *Session) time.Time {
	if !s.Started.IsZero() {
		return s.Started
	}
	return s.Ended
}

// bucketFor keeps the chart to a readable number of columns: days up to a
// month, weeks up to about a year, months beyond.
func bucketFor(from, to time.Time) string {
	switch span := to.Sub(from); {
	case span <= 31*24*time.Hour:
		return "day"
	case span <= 380*24*time.Hour:
		return "week"
	}
	return "month"
}

func bucketStart(t time.Time, bucket string) time.Time {
	t = t.Local()
	y, m, d := t.Date()
	switch bucket {
	case "week": // weeks start on Monday
		wd := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-wd, 0, 0, 0, 0, time.Local)
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func nextBucket(t time.Time, bucket string) time.Time {
	switch bucket {
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(0, 0, 1)
}

// parseAgents reads a comma-separated provider list; empty means all.
func parseAgents(v string) map[string]bool {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := map[string]bool{}
	for _, a := range strings.Split(v, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out[a] = true
		}
	}
	return out
}

// printStats writes the -stats report.
func printStats(st *Stats, days, home string) {
	w := os.Stdout
	span := "since " + st.From.Local().Format("2006-01-02")
	if days != "all" {
		span = "last " + days + " days, " + st.From.Local().Format("2006-01-02") + " to " + st.To.Local().Format("2006-01-02")
	}
	t := st.Totals
	fmt.Fprintf(w, "trail stats, %s\n", span)
	if t.Sessions == 0 {
		fmt.Fprintln(w, "  no sessions")
		return
	}
	fmt.Fprintf(w, "  %s in %s on %s; %s to %s\n", count(t.Sessions, "session"), count(t.Projects, "project"),
		count(t.ActiveDays, "day"), count(t.Edits, "edit"), count(t.Files, "file"))
	fmt.Fprintf(w, "  %s, %d failed (%s)\n", count(t.Tools, "tool call"), t.Failed, pct(t.Failed, t.Tools))
	if u := t.Usage; u != nil {
		fmt.Fprintf(w, "  %s tokens, %s API-equivalent\n", tokensHuman(u.Prompt()+u.Output), costText(u))
	}

	short := func(root string) string {
		if home != "" && strings.HasPrefix(root, home+string(filepath.Separator)) {
			return "~" + root[len(home):]
		}
		return root
	}
	fmt.Fprintln(w, "\nProjects")
	for _, p := range st.ProjectRows[:min(len(st.ProjectRows), 10)] {
		fmt.Fprintf(w, "  %-40s %12s %10s  %s\n", oneLine(short(p.Root), 40), count(p.Sessions, "session"), count(p.Edits, "edit"), failRate(p.Failed, p.Tools))
	}
	if len(st.TopFiles) > 0 {
		fmt.Fprintln(w, "\nMost edited files")
		for _, f := range st.TopFiles[:min(len(st.TopFiles), 10)] {
			path := f.Path
			if st.Totals.Projects > 1 && !filepath.IsAbs(path) {
				path = filepath.Base(f.Root) + "/" + path
			}
			fmt.Fprintf(w, "  %-52s %9s in %s\n", oneLine(short(path), 52), count(f.Edits, "edit"), count(f.Sessions, "session"))
		}
	}
	if len(st.ToolRows) > 0 {
		fmt.Fprintln(w, "\nMost used tools")
		for _, r := range st.ToolRows[:min(len(st.ToolRows), 10)] {
			fmt.Fprintf(w, "  %-9s %-24s %11s  %d failed (%s)\n", r.Provider, oneLine(r.Tool, 24), count(r.Calls, "call"), r.Failed, pct(r.Failed, r.Calls))
		}
	}
	fmt.Fprintln(w, "\nAgents")
	for _, a := range st.AgentRows {
		fmt.Fprintf(w, "  %-9s %12s %10s  %s\n", a.Provider, count(a.Sessions, "session"), count(a.Edits, "edit"), failRate(a.Failed, a.Tools))
	}
	if st.Read < st.Total {
		fmt.Fprintf(w, "\n(tool and file numbers cover %d of %d sessions)\n", st.Read, st.Total)
	}
}

func count(n int, one string) string {
	return strconv.Itoa(n) + " " + plural(n, one, one+"s")
}

// costText is a usage's cost, marked as a floor when some responses had no
// price.
func costText(u *Usage) string {
	if u.Unpriced > 0 && u.Cost == 0 {
		return "no price for " + strings.Join(u.UnpricedModels, ", ") + ", so no cost"
	}
	c := fmt.Sprintf("$%.2f", u.Cost)
	if u.Unpriced > 0 {
		c = "at least " + c
	}
	return c
}

func tokensHuman(n int64) string {
	switch {
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
	case n >= 1e6:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1e4:
		return strconv.FormatInt(n/1000, 10) + "K"
	}
	return strconv.FormatInt(n, 10)
}

func failRate(failed, tools int) string {
	if tools == 0 {
		return "no tool calls"
	}
	return pct(failed, tools) + " of tool calls failed"
}

func pct(n, of int) string {
	if of == 0 {
		return "—"
	}
	return strconv.FormatFloat(100*float64(n)/float64(of), 'f', 1, 64) + "%"
}

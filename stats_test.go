package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestBuildStats(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 10, 0, 0, 0, time.Local) }
	sessions := []*Session{
		{Key: "claude:a", Provider: "claude", Root: "/w/shop", Started: day(1), Ended: day(1), Edits: 3},
		{Key: "claude:b", Provider: "claude", Root: "/w/shop", Worktree: "/w/shop-wt", Started: day(2), Ended: day(2), Edits: 1},
		{Key: "codex:c", Provider: "codex", Root: "/w/api", Started: day(20), Ended: day(21),
			Usage: &Usage{Input: 100, Output: 50, Cost: 0.25, Responses: 2}},
		{Key: "codex:old", Provider: "codex", Root: "/w/api", Started: day(1).AddDate(0, -3, 0)},
	}
	per := map[string]*sessionStats{
		"claude:a": {tools: map[string][2]int{"Bash": {4, 1}, "Edit": {3, 0}}, files: map[string]int{"/w/shop/cart.go": 2, "/w/shop/README.md": 1}},
		"claude:b": {tools: map[string][2]int{"Bash": {2, 1}}, files: map[string]int{"/w/shop-wt/cart.go": 1}},
		"codex:c":  {tools: map[string][2]int{"shell": {5, 0}}},
	}
	q := StatsQuery{From: day(1), To: day(22)}
	st := BuildStats(sessions, per, q)

	if tt := st.Totals; tt.Sessions != 3 || tt.Projects != 2 || tt.ActiveDays != 3 || tt.Edits != 4 || tt.Tools != 14 || tt.Failed != 2 || tt.Files != 2 {
		t.Errorf("totals: %+v", tt)
	}
	if st.Bucket != "day" || len(st.Series) != 22 {
		t.Fatalf("bucket %s with %d buckets", st.Bucket, len(st.Series))
	}
	if b := st.Series[1]; b.ByRoot["/w/shop"] != 1 || b.Tools != 2 || b.Failed != 1 {
		t.Errorf("Oct 2: %+v", b)
	}
	// A worktree's edits count under the main project's paths.
	if f := st.TopFiles[0]; f.Path != "cart.go" || f.Root != "/w/shop" || f.Edits != 3 || f.Sessions != 2 {
		t.Errorf("top file: %+v", f)
	}
	if r := st.ToolRows[0]; r.Tool != "Bash" || r.Calls != 6 || r.Failed != 2 {
		t.Errorf("top tool: %+v", r)
	}
	if p := st.ProjectRows[0]; p.Root != "/w/shop" || p.Sessions != 2 || p.Tools != 9 || p.Failed != 2 {
		t.Errorf("top project: %+v", p)
	}
	if len(st.AgentRows) != 2 || st.AgentRows[0].Provider != "claude" {
		t.Errorf("agents: %+v", st.AgentRows)
	}
	// Usage sums only sessions that recorded tokens.
	if u := st.Totals.Usage; u == nil || u.Cost != 0.25 || u.Prompt()+u.Output != 150 {
		t.Errorf("usage: %+v", u)
	}
	if b := st.Series[19]; b.CostByRoot["/w/api"] != 0.25 || b.TokensByRoot["/w/api"] != 150 {
		t.Errorf("Oct 20 usage: %+v", b)
	}
	if st.ProjectRows[0].Usage != nil || st.ProjectRows[1].Usage == nil {
		t.Errorf("project usage: %+v", st.ProjectRows)
	}

	// Filters by project and agent.
	q.Root = "/w/api"
	if st := BuildStats(sessions, per, q); st.Totals.Sessions != 1 || len(st.TopFiles) != 0 || st.TopFiles == nil {
		t.Errorf("root filter: %+v files=%v", st.Totals, st.TopFiles)
	}
	q.Root, q.Agents = "", map[string]bool{"codex": true}
	if st := BuildStats(sessions, per, q); st.Totals.Sessions != 1 || st.Totals.Tools != 5 {
		t.Errorf("agent filter: %+v", st.Totals)
	}

	// All time starts at the first session; months only past about a year...
	st = BuildStats(sessions, per, StatsQuery{To: day(22)})
	if st.Totals.Sessions != 4 || st.Bucket != "week" || !st.Series[0].Start.Equal(bucketStart(day(1).AddDate(0, -3, 0), "week")) {
		t.Errorf("all time: %d sessions, %s from %v", st.Totals.Sessions, st.Bucket, st.Series[0].Start)
	}
	if st := BuildStats(sessions, per, StatsQuery{From: day(1).AddDate(-2, 0, 0), To: day(22)}); st.Bucket != "month" {
		t.Errorf("two years should bucket by month, got %s", st.Bucket)
	}
	// A session with no timestamps doesn't stretch the range.
	undated := append(sessions, &Session{Key: "claude:z", Provider: "claude", Root: "/w/shop"})
	if st := BuildStats(undated, per, StatsQuery{To: day(22)}); st.Totals.Sessions != 5 || st.Bucket != "week" {
		t.Errorf("undated: %d sessions, %s buckets", st.Totals.Sessions, st.Bucket)
	}
	// ...and an empty range still answers with lists.
	if st := BuildStats(nil, nil, StatsQuery{To: day(22)}); st.Totals.Sessions != 0 || st.ProjectRows == nil || len(st.Series) != 1 {
		t.Errorf("empty: %+v", st)
	}
}

func TestBucketStart(t *testing.T) {
	thu := time.Date(2026, 10, 22, 23, 30, 0, 0, time.Local)
	if got := bucketStart(thu, "week"); got.Weekday() != time.Monday || got.Day() != 19 {
		t.Errorf("week starts %v", got)
	}
	if got := bucketStart(thu, "month"); got.Day() != 1 || got.Month() != 10 {
		t.Errorf("month starts %v", got)
	}
	sun := time.Date(2026, 10, 25, 8, 0, 0, 0, time.Local)
	if got := bucketStart(sun, "week"); got.Day() != 19 {
		t.Errorf("Sunday belongs to the week from Monday the 19th, got %v", got)
	}
}

// TestStatsAPI runs every adapter's fixtures through the search index and
// checks the counts the stats view gets from full transcripts.
func TestStatsAPI(t *testing.T) {
	idx := NewIndex(&claudeProvider{dir: "testdata/claude"}, &codexProvider{dir: "testdata/codex"},
		&opencodeProvider{dir: "testdata/opencode"}, &geminiProvider{dir: "testdata/gemini"},
		&kiroProvider{dir: "testdata/kiro/home", dataDir: "testdata/kiro/data"})
	idx.Scan(0)
	srch := NewSearch(idx)
	srch.Sync()
	srv := httptest.NewServer((&Server{index: idx, search: srch, home: "/work"}).Handler())
	defer srv.Close()

	var st Stats
	getJSON(t, srv.URL+"/api/stats?days=all", &st)
	if n := len(idx.Sessions()); st.Read != st.Total || st.Total != n || st.Totals.Sessions != n || n < 10 {
		t.Fatalf("read %d of %d, %d sessions", st.Read, st.Total, st.Totals.Sessions)
	}
	if st.Totals.Failed == 0 || st.Totals.Tools <= st.Totals.Failed {
		t.Errorf("tool calls: %+v", st.Totals)
	}
	providers := map[string]bool{}
	for _, a := range st.AgentRows {
		providers[a.Provider] = true
	}
	for _, p := range []string{"claude", "codex", "opencode", "gemini", "kiro"} {
		if !providers[p] {
			t.Errorf("no stats for %s: %+v", p, st.AgentRows)
		}
	}
	found := false
	for _, f := range st.TopFiles {
		if f.Root == "/work/shop" && f.Path == "cart.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("cart.go missing from top files: %+v", st.TopFiles)
	}

	getJSON(t, srv.URL+"/api/stats?days=all&agents=claude&root=/work/shop", &st)
	if st.Totals.Sessions != 3 || len(st.AgentRows) != 1 {
		t.Errorf("filtered: %+v %+v", st.Totals, st.AgentRows)
	}
}

// TestStatsUsage checks token and cost totals from the usage fixtures.
func TestStatsUsage(t *testing.T) {
	idx := NewIndex(&claudeProvider{dir: "testdata/usage/claude"}, &codexProvider{dir: "testdata/usage/codex"}, &geminiProvider{dir: "testdata/usage/gemini"})
	idx.Scan(0)
	srch := NewSearch(idx)
	srch.Sync()
	st := (&Server{index: idx, search: srch}).stats("all", "", "")
	u := st.Totals.Usage
	if u == nil || u.Cost <= 0 || u.Prompt() == 0 || u.Output == 0 {
		t.Fatalf("usage: %+v", u)
	}
	var sum float64
	for _, a := range st.AgentRows {
		if a.Usage != nil {
			sum += a.Usage.Cost
		}
	}
	if d := sum - u.Cost; d > 1e-9 || d < -1e-9 {
		t.Errorf("agents add up to %v, total %v", sum, u.Cost)
	}
}

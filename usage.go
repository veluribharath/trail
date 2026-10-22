package main

// Usage is the tokens one model response was billed for, as the agent
// recorded them, and what they would cost at API prices. On a session it is
// the sum over every response, subagents included.
//
// The agents count tokens differently; adapters normalise them so that Input
// never includes cached tokens and Output always includes reasoning.
type Usage struct {
	Input        int64   `json:"input"`                  // uncached input
	CacheRead    int64   `json:"cacheRead,omitempty"`    // cache hits
	CacheWrite   int64   `json:"cacheWrite,omitempty"`   // cache writes, 5-minute or unspecified lifetime
	CacheWrite1h int64   `json:"cacheWrite1h,omitempty"` // cache writes with a 1-hour lifetime (Claude)
	Output       int64   `json:"output"`                 // includes reasoning
	Reasoning    int64   `json:"reasoning,omitempty"`    // the part of Output spent thinking
	Context      int64   `json:"context,omitempty"`      // prompt size of the largest response
	Cost         float64 `json:"cost"`                   // USD at API prices
	Responses    int     `json:"responses"`
	Unpriced     int     `json:"unpriced,omitempty"` // responses with no price for their model
	// Models lists models with no price, so the UI can say which.
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

// Prompt is how many tokens the model read for this response.
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.CacheWrite1h }

func (u Usage) empty() bool {
	return u.Prompt() == 0 && u.Output == 0 && u.Cost == 0
}

// add folds b into u.
func (u *Usage) add(b Usage) {
	u.Input += b.Input
	u.CacheRead += b.CacheRead
	u.CacheWrite += b.CacheWrite
	u.CacheWrite1h += b.CacheWrite1h
	u.Output += b.Output
	u.Reasoning += b.Reasoning
	u.Context = max(u.Context, b.Context)
	u.Cost += b.Cost
	u.Responses += b.Responses
	u.Unpriced += b.Unpriced
	for _, m := range b.UnpricedModels {
		if !contains(u.UnpricedModels, m) {
			u.UnpricedModels = append(u.UnpricedModels, m)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// step is one model response before it is priced.
type step struct {
	Usage
	model string
	// recorded is a cost the agent worked out itself (OpenCode), used for
	// models trail has no price for.
	recorded float64
	fast     bool  // Claude fast mode
	usOnly   bool  // Claude inference_geo "us"
	searches int64 // Claude server-side web searches
}

// addUsage prices a response and adds it to the session total and, when ev
// is an event index, to that event. Responses with no tokens are ignored.
func (t *Transcript) addUsage(st step, ev int) {
	if st.empty() && st.recorded == 0 {
		return
	}
	u := prices.price(st)
	s := t.Session
	if s.Usage == nil {
		s.Usage = &Usage{}
	}
	s.Usage.add(u)
	if ev >= 0 && ev < len(t.Events) {
		e := &t.Events[ev]
		if e.Usage == nil {
			e.Usage = &Usage{}
		}
		e.Usage.add(u)
	}
}

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Prices turn token counts into dollars. trail never fetches prices: they
// ship in prices.json, embedded at build time, and a prices.json in trail's
// config directory is layered on top for models the bundled table lacks or
// rates that differ. The figures are API list prices; someone on a
// subscription pays a flat fee instead, so the UI calls them API-equivalent.

//go:embed prices.json
var bundledPrices []byte

// rates are USD per million tokens.
type rates struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cacheRead"`
	CacheWrite   float64 `json:"cacheWrite,omitempty"`   // 5-minute writes; 0 means writes cost the input rate
	CacheWrite1h float64 `json:"cacheWrite1h,omitempty"` // 0 means the 5-minute rate
}

type priceEntry struct {
	Models []string `json:"models"`
	rates
	// Above applies instead of the base rates to a response whose prompt
	// (input plus cache reads and writes) is over Tokens.
	Above *struct {
		Tokens int64 `json:"tokens"`
		rates
	} `json:"above,omitempty"`
	Fast   *rates `json:"fast,omitempty"` // Claude fast mode
	Source string `json:"source,omitempty"`
	origin string // "bundled" or the override file's path
}

type priceTable struct {
	AsOf    string       `json:"asOf"`
	Entries []priceEntry `json:"prices"`
	byModel map[string]*priceEntry
}

// webSearchCost is Claude's server-side web search, per search.
const webSearchCost = 10.0 / 1000

var prices = mustPrices()

func mustPrices() *priceTable {
	t, err := parsePrices(bundledPrices, "bundled")
	if err != nil {
		panic("prices.json: " + err.Error())
	}
	return t
}

func parsePrices(b []byte, origin string) (*priceTable, error) {
	var t priceTable
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	t.byModel = map[string]*priceEntry{}
	for i := range t.Entries {
		e := &t.Entries[i]
		e.origin = origin
		for _, m := range e.Models {
			t.byModel[normModel(m)] = e
		}
	}
	return &t, nil
}

// pricesPath is the override file: prices.json in trail's config directory.
func pricesPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "trail", "prices.json")
}

// loadPriceOverrides layers the override file over the bundled table. A
// missing file is not an error.
func loadPriceOverrides(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	o, err := parsePrices(b, path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	merged := &priceTable{AsOf: prices.AsOf, byModel: map[string]*priceEntry{}}
	for k, e := range prices.byModel {
		merged.byModel[k] = e
	}
	for k, e := range o.byModel {
		merged.byModel[k] = e
	}
	merged.Entries = append(append([]priceEntry{}, o.Entries...), prices.Entries...)
	prices = merged
	return nil
}

var (
	dateSuffix   = regexp.MustCompile(`(-\d{4}-\d{2}-\d{2}|-\d{8}|@\d{8}|-v\d+(:\d+)?)$`)
	claudeDotVer = regexp.MustCompile(`(\d)\.(\d)`)
)

// normModel maps the ways agents spell a model to one key: provider and
// region prefixes (anthropic/, us.anthropic.), context markers ([1m]) and
// dated snapshot suffixes are dropped, and Claude's dotted versions
// (claude-sonnet-4.5, as OpenRouter writes them) become dashed. Anything else
// must match exactly, so an unknown variant is left unpriced rather than
// priced as its nearest neighbour.
func normModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "anthropic."); i >= 0 {
		m = m[i+len("anthropic."):]
	}
	m = strings.TrimSuffix(m, "[1m]")
	m = strings.TrimSuffix(m, "-1m")
	for {
		n := dateSuffix.ReplaceAllString(m, "")
		if n == m {
			break
		}
		m = n
	}
	if strings.HasPrefix(m, "claude") {
		m = claudeDotVer.ReplaceAllString(m, "$1-$2")
	}
	return m
}

func (t *priceTable) lookup(model string) *priceEntry {
	if model == "" {
		return nil
	}
	return t.byModel[normModel(model)]
}

// price fills in a response's cost. A model with no price keeps the agent's
// own figure if it recorded one, and is otherwise counted as unpriced.
func (t *priceTable) price(st step) Usage {
	u := st.Usage
	u.Responses = 1
	u.Context = u.Prompt()
	e := t.lookup(st.model)
	if e == nil {
		if st.recorded > 0 {
			u.Cost = st.recorded
		} else {
			u.Cost = 0
			u.Unpriced = 1
			name := st.model
			if name == "" {
				name = "unknown model"
			}
			u.UnpricedModels = []string{name}
		}
		return u
	}
	r := e.rates
	if e.Above != nil && u.Prompt() > e.Above.Tokens {
		r = e.Above.rates
	}
	if st.fast && e.Fast != nil {
		r = *e.Fast
	}
	w5 := r.CacheWrite
	if w5 == 0 {
		w5 = r.Input
	}
	w1 := r.CacheWrite1h
	if w1 == 0 {
		w1 = w5
	}
	c := (float64(u.Input)*r.Input + float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*w5 +
		float64(u.CacheWrite1h)*w1 + float64(u.Output)*r.Output) / 1e6
	if st.usOnly {
		c *= 1.1
	}
	c += float64(st.searches) * webSearchCost
	u.Cost = c
	return u
}

// printPrices writes the table for -prices.
func printPrices(w io.Writer) {
	fmt.Fprintf(w, "API prices in USD per million tokens, bundled as of %s.\n", prices.AsOf)
	if p := pricesPath(); p != "" {
		fmt.Fprintf(w, "Overrides are read from %s.\n", p)
	}
	fmt.Fprintf(w, "\n%-34s %8s %9s %9s %9s %8s\n", "model", "input", "cache hit", "write 5m", "write 1h", "output")
	keys := make([]string, 0, len(prices.byModel))
	for k := range prices.byModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	num := func(v float64) string {
		if v == 0 {
			return "-"
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
	}
	for _, k := range keys {
		e := prices.byModel[k]
		w5, w1 := e.CacheWrite, e.CacheWrite1h
		note := ""
		if e.Above != nil {
			note = fmt.Sprintf("  (more above %s prompt tokens)", tokensShort(e.Above.Tokens))
		}
		if e.origin != "bundled" {
			note += "  (override)"
		}
		fmt.Fprintf(w, "%-34s %8s %9s %9s %9s %8s%s\n", k, num(e.Input), num(e.CacheRead), num(w5), num(w1), num(e.Output), note)
	}
}

func tokensShort(n int64) string {
	if n >= 1000 && n%1000 == 0 {
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprint(n)
}

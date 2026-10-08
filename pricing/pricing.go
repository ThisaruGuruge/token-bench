// Package pricing estimates USD cost from token usage using an in-repo price table.
//
// Prices are the Anthropic list prices per model as of 2026-10-08 (input,
// output and cache read from the claude-api model table bundled with Claude
// Code 2.1.293). Cache writes cost 1.25x input for the 5-minute TTL and 2x
// input for the 1-hour TTL. Claude Code mostly writes with the 1-hour TTL (97%
// of cache-write tokens in local sessions on 2026-10-08), so an estimate that
// does not know the TTL split (Cost) understates cache writes; UsageCost takes
// the split from the stream log. Claude Code's own total_cost_usd is recorded
// beside the estimate.
package pricing

import (
	"strings"

	"token-bench/result"
)

// Price is USD per million tokens. CacheWrite is the 5-minute TTL write price,
// CacheWrite1h the 1-hour one.
type Price struct {
	Input        float64
	Output       float64
	CacheRead    float64
	CacheWrite   float64
	CacheWrite1h float64
}

func withCacheWrite(input, output, cacheRead float64) Price {
	return Price{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: input * 1.25, CacheWrite1h: input * 2}
}

// Table maps a model id (or Claude Code alias) to its price. Lookup also
// matches ids carrying a suffix such as "[1m]" or a date.
var Table = map[string]Price{
	"claude-opus-5-5":   withCacheWrite(4, 20, 0.20),
	"claude-opus-5":     withCacheWrite(5, 25, 0.50),
	"claude-opus-4-8":   withCacheWrite(5, 25, 0.50),
	"claude-opus-4-7":   withCacheWrite(5, 25, 0.50),
	"claude-opus-4-6":   withCacheWrite(5, 25, 0.50),
	"claude-sonnet-5-5": withCacheWrite(2, 10, 0.20),
	"claude-sonnet-5":   withCacheWrite(2, 10, 0.20),
	"claude-sonnet-4-6": withCacheWrite(3, 15, 0.30),
	// Haiku 5.5: prompts over 100K tokens cost $0.50/$2.50 (not modelled); cache
	// read assumed at the usual 0.1x input.
	"claude-haiku-5-5": withCacheWrite(0.10, 0.50, 0.01),
	"claude-haiku-4-5": withCacheWrite(1, 5, 0.10),
	"claude-fable-5-1": withCacheWrite(10, 50, 0.25),
	"claude-fable-5":   withCacheWrite(10, 50, 1),

	// Claude Code aliases, resolved by the model catalog baked into Claude
	// Code 2.1.293 (latest_per_family). An ANTHROPIC_DEFAULT_*_MODEL variable or
	// a managed setting can change the resolution; results price the resolved
	// ids from the stream instead when they have them.
	"opus":   withCacheWrite(4, 20, 0.20),
	"sonnet": withCacheWrite(2, 10, 0.20),
	"haiku":  withCacheWrite(0.10, 0.50, 0.01),
	"fable":  withCacheWrite(10, 50, 0.25),
}

// Lookup finds the price for model: exact match first, then the longest table
// key that prefixes it (so "claude-opus-5-5[1m]" and dated ids resolve).
func Lookup(model string) (Price, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if price, ok := Table[model]; ok {
		return price, true
	}
	var best string
	for key := range Table {
		if strings.HasPrefix(model, key) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return Price{}, false
	}
	return Table[best], true
}

// Cost returns the estimated USD cost of tokens at model's price.
func Cost(model string, tokens result.TokenCount) (float64, bool) {
	price, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	return (float64(tokens.Input)*price.Input +
		float64(tokens.Output)*price.Output +
		float64(tokens.CacheRead)*price.CacheRead +
		float64(tokens.CacheWrite)*price.CacheWrite) / million, true
}

// Usage is token usage with cache writes split by TTL.
type Usage struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
}

// UsageCost returns the estimated USD cost of usage at model's price, pricing
// each cache-write TTL separately.
func UsageCost(model string, usage Usage) (float64, bool) {
	price, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	return (float64(usage.Input)*price.Input +
		float64(usage.Output)*price.Output +
		float64(usage.CacheRead)*price.CacheRead +
		float64(usage.CacheWrite5m)*price.CacheWrite +
		float64(usage.CacheWrite1h)*price.CacheWrite1h) / million, true
}

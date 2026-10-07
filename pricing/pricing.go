// Package pricing estimates USD cost from token usage using an in-repo price table.
//
// Prices come from the Anthropic pricing published for each model as of
// 2026-10-07 (input, output and cache read per the claude-api model table).
// Cache write assumes the standard 5-minute prompt-cache write price of 1.25x
// the input price; verify it before relying on absolute numbers. Claude Code's
// own total_cost_usd is preferred when available and recorded beside this
// estimate.
package pricing

import (
	"strings"

	"token-bench/result"
)

// Price is USD per million tokens.
type Price struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

func withCacheWrite(input, output, cacheRead float64) Price {
	return Price{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: input * 1.25}
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
	"claude-haiku-4-5":  withCacheWrite(1, 5, 0.10),
	"claude-fable-5-1":  withCacheWrite(10, 50, 0.25),
	"claude-fable-5":    withCacheWrite(10, 50, 0.25),

	// Claude Code aliases. PLACEHOLDER mapping to the current model of each
	// family as of 2026-10-07; verify against what the alias resolves to.
	"opus":   withCacheWrite(4, 20, 0.20),
	"sonnet": withCacheWrite(2, 10, 0.20),
	"haiku":  withCacheWrite(1, 5, 0.10),
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

package pricing

import (
	"math"
	"testing"

	"token-bench/result"
)

func TestCost(t *testing.T) {
	tokens := result.TokenCount{Input: 1_000_000, Output: 500_000, CacheRead: 2_000_000, CacheWrite: 100_000}
	got, ok := Cost("claude-sonnet-5-5", tokens)
	if !ok {
		t.Fatal("model not found")
	}
	// 1M*2 + 0.5M*10 + 2M*0.20 + 0.1M*2.5 = 2 + 5 + 0.4 + 0.25
	if math.Abs(got-7.65) > 1e-9 {
		t.Fatalf("cost = %v, want 7.65", got)
	}
}

func TestLookupMatchesSuffixesAndPrefersLongestKey(t *testing.T) {
	for model, want := range map[string]Price{
		"opus":                      Table["opus"],
		"Claude-Opus-5-5[1m]":       Table["claude-opus-5-5"],
		"claude-opus-5-20261001":    Table["claude-opus-5"],
		"claude-haiku-4-5-20251001": Table["claude-haiku-4-5"],
	} {
		got, ok := Lookup(model)
		if !ok || got != want {
			t.Errorf("Lookup(%q) = %+v, %v; want %+v", model, got, ok, want)
		}
	}
}

func TestUnknownModelHasNoCost(t *testing.T) {
	if _, ok := Cost("gpt-5.6-sol", result.TokenCount{Input: 1}); ok {
		t.Fatal("unknown model should have no price")
	}
}

func TestUsageCostPricesEachCacheTTL(t *testing.T) {
	usage := Usage{Input: 1_000_000, Output: 500_000, CacheRead: 2_000_000, CacheWrite5m: 100_000, CacheWrite1h: 1_000_000}
	got, ok := UsageCost("claude-sonnet-5-5", usage)
	if !ok {
		t.Fatal("model not found")
	}
	// 2 + 5 + 0.4 + 0.1M*2.5 + 1M*4 = 11.65
	if math.Abs(got-11.65) > 1e-9 {
		t.Fatalf("cost = %v, want 11.65", got)
	}
}

func TestAliasesMatchCurrentModels(t *testing.T) {
	for alias, model := range map[string]string{
		"opus": "claude-opus-5-5", "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5", "fable": "claude-fable-5-1",
	} {
		if Table[alias] != Table[model] {
			t.Errorf("alias %q priced %+v; %s is %+v", alias, Table[alias], model, Table[model])
		}
	}
}

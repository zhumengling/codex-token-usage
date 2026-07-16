package main

import (
	"math"
	"testing"
)

func TestNormalizeCacheTokensPreservesDetailsWithoutOverlap(t *testing.T) {
	tests := []struct {
		name               string
		cached, read, make int64
		want               cacheTokenBreakdown
	}{
		{name: "umbrella", cached: 1000, read: 800, make: 200, want: cacheTokenBreakdown{Read: 800, Creation: 200, Total: 1000}},
		{name: "details exceed umbrella", cached: 800, read: 600, make: 500, want: cacheTokenBreakdown{Read: 600, Creation: 500, Total: 1100}},
		{name: "legacy remainder", cached: 1000, read: 800, want: cacheTokenBreakdown{Read: 1000, Total: 1000}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeCacheTokens(test.cached, test.read, test.make); got != test.want {
				t.Fatalf("normalizeCacheTokens()=%+v, want %+v", got, test.want)
			}
		})
	}
}

func TestCostForTokensUsesOpenAICacheWriteAsInputSubset(t *testing.T) {
	prices := map[string]modelPrice{
		"gpt-test": {
			Prompt: 5, Completion: 30,
			CacheRead: 0.5, CacheCreation: 6.25,
			CacheReadSet: true, CacheCreateSet: true,
		},
	}
	row := costTokenRow{
		Model: "gpt-test", Provider: "codex",
		InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
		CachedTokens: 600, CacheReadTokens: 600, CacheCreationTokens: 100,
	}
	got, ok := costForTokens(row, prices)
	if !ok {
		t.Fatal("cost unavailable")
	}
	want := (300*5.0 + 100*30.0 + 600*0.5 + 100*6.25) / 1_000_000
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost=%v, want %v", got, want)
	}
}

func TestCostForTokensKeepsAnthropicNonCachedInput(t *testing.T) {
	prices := map[string]modelPrice{
		"claude-test": {
			Prompt: 10, Completion: 50,
			CacheCreation: 12.5, CacheCreateSet: true,
		},
	}
	row := costTokenRow{
		Model: "claude-test", Provider: "claude",
		InputTokens: 100, OutputTokens: 50, TotalTokens: 1150,
		CachedTokens: 1000, CacheCreationTokens: 1000,
	}
	got, ok := costForTokens(row, prices)
	if !ok {
		t.Fatal("cost unavailable")
	}
	want := (100*10.0 + 50*50.0 + 1000*12.5) / 1_000_000
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost=%v, want %v", got, want)
	}
}

func TestModelPriceUsesExplicitFlexPrices(t *testing.T) {
	price, ok := modelPriceFromJSON(map[string]any{
		"input_cost_per_token":                 10e-6,
		"output_cost_per_token":                50e-6,
		"cache_read_input_token_cost":          1e-6,
		"cache_creation_input_token_cost":      12.5e-6,
		"input_cost_per_token_flex":            5e-6,
		"output_cost_per_token_flex":           25e-6,
		"cache_read_input_token_cost_flex":     0.5e-6,
		"cache_creation_input_token_cost_flex": 6.25e-6,
	})
	if !ok {
		t.Fatal("price not parsed")
	}
	prices := map[string]modelPrice{"tier-test": price}
	got, ok := costForTokens(costTokenRow{
		Model: "tier-test", Provider: "codex", ServiceTier: "flex",
		InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
		CachedTokens: 600, CacheReadTokens: 600, CacheCreationTokens: 100,
	}, prices)
	if !ok {
		t.Fatal("cost unavailable")
	}
	want := (300*5.0 + 100*25.0 + 600*0.5 + 100*6.25) / 1_000_000
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("flex cost=%v, want %v", got, want)
	}
}

func TestModelPriceDerivesMissingTierCachePricesFromPromptRatio(t *testing.T) {
	price := modelPrice{
		Prompt:         10,
		Completion:     50,
		CacheRead:      1,
		CacheCreation:  12.5,
		CacheReadSet:   true,
		CacheCreateSet: true,
		Flex: modelTierPrice{
			Prompt: 5, Completion: 25,
			PromptSet: true, CompletionSet: true,
		},
	}
	got := effectiveModelPriceForServiceTier(price, "tier-test", "flex")
	if got.Prompt != 5 || got.Completion != 25 || got.CacheRead != 0.5 || got.CacheCreation != 6.25 {
		t.Fatalf("derived flex price=%+v, want prompt=5 completion=25 cache-read=0.5 cache-write=6.25", got)
	}
}

func TestFallbackUsageTotalIsProviderAware(t *testing.T) {
	codex := usageRecord{
		Provider: "codex", Model: "gpt-test",
		Detail: usageDetail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 20, CachedTokens: 800, CacheReadTokens: 800, CacheCreationTokens: 100},
	}
	if got := fallbackUsageTotal(codex); got != 1100 {
		t.Fatalf("Codex fallback total=%d, want 1100", got)
	}
	claude := usageRecord{
		Provider: "claude", Model: "claude-test",
		Detail: usageDetail{InputTokens: 100, OutputTokens: 50, CachedTokens: 1000, CacheCreationTokens: 1000},
	}
	if got := fallbackUsageTotal(claude); got != 1150 {
		t.Fatalf("Claude fallback total=%d, want 1150", got)
	}
	partial := usageRecord{
		Provider: "codex", Model: "gpt-test",
		Detail: usageDetail{ReasoningTokens: 20, CachedTokens: 1000, CacheReadTokens: 1000},
	}
	if got := fallbackUsageTotal(partial); got != 1020 {
		t.Fatalf("partial fallback total=%d, want 1020", got)
	}
}

func TestCacheRateBackendUsesReadOnlyAndProviderAwareInput(t *testing.T) {
	wantClaude := 800.0 * 100 / 1100.0
	if got := cacheRateBackend("claude", "claude-test", 1150, 100, 50, 1000, 800, 200); math.Abs(got-wantClaude) > 1e-12 {
		t.Fatalf("Claude cache rate=%v, want %v", got, wantClaude)
	}
	if got := cacheRateBackend("codex", "gpt-test", 1100, 1000, 100, 700, 600, 100); math.Abs(got-60) > 1e-12 {
		t.Fatalf("Codex cache rate=%v, want 60", got)
	}
}

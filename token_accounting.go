package main

import "strings"

type cacheTokenBreakdown struct {
	Read     int64
	Creation int64
	Total    int64
}

// normalizeCacheTokens treats cached_tokens as a legacy/umbrella field and
// cache_read/cache_creation as more specific details. This keeps overlapping
// host fields from being counted twice while preserving any legacy remainder.
func normalizeCacheTokens(cachedTokens, cacheReadTokens, cacheCreationTokens int64) cacheTokenBreakdown {
	cachedTokens = maxInt64(cachedTokens, 0)
	cacheReadTokens = maxInt64(cacheReadTokens, 0)
	cacheCreationTokens = maxInt64(cacheCreationTokens, 0)
	residualReadTokens := maxInt64(cachedTokens-cacheReadTokens-cacheCreationTokens, 0)
	readTokens := residualReadTokens + cacheReadTokens
	return cacheTokenBreakdown{
		Read:     readTokens,
		Creation: cacheCreationTokens,
		Total:    readTokens + cacheCreationTokens,
	}
}

func providerUsesSeparateCacheInput(provider, model string) bool {
	value := strings.ToLower(strings.TrimSpace(provider + " " + model))
	return strings.Contains(value, "claude") || strings.Contains(value, "anthropic")
}

// cacheTokensIncludedInInput distinguishes OpenAI-style usage, where cache
// details are subsets of input_tokens, from Anthropic-style usage, where cache
// read/write tokens are reported alongside non-cached input_tokens.
func cacheTokensIncludedInInput(provider, model string, totalTokens, inputTokens, outputTokens int64, cache cacheTokenBreakdown) bool {
	if providerUsesSeparateCacheInput(provider, model) {
		return false
	}
	if totalTokens > 0 && cache.Total > 0 {
		baseTokens := maxInt64(inputTokens, 0) + maxInt64(outputTokens, 0)
		if totalTokens-baseTokens >= cache.Total {
			return false
		}
	}
	return true
}

func fallbackUsageTotal(rec usageRecord) int64 {
	if rec.Detail.TotalTokens > 0 {
		return rec.Detail.TotalTokens
	}
	inputTokens := maxInt64(rec.Detail.InputTokens, 0)
	outputTokens := maxInt64(rec.Detail.OutputTokens, rec.Detail.ReasoningTokens)
	outputTokens = maxInt64(outputTokens, 0)
	cache := normalizeCacheTokens(rec.Detail.CachedTokens, rec.Detail.CacheReadTokens, rec.Detail.CacheCreationTokens)
	if cacheTokensIncludedInInput(rec.Provider, rec.Model, 0, inputTokens, outputTokens, cache) {
		inputTokens = maxInt64(inputTokens, cache.Total)
	} else {
		inputTokens += cache.Total
	}
	return inputTokens + outputTokens
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
)

type modelPrice struct {
	Prompt         float64
	Completion     float64
	Cache          float64
	CacheRead      float64
	CacheCreation  float64
	CacheSet       bool
	CacheReadSet   bool
	CacheCreateSet bool
}

type costTokenRow struct {
	Model               string
	Alias               string
	Provider            string
	ServiceTier         string
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

type costSummary struct {
	CostUSD        float64
	UnpricedTokens int64
}

func defaultModelPrices() map[string]modelPrice {
	prices := map[string]modelPrice{
		"gpt-5.5":                     {Prompt: 5, Completion: 30, Cache: 0.50, CacheSet: true},
		"openai/gpt-5.5":              {Prompt: 5, Completion: 30, Cache: 0.50, CacheSet: true},
		"gpt-5.4":                     {Prompt: 2.50, Completion: 15, Cache: 0.25, CacheSet: true},
		"openai/gpt-5.4":              {Prompt: 2.50, Completion: 15, Cache: 0.25, CacheSet: true},
		"gpt-5.4-mini":                {Prompt: 0.75, Completion: 4.50, Cache: 0.075, CacheSet: true},
		"openai/gpt-5.4-mini":         {Prompt: 0.75, Completion: 4.50, Cache: 0.075, CacheSet: true},
		"gpt-5.4-nano":                {Prompt: 0.20, Completion: 1.25, Cache: 0.02, CacheSet: true},
		"gpt-4o":                      {Prompt: 2.50, Completion: 10, Cache: 1.25, CacheSet: true},
		"gpt-4o-mini":                 {Prompt: 0.15, Completion: 0.60, Cache: 0.075, CacheSet: true},
		"claude-sonnet-4.5":           {Prompt: 3, Completion: 15, Cache: 0.30, CacheRead: 0.30, CacheCreation: 3.75, CacheSet: true, CacheReadSet: true, CacheCreateSet: true},
		"anthropic/claude-sonnet-4.5": {Prompt: 3, Completion: 15, Cache: 0.30, CacheRead: 0.30, CacheCreation: 3.75, CacheSet: true, CacheReadSet: true, CacheCreateSet: true},
		"claude-3-5-sonnet-20241022":  {Prompt: 3, Completion: 15, Cache: 0.30, CacheRead: 0.30, CacheCreation: 3.75, CacheSet: true, CacheReadSet: true, CacheCreateSet: true},
		"gemini/gemini-2.5-pro":       {Prompt: 1.25, Completion: 10, Cache: 0.125, CacheRead: 0.125, CacheSet: true, CacheReadSet: true},
		"gemini-2.5-pro":              {Prompt: 1.25, Completion: 10, Cache: 0.125, CacheRead: 0.125, CacheSet: true, CacheReadSet: true},
		"deepseek/deepseek-chat":      {Prompt: 0.28, Completion: 0.42, Cache: 0.028, CacheRead: 0.028, CacheCreation: 0, CacheSet: true, CacheReadSet: true, CacheCreateSet: true},
		"deepseek-chat":               {Prompt: 0.28, Completion: 0.42, Cache: 0.028, CacheRead: 0.028, CacheCreation: 0, CacheSet: true, CacheReadSet: true, CacheCreateSet: true},
		"gpt-4.1":                     {Prompt: 2, Completion: 8, CacheRead: 0.50, CacheReadSet: true},
		"gpt-4.1-mini":                {Prompt: 0.40, Completion: 1.60, CacheRead: 0.10, CacheReadSet: true},
		"gpt-4.1-nano":                {Prompt: 0.10, Completion: 0.40, CacheRead: 0.025, CacheReadSet: true},
		"deepseek-v4-pro":             {Prompt: 1.74, Completion: 3.48},
		"deepseek-v4-flash":           {Prompt: 0.19, Completion: 0.51},
		"kimi-k2.6":                   {Prompt: 0.95, Completion: 4, CacheRead: 0.16, CacheReadSet: true},
		"glm-5.1":                     {Prompt: 1.40, Completion: 4.40, CacheRead: 0.26, CacheReadSet: true},
		"glm-5.2":                     {Prompt: 1.40, Completion: 4.40, CacheRead: 0.26, CacheReadSet: true},
		"minimax-m2.5":                {Prompt: 0.36, Completion: 1.44},
	}
	for model, price := range readModelPricesFromFile() {
		prices[normalizeModelName(model)] = price
	}
	return prices
}

func readModelPricesFromFile() map[string]modelPrice {
	path := strings.TrimSpace(os.Getenv("CPA_MODEL_PRICE_FILE"))
	if path == "" {
		path = "/root/plugins/codex-token-usage/model_prices.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	prices := make(map[string]modelPrice, len(entries))
	for model, entry := range entries {
		price, ok := modelPriceFromJSON(entry)
		if !ok {
			continue
		}
		registerPriceCandidate(prices, model, price)
		if provider := stringFromAny(entry["litellm_provider"]); provider != "" {
			registerPriceCandidate(prices, provider+"/"+model, price)
		}
		if sourceModel := firstNonEmptyString(stringFromAny(entry["source_model_id"]), stringFromAny(entry["sourceModelId"])); sourceModel != "" {
			registerPriceCandidate(prices, sourceModel, price)
		}
	}
	return prices
}

func modelPriceFromJSON(entry map[string]any) (modelPrice, bool) {
	var price modelPrice
	price.Prompt = firstPositiveFloat(
		floatFromAny(entry["prompt"]),
		floatFromAny(entry["prompt_price_per_1m"]),
		floatFromAny(entry["prompt_price_per1_m"]),
		perTokenToPerMillion(floatFromAny(entry["input_cost_per_token"])),
	)
	price.Completion = firstPositiveFloat(
		floatFromAny(entry["completion"]),
		floatFromAny(entry["completion_price_per_1m"]),
		floatFromAny(entry["completion_price_per1_m"]),
		perTokenToPerMillion(floatFromAny(entry["output_cost_per_token"])),
	)
	if v, ok := firstPresentFloat(entry, "cache", "cache_price_per_1m", "cache_price_per1_m", "input_cost_per_token_cache_hit", "cache_read_input_token_cost"); ok {
		if strings.Contains(detectedPriceKey(entry, "cache", "cache_price_per_1m", "cache_price_per1_m", "input_cost_per_token_cache_hit", "cache_read_input_token_cost"), "_cost") {
			v = perTokenToPerMillion(v)
		}
		price.Cache = v
		price.CacheSet = true
	}
	if v, ok := firstPresentFloat(entry, "cacheRead", "cache_read", "cache_read_per_1m", "cache_read_input_token_cost"); ok {
		if strings.Contains(detectedPriceKey(entry, "cacheRead", "cache_read", "cache_read_per_1m", "cache_read_input_token_cost"), "_cost") {
			v = perTokenToPerMillion(v)
		}
		price.CacheRead = v
		price.CacheReadSet = true
	}
	if v, ok := firstPresentFloat(entry, "cacheCreation", "cache_creation", "cache_creation_per_1m", "cache_creation_price_per_1m", "cache_creation_price_per1_m", "cache_creation_input_token_cost"); ok {
		if strings.Contains(detectedPriceKey(entry, "cacheCreation", "cache_creation", "cache_creation_per_1m", "cache_creation_price_per_1m", "cache_creation_price_per1_m", "cache_creation_input_token_cost"), "_cost") {
			v = perTokenToPerMillion(v)
		}
		price.CacheCreation = v
		price.CacheCreateSet = true
	}
	if price.Prompt <= 0 && price.Completion <= 0 {
		return modelPrice{}, false
	}
	return price, true
}

func registerPriceCandidate(prices map[string]modelPrice, model string, price modelPrice) {
	key := normalizeModelName(model)
	key = strings.TrimPrefix(key, "openai/")
	key = strings.Trim(key, "/")
	if key == "" {
		return
	}
	prices[key] = price
	if strings.HasSuffix(key, " openai") {
		alias := strings.TrimSpace(strings.TrimSuffix(key, " openai"))
		if alias != "" {
			prices[alias] = price
		}
	}
}

func applyCosts(ctx context.Context, db *sql.DB, since int64, totals *totalsRow, prices map[string]modelPrice, scope string) error {
	rows, err := queryCostRows(ctx, db, `
SELECT model, alias, provider, service_tier,
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL(scope)+`
GROUP BY model, alias, provider, service_tier`, since)
	if err != nil {
		return err
	}
	sum := summarizeCost(rows, prices)
	totals.CostUSD = sum.CostUSD
	totals.UnpricedTokens = sum.UnpricedTokens
	totals.CostAvailable = costAvailable(totals.TotalTokens, sum.UnpricedTokens)
	return nil
}

func applyAccountCosts(ctx context.Context, db *sql.DB, since int64, accounts []accountRow, prices map[string]modelPrice) error {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(auth_index,''), NULLIF(auth_id,''), 'unknown') AS account_key,
model, alias, provider, service_tier,
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL("codex")+` AND (auth_index <> '' OR auth_id <> '' OR source <> '')
GROUP BY account_key, model, alias, provider, service_tier`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	costs := map[string]costSummary{}
	for rows.Next() {
		var key string
		var row costTokenRow
		if err := rows.Scan(
			&key, &row.Model, &row.Alias, &row.Provider, &row.ServiceTier,
			&row.InputTokens, &row.OutputTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
		); err != nil {
			return err
		}
		addCostSummary(costs, normalizeAccountAlias(key), row, prices)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range accounts {
		key := normalizeAccountAlias(accounts[i].AuthIndex)
		if key == "" {
			key = normalizeAccountAlias(firstNonEmptyString(accounts[i].AuthID, accounts[i].Source))
		}
		sum := costs[key]
		accounts[i].CostUSD = sum.CostUSD
		accounts[i].UnpricedTokens = sum.UnpricedTokens
		accounts[i].CostAvailable = costAvailable(accounts[i].TotalTokens, sum.UnpricedTokens)
	}
	return nil
}

func applyProviderCosts(ctx context.Context, db *sql.DB, since int64, providers []providerRow, prices map[string]modelPrice, scope string) error {
	rows, err := db.QueryContext(ctx, `
SELECT `+cpaProviderSQL()+` AS provider_key, model, alias, provider, service_tier,
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL(scope)+`
GROUP BY provider_key, model, alias, provider, service_tier`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	costs := map[string]costSummary{}
	for rows.Next() {
		var key string
		var row costTokenRow
		if err := rows.Scan(
			&key, &row.Model, &row.Alias, &row.Provider, &row.ServiceTier,
			&row.InputTokens, &row.OutputTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
		); err != nil {
			return err
		}
		addCostSummary(costs, normalizeAccountAlias(key), row, prices)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range providers {
		sum := costs[normalizeAccountAlias(providers[i].Provider)]
		providers[i].CostUSD = sum.CostUSD
		providers[i].UnpricedTokens = sum.UnpricedTokens
		providers[i].CostAvailable = costAvailable(providers[i].TotalTokens, sum.UnpricedTokens)
	}
	return nil
}

func applyModelCosts(ctx context.Context, db *sql.DB, since int64, models []modelRow, prices map[string]modelPrice, scope string) error {
	rows, err := db.QueryContext(ctx, `
SELECT model, alias, `+cpaProviderSQL()+` AS provider_key, service_tier,
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL(scope)+`
GROUP BY model, alias, provider_key, service_tier`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	costs := map[string]costSummary{}
	for rows.Next() {
		var row costTokenRow
		if err := rows.Scan(
			&row.Model, &row.Alias, &row.Provider, &row.ServiceTier,
			&row.InputTokens, &row.OutputTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
		); err != nil {
			return err
		}
		addCostSummary(costs, modelCostKey(row.Model, row.Alias, row.Provider), row, prices)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range models {
		sum := costs[modelCostKey(models[i].Model, models[i].Alias, models[i].Provider)]
		models[i].CostUSD = sum.CostUSD
		models[i].UnpricedTokens = sum.UnpricedTokens
		models[i].CostAvailable = costAvailable(models[i].TotalTokens, sum.UnpricedTokens)
	}
	return nil
}

func queryCostRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]costTokenRow, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []costTokenRow
	for rows.Next() {
		var row costTokenRow
		if err := rows.Scan(
			&row.Model, &row.Alias, &row.Provider, &row.ServiceTier,
			&row.InputTokens, &row.OutputTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func summarizeCost(rows []costTokenRow, prices map[string]modelPrice) costSummary {
	var sum costSummary
	for _, row := range rows {
		addCost(&sum, row, prices)
	}
	return sum
}

func addCostSummary(costs map[string]costSummary, key string, row costTokenRow, prices map[string]modelPrice) {
	sum := costs[key]
	addCost(&sum, row, prices)
	costs[key] = sum
}

func addCost(sum *costSummary, row costTokenRow, prices map[string]modelPrice) {
	cost, ok := costForTokens(row, prices)
	if !ok {
		if usageTokenInputRequiresPricing(row) {
			sum.UnpricedTokens += row.TotalTokens
		}
		return
	}
	sum.CostUSD += cost
}

func costForTokens(row costTokenRow, prices map[string]modelPrice) (float64, bool) {
	price, ok := resolveModelPrice(row, prices)
	if !ok {
		return 0, false
	}
	inputTokens := maxInt64(row.InputTokens, 0)
	outputTokens := maxInt64(row.OutputTokens, 0)
	cachedTokens := maxInt64(row.CachedTokens, 0)
	cacheReadTokens := maxInt64(row.CacheReadTokens, 0)
	cacheCreationTokens := maxInt64(row.CacheCreationTokens, 0)
	residualCachedTokens := cachedTokens
	if cacheReadTokens > 0 || cacheCreationTokens > 0 {
		residualCachedTokens = maxInt64(cachedTokens-cacheReadTokens-cacheCreationTokens, 0)
	}
	promptTokens := maxInt64(inputTokens-cachedTokens, 0)
	cachePrice := effectiveCachePrice(price)
	cacheReadPrice := effectiveCacheReadPrice(price)
	cacheCreationPrice := effectiveCacheCreationPrice(price)
	cost := float64(promptTokens)*price.Prompt/1_000_000.0 +
		float64(outputTokens)*price.Completion/1_000_000.0 +
		float64(residualCachedTokens)*cachePrice/1_000_000.0 +
		float64(cacheReadTokens)*cacheReadPrice/1_000_000.0 +
		float64(cacheCreationTokens)*cacheCreationPrice/1_000_000.0
	return cost * serviceTierMultiplier(row.Model, row.ServiceTier), true
}

func resolveModelPrice(row costTokenRow, prices map[string]modelPrice) (modelPrice, bool) {
	for _, name := range []string{row.Provider + "/" + row.Model, row.Provider + "/" + row.Alias, row.Model, row.Alias} {
		for _, candidate := range priceNameCandidates(name) {
			if price, ok := prices[candidate]; ok {
				return price, true
			}
		}
	}
	return modelPrice{}, false
}

func normalizeModelName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func priceNameCandidates(value string) []string {
	key := normalizeModelName(value)
	if key == "" {
		return nil
	}
	key = strings.TrimPrefix(key, "openai/")
	seen := map[string]bool{}
	var out []string
	add := func(candidate string) {
		candidate = normalizeModelName(candidate)
		candidate = strings.Trim(candidate, "/")
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	add(key)
	if strings.HasSuffix(key, " openai") {
		add(strings.TrimSpace(strings.TrimSuffix(key, " openai")))
	}
	parts := strings.Split(key, "/")
	for i := 1; i < len(parts); i++ {
		add(strings.Join(parts[i:], "/"))
	}
	if len(parts) > 0 {
		add(parts[len(parts)-1])
	}
	for _, candidate := range append([]string(nil), out...) {
		for _, stripped := range stripDateSuffixCandidates(candidate) {
			add(stripped)
		}
	}
	if i := strings.LastIndex(key, ":"); i > 0 {
		add(key[:i])
	}
	return out
}

func stripDateSuffixCandidates(value string) []string {
	var out []string
	current := value
	for {
		i := strings.LastIndex(current, "-")
		if i <= 0 || i == len(current)-1 {
			return out
		}
		suffix := current[i+1:]
		if len(suffix) < 4 || !allDigits(suffix) {
			return out
		}
		current = current[:i]
		out = append(out, current)
	}
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func modelCostKey(model, alias, provider string) string {
	return normalizeModelName(provider) + "\x00" + normalizeModelName(model) + "\x00" + normalizeModelName(alias)
}

func usageTokenInputRequiresPricing(row costTokenRow) bool {
	return row.InputTokens > 0 || row.OutputTokens > 0 || row.CachedTokens > 0 || row.CacheReadTokens > 0 || row.CacheCreationTokens > 0
}

func costAvailable(totalTokens, unpricedTokens int64) bool {
	return totalTokens > 0 && unpricedTokens == 0
}

func fallbackPrice(value float64, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func effectiveCachePrice(price modelPrice) float64 {
	if price.CacheSet {
		return price.Cache
	}
	if price.CacheReadSet {
		return price.CacheRead
	}
	return price.Prompt
}

func effectiveCacheReadPrice(price modelPrice) float64 {
	if price.CacheReadSet {
		return price.CacheRead
	}
	return effectiveCachePrice(price)
}

func effectiveCacheCreationPrice(price modelPrice) float64 {
	if price.CacheCreateSet {
		return price.CacheCreation
	}
	return price.Prompt
}

func recentPriceDetail(price modelPrice) string {
	base := "$" + formatPricePerMillion(price.Prompt) + " / $" + formatPricePerMillion(price.Completion) + "/M"
	cache := effectiveCacheReadPrice(price)
	if cache > 0 && cache != price.Prompt {
		base += " cache $" + formatPricePerMillion(cache)
	}
	if price.CacheCreateSet && price.CacheCreation > 0 && price.CacheCreation != cache {
		base += " write $" + formatPricePerMillion(price.CacheCreation)
	}
	return base
}

func formatPricePerMillion(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	formatted := strconv.FormatFloat(value, 'f', 4, 64)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimRight(formatted, ".")
	return formatted
}

func floatFromAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	default:
		return 0
	}
}

func firstPositiveFloat(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func perTokenToPerMillion(value float64) float64 {
	if value <= 0 {
		return 0
	}
	return value * 1_000_000.0
}

func firstPresentFloat(entry map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := entry[key]; ok {
			return floatFromAny(value), true
		}
	}
	return 0, false
}

func detectedPriceKey(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		if _, ok := entry[key]; ok {
			return key
		}
	}
	return ""
}

func serviceTierMultiplier(modelName string, serviceTier string) float64 {
	tier := strings.ToLower(strings.TrimSpace(serviceTier))
	if tier != "priority" && tier != "fast" {
		return 1
	}
	modelName = normalizeModelName(modelName)
	switch {
	case isModelFamily(modelName, "gpt-5.5"):
		return 2.5
	case isModelFamily(modelName, "gpt-5.4-mini"):
		return 2
	case isModelFamily(modelName, "gpt-5.4"):
		return 2
	case isModelFamily(modelName, "gpt-5.3-codex"):
		return 2
	default:
		return 1
	}
}

func isModelFamily(modelName string, family string) bool {
	return modelName == family || strings.HasPrefix(modelName, family+"-")
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

func durationToMilliseconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value > 10000 {
		return (value + 999999) / 1000000
	}
	return value
}

func queryTrend(ctx context.Context, db *sql.DB, since int64, window string, scope string) ([]trendPoint, error) {
	format := "%Y-%m-%d %H:00"
	if window == "7d" || window == "30d" || window == "all" {
		format = "%Y-%m-%d"
	}
	rows, err := db.QueryContext(ctx, `
SELECT strftime(?, requested_at, 'unixepoch', 'localtime') AS bucket,
COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(total_tokens),0), COALESCE(SUM(output_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL(scope)+`
GROUP BY bucket
ORDER BY bucket ASC
LIMIT 240`, format, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trendPoint
	for rows.Next() {
		var r trendPoint
		if err := rows.Scan(&r.Bucket, &r.Requests, &r.Failed, &r.RateLimited, &r.TotalTokens, &r.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryRecent(ctx context.Context, db *sql.DB, since int64, limit int, scope string, prices map[string]modelPrice) ([]recentRow, error) {
	query := `
SELECT requested_at, ` + cpaProviderSQL() + ` AS provider_key, auth_index, source, model, alias, reasoning_effort, service_tier,
latency_ms, ttft_ms, status_code, failed, total_tokens, input_tokens, output_tokens, reasoning_tokens,
cached_tokens, cache_read_tokens, cache_creation_tokens
FROM usage_events
WHERE requested_at >= ? AND ` + usageScopeSQL(scope) + `
ORDER BY requested_at DESC, id DESC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recentRow
	for rows.Next() {
		var r recentRow
		var ts int64
		var failed int
		if err := rows.Scan(
			&ts, &r.Provider, &r.AuthIndex, &r.Source, &r.Model, &r.Alias, &r.ReasoningEffort, &r.ServiceTier,
			&r.LatencyMs, &r.TTFTMs, &r.StatusCode, &failed, &r.TotalTokens, &r.InputTokens, &r.OutputTokens,
			&r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		); err != nil {
			return nil, err
		}
		r.Time = unixTime(ts)
		r.Failed = failed != 0
		costRow := costTokenRow{
			Model:               r.Model,
			Alias:               r.Alias,
			Provider:            r.Provider,
			ServiceTier:         r.ServiceTier,
			InputTokens:         r.InputTokens,
			OutputTokens:        r.OutputTokens,
			CachedTokens:        r.CachedTokens,
			CacheReadTokens:     r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			TotalTokens:         r.TotalTokens,
		}
		if cost, ok := costForTokens(costRow, prices); ok {
			r.CostUSD = cost
			r.CostAvailable = true
			if price, ok := resolveModelPrice(costRow, prices); ok {
				r.PriceDetail = recentPriceDetail(price)
			}
		} else if usageTokenInputRequiresPricing(costRow) {
			r.UnpricedTokens = r.TotalTokens
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryActiveAutobans(ctx context.Context, db *sql.DB, now int64) ([]autobanRow, error) {
	if err := normalizeStoredResetColumns(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active, last_status_code,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
FROM autoban_bans
WHERE active=1 AND reset_at > ?
ORDER BY reset_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []autobanRow
	for rows.Next() {
		var r autobanRow
		var active int
		var pp, sp sql.NullFloat64
		var pr, sr sql.NullInt64
		if err := rows.Scan(
			&r.AuthID, &r.AuthIndex, &r.Source, &r.Provider, &r.Window, &r.Reason,
			&r.BannedAt, &r.ResetAt, &active, &r.LastStatusCode, &pp, &pr, &sp, &sr,
		); err != nil {
			return nil, err
		}
		r.Active = active != 0
		r.BannedAtText = unixTime(r.BannedAt)
		r.ResetAtText = unixTime(r.ResetAt)
		if r.ResetAt > now {
			r.SecondsRemaining = r.ResetAt - now
		}
		if pp.Valid {
			r.PrimaryUsedPercent = &pp.Float64
		}
		if pr.Valid {
			r.PrimaryResetAt = &pr.Int64
		}
		if sp.Valid {
			r.SecondaryUsedPercent = &sp.Float64
		}
		if sr.Valid {
			r.SecondaryResetAt = &sr.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func headerFloat(headers map[string][]string, key string) *float64 {
	value := headerValue(headers, key)
	if value == "" {
		return nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &f
}

func headerInt(headers map[string][]string, key string) *int64 {
	value := headerValue(headers, key)
	if value == "" {
		return nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func headerIntValue(headers map[string][]string, key string) int64 {
	value := headerInt(headers, key)
	if value == nil {
		return 0
	}
	return *value
}

func headerValue(headers map[string][]string, key string) string {
	if headers == nil {
		return ""
	}
	for k, values := range headers {
		if strings.EqualFold(k, key) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func trim(v string) string {
	return strings.TrimSpace(v)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.Trim(strings.TrimSpace(string(raw)), `"`)
	}
}

func boolFromAny(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "y", "on":
			return true
		default:
			return false
		}
	case float64:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	default:
		return false
	}
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func nullIntPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func unixTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}

func okJSON(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

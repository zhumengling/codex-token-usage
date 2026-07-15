package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
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
	return scanRecentRows(rows, prices)
}

func queryProviderRecent(ctx context.Context, db *sql.DB, since int64, perProvider, limit int, prices map[string]modelPrice) ([]recentRow, error) {
	if perProvider <= 0 {
		perProvider = 30
	}
	if perProvider > 50 {
		perProvider = 50
	}
	if limit <= 0 {
		limit = 300
	}
	if limit > 500 {
		limit = 500
	}
	providerExpr := cpaProviderSQL()
	query := `
SELECT requested_at, provider_key, auth_index, source, model, alias, reasoning_effort, service_tier,
latency_ms, ttft_ms, status_code, failed, total_tokens, input_tokens, output_tokens, reasoning_tokens,
cached_tokens, cache_read_tokens, cache_creation_tokens
FROM (
  SELECT provider_events.*,
  row_number() OVER (PARTITION BY provider_key ORDER BY requested_at DESC, id DESC) AS provider_rank
  FROM (
    SELECT id, requested_at, ` + providerExpr + ` AS provider_key, auth_index, source, model, alias, reasoning_effort, service_tier,
    latency_ms, ttft_ms, status_code, failed, total_tokens, input_tokens, output_tokens, reasoning_tokens,
    cached_tokens, cache_read_tokens, cache_creation_tokens
    FROM usage_events
    WHERE requested_at >= ? AND ` + usageScopeSQL("other") + `
  ) provider_events
)
WHERE provider_rank <= ?
ORDER BY requested_at DESC, id DESC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, since, perProvider, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecentRows(rows, prices)
}

func scanRecentRows(rows *sql.Rows, prices map[string]modelPrice) ([]recentRow, error) {
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
		r.Source = safeExportLabel(r.Source)
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
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active, last_status_code,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at, auth_file, auth_file_mtime
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
			&r.BannedAt, &r.ResetAt, &active, &r.LastStatusCode, &pp, &pr, &sp, &sr, &r.AuthFile, &r.AuthFileMTime,
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

func mergeEffectiveAutobans(bans []autobanRow, invalids []invalidAuthRow) []autobanRow {
	if len(invalids) == 0 {
		return dedupeAutobanRows(bans)
	}
	out := make([]autobanRow, 0, len(bans)+len(invalids))
	out = append(out, bans...)
	for _, invalid := range invalids {
		if !invalid.Active {
			continue
		}
		out = append(out, invalidAuthAsAutoban(invalid))
	}
	return dedupeAutobanRows(out)
}

func invalidAuthAsAutoban(invalid invalidAuthRow) autobanRow {
	status := invalid.LastStatusCode
	if status == 0 {
		status = http.StatusUnauthorized
	}
	reason := strings.TrimSpace(invalid.Reason)
	if reason == "" {
		reason = "401 unauthorized: credential is invalid"
	}
	window := "401"
	resetText := "重新登录后解除"
	if invalidAuthIsWorkspaceDeactivated(invalid) {
		window = "402"
		resetText = "删除或替换认证文件后解除"
		if reason == "" {
			reason = "402 deactivated_workspace: team workspace is deactivated"
		}
	} else if status == http.StatusForbidden {
		window = "403"
		resetText = "删除或替换认证文件后解除"
		if reason == "" {
			reason = "403 forbidden: repeated failures for this credential/workspace"
		}
	}
	return autobanRow{
		AuthID:           invalid.AuthID,
		AuthIndex:        invalid.AuthIndex,
		Source:           invalid.Source,
		Provider:         firstNonEmptyString(invalid.Provider, "codex"),
		AuthFile:         invalid.AuthFile,
		Window:           window,
		Reason:           reason,
		BannedAt:         invalid.InvalidatedAt,
		BannedAtText:     invalid.InvalidatedAtText,
		ResetAtText:      resetText,
		SecondsRemaining: -1,
		Active:           true,
		LastStatusCode:   status,
	}
}

func dedupeAutobanRows(rows []autobanRow) []autobanRow {
	if len(rows) < 2 {
		return rows
	}
	out := make([]autobanRow, 0, len(rows))
	seen := make(map[string]int, len(rows))
	for _, row := range rows {
		key := autobanDedupeKey(row)
		if key == "" {
			out = append(out, row)
			continue
		}
		if idx, ok := seen[key]; ok {
			out[idx] = mergeDuplicateAutobanRow(out[idx], row)
			continue
		}
		seen[key] = len(out)
		out = append(out, row)
	}
	return out
}

func autobanDedupeKey(row autobanRow) string {
	for _, value := range []string{row.AuthFile, row.AuthID, row.AuthIndex, row.Source} {
		if file := fileNameIfJSON(value); file != "" {
			return "file:" + normalizeAccountAlias(file)
		}
	}
	aliases := authStateMatchAliases(row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
	if len(aliases) == 0 {
		return ""
	}
	return "alias:" + aliases[0]
}

func mergeDuplicateAutobanRow(left, right autobanRow) autobanRow {
	merged, other := left, right
	if preferAutobanIdentity(right, left) {
		merged, other = right, left
	}
	merged.Active = merged.Active || other.Active
	merged.Provider = firstNonEmptyString(merged.Provider, other.Provider)
	merged.Source = firstNonEmptyString(merged.Source, other.Source)
	merged.AuthFile = firstNonEmptyString(merged.AuthFile, other.AuthFile)
	if merged.AuthFileMTime == 0 {
		merged.AuthFileMTime = other.AuthFileMTime
	}
	if merged.LastStatusCode == 0 {
		merged.LastStatusCode = other.LastStatusCode
	}
	if merged.BannedAt == 0 || (other.BannedAt > 0 && other.BannedAt < merged.BannedAt) {
		merged.BannedAt = other.BannedAt
		merged.BannedAtText = other.BannedAtText
	}
	if !autobanIsPermanentAuthState(merged) && other.ResetAt > merged.ResetAt {
		merged.ResetAt = other.ResetAt
		merged.ResetAtText = other.ResetAtText
		merged.SecondsRemaining = other.SecondsRemaining
		merged.Window = firstNonEmptyString(other.Window, merged.Window)
		merged.Reason = firstNonEmptyString(other.Reason, merged.Reason)
	}
	if merged.PrimaryUsedPercent == nil {
		merged.PrimaryUsedPercent = other.PrimaryUsedPercent
	}
	if merged.PrimaryResetAt == nil {
		merged.PrimaryResetAt = other.PrimaryResetAt
	}
	if merged.SecondaryUsedPercent == nil {
		merged.SecondaryUsedPercent = other.SecondaryUsedPercent
	}
	if merged.SecondaryResetAt == nil {
		merged.SecondaryResetAt = other.SecondaryResetAt
	}
	return merged
}

func preferAutobanIdentity(candidate, current autobanRow) bool {
	if cp, rp := autobanStatePriority(candidate), autobanStatePriority(current); cp != rp {
		return cp > rp
	}
	if cs, rs := autobanIdentityScore(candidate), autobanIdentityScore(current); cs != rs {
		return cs > rs
	}
	return candidate.ResetAt > current.ResetAt
}

func autobanStatePriority(row autobanRow) int {
	if autobanIsPermanentAuthState(row) {
		return 2
	}
	return 1
}

func autobanIdentityScore(row autobanRow) int {
	if fileNameIfJSON(row.AuthFile) != "" || fileNameIfJSON(row.AuthID) != "" {
		return 3
	}
	if fileNameIfJSON(row.AuthIndex) != "" || fileNameIfJSON(row.Source) != "" {
		return 2
	}
	if strings.TrimSpace(row.AuthID) != "" || strings.TrimSpace(row.AuthIndex) != "" {
		return 1
	}
	return 0
}

func autobanIsPermanentAuthState(row autobanRow) bool {
	window := strings.ToLower(strings.TrimSpace(row.Window))
	return window == "401" || window == "402" || window == "403" ||
		row.LastStatusCode == http.StatusUnauthorized ||
		row.LastStatusCode == http.StatusPaymentRequired ||
		row.LastStatusCode == http.StatusForbidden
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
	return errorEnvelopeWithStatus(code, message, 0)
}

func errorEnvelopeWithStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

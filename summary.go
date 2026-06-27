package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *store) summary(ctx context.Context, window string, limit int) (map[string]any, error) {
	db, path, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	since, label := windowStart(window)
	totals, err := queryOneTotals(ctx, db, since, "codex")
	if err != nil {
		return nil, err
	}
	prices := defaultModelPrices()
	if err := applyCosts(ctx, db, since, &totals, prices, "codex"); err != nil {
		return nil, err
	}
	providerTotals, err := queryOneTotals(ctx, db, since, "other")
	if err != nil {
		return nil, err
	}
	if err := applyCosts(ctx, db, since, &providerTotals, prices, "other"); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if err := backfillAutobansFromUsage(ctx, db, now); err != nil {
		return nil, err
	}
	if err := expireAutobans(ctx, db, now); err != nil {
		return nil, err
	}
	if err := reconcileAutobansWithQuotaSnapshots(ctx, db, now); err != nil {
		return nil, err
	}
	if err := clearReplacedInvalidAuths(ctx, db); err != nil {
		return nil, err
	}
	accounts, err := queryAccounts(ctx, db, since, limit)
	if err != nil {
		return nil, err
	}
	if err := applyAccountCosts(ctx, db, since, accounts, prices); err != nil {
		return nil, err
	}
	configuredAccounts := readConfiguredAuthAccounts()
	accounts = mergeConfiguredAccounts(accounts, configuredAccounts)
	quotaSince := time.Now().Add(-35 * 24 * time.Hour).Unix()
	applyLatestQuotaSnapshots(ctx, db, accounts, quotaSince)
	applySecondaryQuotaEstimates(ctx, db, accounts, &totals, quotaSince)
	invalidAuths, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return nil, err
	}
	applyInvalidAuths(accounts, invalidAuths)
	externalUseAlerts, err := queryExternalUseAlerts(ctx, db, since)
	if err != nil {
		return nil, err
	}
	applyExternalUseAlerts(accounts, externalUseAlerts)
	triggerRuns, err := queryRecentQuotaTriggerRuns(ctx, db, 50)
	if err != nil {
		return nil, err
	}
	applyQuotaTriggerStatuses(accounts, triggerRuns)
	providers, err := queryProviders(ctx, db, since, limit, "other")
	if err != nil {
		return nil, err
	}
	if err := applyProviderCosts(ctx, db, since, providers, prices, "other"); err != nil {
		return nil, err
	}
	providers = mergeConfiguredProviders(providers, readConfiguredProviderNames())
	models, err := queryModels(ctx, db, since, limit, "codex")
	if err != nil {
		return nil, err
	}
	if err := applyModelCosts(ctx, db, since, models, prices, "codex"); err != nil {
		return nil, err
	}
	providerModels, err := queryModels(ctx, db, since, limit, "other")
	if err != nil {
		return nil, err
	}
	if err := applyModelCosts(ctx, db, since, providerModels, prices, "other"); err != nil {
		return nil, err
	}
	trend, err := queryTrend(ctx, db, since, label, "codex")
	if err != nil {
		return nil, err
	}
	providerTrend, err := queryTrend(ctx, db, since, label, "other")
	if err != nil {
		return nil, err
	}
	recent, err := queryRecent(ctx, db, since, 30, "codex", prices)
	if err != nil {
		return nil, err
	}
	providerRecent, err := queryRecent(ctx, db, since, 30, "other", prices)
	if err != nil {
		return nil, err
	}
	autobans, err := queryActiveAutobans(ctx, db, now)
	if err != nil {
		return nil, err
	}
	applyAccountQuotaToAutobans(autobans, accounts)
	return map[string]any{
		"plugin":              pluginID,
		"version":             pluginVersion,
		"generated_at":        time.Now().Format(time.RFC3339),
		"window":              label,
		"since_unix":          since,
		"db_path":             path,
		"totals":              totals,
		"provider_totals":     providerTotals,
		"accounts":            accounts,
		"providers":           providers,
		"models":              models,
		"provider_models":     providerModels,
		"trend":               trend,
		"provider_trend":      providerTrend,
		"recent":              recent,
		"provider_recent":     providerRecent,
		"autobans":            autobans,
		"invalid_auths":       invalidAuths,
		"external_use_alerts": externalUseAlerts,
		"quota_trigger":       globalQuotaTrigger.status(),
		"quota_trigger_runs":  triggerRuns,
	}, nil
}

func windowStart(window string) (int64, string) {
	now := time.Now()
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix(), "today"
	case "7d":
		return now.Add(-7 * 24 * time.Hour).Unix(), "7d"
	case "30d":
		return now.Add(-30 * 24 * time.Hour).Unix(), "30d"
	case "all":
		return 0, "all"
	default:
		return now.Add(-24 * time.Hour).Unix(), "24h"
	}
}

type totalsRow struct {
	Requests                        int64   `json:"requests"`
	Failed                          int64   `json:"failed"`
	RateLimited                     int64   `json:"rate_limited"`
	InputTokens                     int64   `json:"input_tokens"`
	OutputTokens                    int64   `json:"output_tokens"`
	ReasoningTokens                 int64   `json:"reasoning_tokens"`
	CachedTokens                    int64   `json:"cached_tokens"`
	CacheReadTokens                 int64   `json:"cache_read_tokens"`
	CacheCreationTokens             int64   `json:"cache_creation_tokens"`
	TotalTokens                     int64   `json:"total_tokens"`
	CostUSD                         float64 `json:"cost_usd"`
	CostAvailable                   bool    `json:"cost_available"`
	UnpricedTokens                  int64   `json:"unpriced_tokens,omitempty"`
	SecondaryQuotaTotalEstimate     int64   `json:"secondary_quota_total_estimate"`
	SecondaryQuotaRemainingEstimate int64   `json:"secondary_quota_remaining_estimate"`
	SecondaryQuotaEstimatedAccounts int64   `json:"secondary_quota_estimated_accounts"`
}

type accountRow struct {
	AuthIndex                       string   `json:"auth_index"`
	AuthID                          string   `json:"auth_id"`
	Source                          string   `json:"source"`
	Provider                        string   `json:"provider"`
	Email                           string   `json:"email,omitempty"`
	Name                            string   `json:"name,omitempty"`
	AuthFile                        string   `json:"auth_file,omitempty"`
	Configured                      bool     `json:"configured"`
	Disabled                        bool     `json:"disabled,omitempty"`
	Expired                         bool     `json:"expired,omitempty"`
	InvalidAuth                     bool     `json:"invalid_auth,omitempty"`
	InvalidAuthAt                   string   `json:"invalid_auth_at,omitempty"`
	InvalidAuthReason               string   `json:"invalid_auth_reason,omitempty"`
	PlanType                        string   `json:"plan_type,omitempty"`
	Requests                        int64    `json:"requests"`
	Failed                          int64    `json:"failed"`
	RateLimited                     int64    `json:"rate_limited"`
	InputTokens                     int64    `json:"input_tokens"`
	OutputTokens                    int64    `json:"output_tokens"`
	ReasoningTokens                 int64    `json:"reasoning_tokens"`
	CachedTokens                    int64    `json:"cached_tokens"`
	CacheReadTokens                 int64    `json:"cache_read_tokens"`
	CacheCreationTokens             int64    `json:"cache_creation_tokens"`
	TotalTokens                     int64    `json:"total_tokens"`
	CostUSD                         float64  `json:"cost_usd"`
	CostAvailable                   bool     `json:"cost_available"`
	UnpricedTokens                  int64    `json:"unpriced_tokens,omitempty"`
	LastSeen                        string   `json:"last_seen"`
	PrimaryUsedPercent              *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAt                  *int64   `json:"primary_reset_at,omitempty"`
	PrimaryWindowTokens             int64    `json:"primary_window_tokens"`
	SecondaryUsedPercent            *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAt                *int64   `json:"secondary_reset_at,omitempty"`
	SecondaryWindowTokens           int64    `json:"secondary_window_tokens"`
	SecondaryQuotaWindow            string   `json:"secondary_quota_window,omitempty"`
	SecondaryQuotaTotalEstimate     int64    `json:"secondary_quota_total_estimate"`
	SecondaryQuotaRemainingEstimate int64    `json:"secondary_quota_remaining_estimate"`
	ExternalUseSuspected            bool     `json:"external_use_suspected,omitempty"`
	ExternalUseCount                int      `json:"external_use_count,omitempty"`
	ExternalUseWindow               string   `json:"external_use_window,omitempty"`
	ExternalUseDeltaPct             float64  `json:"external_use_delta_percent,omitempty"`
	ExternalUseLocalTokens          int64    `json:"external_use_local_tokens,omitempty"`
	ExternalUseDetectedAt           string   `json:"external_use_detected_at,omitempty"`
	ExternalUseReason               string   `json:"external_use_reason,omitempty"`
	QuotaTriggerLastAt              string   `json:"quota_trigger_last_at,omitempty"`
	QuotaTriggerStatus              string   `json:"quota_trigger_status,omitempty"`
	QuotaTriggerMode                string   `json:"quota_trigger_mode,omitempty"`
	QuotaTriggerHTTPStatus          int      `json:"quota_trigger_http_status,omitempty"`
	QuotaTriggerError               string   `json:"quota_trigger_error,omitempty"`
}

type configuredAccount struct {
	AuthIndex     string
	AuthID        string
	Source        string
	Provider      string
	Email         string
	Name          string
	AuthFile      string
	AuthFileMTime int64
	Disabled      bool
	Expired       bool
	PlanType      string
}

type triggerAuthAccount struct {
	configuredAccount
	AccessToken      string
	ChatGPTAccountID string
}

type quotaTriggerRun struct {
	AuthID               string
	AuthIndex            string
	Source               string
	Provider             string
	AuthFile             string
	Mode                 string
	Status               string
	HTTPStatus           int
	Error                string
	StartedAt            int64
	FinishedAt           int64
	PrimaryUsedPercent   *float64
	PrimaryResetAt       *int64
	SecondaryUsedPercent *float64
	SecondaryResetAt     *int64
	PrimaryUsedTokens    *int64
	PrimaryRemaining     *int64
	PrimaryLimit         *int64
	SecondaryUsedTokens  *int64
	SecondaryRemaining   *int64
	SecondaryLimit       *int64
}

type quotaTriggerAccountStatus struct {
	AuthID     string
	AuthIndex  string
	Source     string
	AuthFile   string
	Mode       string
	Status     string
	HTTPStatus int
	Error      string
	FinishedAt int64
}

type providerRow struct {
	Provider            string  `json:"provider"`
	Requests            int64   `json:"requests"`
	Failed              int64   `json:"failed"`
	RateLimited         int64   `json:"rate_limited"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	CostAvailable       bool    `json:"cost_available"`
	UnpricedTokens      int64   `json:"unpriced_tokens,omitempty"`
	Accounts            int64   `json:"accounts"`
	Models              int64   `json:"models"`
	LastSeen            string  `json:"last_seen"`
}

type modelRow struct {
	Model               string  `json:"model"`
	Alias               string  `json:"alias"`
	Provider            string  `json:"provider"`
	Requests            int64   `json:"requests"`
	TotalTokens         int64   `json:"total_tokens"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	CostAvailable       bool    `json:"cost_available"`
	UnpricedTokens      int64   `json:"unpriced_tokens,omitempty"`
}

type trendPoint struct {
	Bucket       string `json:"bucket"`
	Requests     int64  `json:"requests"`
	Failed       int64  `json:"failed"`
	RateLimited  int64  `json:"rate_limited"`
	TotalTokens  int64  `json:"total_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type recentRow struct {
	Time                string  `json:"time"`
	Provider            string  `json:"provider"`
	AuthIndex           string  `json:"auth_index"`
	Source              string  `json:"source"`
	Model               string  `json:"model"`
	Alias               string  `json:"alias"`
	ReasoningEffort     string  `json:"reasoning_effort"`
	ServiceTier         string  `json:"service_tier"`
	LatencyMs           int64   `json:"latency_ms"`
	TTFTMs              int64   `json:"ttft_ms"`
	StatusCode          int     `json:"status_code"`
	Failed              bool    `json:"failed"`
	TotalTokens         int64   `json:"total_tokens"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	CostAvailable       bool    `json:"cost_available"`
	UnpricedTokens      int64   `json:"unpriced_tokens,omitempty"`
	PriceDetail         string  `json:"price_detail,omitempty"`
}

type autobanRow struct {
	AuthID               string   `json:"auth_id"`
	AuthIndex            string   `json:"auth_index"`
	Source               string   `json:"source"`
	Provider             string   `json:"provider"`
	Window               string   `json:"window"`
	Reason               string   `json:"reason"`
	BannedAt             int64    `json:"banned_at"`
	BannedAtText         string   `json:"banned_at_text"`
	ResetAt              int64    `json:"reset_at"`
	ResetAtText          string   `json:"reset_at_text"`
	SecondsRemaining     int64    `json:"seconds_remaining"`
	Active               bool     `json:"active"`
	LastStatusCode       int      `json:"last_status_code"`
	PrimaryUsedPercent   *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAt       *int64   `json:"primary_reset_at,omitempty"`
	SecondaryUsedPercent *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAt     *int64   `json:"secondary_reset_at,omitempty"`
}

type invalidAuthRow struct {
	AuthID            string `json:"auth_id"`
	AuthIndex         string `json:"auth_index"`
	Source            string `json:"source"`
	Provider          string `json:"provider"`
	Reason            string `json:"reason"`
	InvalidatedAt     int64  `json:"invalidated_at"`
	InvalidatedAtText string `json:"invalidated_at_text"`
	Active            bool   `json:"active"`
	LastStatusCode    int    `json:"last_status_code"`
	AuthFile          string `json:"auth_file,omitempty"`
	AuthFileMTime     int64  `json:"auth_file_mtime,omitempty"`
}

type externalUseAlert struct {
	AuthIndex       string  `json:"auth_index"`
	AuthID          string  `json:"auth_id"`
	Source          string  `json:"source"`
	Provider        string  `json:"provider"`
	Window          string  `json:"window"`
	DetectedAt      int64   `json:"detected_at"`
	DetectedAtText  string  `json:"detected_at_text"`
	PreviousPercent float64 `json:"previous_percent"`
	CurrentPercent  float64 `json:"current_percent"`
	DeltaPercent    float64 `json:"delta_percent"`
	LocalTokens     int64   `json:"local_tokens"`
	IdleSeconds     int64   `json:"idle_seconds"`
	Reason          string  `json:"reason"`
}

func usageScopeSQL(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "codex":
		return "(LOWER(COALESCE(NULLIF(provider,''), '')) = 'codex' OR LOWER(COALESCE(NULLIF(executor_type,''), '')) LIKE '%codex%')"
	case "other":
		return "(LOWER(COALESCE(NULLIF(provider,''), '')) <> 'codex' AND LOWER(COALESCE(NULLIF(executor_type,''), '')) NOT LIKE '%codex%')"
	default:
		return "1=1"
	}
}

func queryOneTotals(ctx context.Context, db *sql.DB, since int64, scope string) (totalsRow, error) {
	var row totalsRow
	query := `
SELECT COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0)
FROM usage_events WHERE requested_at >= ? AND ` + usageScopeSQL(scope)
	err := db.QueryRowContext(ctx, query, since).Scan(
		&row.Requests, &row.Failed, &row.RateLimited, &row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
		&row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
	)
	return row, err
}

func queryAccounts(ctx context.Context, db *sql.DB, since int64, limit int) ([]accountRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(auth_index,''), NULLIF(auth_id,''), 'unknown') AS account_key,
MAX(auth_id), MAX(source), MAX(provider),
COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0), MAX(requested_at)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL("codex")+` AND (auth_index <> '' OR auth_id <> '' OR source <> '')
GROUP BY account_key
ORDER BY SUM(total_tokens) DESC
LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountRow
	for rows.Next() {
		var r accountRow
		var last int64
		if err := rows.Scan(&r.AuthIndex, &r.AuthID, &r.Source, &r.Provider, &r.Requests, &r.Failed, &r.RateLimited,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens,
			&r.CacheCreationTokens, &r.TotalTokens, &last); err != nil {
			return nil, err
		}
		r.LastSeen = unixTime(last)
		pp, pr := queryLatestAccountWindowQuota(ctx, db, r, since, "primary")
		sp, sr := queryLatestAccountWindowQuota(ctx, db, r, since, "secondary")
		applyAccountQuotaSnapshot(&r, pp, pr, sp, sr)
		r.PrimaryWindowTokens = queryAccountWindowTokens(ctx, db, r, pr, 5*time.Hour)
		r.SecondaryWindowTokens = queryAccountWindowTokens(ctx, db, r, sr, 7*24*time.Hour)
		out = append(out, r)
	}
	return out, rows.Err()
}

func readConfiguredAuthAccounts() []configuredAccount {
	authDir := configuredAuthDir()
	if authDir == "" {
		return nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil
	}
	out := make([]configuredAccount, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		authType := firstNonEmptyString(stringFromAny(doc["type"]), stringFromAny(doc["provider"]))
		if authType != "" && !strings.EqualFold(authType, "codex") {
			continue
		}
		email := firstNonEmptyString(stringFromAny(doc["email"]), stringFromAny(doc["account"]), stringFromAny(doc["username"]))
		name := stringFromAny(doc["name"])
		authFile := entry.Name()
		source := firstNonEmptyString(email, name, authFile)
		out = append(out, configuredAccount{
			AuthIndex:     authFile,
			AuthID:        email,
			Source:        source,
			Provider:      firstNonEmptyString(authType, "codex"),
			Email:         email,
			Name:          name,
			AuthFile:      authFile,
			AuthFileMTime: info.ModTime().Unix(),
			Disabled:      boolFromAny(doc["disabled"]),
			Expired:       boolFromAny(doc["expired"]),
			PlanType:      firstNonEmptyString(stringFromAny(doc["plan_type"]), stringFromAny(doc["plan"])),
		})
	}
	return out
}

func readTriggerAuthAccounts() []triggerAuthAccount {
	authDir := configuredAuthDir()
	if authDir == "" {
		return nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil
	}
	out := make([]triggerAuthAccount, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		authType := firstNonEmptyString(stringFromAny(doc["type"]), stringFromAny(doc["provider"]))
		if authType != "" && !strings.EqualFold(authType, "codex") {
			continue
		}
		email := firstNonEmptyString(stringFromAny(doc["email"]), stringFromAny(doc["account"]), stringFromAny(doc["username"]))
		name := stringFromAny(doc["name"])
		authFile := entry.Name()
		source := firstNonEmptyString(email, name, authFile)
		out = append(out, triggerAuthAccount{
			configuredAccount: configuredAccount{
				AuthIndex:     authFile,
				AuthID:        email,
				Source:        source,
				Provider:      firstNonEmptyString(authType, "codex"),
				Email:         email,
				Name:          name,
				AuthFile:      authFile,
				AuthFileMTime: info.ModTime().Unix(),
				Disabled:      boolFromAny(doc["disabled"]),
				Expired:       boolFromAny(doc["expired"]),
				PlanType:      firstNonEmptyString(stringFromAny(doc["plan_type"]), stringFromAny(doc["plan"])),
			},
			AccessToken: firstNonEmptyString(
				stringFromAny(doc["access_token"]),
				stringFromAny(doc["accessToken"]),
				stringFromAny(doc["token"]),
			),
			ChatGPTAccountID: firstNonEmptyString(
				stringFromAny(doc["chatgpt_account_id"]),
				stringFromAny(doc["chatgptAccountId"]),
				stringFromAny(doc["account_id"]),
				stringFromAny(doc["accountId"]),
			),
		})
	}
	return out
}

func configuredAuthDir() string {
	if dir := strings.TrimSpace(os.Getenv("CPA_AUTH_DIR")); dir != "" {
		return dir
	}
	configPath := configuredConfigPath()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	return yamlScalar(string(raw), "auth-dir", "auth_dir")
}

func configuredConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("CPA_CONFIG_PATH")); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.Getenv("CPA_CONFIG_FILE")); path != "" {
		return path
	}
	for _, path := range []string{"/root/config.yaml", "/root/.cli-proxy-api/config.yaml"} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "/root/config.yaml"
}

func readConfiguredProviderNames() []string {
	raw, err := os.ReadFile(configuredConfigPath())
	if err != nil {
		return nil
	}
	return configuredProviderNamesFromYAML(string(raw))
}

func configuredProviderNamesFromYAML(raw string) []string {
	var out []string
	seen := map[string]bool{}
	inOpenAICompatibility := false
	blockIndent := -1
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if !inOpenAICompatibility && strings.HasPrefix(trimmed, "openai-compatibility:") {
			inOpenAICompatibility = true
			blockIndent = indent
			continue
		}
		if inOpenAICompatibility && indent <= blockIndent && !strings.HasPrefix(trimmed, "-") {
			inOpenAICompatibility = false
		}
		if !inOpenAICompatibility || indent > blockIndent+4 {
			continue
		}
		var name string
		switch {
		case strings.HasPrefix(trimmed, "name:"):
			name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		case strings.HasPrefix(trimmed, "- name:"):
			name = strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))
		default:
			continue
		}
		name = strings.Trim(name, `"'`)
		key := normalizeAccountAlias(name)
		if name != "" && !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

func authFileStateForRecord(rec usageRecord) (string, int64) {
	for _, cfg := range readConfiguredAuthAccounts() {
		for _, alias := range normalizeAccountAliases(rec.AuthIndex, rec.AuthID, rec.Source) {
			if alias == "" {
				continue
			}
			for _, cfgAlias := range configuredAliases(cfg) {
				if alias == cfgAlias {
					return cfg.AuthFile, cfg.AuthFileMTime
				}
			}
		}
	}
	authFile := firstNonEmptyString(fileNameIfJSON(rec.AuthIndex), fileNameIfJSON(rec.Source), fileNameIfJSON(rec.AuthID))
	if authFile == "" {
		return "", 0
	}
	authDir := configuredAuthDir()
	if authDir == "" {
		return authFile, 0
	}
	info, err := os.Stat(filepath.Join(authDir, authFile))
	if err != nil {
		return authFile, 0
	}
	return authFile, info.ModTime().Unix()
}

func fileNameIfJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	base := filepath.Base(value)
	if strings.HasSuffix(strings.ToLower(base), ".json") {
		return base
	}
	return ""
}

func yamlScalar(raw string, keys ...string) string {
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, key := range keys {
			prefix := key + ":"
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if i := strings.Index(value, " #"); i >= 0 {
				value = strings.TrimSpace(value[:i])
			}
			value = strings.Trim(value, `"'`)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func mergeConfiguredAccounts(accounts []accountRow, configured []configuredAccount) []accountRow {
	if len(configured) == 0 {
		return accounts
	}
	merged := append([]accountRow(nil), accounts...)
	index := make(map[string]int, len(merged)*4)
	for i := range merged {
		for _, alias := range accountAliases(merged[i]) {
			if alias == "" {
				continue
			}
			if _, exists := index[alias]; !exists {
				index[alias] = i
			}
		}
	}
	seenConfig := make(map[string]bool, len(configured))
	for _, cfg := range configured {
		canonical := normalizeAccountAlias(firstNonEmptyString(cfg.Email, cfg.AuthFile, cfg.Name))
		if canonical != "" && seenConfig[canonical] {
			continue
		}
		if canonical != "" {
			seenConfig[canonical] = true
		}
		match := -1
		for _, alias := range configuredAliases(cfg) {
			if i, ok := index[alias]; ok {
				match = i
				break
			}
		}
		if match >= 0 {
			enrichConfiguredAccount(&merged[match], cfg)
			for _, alias := range accountAliases(merged[match]) {
				if alias != "" {
					index[alias] = match
				}
			}
			continue
		}
		row := accountRow{
			AuthIndex:  cfg.AuthIndex,
			AuthID:     cfg.AuthID,
			Source:     cfg.Source,
			Provider:   firstNonEmptyString(cfg.Provider, "codex"),
			Email:      cfg.Email,
			Name:       cfg.Name,
			AuthFile:   cfg.AuthFile,
			Configured: true,
			Disabled:   cfg.Disabled,
			Expired:    cfg.Expired,
			PlanType:   cfg.PlanType,
		}
		merged = append(merged, row)
		rowIndex := len(merged) - 1
		for _, alias := range accountAliases(row) {
			if alias != "" {
				index[alias] = rowIndex
			}
		}
	}
	return merged
}

func enrichConfiguredAccount(row *accountRow, cfg configuredAccount) {
	row.Configured = true
	row.Disabled = cfg.Disabled
	row.Expired = cfg.Expired
	row.PlanType = firstNonEmptyString(row.PlanType, cfg.PlanType)
	row.Email = firstNonEmptyString(row.Email, cfg.Email)
	row.Name = firstNonEmptyString(row.Name, cfg.Name)
	row.AuthFile = firstNonEmptyString(row.AuthFile, cfg.AuthFile)
	row.Provider = firstNonEmptyString(row.Provider, cfg.Provider, "codex")
	if row.Source == "" || looksOpaqueAccountKey(row.Source) {
		row.Source = firstNonEmptyString(cfg.Source, row.Source)
	}
	if row.AuthID == "" || looksOpaqueAccountKey(row.AuthID) {
		row.AuthID = firstNonEmptyString(cfg.AuthID, row.AuthID)
	}
}

func accountAliases(row accountRow) []string {
	return normalizeAccountAliases(row.AuthIndex, row.AuthID, row.Source, row.Email, row.Name, row.AuthFile)
}

func configuredAliases(cfg configuredAccount) []string {
	return normalizeAccountAliases(cfg.AuthIndex, cfg.AuthID, cfg.Source, cfg.Email, cfg.Name, cfg.AuthFile)
}

func normalizeAccountAliases(values ...string) []string {
	seen := make(map[string]bool, len(values)*3)
	out := make([]string, 0, len(values)*3)
	add := func(value string) {
		alias := normalizeAccountAlias(value)
		if alias == "" || seen[alias] {
			return
		}
		seen[alias] = true
		out = append(out, alias)
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		add(value)
		if strings.HasSuffix(strings.ToLower(value), ".json") {
			add(strings.TrimSuffix(value, ".json"))
		}
		if strings.HasSuffix(strings.ToLower(value), ".cpa.json") {
			add(strings.TrimSuffix(value, ".cpa.json"))
		}
	}
	return out
}

func normalizeAccountAlias(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func looksOpaqueAccountKey(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 16 {
		return false
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

func queryLatestAccountWindowQuota(ctx context.Context, db *sql.DB, account accountRow, since int64, window string) (sql.NullFloat64, sql.NullInt64) {
	percentColumn := "primary_used_percent"
	resetColumn := "primary_reset_at"
	if window == "secondary" {
		percentColumn = "secondary_used_percent"
		resetColumn = "secondary_reset_at"
	}
	query := `
SELECT ` + percentColumn + `, ` + resetColumn + `
FROM (
  SELECT id, requested_at AS observed_at, auth_index, auth_id, source, '' AS auth_file, ` + percentColumn + `, ` + resetColumn + `
  FROM usage_events
  WHERE requested_at >= ? AND (` + percentColumn + ` IS NOT NULL OR ` + resetColumn + ` IS NOT NULL)
  UNION ALL
  SELECT id, finished_at AS observed_at, auth_index, auth_id, source, auth_file, ` + percentColumn + `, ` + resetColumn + `
  FROM quota_trigger_runs
  WHERE finished_at >= ? AND status='success' AND (` + percentColumn + ` IS NOT NULL OR ` + resetColumn + ` IS NOT NULL)
) snapshots
WHERE (
  (auth_index <> '' AND auth_index = ?)
  OR (auth_id <> '' AND auth_id = ?)
  OR (source <> '' AND source = ?)
  OR (auth_file <> '' AND auth_file = ?)
)
ORDER BY observed_at DESC, id DESC
LIMIT 1`
	var percent sql.NullFloat64
	var reset sql.NullInt64
	err := db.QueryRowContext(ctx, query, since, since, account.AuthIndex, account.AuthID, account.Source, account.AuthFile).Scan(&percent, &reset)
	if err != nil {
		return sql.NullFloat64{}, sql.NullInt64{}
	}
	if reset.Valid {
		reset.Int64 = normalizeUnixSeconds(reset.Int64)
		if reset.Int64 <= time.Now().Unix() {
			return sql.NullFloat64{}, sql.NullInt64{}
		}
	}
	return percent, reset
}

func applyAccountQuotaSnapshot(account *accountRow, pp sql.NullFloat64, pr sql.NullInt64, sp sql.NullFloat64, sr sql.NullInt64) {
	if pp.Valid {
		account.PrimaryUsedPercent = &pp.Float64
	}
	if pr.Valid {
		account.PrimaryResetAt = &pr.Int64
	}
	if sp.Valid {
		account.SecondaryUsedPercent = &sp.Float64
	}
	if sr.Valid {
		account.SecondaryResetAt = &sr.Int64
	}
}

func queryAccountWindowTokens(ctx context.Context, db *sql.DB, account accountRow, reset sql.NullInt64, duration time.Duration) int64 {
	if !reset.Valid || reset.Int64 <= 0 {
		return 0
	}
	end := normalizeUnixSeconds(reset.Int64)
	if end <= 0 || end <= time.Now().Unix() {
		return 0
	}
	start := end - int64(duration.Seconds())
	return queryAccountTokensBetween(ctx, db, account, start, end)
}

func queryAccountTokensBetween(ctx context.Context, db *sql.DB, account accountRow, start int64, end int64) int64 {
	if end <= 0 {
		end = time.Now().Unix()
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		return 0
	}
	var total int64
	_ = db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND requested_at <= ?
AND (
  (auth_index <> '' AND auth_index = ?)
  OR (auth_id <> '' AND auth_id = ?)
  OR (source <> '' AND source = ?)
)`, start, end, account.AuthIndex, account.AuthID, account.Source).Scan(&total)
	return total
}

func secondaryQuotaDuration(account accountRow, reset sql.NullInt64) time.Duration {
	if isFreePlan(account.PlanType) {
		return 30 * 24 * time.Hour
	}
	if reset.Valid {
		resetAt := normalizeUnixSeconds(reset.Int64)
		if resetAt-time.Now().Unix() > int64(8*24*time.Hour/time.Second) {
			return 30 * 24 * time.Hour
		}
	}
	return 7 * 24 * time.Hour
}

func secondaryQuotaWindowLabel(duration time.Duration) string {
	if duration >= 28*24*time.Hour {
		return "month"
	}
	return "7d"
}

func isFreePlan(plan string) bool {
	plan = strings.ToLower(strings.TrimSpace(plan))
	return plan == "free" || strings.Contains(plan, "free") || strings.Contains(plan, "trial")
}

type externalUseTracker struct {
	has       bool
	percent   float64
	resetAt   int64
	timestamp int64
	cumTokens int64
}

type externalUseEvent struct {
	timestamp        int64
	provider         string
	authID           string
	authIndex        string
	source           string
	totalTokens      int64
	primaryPercent   sql.NullFloat64
	primaryResetAt   sql.NullInt64
	secondaryPercent sql.NullFloat64
	secondaryResetAt sql.NullInt64
}

type externalUseLocalEvent struct {
	timestamp int64
	tokens    int64
}

func queryExternalUseAlerts(ctx context.Context, db *sql.DB, since int64) ([]externalUseAlert, error) {
	minDeltaPct := envFloat("CPA_EXTERNAL_USE_MIN_DELTA_PERCENT", 5.0)
	maxLocalTokens := envInt64("CPA_EXTERNAL_USE_MAX_LOCAL_TOKENS", 1500)
	minIdleSeconds := envInt64("CPA_EXTERNAL_USE_MIN_IDLE_SECONDS", 600)
	delayGraceSeconds := envInt64("CPA_EXTERNAL_USE_DELAY_GRACE_SECONDS", 3*3600)
	delayGraceMinTokens := envInt64("CPA_EXTERNAL_USE_DELAY_GRACE_MIN_TOKENS", 50000)
	rows, err := db.QueryContext(ctx, `
SELECT requested_at, provider, auth_id, auth_index, source, total_tokens,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
FROM (
  SELECT id, requested_at, provider, auth_id, auth_index, source, total_tokens,
  primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
  FROM usage_events
  WHERE requested_at >= ?
  AND `+usageScopeSQL("codex")+`
  AND (primary_used_percent IS NOT NULL OR secondary_used_percent IS NOT NULL)
  AND (auth_index <> '' OR auth_id <> '' OR source <> '')
  AND NOT (executor_type='quota-trigger' OR model='quota-trigger' OR alias='quota-trigger')
  UNION ALL
  SELECT id, finished_at AS requested_at, provider, auth_id, auth_index, source, 0 AS total_tokens,
  primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
  FROM quota_trigger_runs
  WHERE finished_at >= ?
  AND provider='codex'
  AND status='success'
  AND (primary_used_percent IS NOT NULL OR secondary_used_percent IS NOT NULL)
  AND (auth_index <> '' OR auth_id <> '' OR source <> '')
) snapshots
ORDER BY COALESCE(NULLIF(source,''), NULLIF(auth_id,''), NULLIF(auth_index,''), 'unknown'), requested_at ASC, id ASC`, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []externalUseAlert
	var currentKey string
	var primary externalUseTracker
	var secondary externalUseTracker
	var cumulativeTokens int64
	var recentLocal []externalUseLocalEvent
	var recentLocalTokens int64
	seen := map[string]externalUseAlert{}
	for rows.Next() {
		var ev externalUseEvent
		if err := rows.Scan(
			&ev.timestamp, &ev.provider, &ev.authID, &ev.authIndex, &ev.source, &ev.totalTokens,
			&ev.primaryPercent, &ev.primaryResetAt, &ev.secondaryPercent, &ev.secondaryResetAt,
		); err != nil {
			return nil, err
		}
		key := firstNonEmptyString(ev.source, ev.authID, ev.authIndex, "unknown")
		if key != currentKey {
			currentKey = key
			primary = externalUseTracker{}
			secondary = externalUseTracker{}
			cumulativeTokens = 0
			recentLocal = nil
			recentLocalTokens = 0
		}
		if ev.totalTokens > 0 {
			cumulativeTokens += ev.totalTokens
			recentLocal = append(recentLocal, externalUseLocalEvent{timestamp: ev.timestamp, tokens: ev.totalTokens})
			recentLocalTokens += ev.totalTokens
		}
		if delayGraceSeconds > 0 {
			cutoff := ev.timestamp - delayGraceSeconds
			keep := 0
			for _, item := range recentLocal {
				if item.timestamp >= cutoff {
					recentLocal[keep] = item
					keep++
				} else {
					recentLocalTokens -= item.tokens
				}
			}
			recentLocal = recentLocal[:keep]
		}
		checkExternalUseWindow(&alerts, seen, "5h", ev, ev.primaryPercent, ev.primaryResetAt, &primary, cumulativeTokens, recentLocalTokens, minDeltaPct, maxLocalTokens, minIdleSeconds, delayGraceMinTokens)
		checkExternalUseWindow(&alerts, seen, "7d", ev, ev.secondaryPercent, ev.secondaryResetAt, &secondary, cumulativeTokens, recentLocalTokens, minDeltaPct, maxLocalTokens, minIdleSeconds, delayGraceMinTokens)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return alerts, nil
}

func checkExternalUseWindow(alerts *[]externalUseAlert, seen map[string]externalUseAlert, window string, ev externalUseEvent, percent sql.NullFloat64, reset sql.NullInt64, tracker *externalUseTracker, cumulativeTokens int64, recentLocalTokens int64, minDeltaPct float64, maxLocalTokens int64, minIdleSeconds int64, delayGraceMinTokens int64) {
	if !percent.Valid || !reset.Valid || reset.Int64 <= 0 {
		return
	}
	resetAt := normalizeUnixSeconds(reset.Int64)
	if tracker.has && tracker.resetAt == resetAt {
		deltaPct := percent.Float64 - tracker.percent
		localTokens := cumulativeTokens - tracker.cumTokens
		idleSeconds := ev.timestamp - tracker.timestamp
		if deltaPct >= minDeltaPct && localTokens <= maxLocalTokens && idleSeconds >= minIdleSeconds {
			if delayGraceMinTokens > 0 && recentLocalTokens >= delayGraceMinTokens {
				tracker.has = true
				tracker.percent = percent.Float64
				tracker.resetAt = resetAt
				tracker.timestamp = ev.timestamp
				tracker.cumTokens = cumulativeTokens
				return
			}
			reason := "quota +" + strconv.FormatFloat(deltaPct, 'f', 1, 64) + "%, local " + strconv.FormatInt(localTokens, 10) + " tokens"
			alert := externalUseAlert{
				AuthIndex:       ev.authIndex,
				AuthID:          ev.authID,
				Source:          ev.source,
				Provider:        ev.provider,
				Window:          window,
				DetectedAt:      ev.timestamp,
				DetectedAtText:  unixTime(ev.timestamp),
				PreviousPercent: tracker.percent,
				CurrentPercent:  percent.Float64,
				DeltaPercent:    deltaPct,
				LocalTokens:     localTokens,
				IdleSeconds:     idleSeconds,
				Reason:          reason,
			}
			key := normalizeAccountAlias(firstNonEmptyString(ev.source, ev.authID, ev.authIndex)) + "|" + window
			if previous, ok := seen[key]; !ok || alert.DeltaPercent > previous.DeltaPercent || alert.DetectedAt > previous.DetectedAt {
				seen[key] = alert
			}
		}
	}
	tracker.has = true
	tracker.percent = percent.Float64
	tracker.resetAt = resetAt
	tracker.timestamp = ev.timestamp
	tracker.cumTokens = cumulativeTokens
	*alerts = (*alerts)[:0]
	for _, alert := range seen {
		*alerts = append(*alerts, alert)
	}
}

func applyExternalUseAlerts(accounts []accountRow, alerts []externalUseAlert) {
	if len(alerts) == 0 || len(accounts) == 0 {
		return
	}
	index := make(map[string]int, len(accounts)*4)
	for i := range accounts {
		for _, alias := range accountAliases(accounts[i]) {
			if alias != "" {
				index[alias] = i
			}
		}
	}
	for _, alert := range alerts {
		for _, alias := range normalizeAccountAliases(alert.AuthIndex, alert.AuthID, alert.Source) {
			i, ok := index[alias]
			if !ok {
				continue
			}
			row := &accounts[i]
			row.ExternalUseSuspected = true
			row.ExternalUseCount++
			if alert.DeltaPercent >= row.ExternalUseDeltaPct {
				row.ExternalUseWindow = alert.Window
				row.ExternalUseDeltaPct = alert.DeltaPercent
				row.ExternalUseLocalTokens = alert.LocalTokens
				row.ExternalUseDetectedAt = alert.DetectedAtText
				row.ExternalUseReason = alert.Reason
			}
			break
		}
	}
}

func applyInvalidAuths(accounts []accountRow, invalids []invalidAuthRow) {
	if len(invalids) == 0 || len(accounts) == 0 {
		return
	}
	index := make(map[string]int, len(accounts)*4)
	for i := range accounts {
		for _, alias := range accountAliases(accounts[i]) {
			if alias != "" {
				index[alias] = i
			}
		}
	}
	for _, invalid := range invalids {
		for _, alias := range normalizeAccountAliases(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile) {
			i, ok := index[alias]
			if !ok {
				continue
			}
			accounts[i].InvalidAuth = true
			accounts[i].InvalidAuthAt = invalid.InvalidatedAtText
			accounts[i].InvalidAuthReason = invalid.Reason
			break
		}
	}
}

func applyLatestQuotaSnapshots(ctx context.Context, db *sql.DB, accounts []accountRow, since int64) {
	for i := range accounts {
		pp, pr := queryLatestAccountWindowQuota(ctx, db, accounts[i], since, "primary")
		sp, sr := queryLatestAccountWindowQuota(ctx, db, accounts[i], since, "secondary")
		accounts[i].PrimaryUsedPercent = nil
		accounts[i].PrimaryResetAt = nil
		accounts[i].SecondaryUsedPercent = nil
		accounts[i].SecondaryResetAt = nil
		accounts[i].PrimaryWindowTokens = 0
		accounts[i].SecondaryWindowTokens = 0
		applyAccountQuotaSnapshot(&accounts[i], pp, pr, sp, sr)
		accounts[i].PrimaryWindowTokens = queryAccountWindowTokens(ctx, db, accounts[i], pr, 5*time.Hour)
		secondaryDuration := secondaryQuotaDuration(accounts[i], sr)
		accounts[i].SecondaryWindowTokens = queryAccountWindowTokens(ctx, db, accounts[i], sr, secondaryDuration)
		accounts[i].SecondaryQuotaWindow = secondaryQuotaWindowLabel(secondaryDuration)
	}
}

func queryRecentQuotaTriggerRuns(ctx context.Context, db *sql.DB, limit int) ([]quotaTriggerAccountStatus, error) {
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, auth_file, mode, status, http_status, error, finished_at
FROM quota_trigger_runs
ORDER BY finished_at DESC, id DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]quotaTriggerAccountStatus, 0, limit)
	for rows.Next() {
		var r quotaTriggerAccountStatus
		if err := rows.Scan(&r.AuthID, &r.AuthIndex, &r.Source, &r.AuthFile, &r.Mode, &r.Status, &r.HTTPStatus, &r.Error, &r.FinishedAt); err != nil {
			return nil, err
		}
		r.Error = sanitizeTriggerError(r.Error)
		out = append(out, r)
	}
	return out, rows.Err()
}

func applyQuotaTriggerStatuses(accounts []accountRow, runs []quotaTriggerAccountStatus) {
	if len(accounts) == 0 || len(runs) == 0 {
		return
	}
	index := make(map[string]int, len(accounts)*4)
	for i := range accounts {
		for _, alias := range accountAliases(accounts[i]) {
			if alias != "" {
				index[alias] = i
			}
		}
	}
	seen := map[int]bool{}
	for _, run := range runs {
		for _, alias := range normalizeAccountAliases(run.AuthID, run.AuthIndex, run.Source, run.AuthFile) {
			i, ok := index[alias]
			if !ok || seen[i] {
				continue
			}
			seen[i] = true
			accounts[i].QuotaTriggerLastAt = unixTime(run.FinishedAt)
			accounts[i].QuotaTriggerStatus = run.Status
			accounts[i].QuotaTriggerMode = run.Mode
			accounts[i].QuotaTriggerHTTPStatus = run.HTTPStatus
			accounts[i].QuotaTriggerError = sanitizeTriggerError(run.Error)
			break
		}
	}
}

func applyAccountQuotaToAutobans(bans []autobanRow, accounts []accountRow) {
	if len(bans) == 0 || len(accounts) == 0 {
		return
	}
	index := make(map[string]int, len(accounts)*4)
	for i := range accounts {
		for _, alias := range accountAliases(accounts[i]) {
			if alias != "" {
				index[alias] = i
			}
		}
	}
	for i := range bans {
		match := -1
		for _, alias := range normalizeAccountAliases(bans[i].AuthID, bans[i].AuthIndex, bans[i].Source) {
			if accountIndex, ok := index[alias]; ok {
				match = accountIndex
				break
			}
		}
		if match < 0 {
			continue
		}
		account := accounts[match]
		bans[i].PrimaryUsedPercent = account.PrimaryUsedPercent
		bans[i].PrimaryResetAt = account.PrimaryResetAt
		bans[i].SecondaryUsedPercent = account.SecondaryUsedPercent
		bans[i].SecondaryResetAt = account.SecondaryResetAt
	}
}

func applySecondaryQuotaEstimates(ctx context.Context, db *sql.DB, accounts []accountRow, totals *totalsRow, since int64) {
	if totals == nil {
		return
	}
	var estimatedAccounts int64
	for i := range accounts {
		total, remaining := latestSecondaryQuotaTriggerCapacity(ctx, db, accounts[i], since)
		if total <= 0 {
			total, remaining = estimateQuotaFromUsedPercent(accounts[i].SecondaryWindowTokens, accounts[i].SecondaryUsedPercent)
		}
		if total <= 0 {
			continue
		}
		accounts[i].SecondaryQuotaTotalEstimate = total
		accounts[i].SecondaryQuotaRemainingEstimate = remaining
		totals.SecondaryQuotaTotalEstimate += total
		totals.SecondaryQuotaRemainingEstimate += remaining
		estimatedAccounts++
	}
	totals.SecondaryQuotaEstimatedAccounts = estimatedAccounts
}

func latestSecondaryQuotaTriggerCapacity(ctx context.Context, db *sql.DB, account accountRow, since int64) (int64, int64) {
	var limit, remaining sql.NullInt64
	var reset sql.NullInt64
	var finishedAt int64
	err := db.QueryRowContext(ctx, `
SELECT secondary_limit_tokens, secondary_remaining_tokens, secondary_reset_at, finished_at
FROM quota_trigger_runs
WHERE finished_at >= ?
AND status='success'
AND (secondary_limit_tokens IS NOT NULL OR secondary_remaining_tokens IS NOT NULL)
AND (
  (auth_index <> '' AND auth_index = ?)
  OR (auth_id <> '' AND auth_id = ?)
  OR (source <> '' AND source = ?)
  OR (auth_file <> '' AND auth_file = ?)
)
ORDER BY finished_at DESC, id DESC
LIMIT 1`, since, account.AuthIndex, account.AuthID, account.Source, account.AuthFile).Scan(&limit, &remaining, &reset, &finishedAt)
	if err != nil {
		return 0, 0
	}
	if reset.Valid && normalizeUnixSeconds(reset.Int64) <= time.Now().Unix() {
		return 0, 0
	}
	total := int64(0)
	remain := int64(0)
	if limit.Valid && limit.Int64 > 0 {
		total = limit.Int64
	}
	if remaining.Valid && remaining.Int64 >= 0 {
		remain = remaining.Int64
	}
	if total <= 0 || remain > total {
		return 0, 0
	}
	duration := secondaryQuotaDuration(account, reset)
	windowStart := normalizeUnixSeconds(reset.Int64) - int64(duration.Seconds())
	if windowStart < 0 {
		windowStart = 0
	}
	start := finishedAt
	if start < windowStart {
		start = windowStart
	}
	if usedAfterSnapshot := queryAccountTokensBetween(ctx, db, account, start+1, time.Now().Unix()); usedAfterSnapshot > 0 {
		remain -= usedAfterSnapshot
		if remain < 0 {
			remain = 0
		}
	}
	return total, remain
}

func estimateQuotaFromUsedPercent(usedTokens int64, usedPercent *float64) (int64, int64) {
	if usedTokens <= 0 || usedPercent == nil || *usedPercent <= 0 {
		return 0, 0
	}
	percent := *usedPercent
	total := int64(float64(usedTokens)*100.0/percent + 0.5)
	if total < usedTokens {
		total = usedTokens
	}
	remaining := total - usedTokens
	if remaining < 0 {
		remaining = 0
	}
	return total, remaining
}

func normalizeUnixSeconds(value int64) int64 {
	if value > 1e12 {
		return value / 1000
	}
	return value
}

func normalizeInt64Ptr(value *int64) {
	if value == nil {
		return
	}
	*value = normalizeUnixSeconds(*value)
}

func queryProviders(ctx context.Context, db *sql.DB, since int64, limit int, scope string) ([]providerRow, error) {
	query := `
SELECT ` + cpaProviderSQL() + ` AS provider_key,
COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0),
COUNT(DISTINCT NULLIF(COALESCE(NULLIF(auth_index,''), NULLIF(auth_id,''), NULLIF(source,'')), '')),
COUNT(DISTINCT NULLIF(model,'')),
MAX(requested_at)
FROM usage_events
WHERE requested_at >= ? AND ` + usageScopeSQL(scope) + `
GROUP BY provider_key
ORDER BY SUM(total_tokens) DESC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []providerRow
	for rows.Next() {
		var r providerRow
		var last int64
		if err := rows.Scan(
			&r.Provider, &r.Requests, &r.Failed, &r.RateLimited, &r.InputTokens, &r.OutputTokens,
			&r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
			&r.TotalTokens, &r.Accounts, &r.Models, &last,
		); err != nil {
			return nil, err
		}
		r.LastSeen = unixTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

func cpaProviderSQL() string {
	tail := "substr(auth_id, length('openai-compatibility:') + 1)"
	return `CASE
WHEN auth_id LIKE 'openai-compatibility:%:%' THEN substr(` + tail + `, 1, instr(` + tail + `, ':') - 1)
WHEN provider LIKE 'openai-compatible-%' THEN substr(provider, length('openai-compatible-') + 1)
WHEN provider LIKE 'openai-compatibility-%' THEN substr(provider, length('openai-compatibility-') + 1)
WHEN lower(provider) IN ('openai-compatible', 'openai-compatibility') AND source <> '' AND source NOT LIKE 'sk-%' AND source NOT LIKE 'ark-%' AND source NOT LIKE 'Bearer %' THEN source
WHEN lower(provider) IN ('anthropic', 'claude') THEN 'Claude'
WHEN lower(provider) LIKE 'gemini%' THEN 'Gemini'
WHEN lower(provider) = 'codex' THEN 'Codex'
ELSE COALESCE(NULLIF(provider,''), 'unknown')
END`
}

func mergeConfiguredProviders(rows []providerRow, names []string) []providerRow {
	if len(names) == 0 {
		return rows
	}
	seen := make(map[string]bool, len(rows)+len(names))
	for _, row := range rows {
		seen[normalizeAccountAlias(row.Provider)] = true
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := normalizeAccountAlias(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, providerRow{Provider: name})
	}
	return rows
}

func queryModels(ctx context.Context, db *sql.DB, since int64, limit int, scope string) ([]modelRow, error) {
	query := `
SELECT model, alias, ` + cpaProviderSQL() + ` AS provider_key, COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(input_tokens),0),
COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND ` + usageScopeSQL(scope) + `
GROUP BY model, alias, provider_key
ORDER BY SUM(total_tokens) DESC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelRow
	for rows.Next() {
		var r modelRow
		if err := rows.Scan(
			&r.Model, &r.Alias, &r.Provider, &r.Requests, &r.TotalTokens, &r.InputTokens, &r.OutputTokens,
			&r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type databaseDiagnostics struct {
	Path                 string `json:"path"`
	SizeBytes            int64  `json:"size_bytes"`
	UsageEvents          int64  `json:"usage_events"`
	QuotaTriggerRuns     int64  `json:"quota_trigger_runs"`
	AutobanRows          int64  `json:"autoban_rows"`
	InvalidAuthRows      int64  `json:"invalid_auth_rows"`
	LatestEventAt        string `json:"latest_event_at,omitempty"`
	LatestEventAgeSecs   int64  `json:"latest_event_age_seconds,omitempty"`
	LatestTriggerAt      string `json:"latest_trigger_at,omitempty"`
	LatestTriggerAgeSecs int64  `json:"latest_trigger_age_seconds,omitempty"`
}

type authDiagnostics struct {
	Files                 int `json:"files"`
	Codex                 int `json:"codex"`
	Anthropic             int `json:"anthropic"`
	Antigravity           int `json:"antigravity"`
	Gemini                int `json:"gemini"`
	Disabled              int `json:"disabled"`
	Expired               int `json:"expired"`
	Invalid401            int `json:"invalid_401"`
	Autoban429            int `json:"autoban_429"`
	ExternalUseSuspected  int `json:"external_use_suspected"`
	QuotaTriggerAvailable int `json:"quota_trigger_available"`
}

type schedulerDiagnostics struct {
	ActiveBanCount      int    `json:"active_ban_count"`
	FilteredCandidates  int    `json:"filtered_candidates"`
	UnmatchedActiveBans int    `json:"unmatched_active_bans"`
	LastFilteredAt      string `json:"last_filtered_at,omitempty"`
}

type schedulerDiagnosticsTracker struct {
	mu    sync.Mutex
	state schedulerDiagnostics
}

func (t *schedulerDiagnosticsTracker) record(activeBans int, filteredCandidates int, unmatchedActiveBans int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.ActiveBanCount = activeBans
	t.state.FilteredCandidates = filteredCandidates
	t.state.UnmatchedActiveBans = unmatchedActiveBans
	if filteredCandidates > 0 {
		t.state.LastFilteredAt = time.Now().Format(time.RFC3339)
	}
}

func (t *schedulerDiagnosticsTracker) status(activeBans int) schedulerDiagnostics {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.state
	out.ActiveBanCount = activeBans
	if activeBans == 0 {
		out.UnmatchedActiveBans = 0
	}
	return out
}

type providerDiagnostics struct {
	Configured              int      `json:"configured"`
	Observed                int      `json:"observed"`
	Matched                 int      `json:"matched"`
	UnmatchedConfigured     []string `json:"unmatched_configured,omitempty"`
	PossibleDuplicates      []string `json:"possible_duplicates,omitempty"`
	ConfiguredProviderTypes []string `json:"configured_provider_types,omitempty"`
}

type modelPriceDiagnostics struct {
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url"`
	Path           string `json:"path"`
	IntervalHours  int    `json:"interval_hours"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
	LastUpdatedAt  string `json:"last_updated_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	FileSizeBytes  int64  `json:"file_size_bytes"`
	Entries        int    `json:"entries"`
	LoadedPrices   int    `json:"loaded_prices"`
	Exists         bool   `json:"exists"`
	Stale          bool   `json:"stale"`
	AgeSeconds     int64  `json:"age_seconds,omitempty"`
}

type diagnosticsSummary struct {
	Database     databaseDiagnostics   `json:"database"`
	AuthFiles    authDiagnostics       `json:"auth_files"`
	Scheduler    schedulerDiagnostics  `json:"scheduler"`
	Providers    providerDiagnostics   `json:"providers"`
	ModelPrices  modelPriceDiagnostics `json:"model_prices"`
	QuotaTrigger quotaTriggerState     `json:"quota_trigger"`
	Retention    retentionState        `json:"retention"`
}

type dashboardAlert struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Type      string `json:"type"`
	Scope     string `json:"scope"`
	Target    string `json:"target"`
	Message   string `json:"message"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Active    bool   `json:"active"`
}

type retentionState struct {
	UsageRetentionDays         int    `json:"usage_retention_days"`
	QuotaTriggerRetentionDays  int    `json:"quota_trigger_retention_days"`
	RequestDetailRetentionDays int    `json:"request_detail_retention_days"`
	LastRunAt                  string `json:"last_run_at,omitempty"`
	LastError                  string `json:"last_error,omitempty"`
	LastUsageDeleted           int64  `json:"last_usage_deleted"`
	LastQuotaTriggerDeleted    int64  `json:"last_quota_trigger_deleted"`
	LastSizeBeforeBytes        int64  `json:"last_size_before_bytes"`
	LastSizeAfterBytes         int64  `json:"last_size_after_bytes"`
}

type retentionCleaner struct {
	mu     sync.Mutex
	cfg    pluginConfig
	cancel context.CancelFunc
	state  retentionState
}

func (r *retentionCleaner) configure(cfg pluginConfig) {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.cfg = cfg
	r.state.UsageRetentionDays = cfg.UsageRetentionDays
	r.state.QuotaTriggerRetentionDays = cfg.QuotaTriggerRetentionDays
	r.state.RequestDetailRetentionDays = cfg.RequestDetailRetentionDays
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()
	go r.loop(ctx, cfg)
}

func (r *retentionCleaner) stop() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.mu.Unlock()
}

func (r *retentionCleaner) status() retentionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *retentionCleaner) loop(ctx context.Context, cfg pluginConfig) {
	r.run(ctx, cfg)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.run(ctx, cfg)
		}
	}
}

func (r *retentionCleaner) run(ctx context.Context, cfg pluginConfig) {
	db, path, err := globalStore.open(ctx)
	if err != nil {
		r.record(err, 0, 0, 0, 0)
		return
	}
	before := databaseFileSize(path)
	now := time.Now().Unix()
	usageCutoff := now - int64(cfg.UsageRetentionDays)*86400
	triggerCutoff := now - int64(cfg.QuotaTriggerRetentionDays)*86400
	usageDeleted, err := execDelete(ctx, db, `DELETE FROM usage_events WHERE requested_at < ?`, usageCutoff)
	if err != nil {
		r.record(err, usageDeleted, 0, before, databaseFileSize(path))
		return
	}
	triggerDeleted, err := execDelete(ctx, db, `DELETE FROM quota_trigger_runs WHERE finished_at < ?`, triggerCutoff)
	if err != nil {
		r.record(err, usageDeleted, triggerDeleted, before, databaseFileSize(path))
		return
	}
	r.record(nil, usageDeleted, triggerDeleted, before, databaseFileSize(path))
}

func (r *retentionCleaner) record(err error, usageDeleted, triggerDeleted, before, after int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.LastRunAt = time.Now().Format(time.RFC3339)
	r.state.LastUsageDeleted = usageDeleted
	r.state.LastQuotaTriggerDeleted = triggerDeleted
	r.state.LastSizeBeforeBytes = before
	r.state.LastSizeAfterBytes = after
	if err != nil {
		r.state.LastError = sanitizeTriggerError(err.Error())
	} else {
		r.state.LastError = ""
	}
}

func execDelete(ctx context.Context, db *sql.DB, query string, cutoff int64) (int64, error) {
	res, err := db.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func buildDiagnostics(ctx context.Context, db *sql.DB, dbPath string, accounts []accountRow, providers []providerRow, invalidAuths []invalidAuthRow, autobans []autobanRow, externalAlerts []externalUseAlert) diagnosticsSummary {
	priceState := globalModelPriceUpdater.status()
	return diagnosticsSummary{
		Database:     queryDatabaseDiagnostics(ctx, db, dbPath),
		AuthFiles:    buildAuthDiagnostics(accounts, invalidAuths, autobans, externalAlerts),
		Scheduler:    globalSchedulerDiagnostics.status(len(autobans)),
		Providers:    buildProviderDiagnostics(providers),
		ModelPrices:  buildModelPriceDiagnostics(priceState),
		QuotaTrigger: globalQuotaTrigger.status(),
		Retention:    globalRetentionCleaner.status(),
	}
}

func queryDatabaseDiagnostics(ctx context.Context, db *sql.DB, path string) databaseDiagnostics {
	now := time.Now().Unix()
	out := databaseDiagnostics{
		Path:             path,
		SizeBytes:        databaseFileSize(path),
		UsageEvents:      queryCount(ctx, db, `SELECT COUNT(*) FROM usage_events`),
		QuotaTriggerRuns: queryCount(ctx, db, `SELECT COUNT(*) FROM quota_trigger_runs`),
		AutobanRows:      queryCount(ctx, db, `SELECT COUNT(*) FROM autoban_bans`),
		InvalidAuthRows:  queryCount(ctx, db, `SELECT COUNT(*) FROM invalid_auths`),
	}
	if ts := queryMaxUnix(ctx, db, `SELECT COALESCE(MAX(requested_at),0) FROM usage_events`); ts > 0 {
		out.LatestEventAt = unixTime(ts)
		out.LatestEventAgeSecs = maxInt64(0, now-ts)
	}
	if ts := queryMaxUnix(ctx, db, `SELECT COALESCE(MAX(finished_at),0) FROM quota_trigger_runs`); ts > 0 {
		out.LatestTriggerAt = unixTime(ts)
		out.LatestTriggerAgeSecs = maxInt64(0, now-ts)
	}
	return out
}

func databaseFileSize(path string) int64 {
	var size int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			size += info.Size()
		}
	}
	return size
}

func queryCount(ctx context.Context, db *sql.DB, query string) int64 {
	var n int64
	_ = db.QueryRowContext(ctx, query).Scan(&n)
	return n
}

func queryMaxUnix(ctx context.Context, db *sql.DB, query string) int64 {
	var n int64
	_ = db.QueryRowContext(ctx, query).Scan(&n)
	return normalizeUnixSeconds(n)
}

func buildAuthDiagnostics(accounts []accountRow, invalidAuths []invalidAuthRow, autobans []autobanRow, externalAlerts []externalUseAlert) authDiagnostics {
	files := readConfiguredAuthFiles()
	out := authDiagnostics{
		Files:                len(files),
		Invalid401:           len(invalidAuths),
		Autoban429:           count429Autobans(autobans),
		ExternalUseSuspected: len(externalAlerts),
	}
	for _, file := range files {
		switch strings.ToLower(strings.TrimSpace(file.Provider)) {
		case "codex":
			out.Codex++
		case "anthropic":
			out.Anthropic++
		case "antigravity":
			out.Antigravity++
		case "gemini":
			out.Gemini++
		}
		if file.Disabled {
			out.Disabled++
		}
		if file.Expired {
			out.Expired++
		}
	}
	for _, account := range accounts {
		if isCodexAuthProvider(account.Provider) && !account.Disabled && !account.Expired && !account.InvalidAuth && !account.ExternalUseSuspected {
			out.QuotaTriggerAvailable++
		}
	}
	return out
}

func count429Autobans(autobans []autobanRow) int {
	count := 0
	for _, ban := range autobans {
		if strings.EqualFold(strings.TrimSpace(ban.Window), "401") || ban.LastStatusCode == http.StatusUnauthorized {
			continue
		}
		count++
	}
	return count
}

func buildProviderDiagnostics(providers []providerRow) providerDiagnostics {
	configured := readConfiguredProviderEntries()
	out := providerDiagnostics{Configured: len(configured), Observed: len(providers)}
	observed := map[string]bool{}
	for _, provider := range providers {
		observed[normalizeAccountAlias(providerLabelForBackend(provider.Provider))] = true
	}
	typeSet := map[string]bool{}
	for _, entry := range configured {
		typeSet[entry.Provider] = true
		name := providerLabelForBackend(entry.Name)
		if observed[normalizeAccountAlias(name)] {
			out.Matched++
		} else {
			out.UnmatchedConfigured = append(out.UnmatchedConfigured, entry.Name)
		}
	}
	for typ := range typeSet {
		out.ConfiguredProviderTypes = append(out.ConfiguredProviderTypes, typ)
	}
	sort.Strings(out.ConfiguredProviderTypes)
	out.PossibleDuplicates = possibleProviderDuplicates(providers)
	return out
}

func providerLabelForBackend(name string) string {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	for _, prefix := range []string{"openai-compatible-", "openai-compatibility-"} {
		if strings.HasPrefix(lower, prefix) && len(name) > len(prefix) {
			return name[len(prefix):]
		}
	}
	if strings.HasPrefix(lower, "openai-compatibility:") {
		parts := strings.Split(name, ":")
		if len(parts) >= 2 && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1])
		}
	}
	return name
}

func possibleProviderDuplicates(providers []providerRow) []string {
	groups := map[string][]string{}
	for _, provider := range providers {
		name := providerLabelForBackend(provider.Provider)
		key := normalizeAccountAlias(strings.TrimRight(name, "0123456789 "))
		if key == "" {
			key = normalizeAccountAlias(name)
		}
		groups[key] = append(groups[key], provider.Provider)
	}
	var out []string
	for _, names := range groups {
		if len(names) > 1 {
			sort.Strings(names)
			out = append(out, strings.Join(names, " / "))
		}
	}
	sort.Strings(out)
	return out
}

func buildModelPriceDiagnostics(state modelPriceUpdateState) modelPriceDiagnostics {
	out := modelPriceDiagnostics{
		Enabled:        state.Enabled,
		URL:            state.URL,
		Path:           state.Path,
		IntervalHours:  state.IntervalHours,
		TimeoutSeconds: state.TimeoutSeconds,
		LastCheckedAt:  state.LastCheckedAt,
		LastUpdatedAt:  state.LastUpdatedAt,
		LastError:      state.LastError,
		FileSizeBytes:  state.FileSizeBytes,
		Entries:        state.Entries,
		LoadedPrices:   state.LoadedPrices,
	}
	if info, err := os.Stat(state.Path); err == nil {
		out.Exists = true
		out.FileSizeBytes = info.Size()
		out.AgeSeconds = int64(time.Since(info.ModTime()).Seconds())
		maxAge := time.Duration(maxInt(1, state.IntervalHours*2)) * time.Hour
		out.Stale = time.Since(info.ModTime()) > maxAge
		if out.LastUpdatedAt == "" {
			out.LastUpdatedAt = info.ModTime().Format(time.RFC3339)
		}
	} else {
		out.Stale = true
	}
	return out
}

func buildAlerts(data map[string]any) []dashboardAlert {
	now := time.Now().Format(time.RFC3339)
	var alerts []dashboardAlert
	if rows, ok := data["invalid_auths"].([]invalidAuthRow); ok {
		for _, row := range rows {
			alerts = append(alerts, dashboardAlert{ID: "invalid:" + firstNonEmptyString(row.AuthID, row.AuthIndex, row.Source), Severity: "critical", Type: "401", Scope: "account", Target: firstNonEmptyString(row.Source, row.AuthID, row.AuthIndex), Message: "账号 401 失效，已停止使用", Detail: row.Reason, CreatedAt: row.InvalidatedAtText, Active: row.Active})
		}
	}
	if rows, ok := data["autobans"].([]autobanRow); ok {
		for _, row := range rows {
			if strings.EqualFold(strings.TrimSpace(row.Window), "401") || row.LastStatusCode == http.StatusUnauthorized {
				continue
			}
			alerts = append(alerts, dashboardAlert{ID: "autoban:" + firstNonEmptyString(row.AuthID, row.AuthIndex, row.Source), Severity: "warning", Type: "429", Scope: "account", Target: firstNonEmptyString(row.Source, row.AuthID, row.AuthIndex), Message: "账号 429 自动禁用中", Detail: "恢复时间 " + row.ResetAtText, CreatedAt: row.BannedAtText, Active: row.Active})
		}
	}
	if rows, ok := data["external_use_alerts"].([]externalUseAlert); ok {
		for _, row := range rows {
			alerts = append(alerts, dashboardAlert{ID: "external:" + firstNonEmptyString(row.AuthID, row.AuthIndex, row.Source) + ":" + row.Window, Severity: "critical", Type: "external_use", Scope: "account", Target: firstNonEmptyString(row.Source, row.AuthID, row.AuthIndex), Message: "疑似外部消耗", Detail: row.Window + " 增加 " + fmt.Sprintf("%.1f%%", row.DeltaPercent) + "，本地仅 " + strconv.FormatInt(row.LocalTokens, 10) + " tok", CreatedAt: row.DetectedAtText, Active: true})
		}
	}
	if providers, ok := data["providers"].([]providerRow); ok {
		for _, row := range providers {
			if row.Requests >= 5 && row.Failed*100/row.Requests >= 20 {
				alerts = append(alerts, dashboardAlert{ID: "provider-error:" + row.Provider, Severity: "warning", Type: "provider_error_rate", Scope: "provider", Target: row.Provider, Message: "Provider 错误率偏高", Detail: fmt.Sprintf("失败 %d / 请求 %d", row.Failed, row.Requests), CreatedAt: now, Active: true})
			}
		}
	}
	if diagnostics, ok := data["diagnostics"].(diagnosticsSummary); ok {
		if !diagnostics.ModelPrices.Exists || diagnostics.ModelPrices.Stale || diagnostics.ModelPrices.LoadedPrices == 0 || diagnostics.ModelPrices.LastError != "" {
			alerts = append(alerts, dashboardAlert{ID: "model-prices", Severity: "warning", Type: "model_prices", Scope: "system", Target: "model_prices.json", Message: "模型价格文件需要检查", Detail: firstNonEmptyString(diagnostics.ModelPrices.LastError, "文件缺失、过期或没有可用价格"), CreatedAt: now, Active: true})
		}
		if diagnostics.Database.UsageEvents > 0 && diagnostics.Database.LatestEventAgeSecs > 6*3600 {
			alerts = append(alerts, dashboardAlert{ID: "stale-usage", Severity: "info", Type: "stale_data", Scope: "system", Target: "usage_events", Message: "长时间没有新的 usage 事件", Detail: "最近事件 " + diagnostics.Database.LatestEventAt, CreatedAt: now, Active: true})
		}
		if len(diagnostics.Providers.UnmatchedConfigured) > 0 {
			alerts = append(alerts, dashboardAlert{ID: "provider-unmatched", Severity: "info", Type: "provider_config", Scope: "provider", Target: "CPA config", Message: "存在已配置但暂无流量的接入点", Detail: strings.Join(diagnostics.Providers.UnmatchedConfigured, " / "), CreatedAt: now, Active: true})
		}
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		return alertSeverityRank(alerts[i].Severity) > alertSeverityRank(alerts[j].Severity)
	})
	return alerts
}

func alertSeverityRank(value string) int {
	switch value {
	case "critical":
		return 4
	case "warning":
		return 3
	case "info":
		return 2
	default:
		return 1
	}
}

func handleExport(ctx context.Context, window, kind, format string, limit int) managementResponse {
	data, err := globalSummaryPrecomputer.summary(ctx, globalStore, window, limit)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "summary_failed", "message": err.Error()})
	}
	records, headers := exportRecords(data, kind)
	if strings.EqualFold(format, "json") {
		body, _ := json.MarshalIndent(records, "", "  ")
		return managementResponse{StatusCode: http.StatusOK, Headers: exportHeaders("application/json; charset=utf-8", kind+".json"), Body: body}
	}
	body, err := recordsToCSV(headers, records)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "export_failed", "message": err.Error()})
	}
	return managementResponse{StatusCode: http.StatusOK, Headers: exportHeaders("text/csv; charset=utf-8", kind+".csv"), Body: body}
}

func exportHeaders(contentType, name string) map[string][]string {
	return map[string][]string{
		"content-type":        {contentType},
		"cache-control":       {"no-store"},
		"content-disposition": {"attachment; filename=\"" + strings.ReplaceAll(name, `"`, "") + "\""},
	}
}

func exportRecords(data map[string]any, kind string) ([]map[string]string, []string) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "providers":
		return providerExportRows(anySlice[providerRow](data["providers"])), []string{"provider", "requests", "success_rate", "total_tokens", "cost_usd", "avg_latency_ms", "rate_limited", "last_seen"}
	case "models":
		rows := append(anySlice[modelRow](data["models"]), anySlice[modelRow](data["provider_models"])...)
		return modelExportRows(rows), []string{"provider", "model", "alias", "requests", "total_tokens", "cost_usd", "avg_latency_ms", "cache_rate"}
	case "recent":
		rows := append(anySlice[recentRow](data["recent"]), anySlice[recentRow](data["provider_recent"])...)
		return recentExportRows(rows), []string{"time", "provider", "account", "model", "alias", "status_code", "failed", "total_tokens", "input_tokens", "output_tokens", "cost_usd", "latency_ms"}
	default:
		return accountExportRows(anySlice[accountRow](data["accounts"])), []string{"account", "auth_index", "provider", "requests", "success_rate", "total_tokens", "cost_usd", "quota_total_estimate", "quota_remaining_estimate", "invalid_auth", "external_use_suspected", "last_seen"}
	}
}

func anySlice[T any](value any) []T {
	if rows, ok := value.([]T); ok {
		return rows
	}
	return nil
}

func accountExportRows(rows []accountRow) []map[string]string {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]string{
			"account":                  safeExportLabel(firstNonEmptyString(r.Email, r.Source, r.Name, r.AuthID, r.AuthFile, r.AuthIndex)),
			"auth_index":               r.AuthIndex,
			"provider":                 r.Provider,
			"requests":                 strconv.FormatInt(r.Requests, 10),
			"success_rate":             fmt.Sprintf("%.2f", successRateBackend(r.Requests, r.Failed)),
			"total_tokens":             strconv.FormatInt(r.TotalTokens, 10),
			"cost_usd":                 fmt.Sprintf("%.6f", r.CostUSD),
			"quota_total_estimate":     strconv.FormatInt(r.SecondaryQuotaTotalEstimate, 10),
			"quota_remaining_estimate": strconv.FormatInt(r.SecondaryQuotaRemainingEstimate, 10),
			"invalid_auth":             strconv.FormatBool(r.InvalidAuth),
			"external_use_suspected":   strconv.FormatBool(r.ExternalUseSuspected),
			"last_seen":                r.LastSeen,
		})
	}
	return out
}

func providerExportRows(rows []providerRow) []map[string]string {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]string{
			"provider":       r.Provider,
			"requests":       strconv.FormatInt(r.Requests, 10),
			"success_rate":   fmt.Sprintf("%.2f", successRateBackend(r.Requests, r.Failed)),
			"total_tokens":   strconv.FormatInt(r.TotalTokens, 10),
			"cost_usd":       fmt.Sprintf("%.6f", r.CostUSD),
			"avg_latency_ms": fmt.Sprintf("%.0f", r.AverageLatencyMs),
			"rate_limited":   strconv.FormatInt(r.RateLimited, 10),
			"last_seen":      r.LastSeen,
		})
	}
	return out
}

func modelExportRows(rows []modelRow) []map[string]string {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]string{
			"provider":       r.Provider,
			"model":          r.Model,
			"alias":          r.Alias,
			"requests":       strconv.FormatInt(r.Requests, 10),
			"total_tokens":   strconv.FormatInt(r.TotalTokens, 10),
			"cost_usd":       fmt.Sprintf("%.6f", r.CostUSD),
			"avg_latency_ms": fmt.Sprintf("%.0f", r.AverageLatencyMs),
			"cache_rate":     fmt.Sprintf("%.2f", cacheRateBackend(r.InputTokens, r.CachedTokens, r.CacheReadTokens)),
		})
	}
	return out
}

func recentExportRows(rows []recentRow) []map[string]string {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]string{
			"time":          r.Time,
			"provider":      r.Provider,
			"account":       safeExportLabel(firstNonEmptyString(r.AuthIndex, r.Source)),
			"model":         r.Model,
			"alias":         r.Alias,
			"status_code":   strconv.Itoa(r.StatusCode),
			"failed":        strconv.FormatBool(r.Failed),
			"total_tokens":  strconv.FormatInt(r.TotalTokens, 10),
			"input_tokens":  strconv.FormatInt(r.InputTokens, 10),
			"output_tokens": strconv.FormatInt(r.OutputTokens, 10),
			"cost_usd":      fmt.Sprintf("%.6f", r.CostUSD),
			"latency_ms":    strconv.FormatInt(r.LatencyMs, 10),
		})
	}
	return out
}

func recordsToCSV(headers []string, records []map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	writer := csv.NewWriter(&buf)
	if err := writer.Write(headers); err != nil {
		return nil, err
	}
	for _, record := range records {
		row := make([]string, len(headers))
		for i, header := range headers {
			row[i] = record[header]
		}
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return buf.Bytes(), writer.Error()
}

func safeExportLabel(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "bearer sk-") {
		plain := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
		if len(plain) <= 7 {
			return "sk-****"
		}
		return plain[:3] + "****" + plain[len(plain)-4:]
	}
	return value
}

func successRateBackend(requests, failed int64) float64 {
	if requests <= 0 {
		return 0
	}
	return float64(requests-failed) * 100 / float64(requests)
}

func cacheRateBackend(input, cached, cacheRead int64) float64 {
	cache := cached
	if cacheRead > cache {
		cache = cacheRead
	}
	if input <= 0 {
		return 0
	}
	return float64(cache) * 100 / float64(input)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

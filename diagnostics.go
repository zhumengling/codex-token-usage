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
	XAI                   int `json:"xai"`
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
	Database     databaseDiagnostics      `json:"database"`
	AuthFiles    authDiagnostics          `json:"auth_files"`
	CodexAuth    xaiAuthSourceDiagnostics `json:"codex_auth"`
	XAIAuth      xaiAuthSourceDiagnostics `json:"xai_auth"`
	Scheduler    schedulerDiagnostics     `json:"scheduler"`
	Providers    providerDiagnostics      `json:"providers"`
	ModelPrices  modelPriceDiagnostics    `json:"model_prices"`
	QuotaTrigger quotaTriggerState        `json:"quota_trigger"`
	Retention    retentionState           `json:"retention"`
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
	if _, err := pruneQuotaActivationState(ctx, db, triggerCutoff); err != nil {
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
		CodexAuth:    globalCodexAuthSource.status(),
		XAIAuth:      globalXAIAuthSource.status(),
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
	codexStatus := globalCodexAuthSource.status()
	out := authDiagnostics{
		Files:                len(files),
		Codex:                codexStatus.Accounts,
		XAI:                  globalXAIAuthSource.status().Accounts,
		Invalid401:           len(invalidAuths),
		Autoban429:           count429Autobans(autobans),
		ExternalUseSuspected: len(externalAlerts),
	}
	for _, file := range files {
		switch strings.ToLower(strings.TrimSpace(file.Provider)) {
		case "anthropic":
			out.Anthropic++
		case "antigravity":
			out.Antigravity++
		case "gemini":
			out.Gemini++
		}
		if !isCodexAuthProvider(file.Provider) && file.Disabled {
			out.Disabled++
		}
		if !isCodexAuthProvider(file.Provider) && file.Expired {
			out.Expired++
		}
	}
	for _, account := range accounts {
		if account.Configured && isCodexAuthProvider(account.Provider) && account.Disabled {
			out.Disabled++
		}
		if account.Configured && isCodexAuthProvider(account.Provider) && account.Expired {
			out.Expired++
		}
		if isCodexAuthProvider(account.Provider) && !account.Disabled && !account.Expired && !account.InvalidAuth && !account.ExternalUseSuspected {
			out.QuotaTriggerAvailable++
		}
	}
	return out
}

func count429Autobans(autobans []autobanRow) int {
	count := 0
	for _, ban := range autobans {
		window := strings.TrimSpace(ban.Window)
		if strings.EqualFold(window, "401") || strings.EqualFold(window, "402") || strings.EqualFold(window, "403") ||
			ban.LastStatusCode == http.StatusUnauthorized || ban.LastStatusCode == http.StatusPaymentRequired || ban.LastStatusCode == http.StatusForbidden {
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
			window := strings.TrimSpace(row.Window)
			if strings.EqualFold(window, "401") || strings.EqualFold(window, "402") || strings.EqualFold(window, "403") ||
				row.LastStatusCode == http.StatusUnauthorized || row.LastStatusCode == http.StatusPaymentRequired || row.LastStatusCode == http.StatusForbidden {
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
			alerts = append(alerts, dashboardAlert{ID: "model-prices", Severity: "warning", Type: "model_prices", Scope: "system", Target: modelPriceCacheFileName, Message: "模型价格缓存需要检查", Detail: firstNonEmptyString(diagnostics.ModelPrices.LastError, "文件缺失、过期或没有可用价格"), CreatedAt: now, Active: true})
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

type logExportFilter struct {
	Window   string
	Scope    string
	Provider string
	Account  string
	Model    string
	Date     string
	Status   string
	Limit    int
}

type logExportFilters = logExportFilter

func handleExport(ctx context.Context, window, kind, format string, limit int) managementResponse {
	return handleExportWithFilters(ctx, window, kind, format, limit, nil)
}

func handleExportWithFilters(ctx context.Context, window, kind, format string, limit int, query map[string][]string) managementResponse {
	if strings.EqualFold(strings.TrimSpace(kind), "logs") {
		filters := logExportFilter{
			Window:   window,
			Scope:    firstQuery(query, "scope", "codex"),
			Provider: firstQuery(query, "provider", ""),
			Account:  firstQuery(query, "account", ""),
			Model:    firstQuery(query, "model", ""),
			Date:     firstQuery(query, "date", ""),
			Status:   firstQuery(query, "status", "all"),
			Limit:    limit,
		}
		type logExportResult struct {
			records []map[string]string
			headers []string
		}
		result, err := withSQLiteAutoRepair(ctx, globalStore, "log export", func() (logExportResult, error) {
			db, _, err := globalStore.open(ctx)
			if err != nil {
				return logExportResult{}, err
			}
			records, headers, err := exportLogRecords(ctx, db, filters, defaultModelPrices())
			if err != nil {
				return logExportResult{}, err
			}
			return logExportResult{records: records, headers: headers}, nil
		})
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "export_failed", "message": err.Error()})
		}
		name := exportLogFileName(filters, format)
		if strings.EqualFold(format, "json") {
			body, _ := json.MarshalIndent(result.records, "", "  ")
			return managementResponse{StatusCode: http.StatusOK, Headers: exportHeaders("application/json; charset=utf-8", name), Body: body}
		}
		body, err := recordsToCSV(result.headers, result.records)
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "export_failed", "message": err.Error()})
		}
		return managementResponse{StatusCode: http.StatusOK, Headers: exportHeaders("text/csv; charset=utf-8", name), Body: body}
	}
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

func exportLogFileName(filters logExportFilter, format string) string {
	scope := strings.ToLower(strings.TrimSpace(filters.Scope))
	if scope == "" {
		scope = "codex"
	}
	if scope == "provider" && strings.TrimSpace(filters.Provider) != "" {
		scope = "provider-" + exportSafeFilePart(filters.Provider)
	}
	date := strings.TrimSpace(filters.Date)
	if date == "" {
		date = strings.ToLower(strings.TrimSpace(filters.Window))
	}
	if date == "" {
		date = "all"
	}
	ext := "csv"
	if strings.EqualFold(format, "json") {
		ext = "json"
	}
	return "codex-token-usage-logs-" + exportSafeFilePart(scope) + "-" + exportSafeFilePart(date) + "." + ext
}

func exportSafeFilePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "all"
	}
	return out
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

func exportLogRecords(ctx context.Context, db *sql.DB, filters logExportFilter, prices map[string]modelPrice) ([]map[string]string, []string, error) {
	if filters.Limit <= 0 {
		filters.Limit = 5000
	}
	if filters.Limit > 20000 {
		filters.Limit = 20000
	}
	where := []string{}
	args := []any{}
	if strings.TrimSpace(filters.Date) != "" {
		start, end, ok := localDateRange(filters.Date)
		if ok {
			where = append(where, "requested_at >= ?", "requested_at < ?")
			args = append(args, start, end)
		}
	} else {
		since, _ := windowStart(firstNonEmptyString(filters.Window, "24h"))
		where = append(where, "requested_at >= ?")
		args = append(args, since)
	}
	scope := strings.ToLower(strings.TrimSpace(filters.Scope))
	switch scope {
	case "providers", "provider":
		where = append(where, usageScopeSQL("other"))
	case "xai":
		where = append(where, usageScopeSQL("xai"))
	default:
		where = append(where, usageScopeSQL("codex"))
	}
	providerExpr := cpaProviderSQL()
	if provider := strings.TrimSpace(filters.Provider); provider != "" {
		where = append(where, providerExpr+" = ?")
		args = append(args, provider)
	}
	accountFilter := strings.TrimSpace(filters.Account)
	maskedAccountFilter := strings.Contains(accountFilter, "****")
	if account := normalizeAccountAlias(accountFilter); account != "" && !maskedAccountFilter {
		where = append(where, `(lower(api_key)=? OR lower(auth_id)=? OR lower(auth_index)=? OR lower(source)=?)`)
		args = append(args, account, account, account, account)
	}
	if model := normalizeAccountAlias(filters.Model); model != "" {
		where = append(where, `(lower(model)=? OR lower(alias)=?)`)
		args = append(args, model, model)
	}
	if status := strings.ToLower(strings.TrimSpace(filters.Status)); status != "" && status != "all" {
		switch status {
		case "success":
			where = append(where, `(failed=0 AND (status_code=0 OR (status_code >= 200 AND status_code < 300)))`)
		case "failed":
			where = append(where, `(failed=1 OR status_code >= 400)`)
		case "401", "402", "403", "429":
			code, _ := strconv.Atoi(status)
			where = append(where, `status_code=?`)
			args = append(args, code)
		case "5xx":
			where = append(where, `status_code >= 500`)
		}
	}
	query := `
SELECT requested_at,
CASE WHEN ` + usageScopeSQL("codex") + ` THEN 'codex' WHEN ` + usageScopeSQL("xai") + ` THEN 'xai' ELSE 'providers' END AS scope_key,
` + providerExpr + ` AS provider_key,
api_key, auth_id, auth_index, source, model, alias, reasoning_effort, service_tier,
latency_ms, ttft_ms, status_code, failed, input_tokens, output_tokens, reasoning_tokens,
cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens
FROM usage_events
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY requested_at DESC, id DESC
LIMIT ?`
	args = append(args, filters.Limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	headers := []string{
		"time", "scope", "provider", "account", "api_key", "auth_id", "auth_index", "source",
		"model", "alias", "status_code", "failed", "input_tokens", "output_tokens", "cached_tokens",
		"cache_read_tokens", "cache_creation_tokens", "reasoning_tokens", "total_tokens", "cost_usd",
		"latency_ms", "ttft_ms", "output_tokens_per_second", "error_summary",
	}
	out := []map[string]string{}
	for rows.Next() {
		var ts int64
		var scope, provider, apiKey, authID, authIndex, source, model, alias, reasoning, serviceTier string
		var latency, ttft, input, output, reasoningTokens, cached, cacheRead, cacheCreation, total int64
		var status, failedInt int
		if err := rows.Scan(
			&ts, &scope, &provider, &apiKey, &authID, &authIndex, &source, &model, &alias, &reasoning, &serviceTier,
			&latency, &ttft, &status, &failedInt, &input, &output, &reasoningTokens, &cached, &cacheRead, &cacheCreation, &total,
		); err != nil {
			return nil, nil, err
		}
		costRow := costTokenRow{
			Model:               model,
			Alias:               alias,
			Provider:            provider,
			ServiceTier:         serviceTier,
			InputTokens:         input,
			OutputTokens:        output,
			CachedTokens:        cached,
			CacheReadTokens:     cacheRead,
			CacheCreationTokens: cacheCreation,
			TotalTokens:         total,
		}
		cost := 0.0
		if value, ok := costForTokens(costRow, prices); ok {
			cost = value
		}
		throughput := ""
		if output > 0 {
			ms := latency
			if ttft > ms {
				ms = ttft
			}
			if ms >= 1000 {
				throughput = fmt.Sprintf("%.2f", float64(output)/(float64(ms)/1000.0))
			}
		}
		failed := failedInt != 0
		errorSummary := ""
		if failed || status >= 400 {
			if status == 0 {
				status = 599
			}
			errorSummary = "http " + strconv.Itoa(status)
		}
		account := safeExportLabel(firstNonEmptyString(apiKey, authIndex, authID, source))
		if maskedAccountFilter && account != accountFilter {
			continue
		}
		out = append(out, map[string]string{
			"time":                     unixTime(ts),
			"scope":                    scope,
			"provider":                 provider,
			"account":                  account,
			"api_key":                  safeExportLabel(apiKey),
			"auth_id":                  safeExportLabel(authID),
			"auth_index":               safeExportLabel(authIndex),
			"source":                   safeExportLabel(source),
			"model":                    model,
			"alias":                    alias,
			"status_code":              strconv.Itoa(status),
			"failed":                   strconv.FormatBool(failed),
			"input_tokens":             strconv.FormatInt(input, 10),
			"output_tokens":            strconv.FormatInt(output, 10),
			"cached_tokens":            strconv.FormatInt(cached, 10),
			"cache_read_tokens":        strconv.FormatInt(cacheRead, 10),
			"cache_creation_tokens":    strconv.FormatInt(cacheCreation, 10),
			"reasoning_tokens":         strconv.FormatInt(reasoningTokens, 10),
			"total_tokens":             strconv.FormatInt(total, 10),
			"cost_usd":                 fmt.Sprintf("%.6f", cost),
			"latency_ms":               strconv.FormatInt(latency, 10),
			"ttft_ms":                  strconv.FormatInt(ttft, 10),
			"output_tokens_per_second": throughput,
			"error_summary":            errorSummary,
		})
	}
	return out, headers, rows.Err()
}

func localDateRange(value string) (int64, int64, bool) {
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local)
	if err != nil {
		return 0, 0, false
	}
	return date.Unix(), date.Add(24 * time.Hour).Unix(), true
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
			"cache_rate":     fmt.Sprintf("%.2f", cacheRateBackend(r.Provider, r.Model, r.TotalTokens, r.InputTokens, r.OutputTokens, r.CachedTokens, r.CacheReadTokens, r.CacheCreationTokens)),
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

func cacheRateBackend(provider, model string, total, input, output, cached, cacheRead, cacheCreation int64) float64 {
	cache := normalizeCacheTokens(cached, cacheRead, cacheCreation)
	input = maxInt64(input, 0)
	if cacheTokensIncludedInInput(provider, model, total, input, output, cache) {
		input = maxInt64(input, cache.Total)
	} else {
		input += cache.Total
	}
	if input <= 0 {
		return 0
	}
	return float64(cache.Read) * 100 / float64(input)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

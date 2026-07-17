package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CPA_TOKEN_USAGE_DIR", dir)
	t.Setenv("CPA_AUTH_DIR", filepath.Join(dir, "auth"))
	s := &store{}
	t.Cleanup(s.close)
	return s
}

func TestSummaryCacheKeyCanonicalizesAndClamps(t *testing.T) {
	tests := []struct {
		in   summaryCacheKey
		want summaryCacheKey
	}{
		{in: summaryCacheKey{Window: "24h\\", Limit: 50}, want: summaryCacheKey{Window: "24h", Limit: 50}},
		{in: summaryCacheKey{Window: " TODAY ", Limit: 10}, want: summaryCacheKey{Window: "today", Limit: 10}},
		{in: summaryCacheKey{Window: "all", Limit: 9000}, want: summaryCacheKey{Window: "all", Limit: 5000}},
		{in: summaryCacheKey{}, want: summaryCacheKey{Window: "24h", Limit: 50}},
	}
	for _, test := range tests {
		if got := normalizeSummaryCacheKey(test.in); got != test.want {
			t.Fatalf("normalizeSummaryCacheKey(%+v)=%+v, want %+v", test.in, got, test.want)
		}
	}
}

func TestSummaryMemoryCacheIsBounded(t *testing.T) {
	m := &summaryPrecomputeManager{}
	now := time.Now()
	for i := 1; i <= summaryMemoryMaxEntries+20; i++ {
		m.rememberMemory(summaryCacheKey{Window: "24h", Limit: i}, summaryCacheEntry{
			data:     map[string]any{"limit": i},
			cachedAt: now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	if got := len(m.entries); got != summaryMemoryMaxEntries {
		t.Fatalf("memory cache entries=%d, want %d", got, summaryMemoryMaxEntries)
	}
}

func TestSummarySQLiteCacheIsCanonicalAndBounded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i := 1; i <= summaryStorageMaxEntries+20; i++ {
		if err := s.saveSummaryCacheEntry(ctx, summaryCacheKey{Window: "24h", Limit: i}, summaryCacheEntry{
			data:     map[string]any{"limit": i},
			cachedAt: time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	db, _, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM summary_cache`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != summaryStorageMaxEntries {
		t.Fatalf("SQLite cache entries=%d, want %d", count, summaryStorageMaxEntries)
	}
	if err := s.saveSummaryCacheEntry(ctx, summaryCacheKey{Window: "bad-window", Limit: 50}, summaryCacheEntry{
		data: map[string]any{"ok": true}, cachedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	var window string
	if err := db.QueryRow(`SELECT window FROM summary_cache WHERE cache_key='24h|50'`).Scan(&window); err != nil {
		t.Fatal(err)
	}
	if window != "24h" {
		t.Fatalf("stored window=%q, want canonical 24h", window)
	}
}

func TestSummarySyncRefreshesAfterUsageRevisionChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := normalizePluginConfig(defaultPluginConfig())
	cfg.SummaryPrecomputeMode = "active_dirty"

	m := &summaryPrecomputeManager{}
	data, err := m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatalf("first summary: %v", err)
	}
	if totals, ok := data["totals"].(totalsRow); !ok || totals.Requests != 0 {
		t.Fatalf("initial totals = %#v, want 0 requests", data["totals"])
	}
	if _, ok := data["store_revision"]; !ok {
		t.Fatalf("summary missing store_revision")
	}

	if err := s.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		Model:        "gpt-5.5",
		AuthID:       "alice@example.com",
		AuthIndex:    "alice.cpa.json",
		Source:       "alice@example.com",
		RequestedAt:  time.Now(),
		Detail:       usageDetail{InputTokens: 11, OutputTokens: 22, TotalTokens: 33},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	data, err = m.summarySync(ctx, s, "24h", 50)
	if err != nil {
		t.Fatalf("second summary: %v", err)
	}
	totals, ok := data["totals"].(totalsRow)
	if !ok {
		t.Fatalf("totals type = %T", data["totals"])
	}
	if totals.Requests != 1 || totals.TotalTokens != 33 {
		t.Fatalf("totals after usage = %+v, want one fresh request", totals)
	}
	if pre, ok := data["precompute"].(summaryPrecomputeInfo); ok && pre.Hit {
		t.Fatalf("summary reused stale cache after usage revision changed: %+v", pre)
	}
}

func TestSummaryReturnsStaleCacheWhileRevisionRefreshRuns(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := &summaryPrecomputeManager{}
	data, err := m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	if totals := data["totals"].(totalsRow); totals.Requests != 0 {
		t.Fatalf("initial requests = %d", totals.Requests)
	}
	if err := s.recordUsage(ctx, usageRecord{
		Provider: "codex", AuthID: "alice", AuthIndex: "alice", Source: "alice",
		RequestedAt: time.Now(), Detail: usageDetail{TotalTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: "24h", Limit: 50})
	m.mu.Lock()
	if m.refreshing == nil {
		m.refreshing = map[summaryCacheKey]bool{}
	}
	m.refreshing[key] = true
	m.mu.Unlock()
	data, err = m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	if totals := data["totals"].(totalsRow); totals.Requests != 0 {
		t.Fatalf("stale response requests = %d, want cached 0", totals.Requests)
	}
	pre, ok := data["precompute"].(summaryPrecomputeInfo)
	if !ok || !pre.Hit || !pre.Stale || pre.Synchronous || pre.Reason != "revision_stale" {
		t.Fatalf("precompute = %#v, want asynchronous revision-stale hit", data["precompute"])
	}
}

func TestSummaryAsyncRefreshIsThrottledWithinPrecomputeInterval(t *testing.T) {
	cfg := normalizePluginConfig(defaultPluginConfig())
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: "24h", Limit: 50})
	m := &summaryPrecomputeManager{
		entries: map[summaryCacheKey]summaryCacheEntry{
			key: {data: map[string]any{"ok": true}, cachedAt: time.Now(), revision: "old"},
		},
		refreshing: map[summaryCacheKey]bool{},
	}
	m.refreshAsyncThrottled(nil, cfg, key)
	m.mu.Lock()
	refreshing := m.refreshing[key]
	m.mu.Unlock()
	if refreshing {
		t.Fatal("recent cache entry unexpectedly started another asynchronous refresh")
	}
}

func TestSummaryMaintenanceSkipsWhenRevisionAndAuthFilesUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	globalStore = s
	t.Cleanup(func() { globalStore = &store{} })

	if err := s.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		Model:       "gpt-5.5",
		AuthID:      "alice@example.com",
		AuthIndex:   "alice.cpa.json",
		Source:      "alice@example.com",
		RequestedAt: time.Now(),
		Detail:      usageDetail{TotalTokens: 1},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	m := &summaryMaintenanceManager{}
	m.run(ctx)
	first := m.status()
	if first.SkippedReason != "" {
		t.Fatalf("first maintenance skipped unexpectedly: %+v", first)
	}
	m.run(ctx)
	second := m.status()
	if second.SkippedReason != "unchanged" {
		t.Fatalf("second maintenance skipped_reason = %q, want unchanged; state=%+v", second.SkippedReason, second)
	}
	if second.LastProcessedUsageEventID == 0 {
		t.Fatalf("maintenance did not record processed usage event id: %+v", second)
	}
}

func TestSummaryMaintenanceUsesLightModeAfterNewUsageWithoutAuthFileChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	globalStore = s
	t.Cleanup(func() { globalStore = &store{} })

	m := &summaryMaintenanceManager{}
	m.run(ctx)
	first := m.status()
	if first.LastMode != "full" {
		t.Fatalf("first maintenance mode = %q, want full; state=%+v", first.LastMode, first)
	}

	if err := s.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		Model:       "gpt-5.5",
		AuthID:      "alice@example.com",
		AuthIndex:   "alice.cpa.json",
		Source:      "alice@example.com",
		RequestedAt: time.Now(),
		Detail:      usageDetail{TotalTokens: 2},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	m.run(ctx)
	second := m.status()
	if second.LastMode != "light" {
		t.Fatalf("maintenance mode after new usage = %q, want light; state=%+v", second.LastMode, second)
	}
	if second.SkippedReason != "" {
		t.Fatalf("light maintenance should run, not skip: %+v", second)
	}
}

func TestParseLowUsageConfigDefaultsAndOverrides(t *testing.T) {
	cfg := normalizePluginConfig(defaultPluginConfig())
	if cfg.SummaryPrecomputeMode != "active_dirty" {
		t.Fatalf("default precompute mode = %q, want active_dirty", cfg.SummaryPrecomputeMode)
	}
	if cfg.SummaryCacheMaxAgeSeconds != 30 {
		t.Fatalf("default cache max age = %d, want 30", cfg.SummaryCacheMaxAgeSeconds)
	}
	if cfg.SummaryMaintenanceIntervalSeconds != 180 {
		t.Fatalf("default maintenance interval = %d, want 180", cfg.SummaryMaintenanceIntervalSeconds)
	}
	if cfg.SummaryPrecomputeActiveWindowTTLSeconds != 120 {
		t.Fatalf("default active window TTL = %d, want 120", cfg.SummaryPrecomputeActiveWindowTTLSeconds)
	}

	cfg = parsePluginConfigYAML([]byte(`
summary_precompute_mode: legacy
summary_cache_max_age_seconds: 9
summary_maintenance_interval_seconds: 240
summary_precompute_active_window_ttl_seconds: 900
`), cfg)
	cfg = normalizePluginConfig(cfg)
	if cfg.SummaryPrecomputeMode != "legacy" || cfg.SummaryCacheMaxAgeSeconds != 9 || cfg.SummaryMaintenanceIntervalSeconds != 240 || cfg.SummaryPrecomputeActiveWindowTTLSeconds != 900 {
		t.Fatalf("overridden config not applied: %+v", cfg)
	}
}

func TestQuotaTriggerWarningLabelAndConfigCompatibility(t *testing.T) {
	const warningName = "开启定时额度触发（不建议账号多的情况下开启）"
	found := false
	for _, field := range pluginConfigFields() {
		if field.Name == warningName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("plugin config fields do not contain %q", warningName)
	}
	for _, key := range []string{"quota_trigger_enabled", warningName, "开启定时额度触发"} {
		cfg := defaultPluginConfig()
		cfg.QuotaTriggerEnabled = false
		cfg = parsePluginConfigYAML([]byte(key+": true\n"), cfg)
		if !cfg.QuotaTriggerEnabled {
			t.Fatalf("quota trigger config key %q was not accepted", key)
		}
	}
}

func TestSessionAffinityConfigLabelAndCompatibility(t *testing.T) {
	const label = "同一个Session优先固定到同一个账号"
	fields := pluginConfigFields()
	index := -1
	for i, field := range fields {
		if field.Name == label {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("plugin config fields do not contain %q", label)
	}
	if index+1 >= len(fields) || fields[index+1].Name != "自动更新模型价格表" {
		t.Fatalf("session affinity field should be immediately above model price auto-update field, got %q", fields[index+1].Name)
	}
	if !defaultPluginConfig().SchedulerSessionAffinityEnabled {
		t.Fatal("session affinity should remain enabled by default for compatibility")
	}
	for _, key := range []string{"scheduler_session_affinity_enabled", "session_affinity_enabled", label} {
		cfg := defaultPluginConfig()
		cfg = parsePluginConfigYAML([]byte(key+": false\n"), cfg)
		if cfg.SchedulerSessionAffinityEnabled {
			t.Fatalf("session affinity config key %q was not accepted", key)
		}
	}
}

func TestAccountProtectionWarningLabelIsLastConfigGroupAndKeepsCompatibility(t *testing.T) {
	const warningName = "开启账号保护调度（可能会影响缓存）"
	wantLastGroup := []string{
		warningName,
		"Free 并发上限",
		"Plus 并发上限",
		"K12 并发上限",
		"Team 并发上限",
		"Pro 并发上限",
		"Free 5 分钟 Token 上限",
		"Plus 5 分钟 Token 上限",
		"K12 5 分钟 Token 上限",
		"Team 5 分钟 Token 上限",
		"Pro 5 分钟 Token 上限",
		"账号保护 Token 窗口秒数",
		"账号保护预约超时秒数",
	}
	fields := pluginConfigFields()
	if len(fields) < len(wantLastGroup) {
		t.Fatalf("plugin config fields = %d, want at least %d", len(fields), len(wantLastGroup))
	}
	lastGroup := fields[len(fields)-len(wantLastGroup):]
	for i, want := range wantLastGroup {
		if lastGroup[i].Name != want {
			t.Fatalf("last config group field %d = %q, want %q", i, lastGroup[i].Name, want)
		}
	}
	for _, key := range []string{"account_protection_enabled", warningName, "开启账号保护调度"} {
		cfg := defaultPluginConfig()
		cfg.AccountProtectionEnabled = false
		cfg = parsePluginConfigYAML([]byte(key+": true\n"), cfg)
		if !cfg.AccountProtectionEnabled {
			t.Fatalf("account protection config key %q was not accepted", key)
		}
	}
}

func TestConfiguredAuthFilesCacheInvalidatesOnFileChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", dir)
	path := filepath.Join(dir, "alice.json")
	if err := os.WriteFile(path, []byte(`{"provider":"codex","email":"alice@example.com","plan_type":"plus"}`), 0600); err != nil {
		t.Fatal(err)
	}
	first := readConfiguredAuthFiles()
	if len(first) != 1 || first[0].PlanType != "plus" {
		t.Fatalf("first read = %+v", first)
	}
	first[0].PlanType = "mutated"
	second := readConfiguredAuthFiles()
	if len(second) != 1 || second[0].PlanType != "plus" {
		t.Fatalf("cached clone was mutated: %+v", second)
	}
	if err := os.WriteFile(path, []byte(`{"provider":"codex","email":"alice@example.com","plan_type":"team","name":"changed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	third := readConfiguredAuthFiles()
	if len(third) != 1 || third[0].PlanType != "team" {
		t.Fatalf("cache did not invalidate: %+v", third)
	}
}

func TestExternalUseScanIsCappedAt24Hours(t *testing.T) {
	now := time.Now().Unix()
	if got := externalUseScanSince(0, now); got != now-int64((24*time.Hour)/time.Second) {
		t.Fatalf("all-window scan since = %d", got)
	}
	recent := now - int64(time.Hour/time.Second)
	if got := externalUseScanSince(recent, now); got != recent {
		t.Fatalf("recent scan since = %d, want %d", got, recent)
	}
}

func TestQueryHasXAIUsageUsesProviderIndexPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db, _, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := queryHasXAIUsage(ctx, db, 0); err != nil || found {
		t.Fatalf("empty xAI usage found=%v err=%v", found, err)
	}
	if err := s.recordUsage(ctx, usageRecord{
		Provider: "xai", AuthID: "grok", AuthIndex: "grok", Source: "grok",
		RequestedAt: time.Now(), Detail: usageDetail{TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if found, err := queryHasXAIUsage(ctx, db, 0); err != nil || !found {
		t.Fatalf("xAI usage found=%v err=%v", found, err)
	}
}

func TestInvalidAuthUsesEventTimeSoNewAuthFileClearsOld401(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	authDir := os.Getenv("CPA_AUTH_DIR")
	if err := os.MkdirAll(authDir, 0755); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	authFile := "alice.cpa.json"
	authPath := filepath.Join(authDir, authFile)
	oldFailureAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	newFileAt := oldFailureAt.Add(5 * time.Minute)
	if err := os.WriteFile(authPath, []byte(`{"email":"alice@example.com","provider":"codex","access_token":"token"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	if err := os.Chtimes(authPath, newFileAt, newFileAt); err != nil {
		t.Fatalf("chtimes auth file: %v", err)
	}
	db, _, err := s.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	err = recordInvalidAuthIfNeeded(ctx, db, usageRecord{
		Provider:    "codex",
		AuthID:      "alice@example.com",
		AuthIndex:   authFile,
		AuthFile:    authFile,
		Source:      "alice@example.com",
		RequestedAt: oldFailureAt,
		Failed:      true,
		Failure:     usageFailure{StatusCode: 401},
	}, 401)
	if err != nil {
		t.Fatalf("record invalid auth: %v", err)
	}
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		t.Fatalf("query invalids: %v", err)
	}
	if len(invalids) != 1 {
		t.Fatalf("active invalids after record = %d, want 1", len(invalids))
	}
	if invalids[0].InvalidatedAt != oldFailureAt.Unix() {
		t.Fatalf("invalidated_at = %d, want event time %d", invalids[0].InvalidatedAt, oldFailureAt.Unix())
	}
	if err := clearReplacedInvalidAuths(ctx, db); err != nil {
		t.Fatalf("clear replaced invalid auths: %v", err)
	}
	invalids, err = queryActiveInvalidAuths(ctx, db)
	if err != nil {
		t.Fatalf("query invalids after clear: %v", err)
	}
	if len(invalids) != 0 {
		t.Fatalf("old 401 remained active after newer auth file: %+v", invalids)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

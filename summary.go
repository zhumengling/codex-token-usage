package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type summaryCacheKey struct {
	Window string
	Limit  int
}

type summaryPrecomputeInfo struct {
	Enabled      bool   `json:"enabled"`
	Hit          bool   `json:"hit"`
	Window       string `json:"window"`
	Limit        int    `json:"limit"`
	CachedAt     string `json:"cached_at,omitempty"`
	AgeSeconds   int64  `json:"age_seconds,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	IntervalSecs int    `json:"interval_seconds"`
	LastError    string `json:"last_error,omitempty"`
	Precomputed  bool   `json:"precomputed"`
	Synchronous  bool   `json:"synchronous"`
	Stale        bool   `json:"stale,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type summaryCacheEntry struct {
	data       map[string]any
	cachedAt   time.Time
	durationMs int64
	err        string
	revision   string
}

type storeRevision struct {
	Revision          string
	UsageMaxID        int64
	QuotaMaxID        int64
	InvalidActive     int64
	InvalidMaxChanged int64
	BanActive         int64
	BanMaxChanged     int64
	NextBanResetAt    int64
	XAIStateActive    int64
	XAIStateChanged   int64
	NextXAIResetAt    int64
	AuthFilesRevision string
}

type summaryPrecomputeManager struct {
	mu         sync.Mutex
	refreshMu  sync.Mutex
	cfg        pluginConfig
	cancel     context.CancelFunc
	entries    map[summaryCacheKey]summaryCacheEntry
	refreshing map[summaryCacheKey]bool
	active     map[summaryCacheKey]time.Time
}

func (m *summaryPrecomputeManager) configure(cfg pluginConfig) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.cfg = cfg
	if m.entries == nil {
		m.entries = map[summaryCacheKey]summaryCacheEntry{}
	}
	if m.refreshing == nil {
		m.refreshing = map[summaryCacheKey]bool{}
	}
	if m.active == nil {
		m.active = map[summaryCacheKey]time.Time{}
	}
	if !cfg.SummaryPrecomputeEnabled {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()
	go m.loop(ctx, cfg)
}

func (m *summaryPrecomputeManager) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
}

func (m *summaryPrecomputeManager) loop(ctx context.Context, cfg pluginConfig) {
	if cfg.SummaryPrecomputeMode == "legacy" {
		_ = m.refresh(ctx, globalStore, cfg, defaultSummaryPrecomputeKeys())
	}
	ticker := time.NewTicker(time.Duration(cfg.SummaryPrecomputeIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cfg.SummaryPrecomputeMode == "legacy" {
				_ = m.refresh(ctx, globalStore, cfg, defaultSummaryPrecomputeKeys())
			} else {
				_ = m.refresh(ctx, globalStore, cfg, m.activeKeys(cfg))
			}
		}
	}
}

func defaultSummaryPrecomputeKeys() []summaryCacheKey {
	return []summaryCacheKey{
		{Window: "today", Limit: 2000},
		{Window: "24h", Limit: 2000},
		{Window: "7d", Limit: 2000},
		{Window: "30d", Limit: 2000},
		{Window: "all", Limit: 2000},
		{Window: "24h", Limit: 100},
		{Window: "24h", Limit: 500},
		{Window: "all", Limit: 500},
	}
}

func (m *summaryPrecomputeManager) refresh(ctx context.Context, store *store, cfg pluginConfig, keys []summaryCacheKey) error {
	if !cfg.SummaryPrecomputeEnabled {
		return nil
	}
	if len(keys) == 0 {
		return nil
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	revision, err := store.currentRevision(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, key := range keys {
		key = normalizeSummaryCacheKey(key)
		if cfg.SummaryPrecomputeMode != "legacy" && m.cacheRevisionMatches(key, revision.Revision) {
			continue
		}
		started := time.Now()
		data, err := store.summary(ctx, key.Window, key.Limit)
		durationMs := time.Since(started).Milliseconds()
		entry := summaryCacheEntry{
			data:       data,
			cachedAt:   time.Now(),
			durationMs: durationMs,
			revision:   revision.Revision,
		}
		if err != nil {
			entry.err = sanitizeTriggerError(err)
			if firstErr == nil {
				firstErr = err
			}
			var entryToSave summaryCacheEntry
			m.mu.Lock()
			if m.entries == nil {
				m.entries = map[summaryCacheKey]summaryCacheEntry{}
			}
			if previous, ok := m.entries[key]; ok && previous.data != nil {
				previous.err = entry.err
				m.entries[key] = previous
				entryToSave = previous
			} else {
				m.entries[key] = entry
			}
			m.mu.Unlock()
			if entryToSave.data != nil && store != nil {
				_ = store.saveSummaryCacheEntry(ctx, key, entryToSave)
			}
			continue
		}
		m.remember(ctx, store, key, entry)
	}
	return firstErr
}

func (m *summaryPrecomputeManager) markActive(key summaryCacheKey) {
	key = normalizeSummaryCacheKey(key)
	m.mu.Lock()
	if m.active == nil {
		m.active = map[summaryCacheKey]time.Time{}
	}
	m.active[key] = time.Now()
	m.mu.Unlock()
}

func (m *summaryPrecomputeManager) activeKeys(cfg pluginConfig) []summaryCacheKey {
	ttl := time.Duration(maxInt(cfg.SummaryPrecomputeActiveWindowTTLSeconds, 30)) * time.Second
	cutoff := time.Now().Add(-ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) == 0 {
		return nil
	}
	keys := make([]summaryCacheKey, 0, len(m.active))
	for key, lastSeen := range m.active {
		if lastSeen.Before(cutoff) {
			delete(m.active, key)
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Window == keys[j].Window {
			return keys[i].Limit < keys[j].Limit
		}
		return keys[i].Window < keys[j].Window
	})
	return keys
}

func (m *summaryPrecomputeManager) cacheRevisionMatches(key summaryCacheKey, revision string) bool {
	key = normalizeSummaryCacheKey(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	return ok && entry.data != nil && entry.revision == revision && time.Since(entry.cachedAt) <= summaryCacheTTL(m.configLocked())
}

func (m *summaryPrecomputeManager) summary(ctx context.Context, store *store, window string, limit int) (map[string]any, error) {
	requestStarted := time.Now()
	cfg := m.config()
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: window, Limit: limit})
	m.markActive(key)
	if !cfg.SummaryPrecomputeEnabled {
		data, err := store.summary(ctx, window, limit)
		if err == nil {
			data = cloneSummaryMap(data)
			data["precompute"] = summaryPrecomputeInfo{Enabled: false, Window: window, Limit: limit, Reason: "disabled"}
			attachSummaryRuntimeInfo(data, time.Since(requestStarted).Milliseconds())
		}
		return data, err
	}
	revision, err := store.currentRevision(ctx)
	if err != nil {
		return nil, err
	}
	if data, ok := m.cached(ctx, store, key, cfg, revision.Revision); ok {
		if summaryPrecomputeStale(data) {
			m.refreshAsyncThrottled(store, cfg, key)
		}
		attachSummaryRuntimeInfo(data, time.Since(requestStarted).Milliseconds())
		return data, nil
	}
	if data, ok := m.cachedAny(ctx, store, key, cfg, revision.Revision); ok {
		m.refreshAsyncThrottled(store, cfg, key)
		attachSummaryRuntimeInfo(data, time.Since(requestStarted).Milliseconds())
		return data, nil
	}
	started := time.Now()
	data, err := store.summary(ctx, key.Window, key.Limit)
	durationMs := time.Since(started).Milliseconds()
	if err != nil {
		return nil, err
	}
	entry := summaryCacheEntry{data: data, cachedAt: time.Now(), durationMs: durationMs, revision: revision.Revision}
	m.remember(ctx, store, key, entry)
	out := cloneSummaryMap(data)
	out["precompute"] = summaryPrecomputeInfo{
		Enabled:      true,
		Hit:          false,
		Window:       key.Window,
		Limit:        key.Limit,
		CachedAt:     time.Now().Format(time.RFC3339),
		DurationMs:   durationMs,
		IntervalSecs: cfg.SummaryPrecomputeIntervalSeconds,
		Precomputed:  false,
		Synchronous:  true,
		Reason:       "revision_miss",
	}
	attachSummaryRevision(out, revision.Revision, entry.revision)
	attachSummaryRuntimeInfo(out, time.Since(requestStarted).Milliseconds())
	return out, nil
}

func (m *summaryPrecomputeManager) summaryFresh(ctx context.Context, store *store, window string, limit int) (map[string]any, error) {
	requestStarted := time.Now()
	cfg := m.config()
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: window, Limit: limit})
	m.markActive(key)
	if cfg.SummaryPrecomputeEnabled {
		revision, err := store.currentRevision(ctx)
		if err != nil {
			return nil, err
		}
		if data, ok := m.cachedAny(ctx, store, key, cfg, revision.Revision); ok {
			m.refreshAsync(store, cfg, key)
			attachSummaryRuntimeInfo(data, time.Since(requestStarted).Milliseconds())
			return data, nil
		}
	}
	return m.summarySyncWithStarted(ctx, store, cfg, key, requestStarted)
}

func (m *summaryPrecomputeManager) summarySync(ctx context.Context, store *store, window string, limit int) (map[string]any, error) {
	cfg := m.config()
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: window, Limit: limit})
	m.markActive(key)
	return m.summarySyncWithStarted(ctx, store, cfg, key, time.Now())
}

func (m *summaryPrecomputeManager) summarySyncWithStarted(ctx context.Context, store *store, cfg pluginConfig, key summaryCacheKey, requestStarted time.Time) (map[string]any, error) {
	key = normalizeSummaryCacheKey(key)
	started := time.Now()
	data, err := store.summary(ctx, key.Window, key.Limit)
	durationMs := time.Since(started).Milliseconds()
	if err != nil {
		return nil, err
	}
	revision, revErr := store.currentRevision(ctx)
	if revErr != nil {
		return nil, revErr
	}
	entry := summaryCacheEntry{data: data, cachedAt: time.Now(), durationMs: durationMs, revision: revision.Revision}
	m.remember(ctx, store, key, entry)
	out := cloneSummaryMap(data)
	out["precompute"] = summaryPrecomputeInfo{
		Enabled:      cfg.SummaryPrecomputeEnabled,
		Hit:          false,
		Window:       key.Window,
		Limit:        key.Limit,
		CachedAt:     time.Now().Format(time.RFC3339),
		DurationMs:   durationMs,
		IntervalSecs: cfg.SummaryPrecomputeIntervalSeconds,
		Precomputed:  false,
		Synchronous:  true,
		Reason:       "sync_refresh",
	}
	attachSummaryRevision(out, revision.Revision, entry.revision)
	attachSummaryRuntimeInfo(out, time.Since(requestStarted).Milliseconds())
	return out, nil
}

func (m *summaryPrecomputeManager) refreshAsync(store *store, cfg pluginConfig, key summaryCacheKey) {
	m.refreshAsyncMode(store, cfg, key, false)
}

func (m *summaryPrecomputeManager) refreshAsyncThrottled(store *store, cfg pluginConfig, key summaryCacheKey) {
	m.refreshAsyncMode(store, cfg, key, true)
}

func (m *summaryPrecomputeManager) refreshAsyncMode(store *store, cfg pluginConfig, key summaryCacheKey, throttle bool) {
	if !cfg.SummaryPrecomputeEnabled {
		return
	}
	key = normalizeSummaryCacheKey(key)
	m.mu.Lock()
	if m.refreshing == nil {
		m.refreshing = map[summaryCacheKey]bool{}
	}
	if m.refreshing[key] {
		m.mu.Unlock()
		return
	}
	if throttle {
		if entry, ok := m.entries[key]; ok && entry.data != nil {
			minimumAge := time.Duration(maxInt(cfg.SummaryPrecomputeIntervalSeconds, 1)) * time.Second
			if time.Since(entry.cachedAt) < minimumAge {
				m.mu.Unlock()
				return
			}
		}
	}
	m.refreshing[key] = true
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = m.refresh(ctx, store, cfg, []summaryCacheKey{key})
		m.mu.Lock()
		delete(m.refreshing, key)
		m.mu.Unlock()
	}()
}

func (m *summaryPrecomputeManager) cached(ctx context.Context, store *store, key summaryCacheKey, cfg pluginConfig, revision string) (map[string]any, bool) {
	key = normalizeSummaryCacheKey(key)
	if data, ok := m.cachedEntry(key, cfg, revision); ok {
		return data, true
	}
	if store == nil {
		return nil, false
	}
	entry, ok, err := store.loadSummaryCacheEntry(ctx, key)
	if err != nil || !ok || entry.revision != revision {
		return nil, false
	}
	m.rememberMemory(key, entry)
	age := time.Since(entry.cachedAt)
	return cloneCachedSummary(entry, key, cfg, age), true
}

func (m *summaryPrecomputeManager) cachedAny(ctx context.Context, store *store, key summaryCacheKey, cfg pluginConfig, revision string) (map[string]any, bool) {
	key = normalizeSummaryCacheKey(key)
	if data, ok := m.cachedAnyEntry(key, cfg, revision); ok {
		return data, true
	}
	if store == nil {
		return nil, false
	}
	entry, ok, err := store.loadSummaryCacheEntry(ctx, key)
	if err != nil || !ok {
		return nil, false
	}
	age := time.Since(entry.cachedAt)
	if age > summaryStaleMaxAge(cfg) {
		return nil, false
	}
	m.rememberMemory(key, entry)
	return cloneCachedSummaryForRevision(entry, key, cfg, age, revision), true
}

func (m *summaryPrecomputeManager) cachedAnyEntry(key summaryCacheKey, cfg pluginConfig, revision string) (map[string]any, bool) {
	key = normalizeSummaryCacheKey(key)
	m.mu.Lock()
	entry, ok := m.entries[key]
	m.mu.Unlock()
	if !ok || entry.data == nil {
		return nil, false
	}
	age := time.Since(entry.cachedAt)
	if age > summaryStaleMaxAge(cfg) {
		return nil, false
	}
	return cloneCachedSummaryForRevision(entry, key, cfg, age, revision), true
}

func (m *summaryPrecomputeManager) cachedEntry(key summaryCacheKey, cfg pluginConfig, revision string) (map[string]any, bool) {
	m.mu.Lock()
	entry, ok := m.entries[key]
	m.mu.Unlock()
	if !ok || entry.data == nil || entry.revision != revision {
		return nil, false
	}
	age := time.Since(entry.cachedAt)
	return cloneCachedSummary(entry, key, cfg, age), true
}

func (m *summaryPrecomputeManager) remember(ctx context.Context, store *store, key summaryCacheKey, entry summaryCacheEntry) {
	m.rememberMemory(key, entry)
	if store != nil && entry.data != nil {
		_ = store.saveSummaryCacheEntry(ctx, key, entry)
	}
}

func (m *summaryPrecomputeManager) rememberMemory(key summaryCacheKey, entry summaryCacheEntry) {
	key = normalizeSummaryCacheKey(key)
	if entry.data == nil {
		return
	}
	if entry.cachedAt.IsZero() {
		entry.cachedAt = time.Now()
	}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = map[summaryCacheKey]summaryCacheEntry{}
	}
	m.entries[key] = entry
	m.mu.Unlock()
}

func summaryCacheTTL(cfg pluginConfig) time.Duration {
	return time.Duration(maxInt(cfg.SummaryCacheMaxAgeSeconds, 1)) * time.Second
}

func summaryStaleMaxAge(cfg pluginConfig) time.Duration {
	return time.Duration(maxInt(cfg.SummaryCacheMaxAgeSeconds, cfg.SummaryPrecomputeIntervalSeconds*2)) * time.Second
}

type summaryCacheLoadResult struct {
	entry summaryCacheEntry
	ok    bool
}

func summaryCacheStorageKey(key summaryCacheKey) string {
	key = normalizeSummaryCacheKey(key)
	return key.Window + "|" + strconv.Itoa(key.Limit)
}

func (s *store) loadSummaryCacheEntry(ctx context.Context, key summaryCacheKey) (summaryCacheEntry, bool, error) {
	key = normalizeSummaryCacheKey(key)
	result, err := withSQLiteAutoRepair(ctx, s, "load summary cache", func() (summaryCacheLoadResult, error) {
		db, _, err := s.open(ctx)
		if err != nil {
			return summaryCacheLoadResult{}, err
		}
		var raw string
		var cachedAt int64
		var durationMs int64
		var lastError string
		var revision string
		err = db.QueryRowContext(ctx, `
SELECT data_json, cached_at, duration_ms, last_error, revision
FROM summary_cache
WHERE cache_key=?`, summaryCacheStorageKey(key)).Scan(&raw, &cachedAt, &durationMs, &lastError, &revision)
		if err == sql.ErrNoRows {
			return summaryCacheLoadResult{}, nil
		}
		if err != nil {
			return summaryCacheLoadResult{}, err
		}
		if strings.TrimSpace(raw) == "" || cachedAt <= 0 {
			return summaryCacheLoadResult{}, nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return summaryCacheLoadResult{}, err
		}
		return summaryCacheLoadResult{
			entry: summaryCacheEntry{
				data:       data,
				cachedAt:   time.Unix(cachedAt, 0),
				durationMs: durationMs,
				err:        lastError,
				revision:   revision,
			},
			ok: true,
		}, nil
	})
	if err != nil {
		return summaryCacheEntry{}, false, err
	}
	return result.entry, result.ok, nil
}

func (s *store) saveSummaryCacheEntry(ctx context.Context, key summaryCacheKey, entry summaryCacheEntry) error {
	if s == nil || entry.data == nil {
		return nil
	}
	key = normalizeSummaryCacheKey(key)
	if entry.cachedAt.IsZero() {
		entry.cachedAt = time.Now()
	}
	payload, err := json.Marshal(entry.data)
	if err != nil {
		return err
	}
	_, err = withSQLiteAutoRepair(ctx, s, "save summary cache", func() (struct{}, error) {
		db, _, err := s.open(ctx)
		if err != nil {
			return struct{}{}, err
		}
		_, err = db.ExecContext(ctx, `
INSERT INTO summary_cache (cache_key, window, limit_count, cached_at, duration_ms, revision, last_error, data_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET
  window=excluded.window,
  limit_count=excluded.limit_count,
  cached_at=excluded.cached_at,
  duration_ms=excluded.duration_ms,
  revision=excluded.revision,
  last_error=excluded.last_error,
  data_json=excluded.data_json`,
			summaryCacheStorageKey(key), key.Window, key.Limit, entry.cachedAt.Unix(), entry.durationMs, entry.revision, entry.err, string(payload),
		)
		return struct{}{}, err
	})
	return err
}

func (m *summaryPrecomputeManager) config() pluginConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.SummaryPrecomputeIntervalSeconds <= 0 {
		return normalizePluginConfig(defaultPluginConfig())
	}
	return m.cfg
}

func (m *summaryPrecomputeManager) configLocked() pluginConfig {
	if m.cfg.SummaryPrecomputeIntervalSeconds <= 0 {
		return normalizePluginConfig(defaultPluginConfig())
	}
	return m.cfg
}

func normalizeSummaryCacheKey(key summaryCacheKey) summaryCacheKey {
	key.Window = strings.ToLower(strings.TrimSpace(key.Window))
	if key.Window == "" {
		key.Window = "24h"
	}
	if key.Limit <= 0 {
		key.Limit = 50
	}
	return key
}

func cloneSummaryMap(data map[string]any) map[string]any {
	out := make(map[string]any, len(data)+8)
	for key, value := range data {
		out[key] = value
	}
	return out
}

func cloneCachedSummary(entry summaryCacheEntry, key summaryCacheKey, cfg pluginConfig, age time.Duration) map[string]any {
	return cloneCachedSummaryForRevision(entry, key, cfg, age, entry.revision)
}

func cloneCachedSummaryForRevision(entry summaryCacheEntry, key summaryCacheKey, cfg pluginConfig, age time.Duration, storeRevision string) map[string]any {
	stale := entry.revision != storeRevision || age > summaryCacheTTL(cfg)
	reason := "revision_hit"
	if entry.revision != storeRevision {
		reason = "revision_stale"
	} else if stale {
		reason = "age_stale"
	}
	out := cloneSummaryMap(entry.data)
	out["precompute"] = summaryPrecomputeInfo{
		Enabled:      true,
		Hit:          true,
		Window:       key.Window,
		Limit:        key.Limit,
		CachedAt:     entry.cachedAt.Format(time.RFC3339),
		AgeSeconds:   int64(age.Seconds()),
		DurationMs:   entry.durationMs,
		IntervalSecs: cfg.SummaryPrecomputeIntervalSeconds,
		LastError:    entry.err,
		Precomputed:  true,
		Synchronous:  false,
		Stale:        stale,
		Reason:       reason,
	}
	attachSummaryRevision(out, storeRevision, entry.revision)
	return out
}

func summaryPrecomputeStale(data map[string]any) bool {
	info, ok := data["precompute"].(summaryPrecomputeInfo)
	return ok && info.Stale
}

func (s *store) currentRevision(ctx context.Context) (storeRevision, error) {
	if s == nil {
		return storeRevision{Revision: "empty"}, nil
	}
	db, _, err := s.open(ctx)
	if err != nil {
		return storeRevision{}, err
	}
	var r storeRevision
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&r.UsageMaxID); err != nil {
		return storeRevision{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM quota_trigger_runs`).Scan(&r.QuotaMaxID); err != nil {
		return storeRevision{}, err
	}
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MAX(invalidated_at),0)
FROM invalid_auths
WHERE active=1`).Scan(&r.InvalidActive, &r.InvalidMaxChanged); err != nil {
		return storeRevision{}, err
	}
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MAX(observed_at),0), COALESCE(MIN(CASE WHEN active=1 AND reset_at>0 THEN reset_at END),0)
FROM xai_account_states
WHERE active=1`).Scan(&r.XAIStateActive, &r.XAIStateChanged, &r.NextXAIResetAt); err != nil {
		return storeRevision{}, err
	}
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*),
  COALESCE(MAX(CASE WHEN released_at > banned_at THEN released_at ELSE banned_at END),0),
  COALESCE(MIN(CASE WHEN active=1 THEN reset_at END),0)
FROM autoban_bans
WHERE active=1 OR released_at > 0`).Scan(&r.BanActive, &r.BanMaxChanged, &r.NextBanResetAt); err != nil {
		return storeRevision{}, err
	}
	r.AuthFilesRevision = authFilesRevision()
	r.Revision = strings.Join([]string{
		"u:" + strconv.FormatInt(r.UsageMaxID, 10),
		"q:" + strconv.FormatInt(r.QuotaMaxID, 10),
		"i:" + strconv.FormatInt(r.InvalidActive, 10) + ":" + strconv.FormatInt(r.InvalidMaxChanged, 10),
		"b:" + strconv.FormatInt(r.BanActive, 10) + ":" + strconv.FormatInt(r.BanMaxChanged, 10) + ":" + strconv.FormatInt(r.NextBanResetAt, 10),
		"x:" + strconv.FormatInt(r.XAIStateActive, 10) + ":" + strconv.FormatInt(r.XAIStateChanged, 10) + ":" + strconv.FormatInt(r.NextXAIResetAt, 10),
		"a:" + r.AuthFilesRevision,
	}, "|")
	return r, nil
}

func authFilesRevision() string {
	_ = readConfiguredAuthAccounts()
	if status := globalCodexAuthSource.status(); status.Authoritative && status.Source == "host_callback" {
		return "host:" + globalCodexAuthSource.currentRevision()
	}
	authDir := configuredAuthDir()
	if authDir == "" {
		return "none"
	}
	_, revision, err := configuredAuthDirectorySnapshot(authDir)
	if err != nil {
		return "unreadable"
	}
	return revision
}

func attachSummaryRuntimeInfo(data map[string]any, durationMs int64) {
	if data == nil {
		return
	}
	if durationMs >= 0 {
		data["duration_ms"] = durationMs
	}
	maintenance := globalSummaryMaintenance.status()
	dbHealth := globalDBHealth.status()
	data["maintenance_last_run_at"] = maintenance.LastRunAt
	data["maintenance"] = maintenance
	data["db_health_last_check_at"] = dbHealth.LastCheckAt
	data["db_health_status"] = firstNonEmptyString(dbHealth.Status, "unknown")
}

func attachSummaryRevision(data map[string]any, storeRevision string, cacheRevision string) {
	if data == nil {
		return
	}
	data["store_revision"] = storeRevision
	data["cache_revision"] = cacheRevision
}

func (s *store) summary(ctx context.Context, window string, limit int) (map[string]any, error) {
	started := time.Now()
	data, err := withSQLiteAutoRepair(ctx, s, "summary", func() (map[string]any, error) {
		return s.summaryOnce(ctx, window, limit)
	})
	if err == nil {
		attachSummaryRuntimeInfo(data, time.Since(started).Milliseconds())
	}
	return data, err
}

func (s *store) summaryOnce(ctx context.Context, window string, limit int) (map[string]any, error) {
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
	configuredXAIAccounts := readConfiguredXAIAccounts()
	xaiHasUsage, err := queryHasXAIUsage(ctx, db, since)
	if err != nil {
		return nil, err
	}
	var xaiTotals totalsRow
	if xaiHasUsage {
		xaiTotals, err = queryOneTotals(ctx, db, since, "xai")
		if err != nil {
			return nil, err
		}
		if err := applyCosts(ctx, db, since, &xaiTotals, prices, "xai"); err != nil {
			return nil, err
		}
	}
	accounts, err := queryAccounts(ctx, db, since, limit, "codex")
	if err != nil {
		return nil, err
	}
	if err := applyAccountCosts(ctx, db, since, accounts, prices); err != nil {
		return nil, err
	}
	configuredAccounts := readConfiguredAuthAccounts()
	authDirReadable := globalCodexAuthSource.authoritative()
	hostAuthInventory, hostAuthInventoryErr := readCodexHostAuthInventory()
	hostAuthInventoryAuthoritative := hostAuthInventoryErr == nil
	accounts = mergeConfiguredAccounts(accounts, configuredAccounts)
	accounts = filterCurrentConfiguredAccounts(accounts, configuredAccounts, authDirReadable)
	if globalAccountProtection.enabled() {
		applyAccountProtectionState(ctx, db, accounts)
	}
	var xaiAccounts []accountRow
	if xaiHasUsage {
		xaiAccounts, err = queryAccounts(ctx, db, since, limit, "xai")
		if err != nil {
			return nil, err
		}
		if err := applyScopedAccountCosts(ctx, db, since, xaiAccounts, prices, "xai"); err != nil {
			return nil, err
		}
	}
	xaiAccounts = mergeConfiguredAccounts(xaiAccounts, configuredXAIAccounts)
	xaiAccounts = filterCurrentConfiguredAccounts(xaiAccounts, configuredXAIAccounts, globalXAIAuthSource.authoritative())
	xaiStates, err := queryActiveXAIStates(ctx, db, now)
	if err != nil {
		return nil, err
	}
	xaiStates = filterMissingXAIStateRows(xaiStates, configuredXAIAccounts, globalXAIAuthSource.authoritative())
	applyXAIStates(xaiAccounts, xaiStates)
	quotaSince := time.Now().Add(-35 * 24 * time.Hour).Unix()
	applyLatestQuotaSnapshots(ctx, db, accounts, quotaSince)
	applySecondaryQuotaEstimates(ctx, db, accounts, &totals, quotaSince)
	invalidAuths, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return nil, err
	}
	invalidAuths = classifyInvalidAuthRows(invalidAuths, hostAuthInventory)
	invalidAuths = filterMissingInvalidAuthRows(invalidAuths, hostAuthInventory, hostAuthInventoryAuthoritative)
	applyInvalidAuths(accounts, invalidAuths)
	workspaceDeactivatedAuths := filterWorkspaceDeactivatedAuths(invalidAuths)
	unauthorizedInvalidAuths := filterUnauthorizedInvalidAuths(invalidAuths)
	forbiddenInvalidAuths := filterForbiddenInvalidAuths(invalidAuths)
	externalUseAlerts, err := queryExternalUseAlerts(ctx, db, externalUseScanSince(since, now))
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
	keySummaries, err := queryKeySummaries(ctx, db, since, limit)
	if err != nil {
		return nil, err
	}
	if err := applyKeySummaryCosts(ctx, db, since, keySummaries, prices); err != nil {
		return nil, err
	}
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
	var xaiModels []modelRow
	if xaiHasUsage {
		xaiModels, err = queryModels(ctx, db, since, limit, "xai")
		if err != nil {
			return nil, err
		}
		if err := applyModelCosts(ctx, db, since, xaiModels, prices, "xai"); err != nil {
			return nil, err
		}
	}
	trend, err := queryTrend(ctx, db, since, label, "codex")
	if err != nil {
		return nil, err
	}
	providerTrend, err := queryTrend(ctx, db, since, label, "other")
	if err != nil {
		return nil, err
	}
	var xaiTrend []trendPoint
	if xaiHasUsage {
		xaiTrend, err = queryTrend(ctx, db, since, label, "xai")
		if err != nil {
			return nil, err
		}
	}
	recent, err := queryRecent(ctx, db, since, 30, "codex", prices)
	if err != nil {
		return nil, err
	}
	providerRecent, err := queryProviderRecent(ctx, db, since, 30, providerRecentLimit(limit), prices)
	if err != nil {
		return nil, err
	}
	var xaiRecent []recentRow
	if xaiHasUsage {
		xaiRecent, err = queryRecent(ctx, db, since, 30, "xai", prices)
		if err != nil {
			return nil, err
		}
	}
	autobans, err := queryActiveAutobans(ctx, db, now)
	if err != nil {
		return nil, err
	}
	autobans = filterMissingAutobanRows(autobans, configuredAccounts, authDirReadable)
	autobans = mergeEffectiveAutobans(autobans, invalidAuths)
	applyAccountQuotaToAutobans(autobans, accounts)
	diagnostics := buildDiagnostics(ctx, db, path, accounts, providers, unauthorizedInvalidAuths, autobans, externalUseAlerts)
	result := map[string]any{
		"plugin":                      pluginID,
		"version":                     pluginVersion,
		"generated_at":                time.Now().Format(time.RFC3339),
		"window":                      label,
		"since_unix":                  since,
		"db_path":                     path,
		"totals":                      totals,
		"provider_totals":             providerTotals,
		"xai_totals":                  xaiTotals,
		"accounts":                    accounts,
		"xai_accounts":                xaiAccounts,
		"providers":                   providers,
		"key_summaries":               keySummaries,
		"models":                      models,
		"provider_models":             providerModels,
		"xai_models":                  xaiModels,
		"trend":                       trend,
		"provider_trend":              providerTrend,
		"xai_trend":                   xaiTrend,
		"recent":                      recent,
		"provider_recent":             providerRecent,
		"xai_recent":                  xaiRecent,
		"xai_states":                  xaiStates,
		"autobans":                    autobans,
		"invalid_auths":               unauthorizedInvalidAuths,
		"forbidden_auths":             forbiddenInvalidAuths,
		"workspace_deactivated_auths": workspaceDeactivatedAuths,
		"external_use_alerts":         externalUseAlerts,
		"quota_trigger":               globalQuotaTrigger.status(),
		"quota_trigger_runs":          triggerRuns,
		"model_prices":                globalModelPriceUpdater.status(),
		"diagnostics":                 diagnostics,
	}
	result["alerts"] = buildAlerts(result)
	return result, nil
}

func filterMissingAutobanRows(rows []autobanRow, configured []configuredAccount, authDirReadable bool) []autobanRow {
	if !authDirReadable || len(rows) == 0 {
		return rows
	}
	aliases := configuredAliasSet(configured)
	strictAliases := configuredStrictAliasSet(configured)
	out := rows[:0]
	for _, row := range rows {
		cleanupAliases := fileBackedCleanupAliases(row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
		if len(cleanupAliases) > 0 {
			if aliasesContainAny(strictAliases, cleanupAliases...) {
				out = append(out, row)
			}
			continue
		}
		rowStrictAliases := strictAuthStateAliasesForValues(row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
		if len(rowStrictAliases) > 0 {
			if aliasesContainAny(strictAliases, rowStrictAliases...) {
				out = append(out, row)
			}
			continue
		}
		if !fileBackedAuthState(row.AuthID, row.AuthIndex, row.Source, row.AuthFile) || aliasesContainAny(aliases, row.AuthID, row.AuthIndex, row.Source, row.AuthFile) {
			out = append(out, row)
			continue
		}
	}
	return out
}

func filterMissingInvalidAuthRows(rows []invalidAuthRow, configured []configuredAccount, authDirReadable bool) []invalidAuthRow {
	if !authDirReadable || len(rows) == 0 {
		return rows
	}
	identityIndex := newCodexAuthIdentityIndex(configured)
	out := rows[:0]
	for _, row := range rows {
		if _, ok := identityIndex.match(row); ok {
			out = append(out, row)
			continue
		}
		// CPA intentionally hides disabled runtime-only credentials from
		// host.auth.list. Their absence is therefore not proof that the stored
		// 401 is stale; keep the explicit runtime record actionable until the
		// user disables/resolves it or a later successful request clears it.
		if inferInvalidAuthSourceKind(row) == authSourceKindRuntimeOnly {
			out = append(out, row)
		}
	}
	return out
}

func classifyInvalidAuthRows(rows []invalidAuthRow, inventory []configuredAccount) []invalidAuthRow {
	identityIndex := newCodexAuthIdentityIndex(inventory)
	for i := range rows {
		if match, ok := identityIndex.match(rows[i]); ok {
			rows[i].AuthSourceKind = normalizeAuthSourceKind(match.AuthSourceKind)
			continue
		}
		rows[i].AuthSourceKind = inferInvalidAuthSourceKind(rows[i])
	}
	return rows
}

func inferInvalidAuthSourceKind(row invalidAuthRow) string {
	if kind := strings.ToLower(strings.TrimSpace(row.AuthSourceKind)); kind == authSourceKindFile || kind == authSourceKindRuntimeOnly {
		return kind
	}
	return authSourceKindLegacy
}

func providerRecentLimit(limit int) int {
	if limit < 30 {
		limit = 30
	}
	n := limit * 20
	if n < 500 {
		n = 500
	}
	if n > 2000 {
		n = 2000
	}
	return n
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
	AverageLatencyMs                float64 `json:"avg_latency_ms"`
	AverageTTFTMs                   float64 `json:"avg_ttft_ms"`
	OutputTokensPerSecond           float64 `json:"output_tokens_per_second"`
	SlowRequests                    int64   `json:"slow_requests"`
	SlowTTFTRequests                int64   `json:"slow_ttft_requests"`
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
	AuthFileMTime                   int64    `json:"-"`
	ChatGPTAccountID                string   `json:"chatgpt_account_id,omitempty"`
	Configured                      bool     `json:"configured"`
	Disabled                        bool     `json:"disabled,omitempty"`
	Expired                         bool     `json:"expired,omitempty"`
	InvalidAuth                     bool     `json:"invalid_auth,omitempty"`
	InvalidAuthAt                   string   `json:"invalid_auth_at,omitempty"`
	InvalidAuthReason               string   `json:"invalid_auth_reason,omitempty"`
	InvalidAuthStatusCode           int      `json:"invalid_auth_status_code,omitempty"`
	WorkspaceDeactivated            bool     `json:"workspace_deactivated,omitempty"`
	WorkspaceDeactivatedAt          string   `json:"workspace_deactivated_at,omitempty"`
	WorkspaceDeactivatedReason      string   `json:"workspace_deactivated_reason,omitempty"`
	PlanType                        string   `json:"plan_type,omitempty"`
	XAITier                         string   `json:"xai_tier,omitempty"`
	XAITierSource                   string   `json:"xai_tier_source,omitempty"`
	XAITierDetail                   string   `json:"xai_tier_detail,omitempty"`
	RuntimeStatus                   string   `json:"runtime_status,omitempty"`
	RuntimeMessage                  string   `json:"runtime_message,omitempty"`
	RuntimeUnavailable              bool     `json:"runtime_unavailable,omitempty"`
	XAIState                        string   `json:"xai_state,omitempty"`
	XAIStateReason                  string   `json:"xai_state_reason,omitempty"`
	XAIStateObservedAt              string   `json:"xai_state_observed_at,omitempty"`
	XAIStateResetAt                 int64    `json:"xai_state_reset_at,omitempty"`
	XAIStateResetAtText             string   `json:"xai_state_reset_at_text,omitempty"`
	XAIStateSecondsRemaining        int64    `json:"xai_state_seconds_remaining,omitempty"`
	XAILastStatusCode               int      `json:"xai_last_status_code,omitempty"`
	ProtectionPlan                  string   `json:"protection_plan,omitempty"`
	ProtectionInFlight              int      `json:"protection_in_flight,omitempty"`
	ProtectionConcurrencyLimit      int      `json:"protection_concurrency_limit,omitempty"`
	ProtectionWindowTokens          int64    `json:"protection_window_tokens,omitempty"`
	ProtectionTokenLimit            int64    `json:"protection_token_limit,omitempty"`
	ProtectionTokenDemoted          bool     `json:"protection_token_demoted,omitempty"`
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
	AverageLatencyMs                float64  `json:"avg_latency_ms"`
	AverageTTFTMs                   float64  `json:"avg_ttft_ms"`
	OutputTokensPerSecond           float64  `json:"output_tokens_per_second"`
	SlowRequests                    int64    `json:"slow_requests"`
	SlowTTFTRequests                int64    `json:"slow_ttft_requests"`
	LastSeen                        string   `json:"last_seen"`
	PrimaryUsedPercent              *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAt                  *int64   `json:"primary_reset_at,omitempty"`
	PrimaryQuotaWindow              string   `json:"primary_quota_window,omitempty"`
	PrimaryQuotaSource              string   `json:"primary_quota_source,omitempty"`
	PrimaryQuotaObservedFrom        string   `json:"primary_quota_observed_from,omitempty"`
	PrimaryWindowTokens             int64    `json:"primary_window_tokens"`
	SecondaryUsedPercent            *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAt                *int64   `json:"secondary_reset_at,omitempty"`
	SecondaryWindowTokens           int64    `json:"secondary_window_tokens"`
	SecondaryQuotaWindow            string   `json:"secondary_quota_window,omitempty"`
	QuotaWindowSource               string   `json:"quota_window_source,omitempty"`
	SecondaryQuotaSource            string   `json:"secondary_quota_source,omitempty"`
	SecondaryQuotaObservedFrom      string   `json:"secondary_quota_observed_from,omitempty"`
	SecondaryQuotaTotalEstimate     int64    `json:"secondary_quota_total_estimate"`
	SecondaryQuotaRemainingEstimate int64    `json:"secondary_quota_remaining_estimate"`
	SecondaryQuotaEstimateSource    string   `json:"secondary_quota_estimate_source,omitempty"`
	SecondaryQuotaEstimateMethod    string   `json:"secondary_quota_estimate_method,omitempty"`
	QuotaSource                     string   `json:"quota_source,omitempty"`
	QuotaCredibility                string   `json:"quota_credibility,omitempty"`
	QuotaEstimateNote               string   `json:"quota_estimate_note,omitempty"`
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
	AuthIndex          string
	AuthID             string
	Source             string
	Provider           string
	Email              string
	Name               string
	AuthFile           string
	AuthFileMTime      int64
	AuthSourceKind     string
	Disabled           bool
	Expired            bool
	PlanType           string
	XAITier            string
	XAITierSource      string
	XAITierDetail      string
	RuntimeStatus      string
	RuntimeMessage     string
	RuntimeUnavailable bool
	AccessToken        string
	ChatGPTAccountID   string
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
	AuthFileMTime        int64
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
	ResponseHeaders      map[string][]string
}

type quotaTriggerAccountStatus struct {
	AuthID        string
	AuthIndex     string
	Source        string
	AuthFile      string
	AuthFileMTime int64
	Mode          string
	Status        string
	HTTPStatus    int
	Error         string
	FinishedAt    int64
}

type providerRow struct {
	Provider              string  `json:"provider"`
	Requests              int64   `json:"requests"`
	Failed                int64   `json:"failed"`
	RateLimited           int64   `json:"rate_limited"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	CostAvailable         bool    `json:"cost_available"`
	UnpricedTokens        int64   `json:"unpriced_tokens,omitempty"`
	AverageLatencyMs      float64 `json:"avg_latency_ms"`
	AverageTTFTMs         float64 `json:"avg_ttft_ms"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
	SlowRequests          int64   `json:"slow_requests"`
	SlowTTFTRequests      int64   `json:"slow_ttft_requests"`
	Accounts              int64   `json:"accounts"`
	Models                int64   `json:"models"`
	LastSeen              string  `json:"last_seen"`
}

type keySummaryRow struct {
	KeyID                 string  `json:"key_id"`
	RawKeyID              string  `json:"-"`
	Protocol              string  `json:"protocol"`
	Provider              string  `json:"provider,omitempty"`
	Providers             int64   `json:"providers,omitempty"`
	ProviderNames         string  `json:"provider_names,omitempty"`
	Requests              int64   `json:"requests"`
	Failed                int64   `json:"failed"`
	RateLimited           int64   `json:"rate_limited"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	QuotaUsedTokens       int64   `json:"quota_used_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	CostAvailable         bool    `json:"cost_available"`
	UnpricedTokens        int64   `json:"unpriced_tokens,omitempty"`
	AverageLatencyMs      float64 `json:"avg_latency_ms"`
	AverageTTFTMs         float64 `json:"avg_ttft_ms"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
	SlowRequests          int64   `json:"slow_requests"`
	SlowTTFTRequests      int64   `json:"slow_ttft_requests"`
	Models                int64   `json:"models"`
	LastSeen              string  `json:"last_seen"`
}

type modelRow struct {
	Model                 string  `json:"model"`
	Alias                 string  `json:"alias"`
	Provider              string  `json:"provider"`
	Requests              int64   `json:"requests"`
	TotalTokens           int64   `json:"total_tokens"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	CostAvailable         bool    `json:"cost_available"`
	UnpricedTokens        int64   `json:"unpriced_tokens,omitempty"`
	AverageLatencyMs      float64 `json:"avg_latency_ms"`
	AverageTTFTMs         float64 `json:"avg_ttft_ms"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
	SlowRequests          int64   `json:"slow_requests"`
	SlowTTFTRequests      int64   `json:"slow_ttft_requests"`
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
	AuthFile             string   `json:"auth_file,omitempty"`
	AuthFileMTime        int64    `json:"auth_file_mtime,omitempty"`
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
	AuthSourceKind    string `json:"auth_source_kind"`
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
	return usageScopeSQLWithEntries(scope, readConfiguredProviderEntries())
}

func usageScopeSQLWithEntries(scope string, entries []providerConfigEntry) string {
	codexAccount := `((LOWER(COALESCE(NULLIF(provider,''), '')) = 'codex' OR LOWER(COALESCE(NULLIF(executor_type,''), '')) LIKE '%codex%')
AND LOWER(COALESCE(NULLIF(auth_type,''), '')) NOT IN ('apikey', 'api_key', 'key')
AND LOWER(COALESCE(NULLIF(auth_id,''), '')) NOT LIKE 'codex:apikey:%'
AND COALESCE(NULLIF(source,''), '') NOT LIKE 'sk-%'
AND COALESCE(NULLIF(source,''), '') NOT LIKE 'Bearer sk-%')`
	codexAPIKey := codexAPIKeyProviderScopeSQL(entries)
	xaiAccount := `(LOWER(COALESCE(NULLIF(provider,''), '')) = 'xai' OR LOWER(COALESCE(NULLIF(executor_type,''), '')) LIKE '%xai%')`
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "codex":
		return codexAccount
	case "other":
		return "((NOT " + codexAccount + ") AND (NOT " + xaiAccount + ") AND (NOT (LOWER(COALESCE(NULLIF(provider,''), '')) = 'codex' AND LOWER(COALESCE(NULLIF(auth_type,''), '')) IN ('apikey', 'api_key', 'key')) OR " + codexAPIKey + "))"
	case "xai":
		return xaiAccount
	default:
		return "1=1"
	}
}

func codexAPIKeyProviderScopeSQL(entries []providerConfigEntry) string {
	var parts []string
	for _, entry := range entries {
		if !strings.EqualFold(entry.Provider, "Codex") || strings.TrimSpace(entry.APIKey) == "" {
			continue
		}
		key := strings.TrimSpace(entry.APIKey)
		parts = append(parts, "source = "+sqlQuote(key))
		parts = append(parts, "source = "+sqlQuote("Bearer "+key))
	}
	if len(parts) == 0 {
		return "0=1"
	}
	return "(LOWER(COALESCE(NULLIF(provider,''), '')) = 'codex' AND LOWER(COALESCE(NULLIF(auth_type,''), '')) IN ('apikey', 'api_key', 'key') AND (" + strings.Join(parts, " OR ") + "))"
}

func throughputSQL() string {
	duration := `max(latency_ms, ttft_ms)`
	valid := `output_tokens > 0 AND ` + duration + ` >= 1000 AND NOT (latency_ms = ttft_ms AND output_tokens >= 1000 AND ` + duration + ` < 5000)`
	return `COALESCE(SUM(CASE WHEN ` + valid + ` THEN output_tokens ELSE 0 END) * 1000.0 / NULLIF(SUM(CASE WHEN ` + valid + ` THEN ` + duration + ` ELSE 0 END),0),0)`
}

func queryOneTotals(ctx context.Context, db *sql.DB, since int64, scope string) (totalsRow, error) {
	var row totalsRow
	query := `
SELECT COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0),
COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END),0),
COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END),0),
` + throughputSQL() + `,
COALESCE(SUM(CASE WHEN latency_ms >= 12000 THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ttft_ms >= 3000 THEN 1 ELSE 0 END),0)
FROM usage_events WHERE requested_at >= ? AND ` + usageScopeSQL(scope)
	err := db.QueryRowContext(ctx, query, since).Scan(
		&row.Requests, &row.Failed, &row.RateLimited, &row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
		&row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens,
		&row.AverageLatencyMs, &row.AverageTTFTMs, &row.OutputTokensPerSecond, &row.SlowRequests, &row.SlowTTFTRequests,
	)
	return row, err
}

func queryHasXAIUsage(ctx context.Context, db *sql.DB, since int64) (bool, error) {
	var found int
	err := db.QueryRowContext(ctx, `
SELECT 1
FROM usage_events INDEXED BY idx_usage_events_provider_requested
WHERE provider IN ('xai','XAI') AND requested_at >= ?
LIMIT 1`, since).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func queryAccounts(ctx context.Context, db *sql.DB, since int64, limit int, scope string) ([]accountRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(auth_index,''), NULLIF(auth_id,''), 'unknown') AS account_key,
MAX(auth_id), MAX(source), MAX(provider),
COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0),
COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END),0),
COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END),0),
`+throughputSQL()+`,
COALESCE(SUM(CASE WHEN latency_ms >= 12000 THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ttft_ms >= 3000 THEN 1 ELSE 0 END),0),
MAX(requested_at)
FROM usage_events
WHERE requested_at >= ? AND `+usageScopeSQL(scope)+` AND (auth_index <> '' OR auth_id <> '' OR source <> '')
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
			&r.CacheCreationTokens, &r.TotalTokens, &r.AverageLatencyMs, &r.AverageTTFTMs, &r.OutputTokensPerSecond,
			&r.SlowRequests, &r.SlowTTFTRequests, &last); err != nil {
			return nil, err
		}
		r.LastSeen = unixTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

func readConfiguredAuthAccounts() []configuredAccount {
	accounts, err := globalCodexAuthSource.hostAccounts()
	if err == nil {
		var metadata []configuredAccount
		for _, file := range readConfiguredAuthFiles() {
			if isCodexAuthProvider(file.Provider) {
				metadata = append(metadata, file)
			}
		}
		return mergeConfiguredAccountMetadata(accounts, metadata)
	}
	files := readConfiguredAuthFiles()
	out := make([]configuredAccount, 0, len(files))
	for _, file := range files {
		if isCodexAuthProvider(file.Provider) {
			out = append(out, file)
		}
	}
	globalCodexAuthSource.markFilesystemFallback(out, err)
	return out
}

func readConfiguredXAIAccounts() []configuredAccount {
	accounts, err := globalXAIAuthSource.hostAccounts()
	if err == nil {
		return accounts
	}
	files := readConfiguredAuthFiles()
	out := make([]configuredAccount, 0, len(files))
	for _, file := range files {
		if strings.EqualFold(strings.TrimSpace(file.Provider), "xai") {
			out = append(out, file)
		}
	}
	globalXAIAuthSource.markFilesystemFallback(out, err)
	return out
}

type configuredAuthFilesCacheState struct {
	mu       sync.Mutex
	dir      string
	revision string
	accounts []configuredAccount
}

var configuredAuthFilesCache configuredAuthFilesCacheState

func readConfiguredAuthFiles() []configuredAccount {
	authDir := configuredAuthDir()
	if authDir == "" {
		return nil
	}
	entries, revision, err := configuredAuthDirectorySnapshot(authDir)
	if err != nil {
		return nil
	}
	configuredAuthFilesCache.mu.Lock()
	if configuredAuthFilesCache.dir == authDir && configuredAuthFilesCache.revision == revision {
		accounts := cloneConfiguredAccounts(configuredAuthFilesCache.accounts)
		configuredAuthFilesCache.mu.Unlock()
		return accounts
	}
	configuredAuthFilesCache.mu.Unlock()
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
		authType := normalizeAuthProvider(firstNonEmptyString(
			stringFromAny(doc["provider"]),
			stringFromAny(doc["platform"]),
			stringFromAny(doc["type"]),
			stringFromAny(doc["auth_type"]),
		), entry.Name())
		email := firstNonEmptyString(stringFromAny(doc["email"]), stringFromAny(doc["account"]), stringFromAny(doc["username"]))
		name := stringFromAny(doc["name"])
		authFile := entry.Name()
		source := firstNonEmptyString(email, name, authFile)
		xaiTier := xaiTierClassification{}
		if strings.EqualFold(authType, "xai") {
			xaiTier = classifyXAITierDocument(doc)
		}
		out = append(out, configuredAccount{
			AuthIndex:      authFile,
			AuthID:         email,
			Source:         source,
			Provider:       firstNonEmptyString(authType, "codex"),
			Email:          email,
			Name:           name,
			AuthFile:       authFile,
			AuthFileMTime:  info.ModTime().Unix(),
			AuthSourceKind: authSourceKindFile,
			Disabled:       boolFromAny(doc["disabled"]),
			Expired:        boolFromAny(doc["expired"]),
			PlanType:       firstNonEmptyString(stringFromAny(doc["plan_type"]), stringFromAny(doc["plan"])),
			XAITier:        xaiTier.Tier,
			XAITierSource:  xaiTier.Source,
			XAITierDetail:  xaiTier.Detail,
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
	configuredAuthFilesCache.mu.Lock()
	configuredAuthFilesCache.dir = authDir
	configuredAuthFilesCache.revision = revision
	configuredAuthFilesCache.accounts = cloneConfiguredAccounts(out)
	configuredAuthFilesCache.mu.Unlock()
	return cloneConfiguredAccounts(out)
}

func configuredAuthDirectorySnapshot(authDir string) ([]os.DirEntry, string, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil, "", err
	}
	hash := fnv.New64a()
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		count++
		_, _ = hash.Write([]byte(entry.Name()))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strconv.FormatInt(info.Size(), 10)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
		_, _ = hash.Write([]byte{0})
	}
	return entries, strconv.Itoa(count) + ":" + strconv.FormatUint(hash.Sum64(), 16), nil
}

func normalizeAuthProvider(value, filename string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "codex" || value == "openai" || value == "chatgpt":
		return "codex"
	case value == "anthropic" || value == "claude":
		return "anthropic"
	case value == "antigravity":
		return "antigravity"
	case value == "gemini" || value == "google":
		return "gemini"
	case value == "xai" || value == "grok":
		return "xai"
	}
	name := strings.ToLower(strings.TrimSpace(filename))
	switch {
	case strings.Contains(name, "anthropic") || strings.Contains(name, "claude"):
		return "anthropic"
	case strings.Contains(name, "antigravity"):
		return "antigravity"
	case strings.Contains(name, "gemini") || strings.Contains(name, "google"):
		return "gemini"
	case strings.Contains(name, "xai") || strings.Contains(name, "grok"):
		return "xai"
	default:
		return "codex"
	}
}

func isCodexAuthProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

func readTriggerAuthAccounts() []triggerAuthAccount {
	files := readConfiguredAuthFiles()
	out := make([]triggerAuthAccount, 0, len(files))
	for _, file := range files {
		if !isCodexAuthProvider(file.Provider) {
			continue
		}
		out = append(out, triggerAuthAccount{
			configuredAccount: file,
			AccessToken:       file.AccessToken,
			ChatGPTAccountID:  file.ChatGPTAccountID,
		})
	}
	return out
}

func configuredAuthDir() string {
	if dir := strings.TrimSpace(os.Getenv("CPA_AUTH_DIR")); dir != "" {
		return resolveConfiguredAuthDir(dir)
	}
	configPath := configuredConfigPath()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	dir := yamlScalar(string(raw), "auth-dir", "auth_dir")
	if dir == "" {
		dir = "~/.cli-proxy-api"
	}
	return resolveConfiguredAuthDir(dir)
}

func resolveConfiguredAuthDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "~") {
		return filepath.Clean(path)
	}
	home, err := currentUserHomeDir()
	if err != nil {
		return ""
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
	if remainder == "" {
		return filepath.Clean(home)
	}
	remainder = strings.ReplaceAll(remainder, "\\", "/")
	return filepath.Clean(filepath.Join(home, filepath.FromSlash(remainder)))
}

func readConfiguredProviderNames() []string {
	entries := readConfiguredProviderEntries()
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	return out
}

type providerConfigEntryCacheState struct {
	mu      sync.Mutex
	path    string
	modTime int64
	size    int64
	entries []providerConfigEntry
}

var configuredProviderEntriesCache providerConfigEntryCacheState

func readConfiguredProviderEntries() []providerConfigEntry {
	path := configuredConfigPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	modTime := info.ModTime().UnixNano()
	size := info.Size()
	configuredProviderEntriesCache.mu.Lock()
	if configuredProviderEntriesCache.path == path && configuredProviderEntriesCache.modTime == modTime && configuredProviderEntriesCache.size == size {
		entries := cloneProviderConfigEntries(configuredProviderEntriesCache.entries)
		configuredProviderEntriesCache.mu.Unlock()
		return entries
	}
	configuredProviderEntriesCache.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	entries := configuredProviderEntriesFromYAML(string(raw))
	configuredProviderEntriesCache.mu.Lock()
	configuredProviderEntriesCache.path = path
	configuredProviderEntriesCache.modTime = modTime
	configuredProviderEntriesCache.size = size
	configuredProviderEntriesCache.entries = cloneProviderConfigEntries(entries)
	configuredProviderEntriesCache.mu.Unlock()
	return entries
}

func cloneProviderConfigEntries(entries []providerConfigEntry) []providerConfigEntry {
	if len(entries) == 0 {
		return nil
	}
	return append([]providerConfigEntry(nil), entries...)
}

func configuredProviderNamesFromYAML(raw string) []string {
	entries := configuredProviderEntriesFromYAML(raw)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	return out
}

func configuredProviderEntriesFromYAML(raw string) []providerConfigEntry {
	blocks := map[string]string{
		"openai-compatibility": "OpenAI",
		"openai-compatible":    "OpenAI",
		"codex-api-key":        "Codex",
		"claude-api-key":       "Claude",
		"anthropic-api-key":    "Claude",
		"gemini-api-key":       "Gemini",
		"antigravity-api-key":  "Antigravity",
		"antigravity-oauth":    "Antigravity",
		"anthropic-oauth":      "Claude",
	}
	var out []providerConfigEntry
	seen := map[string]bool{}
	add := func(provider, name, apiKey string) {
		name = strings.TrimSpace(strings.Trim(name, `"'`))
		key := normalizeAccountAlias(name)
		if name != "" && !seen[key] {
			seen[key] = true
			out = append(out, providerConfigEntry{Name: name, Provider: provider, APIKey: strings.TrimSpace(strings.Trim(apiKey, `"'`))})
		}
	}
	var current *providerConfigBlock
	blockIndent := -1
	itemIndent := -1
	itemName := ""
	itemBaseURL := ""
	itemAPIKey := ""
	flushItem := func() {
		if current == nil || itemIndent < 0 {
			return
		}
		add(current.DisplayName, providerConfigEntryName(current.DisplayName, itemName, itemBaseURL), itemAPIKey)
		itemIndent = -1
		itemName = ""
		itemBaseURL = ""
		itemAPIKey = ""
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 && strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "-") {
			flushItem()
			current = nil
			itemIndent = -1
			blockIndent = indent
			key, value := yamlKeyValue(trimmed)
			if display, ok := blocks[key]; ok {
				current = &providerConfigBlock{Key: key, DisplayName: display}
				if yamlScalarHasValue(value) {
					add(display, display, "")
				}
			}
			continue
		}
		if current == nil {
			continue
		}
		if indent <= blockIndent && !strings.HasPrefix(trimmed, "-") {
			flushItem()
			current = nil
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && indent > blockIndent && (itemIndent < 0 || indent <= itemIndent) {
			flushItem()
			itemIndent = indent
			inline := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			key, value := yamlKeyValue(inline)
			switch key {
			case "name":
				itemName = value
			case "base-url", "base_url":
				itemBaseURL = value
			case "api-key", "api_key":
				itemAPIKey = value
			}
			continue
		}
		if itemIndent >= 0 && indent == itemIndent+2 {
			key, value := yamlKeyValue(trimmed)
			switch key {
			case "name":
				itemName = value
			case "base-url", "base_url":
				itemBaseURL = value
			case "api-key", "api_key":
				itemAPIKey = value
			}
		}
		if itemIndent >= 0 && indent > itemIndent && itemAPIKey == "" {
			key, value := yamlKeyValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			if key == "api-key" || key == "api_key" {
				itemAPIKey = value
			}
		}
	}
	flushItem()
	return out
}

type providerConfigBlock struct {
	Key         string
	DisplayName string
}

type providerConfigEntry struct {
	Name     string
	Provider string
	APIKey   string
}

func yamlKeyValue(line string) (string, string) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", ""
	}
	value = strings.TrimSpace(value)
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(strings.Trim(value, `"'`))
}

func yamlScalarHasValue(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "[]", "{}", "null", "~":
		return false
	default:
		return true
	}
}

func providerConfigEntryName(provider, name, baseURL string) string {
	name = strings.TrimSpace(strings.Trim(name, `"'`))
	if name != "" {
		return name
	}
	host := hostFromURL(baseURL)
	if host != "" {
		return strings.TrimSpace(provider + " · " + host)
	}
	return strings.TrimSpace(provider)
}

func hostFromURL(value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		if parsed, err = url.Parse("https://" + strings.TrimPrefix(value, "//")); err != nil || parsed.Host == "" {
			return ""
		}
	}
	host := parsed.Hostname()
	if host == "" {
		host = parsed.Host
	}
	return host
}

func authFileStateForRecord(rec usageRecord) (string, int64) {
	if authFile := fileNameIfJSON(rec.AuthFile); authFile != "" {
		return authFileStateForName(authFile)
	}
	if authFile := firstNonEmptyString(fileNameIfJSON(rec.AuthIndex), fileNameIfJSON(rec.Source), fileNameIfJSON(rec.AuthID)); authFile != "" {
		return authFileStateForName(authFile)
	}
	configured := readConfiguredAuthAccounts()
	emailCounts := configuredEmailCounts(configured)
	for _, cfg := range configured {
		for _, alias := range normalizeAccountAliases(rec.AuthIndex, rec.AuthID, rec.Source) {
			if alias == "" {
				continue
			}
			for _, cfgAlias := range configuredAccountMatchAliases(cfg, emailCounts) {
				if alias == cfgAlias {
					return cfg.AuthFile, cfg.AuthFileMTime
				}
			}
		}
	}
	return "", 0
}

func authFileStateForName(authFile string) (string, int64) {
	authFile = fileNameIfJSON(authFile)
	if authFile == "" {
		return "", 0
	}
	targetAliases := normalizeAccountAliases(authFile)
	for _, cfg := range readConfiguredAuthAccounts() {
		matched := false
		for _, left := range normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex, cfg.AuthID) {
			for _, right := range targetAliases {
				if left == right {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if matched && cfg.AuthFileMTime > 0 {
			return firstNonEmptyString(cfg.AuthFile, authFile), cfg.AuthFileMTime
		}
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

func configuredAuthDirectoryReadable() bool {
	authDir := configuredAuthDir()
	if authDir == "" {
		return false
	}
	_, err := os.ReadDir(authDir)
	return err == nil
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
		for _, alias := range accountMergeAliases(merged[i]) {
			if alias == "" {
				continue
			}
			if _, exists := index[alias]; !exists {
				index[alias] = i
			}
		}
	}
	seenConfig := make(map[string]bool, len(configured))
	emailCounts := configuredEmailCounts(configured)
	for _, cfg := range configured {
		canonical := configuredAccountKey(cfg)
		if canonical != "" && seenConfig[canonical] {
			continue
		}
		if canonical != "" {
			seenConfig[canonical] = true
		}
		match := -1
		for _, alias := range configuredAccountMatchAliases(cfg, emailCounts) {
			if i, ok := index[alias]; ok {
				match = i
				break
			}
		}
		if match >= 0 {
			enrichConfiguredAccount(&merged[match], cfg)
			for _, alias := range accountMergeAliases(merged[match]) {
				if alias != "" {
					index[alias] = match
				}
			}
			continue
		}
		row := accountRow{
			AuthIndex:          cfg.AuthIndex,
			AuthID:             cfg.AuthID,
			Source:             cfg.Source,
			Provider:           firstNonEmptyString(cfg.Provider, "codex"),
			Email:              cfg.Email,
			Name:               cfg.Name,
			AuthFile:           cfg.AuthFile,
			AuthFileMTime:      cfg.AuthFileMTime,
			ChatGPTAccountID:   cfg.ChatGPTAccountID,
			Configured:         true,
			Disabled:           cfg.Disabled,
			Expired:            cfg.Expired,
			PlanType:           cfg.PlanType,
			XAITier:            cfg.XAITier,
			XAITierSource:      cfg.XAITierSource,
			XAITierDetail:      cfg.XAITierDetail,
			RuntimeStatus:      cfg.RuntimeStatus,
			RuntimeMessage:     cfg.RuntimeMessage,
			RuntimeUnavailable: cfg.RuntimeUnavailable,
		}
		merged = append(merged, row)
		rowIndex := len(merged) - 1
		for _, alias := range accountMergeAliases(row) {
			if alias != "" {
				index[alias] = rowIndex
			}
		}
	}
	return merged
}

func filterCurrentConfiguredAccounts(accounts []accountRow, configured []configuredAccount, authDirReadable bool) []accountRow {
	if !authDirReadable {
		return accounts
	}
	// A readable but empty auth directory means every configured account was removed.
	// Do not fall back to historical usage rows in that case.
	if len(configured) == 0 {
		return nil
	}
	aliases := make(map[string]struct{}, len(configured)*6)
	for _, cfg := range configured {
		for _, alias := range configuredAliases(cfg) {
			if alias != "" {
				aliases[alias] = struct{}{}
			}
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	strictAliases := configuredStrictAliasSet(configured)
	out := accounts[:0]
	for _, account := range accounts {
		cleanupAliases := accountFileIdentityAliases(account)
		if len(cleanupAliases) > 0 {
			if aliasesAllContained(strictAliases, cleanupAliases...) {
				out = append(out, account)
			}
			continue
		}
		keep := false
		for _, alias := range accountAliases(account) {
			if _, ok := aliases[alias]; ok {
				keep = true
				break
			}
		}
		if keep {
			out = append(out, account)
		}
	}
	return out
}

func accountMergeAliases(row accountRow) []string {
	if aliases := accountFileIdentityAliases(row); len(aliases) > 0 {
		return aliases
	}
	return accountAliases(row)
}

func accountFileIdentityAliases(row accountRow) []string {
	var values []string
	for _, value := range []string{row.AuthFile, row.AuthID, row.AuthIndex, row.Source} {
		if file := fileNameIfJSON(value); file != "" {
			values = append(values, file)
		}
	}
	return normalizeAccountAliases(values...)
}

func enrichConfiguredAccount(row *accountRow, cfg configuredAccount) {
	row.Configured = true
	row.Disabled = cfg.Disabled
	row.Expired = cfg.Expired
	row.PlanType = firstNonEmptyString(row.PlanType, cfg.PlanType)
	row.XAITier = firstNonEmptyString(row.XAITier, cfg.XAITier)
	row.XAITierSource = firstNonEmptyString(row.XAITierSource, cfg.XAITierSource)
	row.XAITierDetail = firstNonEmptyString(row.XAITierDetail, cfg.XAITierDetail)
	row.RuntimeStatus = firstNonEmptyString(row.RuntimeStatus, cfg.RuntimeStatus)
	row.RuntimeMessage = firstNonEmptyString(row.RuntimeMessage, cfg.RuntimeMessage)
	row.RuntimeUnavailable = row.RuntimeUnavailable || cfg.RuntimeUnavailable
	row.Email = firstNonEmptyString(row.Email, cfg.Email)
	row.Name = firstNonEmptyString(row.Name, cfg.Name)
	row.AuthFile = firstNonEmptyString(row.AuthFile, cfg.AuthFile)
	if row.AuthFileMTime == 0 {
		row.AuthFileMTime = cfg.AuthFileMTime
	}
	row.ChatGPTAccountID = firstNonEmptyString(row.ChatGPTAccountID, cfg.ChatGPTAccountID)
	row.Provider = firstNonEmptyString(row.Provider, cfg.Provider, "codex")
	if row.Source == "" || looksOpaqueAccountKey(row.Source) {
		row.Source = firstNonEmptyString(cfg.Source, row.Source)
	}
	if row.AuthID == "" || looksOpaqueAccountKey(row.AuthID) {
		row.AuthID = firstNonEmptyString(cfg.AuthID, row.AuthID)
	}
}

func accountAliases(row accountRow) []string {
	return accountIdentityAliases(accountIdentity{
		AuthIndex: row.AuthIndex,
		AuthID:    row.AuthID,
		Source:    row.Source,
		Email:     row.Email,
		Name:      row.Name,
		AuthFile:  row.AuthFile,
	})
}

func configuredAliases(cfg configuredAccount) []string {
	return accountIdentityAliases(accountIdentity{
		AuthIndex: cfg.AuthIndex,
		AuthID:    cfg.AuthID,
		Source:    cfg.Source,
		Email:     cfg.Email,
		Name:      cfg.Name,
		AuthFile:  cfg.AuthFile,
	})
}

func configuredEmailCounts(configured []configuredAccount) map[string]int {
	counts := make(map[string]int, len(configured))
	for _, cfg := range configured {
		email := normalizeAccountAlias(cfg.Email)
		if email != "" {
			counts[email]++
		}
	}
	return counts
}

func configuredAccountKey(cfg configuredAccount) string {
	if alias := normalizeAccountAlias(cfg.AuthFile); alias != "" {
		return "file:" + alias
	}
	if alias := normalizeAccountAlias(cfg.AuthIndex); alias != "" {
		return "index:" + alias
	}
	email := normalizeAccountAlias(cfg.Email)
	accountID := normalizeAccountAlias(cfg.ChatGPTAccountID)
	if email != "" && accountID != "" {
		return "email-account:" + email + "|" + accountID
	}
	if alias := normalizeAccountAlias(firstNonEmptyString(cfg.AuthID, cfg.Email, cfg.Name)); alias != "" {
		return "alias:" + alias
	}
	return ""
}

func configuredAccountMatchAliases(cfg configuredAccount, emailCounts map[string]int) []string {
	email := normalizeAccountAlias(cfg.Email)
	if email == "" || emailCounts[email] <= 1 {
		return normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex, cfg.AuthID, cfg.Source, cfg.Email, cfg.Name)
	}
	return normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex)
}

type accountIdentity struct {
	AuthID    string
	AuthIndex string
	Source    string
	AuthFile  string
	Email     string
	Name      string
	Path      string
}

func accountIdentityAliases(identity accountIdentity) []string {
	return normalizeAccountAliases(
		identity.AuthID,
		identity.AuthIndex,
		identity.Source,
		identity.AuthFile,
		identity.Email,
		identity.Name,
		identity.Path,
	)
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
		if value == "" {
			continue
		}
		add(value)
		base := filepath.Base(value)
		if base != value {
			add(base)
		}
		for _, candidate := range []string{value, base} {
			lower := strings.ToLower(candidate)
			if strings.HasSuffix(lower, ".cpa.json") {
				add(candidate[:len(candidate)-len(".cpa.json")])
				add(candidate[:len(candidate)-len(".json")])
				continue
			}
			if strings.HasSuffix(lower, ".json") {
				add(candidate[:len(candidate)-len(".json")])
			}
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

type quotaWindowSnapshot struct {
	Percent       sql.NullFloat64
	ResetAt       sql.NullInt64
	Source        string
	ObservedAt    int64
	ID            int64
	AuthIndex     string
	AuthID        string
	AuthFile      string
	AuthFileMTime int64
}

func queryLatestAccountWindowQuota(ctx context.Context, db *sql.DB, account accountRow, since int64, window string) quotaWindowSnapshot {
	snapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, []accountRow{account}, since, window)
	return snapshots[0]
}

func queryLatestAccountWindowQuotaSnapshots(ctx context.Context, db *sql.DB, accounts []accountRow, since int64, window string) map[int]quotaWindowSnapshot {
	out := make(map[int]quotaWindowSnapshot, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	index := newQuotaAccountIdentityIndex(accounts)
	percentColumn := "primary_used_percent"
	resetColumn := "primary_reset_at"
	if window == "secondary" {
		percentColumn = "secondary_used_percent"
		resetColumn = "secondary_reset_at"
	}
	query := `
SELECT source_type, id, observed_at, auth_index, auth_id, source, auth_file, auth_file_mtime, ` + percentColumn + `, ` + resetColumn + `
FROM (
  SELECT 'usage' AS source_type, id, requested_at AS observed_at, lower(auth_index) AS auth_index, lower(auth_id) AS auth_id, lower(source) AS source, '' AS auth_file, 0 AS auth_file_mtime, ` + percentColumn + `, ` + resetColumn + `
  FROM usage_events
  WHERE requested_at >= ?
  AND (` + trustedUsageQuotaSnapshotSQL() + `)
  AND (` + percentColumn + ` IS NOT NULL OR ` + resetColumn + ` IS NOT NULL)
  UNION ALL
  SELECT 'trigger' AS source_type, id, finished_at AS observed_at, lower(auth_index) AS auth_index, lower(auth_id) AS auth_id, lower(source) AS source, lower(auth_file) AS auth_file, auth_file_mtime, ` + percentColumn + `, ` + resetColumn + `
  FROM quota_trigger_runs
  WHERE finished_at >= ? AND status='success' AND (` + percentColumn + ` IS NOT NULL OR ` + resetColumn + ` IS NOT NULL)
) snapshots
ORDER BY observed_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, query, since, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	now := time.Now().Unix()
	for rows.Next() {
		var snapshot quotaWindowSnapshot
		var source string
		if err := rows.Scan(&snapshot.Source, &snapshot.ID, &snapshot.ObservedAt, &snapshot.AuthIndex, &snapshot.AuthID, &source, &snapshot.AuthFile, &snapshot.AuthFileMTime, &snapshot.Percent, &snapshot.ResetAt); err != nil {
			continue
		}
		if snapshot.ResetAt.Valid {
			snapshot.ResetAt.Int64 = normalizeUnixSeconds(snapshot.ResetAt.Int64)
			if snapshot.ResetAt.Int64 <= now {
				continue
			}
		}
		accountIndex, ok := index.match(accounts, snapshot)
		if !ok {
			continue
		}
		if _, exists := out[accountIndex]; !exists {
			out[accountIndex] = snapshot
		}
	}
	return out
}

type quotaAccountIdentityIndex struct {
	stable map[string][]int
	files  map[string][]int
}

func newQuotaAccountIdentityIndex(accounts []accountRow) quotaAccountIdentityIndex {
	index := quotaAccountIdentityIndex{
		stable: make(map[string][]int, len(accounts)*3),
		files:  make(map[string][]int, len(accounts)*2),
	}
	for i := range accounts {
		for _, alias := range stableQuotaIdentityAliases(accounts[i].AuthIndex, accounts[i].AuthID, accounts[i].ChatGPTAccountID) {
			index.stable[alias] = append(index.stable[alias], i)
		}
		for _, alias := range strictFileIdentityAliases(accounts[i].AuthFile) {
			index.files[alias] = append(index.files[alias], i)
		}
	}
	return index
}

func (index quotaAccountIdentityIndex) match(accounts []accountRow, snapshot quotaWindowSnapshot) (int, bool) {
	if matched, ok := uniqueQuotaIdentityMatch(index.stable, stableQuotaIdentityAliases(snapshot.AuthIndex, snapshot.AuthID)); ok {
		return matched, true
	}
	matched, ok := uniqueQuotaIdentityMatch(index.files, strictFileIdentityAliases(snapshot.AuthFile, snapshot.AuthIndex, snapshot.AuthID))
	if !ok || matched < 0 || matched >= len(accounts) {
		return 0, false
	}
	account := accounts[matched]
	switch snapshot.Source {
	case "trigger":
		if snapshot.AuthFileMTime <= 0 || account.AuthFileMTime <= 0 || snapshot.AuthFileMTime != account.AuthFileMTime {
			return 0, false
		}
		return matched, true
	case "usage":
		if account.Requests <= 0 {
			return 0, false
		}
		return matched, true
	default:
		return 0, false
	}
}

func uniqueQuotaIdentityMatch(index map[string][]int, aliases []string) (int, bool) {
	matched := -1
	for _, alias := range uniqueNonEmptyAliases(aliases) {
		candidates := index[alias]
		if len(candidates) != 1 {
			continue
		}
		if matched >= 0 && matched != candidates[0] {
			return 0, false
		}
		matched = candidates[0]
	}
	return matched, matched >= 0
}

func stableQuotaIdentityAliases(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(value, "@") || fileNameIfJSON(value) != "" {
			continue
		}
		out = append(out, normalizeAccountAlias(value))
	}
	return uniqueNonEmptyAliases(out)
}

func accountAliasIndex(accounts []accountRow) map[string][]int {
	index := make(map[string][]int, len(accounts)*4)
	for i := range accounts {
		seen := map[string]bool{}
		for _, alias := range accountAliases(accounts[i]) {
			if alias == "" || seen[alias] {
				continue
			}
			seen[alias] = true
			index[alias] = append(index[alias], i)
		}
	}
	return index
}

func accountQuotaAliasIndex(accounts []accountRow) map[string][]int {
	sets := accountQuotaAliasSets(accounts)
	index := make(map[string][]int, len(accounts)*4)
	for i, aliases := range sets {
		seen := map[string]bool{}
		for _, alias := range aliases {
			if alias == "" || seen[alias] {
				continue
			}
			seen[alias] = true
			index[alias] = append(index[alias], i)
		}
	}
	return index
}

func accountQuotaAliasSets(accounts []accountRow) [][]string {
	rawAliases := make([][]string, len(accounts))
	aliasCounts := make(map[string]int, len(accounts)*5)
	for i := range accounts {
		rawAliases[i] = uniqueNonEmptyAliases(append(
			stableQuotaIdentityAliases(accounts[i].AuthIndex, accounts[i].AuthID, accounts[i].ChatGPTAccountID),
			strictFileIdentityAliases(accounts[i].AuthFile)...,
		))
		for _, alias := range rawAliases[i] {
			aliasCounts[alias]++
		}
	}
	out := make([][]string, len(accounts))
	for i := range accounts {
		var aliases []string
		for _, alias := range rawAliases[i] {
			if aliasCounts[alias] == 1 {
				aliases = append(aliases, alias)
			}
		}
		out[i] = uniqueNonEmptyAliases(aliases)
	}
	return out
}

func strictFileIdentityAliases(values ...string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		base := filepath.Base(value)
		for _, candidate := range []string{value, base} {
			alias := normalizeAccountAlias(candidate)
			if alias == "" {
				continue
			}
			out = append(out, alias)
			lower := strings.ToLower(candidate)
			if strings.HasSuffix(lower, ".json") {
				out = append(out, normalizeAccountAlias(candidate[:len(candidate)-len(".json")]))
			}
		}
	}
	return uniqueNonEmptyAliases(out)
}

func uniqueNonEmptyAliases(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		alias := normalizeAccountAlias(value)
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		out = append(out, alias)
	}
	return out
}

func trustedUsageQuotaSnapshotSQL() string {
	return "((failed=0 AND (status_code=0 OR (status_code >= 200 AND status_code < 300))) OR status_code=429)"
}

func applyAccountQuotaSnapshot(account *accountRow, primary quotaWindowSnapshot, secondary quotaWindowSnapshot) {
	if primary.Percent.Valid {
		account.PrimaryUsedPercent = &primary.Percent.Float64
	}
	if primary.ResetAt.Valid {
		account.PrimaryResetAt = &primary.ResetAt.Int64
		account.PrimaryQuotaWindow = "5h"
	}
	if primary.Source != "" {
		account.PrimaryQuotaSource = primary.Source
		account.PrimaryQuotaObservedFrom = quotaObservedFrom(primary.Source)
	}
	if secondary.Percent.Valid {
		account.SecondaryUsedPercent = &secondary.Percent.Float64
	}
	if secondary.ResetAt.Valid {
		account.SecondaryResetAt = &secondary.ResetAt.Int64
	}
	if secondary.Source != "" {
		account.SecondaryQuotaSource = secondary.Source
		account.SecondaryQuotaObservedFrom = quotaObservedFrom(secondary.Source)
	}
}

func quotaObservedFrom(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "usage":
		return "response_header"
	case "trigger":
		return "quota_trigger"
	default:
		return source
	}
}

func applyAccountQuotaSource(account *accountRow) {
	if account.SecondaryQuotaEstimateSource != "" {
		account.QuotaSource = account.SecondaryQuotaEstimateSource
		account.QuotaCredibility = account.SecondaryQuotaEstimateSource
		return
	}
	if account.SecondaryQuotaSource != "" {
		account.QuotaSource = account.SecondaryQuotaSource
		account.QuotaCredibility = account.SecondaryQuotaSource
		return
	}
	if account.PrimaryQuotaSource != "" {
		account.QuotaSource = account.PrimaryQuotaSource
		account.QuotaCredibility = account.PrimaryQuotaSource
		return
	}
	account.QuotaSource = ""
	account.QuotaCredibility = ""
}

type accountTokenWindow struct {
	Start   int64
	End     int64
	Aliases []string
}

func accountTokenWindowForReset(reset sql.NullInt64, duration time.Duration, aliases []string) accountTokenWindow {
	if !reset.Valid || reset.Int64 <= 0 {
		return accountTokenWindow{}
	}
	end := normalizeUnixSeconds(reset.Int64)
	now := time.Now().Unix()
	if end <= 0 || end <= now {
		return accountTokenWindow{}
	}
	start := end - int64(duration.Seconds())
	if start > now {
		start = now - int64(duration.Seconds())
	}
	if start < 0 {
		start = 0
	}
	return accountTokenWindow{Start: start, End: end, Aliases: aliases}
}

func queryAccountWindowTokens(ctx context.Context, db *sql.DB, account accountRow, reset sql.NullInt64, duration time.Duration, aliases []string) int64 {
	window := accountTokenWindowForReset(reset, duration, aliases)
	if len(window.Aliases) == 0 {
		return 0
	}
	tokens := queryAccountWindowTokensBatch(ctx, db, []accountTokenWindow{window})
	if len(tokens) == 0 {
		return 0
	}
	return tokens[0]
}

func queryAccountWindowTokensBatch(ctx context.Context, db *sql.DB, windows []accountTokenWindow) []int64 {
	out := make([]int64, len(windows))
	if len(windows) == 0 {
		return out
	}
	aliasToWindows := map[string][]int{}
	minStart, maxEnd := int64(0), int64(0)
	for i, window := range windows {
		if window.End <= 0 || window.End < window.Start {
			continue
		}
		if minStart == 0 || window.Start < minStart {
			minStart = window.Start
		}
		if window.End > maxEnd {
			maxEnd = window.End
		}
		seen := map[string]bool{}
		for _, alias := range uniqueNonEmptyAliases(window.Aliases) {
			if alias == "" || seen[alias] {
				continue
			}
			seen[alias] = true
			aliasToWindows[alias] = append(aliasToWindows[alias], i)
		}
	}
	if minStart == 0 || maxEnd == 0 || len(aliasToWindows) == 0 {
		return out
	}
	aliases := make([]string, 0, len(aliasToWindows))
	for alias := range aliasToWindows {
		aliases = append(aliases, alias)
	}
	const chunkSize = 250
	for start := 0; start < len(aliases); start += chunkSize {
		end := start + chunkSize
		if end > len(aliases) {
			end = len(aliases)
		}
		chunk := aliases[start:end]
		placeholders := sqlPlaceholders(len(chunk))
		args := make([]any, 0, 2+len(chunk)*3)
		args = append(args, minStart, maxEnd)
		for i := 0; i < 3; i++ {
			for _, alias := range chunk {
				args = append(args, alias)
			}
		}
		rows, err := db.QueryContext(ctx, `
SELECT requested_at, total_tokens, lower(auth_index), lower(auth_id), lower(source)
FROM usage_events
WHERE requested_at >= ? AND requested_at <= ?
AND (
  (auth_index <> '' AND lower(auth_index) IN (`+placeholders+`))
  OR (auth_id <> '' AND lower(auth_id) IN (`+placeholders+`))
  OR (source <> '' AND lower(source) IN (`+placeholders+`))
)`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var ts, tokens int64
			var authIndex, authID, source string
			if err := rows.Scan(&ts, &tokens, &authIndex, &authID, &source); err != nil {
				continue
			}
			matched := map[int]bool{}
			for _, alias := range []string{authIndex, authID, source} {
				for _, windowIndex := range aliasToWindows[normalizeAccountAlias(alias)] {
					if matched[windowIndex] {
						continue
					}
					window := windows[windowIndex]
					if ts < window.Start || ts > window.End {
						continue
					}
					out[windowIndex] += tokens
					matched[windowIndex] = true
				}
			}
		}
		_ = rows.Close()
	}
	return out
}

func queryAccountTokensBetween(ctx context.Context, db *sql.DB, account accountRow, start int64, end int64, aliases []string) int64 {
	if end <= 0 {
		end = time.Now().Unix()
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		return 0
	}
	if len(aliases) == 0 {
		aliases = accountAliases(account)
	}
	if len(aliases) == 0 {
		return 0
	}
	placeholders := sqlPlaceholders(len(aliases))
	args := make([]any, 0, 2+len(aliases)*3)
	args = append(args, start, end)
	for i := 0; i < 3; i++ {
		for _, alias := range aliases {
			args = append(args, alias)
		}
	}
	var total int64
	_ = db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(total_tokens),0)
FROM usage_events
WHERE requested_at >= ? AND requested_at <= ?
AND (
  (auth_index <> '' AND lower(auth_index) IN (`+placeholders+`))
  OR (auth_id <> '' AND lower(auth_id) IN (`+placeholders+`))
  OR (source <> '' AND lower(source) IN (`+placeholders+`))
)`, args...).Scan(&total)
	return total
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
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

func secondaryQuotaWindowSource(account accountRow, reset sql.NullInt64, duration time.Duration) string {
	if isFreePlan(account.PlanType) {
		return "plan_type"
	}
	if reset.Valid && duration >= 28*24*time.Hour {
		return "reset_duration"
	}
	return "default_7d"
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
  AND (`+trustedUsageQuotaSnapshotSQL()+`)
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

func externalUseScanSince(summarySince int64, now int64) int64 {
	return maxInt64(summarySince, now-int64((24*time.Hour)/time.Second))
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
	inventory := make([]configuredAccount, len(accounts))
	for i := range accounts {
		inventory[i] = configuredAccount{
			AuthID:    accounts[i].AuthID,
			AuthIndex: accounts[i].AuthIndex,
			AuthFile:  accounts[i].AuthFile,
			Source:    accounts[i].Source,
			Email:     accounts[i].Email,
		}
	}
	identityIndex := newCodexAuthIdentityIndex(inventory)
	for _, invalid := range invalids {
		if i, ok := identityIndex.matchIndex(invalid); ok {
			applyInvalidAuthToAccount(&accounts[i], invalid)
		}
	}
}

func applyInvalidAuthToAccount(account *accountRow, invalid invalidAuthRow) {
	if account == nil {
		return
	}
	if invalidAuthIsWorkspaceDeactivated(invalid) {
		account.WorkspaceDeactivated = true
		account.WorkspaceDeactivatedAt = invalid.InvalidatedAtText
		account.WorkspaceDeactivatedReason = invalid.Reason
	} else {
		account.InvalidAuth = true
		account.InvalidAuthAt = invalid.InvalidatedAtText
		account.InvalidAuthReason = invalid.Reason
		account.InvalidAuthStatusCode = invalid.LastStatusCode
	}
}

func invalidAuthIsWorkspaceDeactivated(row invalidAuthRow) bool {
	if row.LastStatusCode == http.StatusPaymentRequired {
		return true
	}
	return strings.Contains(strings.ToLower(row.Reason), "deactivated_workspace")
}

func filterUnauthorizedInvalidAuths(rows []invalidAuthRow) []invalidAuthRow {
	if len(rows) == 0 {
		return rows
	}
	out := make([]invalidAuthRow, 0, len(rows))
	for _, row := range rows {
		if row.LastStatusCode == http.StatusUnauthorized {
			out = append(out, row)
		}
	}
	return out
}

func filterForbiddenInvalidAuths(rows []invalidAuthRow) []invalidAuthRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]invalidAuthRow, 0, len(rows))
	for _, row := range rows {
		if row.LastStatusCode == http.StatusForbidden {
			out = append(out, row)
		}
	}
	return out
}

func filterWorkspaceDeactivatedAuths(rows []invalidAuthRow) []invalidAuthRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]invalidAuthRow, 0, len(rows))
	for _, row := range rows {
		if invalidAuthIsWorkspaceDeactivated(row) {
			out = append(out, row)
		}
	}
	return out
}

func applyLatestQuotaSnapshots(ctx context.Context, db *sql.DB, accounts []accountRow, since int64) {
	primarySnapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, accounts, since, "primary")
	secondarySnapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, accounts, since, "secondary")
	quotaAliases := accountQuotaAliasSets(accounts)
	primaryWindows := make([]accountTokenWindow, len(accounts))
	secondaryWindows := make([]accountTokenWindow, len(accounts))
	secondaryDurations := make([]time.Duration, len(accounts))
	for i := range accounts {
		primary := primarySnapshots[i]
		secondary := secondarySnapshots[i]
		primary, secondary = moveMonthlyPrimaryQuotaToSecondary(accounts[i], primary, secondary)
		if accounts[i].Requests <= 0 {
			primary = quotaWindowSnapshot{}
			secondary = quotaWindowSnapshot{}
		}
		secondaryDurations[i] = secondaryQuotaDuration(accounts[i], secondary.ResetAt)
		accounts[i].PrimaryUsedPercent = nil
		accounts[i].PrimaryResetAt = nil
		accounts[i].PrimaryQuotaWindow = ""
		accounts[i].PrimaryQuotaSource = ""
		accounts[i].PrimaryQuotaObservedFrom = ""
		accounts[i].SecondaryUsedPercent = nil
		accounts[i].SecondaryResetAt = nil
		accounts[i].SecondaryQuotaSource = ""
		accounts[i].SecondaryQuotaObservedFrom = ""
		accounts[i].PrimaryWindowTokens = 0
		accounts[i].SecondaryWindowTokens = 0
		applyAccountQuotaSnapshot(&accounts[i], primary, secondary)
		primaryWindows[i] = accountTokenWindowForReset(primary.ResetAt, 5*time.Hour, quotaAliases[i])
		secondaryWindows[i] = accountTokenWindowForReset(secondary.ResetAt, secondaryDurations[i], quotaAliases[i])
	}
	primaryTokens := queryAccountWindowTokensBatch(ctx, db, primaryWindows)
	secondaryTokens := queryAccountWindowTokensBatch(ctx, db, secondaryWindows)
	for i := range accounts {
		if i < len(primaryTokens) {
			accounts[i].PrimaryWindowTokens = primaryTokens[i]
		}
		if i < len(secondaryTokens) {
			accounts[i].SecondaryWindowTokens = secondaryTokens[i]
		}
		secondary := secondarySnapshots[i]
		if primary, ok := primarySnapshots[i]; ok {
			primary, secondary = moveMonthlyPrimaryQuotaToSecondary(accounts[i], primary, secondary)
			_ = primary
		}
		accounts[i].SecondaryQuotaWindow = secondaryQuotaWindowLabel(secondaryDurations[i])
		accounts[i].QuotaWindowSource = secondaryQuotaWindowSource(accounts[i], secondary.ResetAt, secondaryDurations[i])
		applyAccountQuotaSource(&accounts[i])
	}
}

func moveMonthlyPrimaryQuotaToSecondary(account accountRow, primary quotaWindowSnapshot, secondary quotaWindowSnapshot) (quotaWindowSnapshot, quotaWindowSnapshot) {
	if !primary.Percent.Valid || !primary.ResetAt.Valid || secondary.ResetAt.Valid {
		return primary, secondary
	}
	resetAt := normalizeUnixSeconds(primary.ResetAt.Int64)
	if resetAt-time.Now().Unix() <= int64(8*24*time.Hour/time.Second) {
		return primary, secondary
	}
	secondary = primary
	secondary.ResetAt = sql.NullInt64{Int64: resetAt, Valid: true}
	primary = quotaWindowSnapshot{}
	return primary, secondary
}

func queryRecentQuotaTriggerRuns(ctx context.Context, db *sql.DB, limit int) ([]quotaTriggerAccountStatus, error) {
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, auth_file, auth_file_mtime, mode, status, http_status, error, finished_at
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
		if err := rows.Scan(&r.AuthID, &r.AuthIndex, &r.Source, &r.AuthFile, &r.AuthFileMTime, &r.Mode, &r.Status, &r.HTTPStatus, &r.Error, &r.FinishedAt); err != nil {
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
	index := newQuotaAccountIdentityIndex(accounts)
	seen := map[int]bool{}
	for _, run := range runs {
		i, ok := index.match(accounts, quotaWindowSnapshot{
			Source: "trigger", AuthID: run.AuthID, AuthIndex: run.AuthIndex,
			AuthFile: run.AuthFile, AuthFileMTime: run.AuthFileMTime,
		})
		if !ok || seen[i] {
			continue
		}
		seen[i] = true
		accounts[i].QuotaTriggerLastAt = unixTime(run.FinishedAt)
		accounts[i].QuotaTriggerStatus = run.Status
		accounts[i].QuotaTriggerMode = run.Mode
		accounts[i].QuotaTriggerHTTPStatus = run.HTTPStatus
		accounts[i].QuotaTriggerError = sanitizeTriggerError(run.Error)
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
		for _, alias := range normalizeAccountAliases(bans[i].AuthID, bans[i].AuthIndex, bans[i].Source, bans[i].AuthFile) {
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
	capacities := latestSecondaryQuotaTriggerCapacities(ctx, db, accounts, since)
	quotaAliases := accountQuotaAliasSets(accounts)
	for i := range accounts {
		accounts[i].SecondaryQuotaEstimateSource = ""
		accounts[i].SecondaryQuotaEstimateMethod = ""
		accounts[i].QuotaEstimateNote = ""
		total, remaining := int64(0), int64(0)
		if accounts[i].Requests <= 0 {
			applyAccountQuotaSource(&accounts[i])
			continue
		}
		if capacity, ok := capacities[i]; ok {
			total, remaining = adjustedSecondaryQuotaTriggerCapacity(ctx, db, accounts[i], capacity, quotaAliases[i])
		}
		if total <= 0 {
			total, remaining = estimateQuotaFromUsedPercent(accounts[i].SecondaryWindowTokens, accounts[i].SecondaryUsedPercent)
			if total > 0 {
				accounts[i].SecondaryQuotaEstimateSource = "estimated"
				accounts[i].SecondaryQuotaEstimateMethod = "local_tokens_percent_estimate"
			}
		} else {
			accounts[i].SecondaryQuotaEstimateSource = "trigger"
			accounts[i].SecondaryQuotaEstimateMethod = "quota_trigger_capacity"
		}
		if total <= 0 {
			if accounts[i].SecondaryUsedPercent != nil && accounts[i].SecondaryWindowTokens <= 0 {
				accounts[i].QuotaCredibility = "insufficient_local_tokens"
				accounts[i].SecondaryQuotaEstimateMethod = "not_enough_local_tokens"
				accounts[i].QuotaEstimateNote = "无足够本地 token，无法估算总额"
			}
			applyAccountQuotaSource(&accounts[i])
			continue
		}
		accounts[i].SecondaryQuotaTotalEstimate = total
		accounts[i].SecondaryQuotaRemainingEstimate = remaining
		totals.SecondaryQuotaTotalEstimate += total
		totals.SecondaryQuotaRemainingEstimate += remaining
		estimatedAccounts++
		applyAccountQuotaSource(&accounts[i])
	}
	totals.SecondaryQuotaEstimatedAccounts = estimatedAccounts
}

type secondaryQuotaCapacitySnapshot struct {
	Total      int64
	Remaining  int64
	ResetAt    sql.NullInt64
	FinishedAt int64
}

func latestSecondaryQuotaTriggerCapacity(ctx context.Context, db *sql.DB, account accountRow, since int64) (int64, int64) {
	capacities := latestSecondaryQuotaTriggerCapacities(ctx, db, []accountRow{account}, since)
	capacity, ok := capacities[0]
	if !ok {
		return 0, 0
	}
	return adjustedSecondaryQuotaTriggerCapacity(ctx, db, account, capacity, nil)
}

func latestSecondaryQuotaTriggerCapacities(ctx context.Context, db *sql.DB, accounts []accountRow, since int64) map[int]secondaryQuotaCapacitySnapshot {
	out := make(map[int]secondaryQuotaCapacitySnapshot, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	index := newQuotaAccountIdentityIndex(accounts)
	rows, err := db.QueryContext(ctx, `
SELECT auth_index, auth_id, source, auth_file, auth_file_mtime, secondary_limit_tokens, secondary_remaining_tokens, secondary_reset_at, finished_at
FROM quota_trigger_runs
WHERE finished_at >= ?
AND status='success'
AND (secondary_limit_tokens IS NOT NULL OR secondary_remaining_tokens IS NOT NULL)
ORDER BY finished_at DESC, id DESC`, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	now := time.Now().Unix()
	for rows.Next() {
		var snapshot quotaWindowSnapshot
		var source string
		var limit, remaining sql.NullInt64
		var reset sql.NullInt64
		var finishedAt int64
		if err := rows.Scan(&snapshot.AuthIndex, &snapshot.AuthID, &source, &snapshot.AuthFile, &snapshot.AuthFileMTime, &limit, &remaining, &reset, &finishedAt); err != nil {
			continue
		}
		if reset.Valid {
			reset.Int64 = normalizeUnixSeconds(reset.Int64)
			if reset.Int64 <= now {
				continue
			}
		}
		if !limit.Valid || limit.Int64 <= 0 || !remaining.Valid || remaining.Int64 < 0 || remaining.Int64 > limit.Int64 {
			continue
		}
		capacity := secondaryQuotaCapacitySnapshot{
			Total:      limit.Int64,
			Remaining:  remaining.Int64,
			ResetAt:    reset,
			FinishedAt: finishedAt,
		}
		snapshot.Source = "trigger"
		accountIndex, ok := index.match(accounts, snapshot)
		if !ok {
			continue
		}
		if _, exists := out[accountIndex]; !exists {
			out[accountIndex] = capacity
		}
	}
	return out
}

func adjustedSecondaryQuotaTriggerCapacity(ctx context.Context, db *sql.DB, account accountRow, capacity secondaryQuotaCapacitySnapshot, aliases []string) (int64, int64) {
	total := capacity.Total
	remain := capacity.Remaining
	if total <= 0 || remain > total {
		return 0, 0
	}
	duration := secondaryQuotaDuration(account, capacity.ResetAt)
	windowStart := normalizeUnixSeconds(capacity.ResetAt.Int64) - int64(duration.Seconds())
	if windowStart < 0 {
		windowStart = 0
	}
	start := capacity.FinishedAt
	if start < windowStart {
		start = windowStart
	}
	if usedAfterSnapshot := queryAccountTokensBetween(ctx, db, account, start+1, time.Now().Unix(), aliases); usedAfterSnapshot > 0 {
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
COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END),0),
COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END),0),
` + throughputSQL() + `,
COALESCE(SUM(CASE WHEN latency_ms >= 12000 THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ttft_ms >= 3000 THEN 1 ELSE 0 END),0),
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
			&r.TotalTokens, &r.AverageLatencyMs, &r.AverageTTFTMs, &r.OutputTokensPerSecond,
			&r.SlowRequests, &r.SlowTTFTRequests, &r.Accounts, &r.Models, &last,
		); err != nil {
			return nil, err
		}
		r.LastSeen = unixTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryKeySummaries(ctx context.Context, db *sql.DB, since int64, limit int) ([]keySummaryRow, error) {
	query := `
SELECT raw_key, protocol_key,
COUNT(DISTINCT provider_key), GROUP_CONCAT(DISTINCT provider_key),
COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(CASE WHEN status_code=429 THEN 1 ELSE 0 END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0),
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0),
COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END),0),
COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END),0),
` + throughputSQL() + `,
COALESCE(SUM(CASE WHEN latency_ms >= 12000 THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ttft_ms >= 3000 THEN 1 ELSE 0 END),0),
COUNT(DISTINCT NULLIF(model,'')),
MAX(requested_at)
FROM (
  SELECT api_key AS raw_key, ` + keyProtocolSQL() + ` AS protocol_key, ` + cpaProviderSQL() + ` AS provider_key,
    requested_at, failed, status_code, input_tokens, output_tokens, reasoning_tokens,
    cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    latency_ms, ttft_ms, output_tokens, model
  FROM usage_events
  WHERE requested_at >= ? AND api_key <> ''
) keyed
GROUP BY raw_key, protocol_key
ORDER BY SUM(total_tokens) DESC, COUNT(*) DESC
LIMIT ?`
	rows, err := db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keySummaryRow
	for rows.Next() {
		var r keySummaryRow
		var providers string
		var last int64
		if err := rows.Scan(
			&r.RawKeyID, &r.Protocol, &r.Providers, &providers, &r.Requests, &r.Failed, &r.RateLimited,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
			&r.TotalTokens, &r.AverageLatencyMs, &r.AverageTTFTMs, &r.OutputTokensPerSecond, &r.SlowRequests,
			&r.SlowTTFTRequests, &r.Models, &last,
		); err != nil {
			return nil, err
		}
		r.KeyID = safeExportLabel(r.RawKeyID)
		r.ProviderNames = normalizeKeyProviderNames(providers)
		r.Provider = firstKeyProviderName(r.ProviderNames)
		r.QuotaUsedTokens = r.TotalTokens
		r.LastSeen = unixTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

func keyProtocolSQL() string {
	return `CASE
WHEN lower(auth_type) IN ('apikey', 'api_key', 'key', 'api-key') THEN 'apikey'
WHEN lower(auth_type) IN ('oauth', 'codex') THEN lower(auth_type)
WHEN lower(executor_type) LIKE '%codex%' THEN 'codex'
WHEN lower(executor_type) LIKE '%claude%' OR lower(provider) IN ('anthropic','claude') THEN 'claude'
WHEN lower(executor_type) LIKE '%gemini%' OR lower(provider) LIKE 'gemini%' THEN 'gemini'
WHEN lower(provider) LIKE 'openai%' THEN 'openai-compatible'
ELSE COALESCE(NULLIF(lower(auth_type),''), NULLIF(lower(provider),''), 'unknown')
END`
}

func keySummaryGroupKey(rawKey, protocol string) string {
	return normalizeAccountAlias(rawKey) + "\x00" + strings.ToLower(strings.TrimSpace(protocol))
}

func normalizeKeyProviderNames(value string) string {
	parts := strings.Split(value, ",")
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := normalizeAccountAlias(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, part)
	}
	sort.Strings(out)
	return strings.Join(out, " / ")
}

func firstKeyProviderName(value string) string {
	parts := strings.Split(value, " / ")
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			return strings.TrimSpace(part)
		}
	}
	return ""
}

func cpaProviderSQL() string {
	return cpaProviderSQLWithEntries(readConfiguredProviderEntries())
}

func cpaProviderSQLWithEntries(entries []providerConfigEntry) string {
	tail := "substr(auth_id, length('openai-compatibility:') + 1)"
	var apiKeyCases strings.Builder
	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		apiKey := strings.TrimSpace(entry.APIKey)
		name := strings.TrimSpace(entry.Name)
		if provider == "" || apiKey == "" || name == "" {
			continue
		}
		if !providerSupportsAPIKeyEndpointName(provider) {
			continue
		}
		apiKeyCases.WriteString("WHEN ")
		apiKeyCases.WriteString(providerSQLPredicate(provider))
		apiKeyCases.WriteString(" AND lower(auth_type) IN ('apikey', 'api_key', 'key') AND source = ")
		apiKeyCases.WriteString(sqlQuote(apiKey))
		apiKeyCases.WriteString(" THEN ")
		apiKeyCases.WriteString(sqlQuote(name))
		apiKeyCases.WriteString("\n")
	}
	return `CASE
WHEN auth_id LIKE 'openai-compatibility:%:%' THEN substr(` + tail + `, 1, instr(` + tail + `, ':') - 1)
WHEN provider LIKE 'openai-compatible-%' THEN substr(provider, length('openai-compatible-') + 1)
WHEN provider LIKE 'openai-compatibility-%' THEN substr(provider, length('openai-compatibility-') + 1)
WHEN lower(provider) IN ('openai-compatible', 'openai-compatibility') AND source <> '' AND source NOT LIKE 'sk-%' AND source NOT LIKE 'ark-%' AND source NOT LIKE 'Bearer %' THEN source
` + apiKeyCases.String() + `
WHEN lower(provider) IN ('anthropic', 'claude') THEN 'Claude'
WHEN lower(provider) LIKE 'gemini%' THEN 'Gemini'
WHEN lower(provider) = 'codex' THEN 'Codex'
ELSE COALESCE(NULLIF(provider,''), 'unknown')
END`
}

func providerSupportsAPIKeyEndpointName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude", "gemini", "antigravity":
		return true
	default:
		return false
	}
}

func providerSQLPredicate(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "lower(provider) IN ('anthropic', 'claude')"
	case "gemini":
		return "lower(provider) LIKE 'gemini%'"
	default:
		return "lower(provider) = " + sqlQuote(strings.ToLower(strings.TrimSpace(provider)))
	}
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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
COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(AVG(CASE WHEN latency_ms > 0 THEN latency_ms END),0),
COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END),0),
` + throughputSQL() + `,
COALESCE(SUM(CASE WHEN latency_ms >= 12000 THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ttft_ms >= 3000 THEN 1 ELSE 0 END),0)
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
			&r.AverageLatencyMs, &r.AverageTTFTMs, &r.OutputTokensPerSecond, &r.SlowRequests, &r.SlowTTFTRequests,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

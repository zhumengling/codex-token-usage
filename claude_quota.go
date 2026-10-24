package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Claude subscription quota estimation.
//
// Anthropic reports subscription usage only as a utilization fraction per
// window, in the anthropic-ratelimit-unified-<window>-utilization / -reset
// response headers that CPA forwards with every Claude usage record. The
// sampler stores a reading whenever a window's value changes; the estimator
// relates those readings to the API-priced cost of the same account's
// requests since the window started:
//
//	utilization% = (base + k*cacheRead) / limit * 100
//
// base is input, output and cache-write cost at API prices; cacheRead is the
// cache-hit cost at API prices. The "api" tier fixes k=1. The "fitted" tier
// solves for k by least squares, k being the share of the API cache-hit price
// the subscription actually charges.
//
// Anthropic can change the limit or the price of cache hits at any time, and a
// fit over steps on both sides of such a change blends the two. The estimator
// looks for the most recent change and fits only the steps after it.
//
// The subscription can also be used outside CPA, directly or on claude.ai.
// That usage raises the utilization with no cost to show for it, so the
// estimator leaves out the steps it falls in: the shortest unscoped window
// finds them by how far they rise above the fit, and the account's other
// windows, whose steps are too coarse for that, leave out each step that
// overlaps one of them or that the shortest window has no steps for.
//
// None of this may disturb CPA: sampling runs on the asynchronous usage queue,
// never fails the usage record and recovers from panics; estimation runs only
// when the management route is read, and its result is cached.

const (
	claudeUnifiedHeaderPrefix      = "anthropic-ratelimit-unified-"
	claudeQuotaSampleHeartbeatMs   = int64(10 * 60 * 1000)
	claudeQuotaFitLookback         = 28 * 24 * time.Hour
	claudeQuotaReportTTL           = 60 * time.Second
	claudeQuotaComputeTimeout      = 20 * time.Second
	claudeQuotaMinRegressionPoints = 3
	claudeQuotaMinFitPoints        = 6
	claudeQuotaTopModels           = 5
	claudeQuotaChangeMinPoints     = 20
	claudeQuotaChangeMinWald       = 100.0
	claudeQuotaChangeMinShift      = 0.1
	claudeQuotaScreenBlockSteps    = 5
	claudeQuotaScreenSigmas        = 3.0
	claudeQuotaScreenMinExcess     = 1.0 // percentage points; Anthropic reports whole percent
	claudeQuotaScreenRounds        = 10
	claudeQuotaScreenFitIterations = 30
	claudeQuotaScreenPersist       = 12 * time.Hour
	claudeQuotaScreenRunShare      = 0.8
)

type claudeWindowReading struct {
	Window      string
	Utilization float64 // percent
	ResetAt     int64   // unix seconds
	Status      string
}

// parseClaudeUnifiedWindows reads every unified window that reports a
// utilization, such as 5h, 7d or 7d_sonnet.
func parseClaudeUnifiedWindows(headers map[string][]string) []claudeWindowReading {
	values := map[string]string{}
	for key, list := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(lower, claudeUnifiedHeaderPrefix) || len(list) == 0 {
			continue
		}
		values[strings.TrimPrefix(lower, claudeUnifiedHeaderPrefix)] = strings.TrimSpace(list[0])
	}
	var out []claudeWindowReading
	for key, raw := range values {
		window, ok := strings.CutSuffix(key, "-utilization")
		if !ok {
			continue
		}
		if _, _, ok := claudeWindowSpec(window); !ok {
			continue
		}
		fraction, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 || fraction > 10 {
			continue
		}
		reading := claudeWindowReading{
			Window:      window,
			Utilization: math.Round(fraction*100*1e6) / 1e6,
			Status:      values[window+"-status"],
		}
		if reset, err := strconv.ParseFloat(values[window+"-reset"], 64); err == nil && reset > 0 {
			reading.ResetAt = normalizeUnixSeconds(int64(reset))
		}
		out = append(out, reading)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out
}

// claudeWindowSpec splits a unified window name into its duration in seconds
// and optional scope: "7d" → (604800, ""), "7d_sonnet" → (604800, "sonnet").
func claudeWindowSpec(window string) (int64, string, bool) {
	base, scope, _ := strings.Cut(window, "_")
	if len(base) < 2 {
		return 0, "", false
	}
	n, err := strconv.ParseInt(base[:len(base)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	var unit int64
	switch base[len(base)-1] {
	case 'm':
		unit = 60
	case 'h':
		unit = 3600
	case 'd':
		unit = 86400
	default:
		return 0, "", false
	}
	return n * unit, scope, true
}

// claudeScopeModelFamily returns the model family a scoped window counts.
// Scopes that are not a model family (7d_oi, for example) are shown but not
// estimated, because it is unknown which requests they count.
func claudeScopeModelFamily(scope string) (string, bool) {
	switch scope {
	case "":
		return "", true
	case "opus", "sonnet", "haiku", "fable":
		return scope, true
	}
	return "", false
}

// claudeQuotaAccountKey groups usage by subscription. Several CPA auth files
// can hold the same login, and they share one quota, so the login comes first.
func claudeQuotaAccountKey(source, authIndex, authID string) string {
	for _, value := range []string{source, authIndex, authID} {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			return value
		}
	}
	return ""
}

func isClaudeSubscriptionUsage(provider, authType string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "claude") && !isAPIKeyAuthType(authType)
}

// claudeObservedAtMs approximates when the response headers arrived.
func claudeObservedAtMs(rec usageRecord) int64 {
	at := rec.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	offset := durationToMilliseconds(rec.TTFT)
	if offset <= 0 {
		offset = durationToMilliseconds(rec.Latency)
	}
	return at.UnixMilli() + offset
}

type claudeQuotaSampleState struct {
	Utilization float64
	ResetAt     int64
	ObservedMs  int64
}

type claudeQuotaSampler struct {
	mu        sync.Mutex
	enabled   bool
	last      map[string]claudeQuotaSampleState
	stored    int64
	lastError string
}

type claudeQuotaSamplerStatus struct {
	Enabled   bool   `json:"enabled"`
	Stored    int64  `json:"stored_since_start"`
	LastError string `json:"last_error,omitempty"`
}

var globalClaudeQuota = &claudeQuotaSampler{enabled: true}

func (s *claudeQuotaSampler) configure(cfg pluginConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = cfg.ClaudeQuotaEnabled
}

func (s *claudeQuotaSampler) status() claudeQuotaSamplerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return claudeQuotaSamplerStatus{Enabled: s.enabled, Stored: s.stored, LastError: s.lastError}
}

func (s *claudeQuotaSampler) setError(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = sanitizeTriggerError(message)
}

// record stores the unified windows carried by a Claude usage record. It is
// best effort: errors and panics end up in the sampler status instead of
// failing the usage record. A window is written only when its value or reset
// changes, plus a heartbeat every ten minutes.
func (s *claudeQuotaSampler) record(ctx context.Context, db *sql.DB, rec usageRecord) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.setError(fmt.Sprintf("panic: %v", recovered))
		}
	}()
	if db == nil || !isClaudeSubscriptionUsage(rec.Provider, rec.AuthType) {
		return
	}
	readings := parseClaudeUnifiedWindows(rec.ResponseHeaders)
	if len(readings) == 0 {
		return
	}
	account := claudeQuotaAccountKey(rec.Source, rec.AuthIndex, rec.AuthID)
	if account == "" {
		return
	}
	observedMs := claudeObservedAtMs(rec)
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return
	}
	if s.last == nil {
		s.last = map[string]claudeQuotaSampleState{}
	}
	var due []claudeWindowReading
	for _, reading := range readings {
		key := account + "\x00" + reading.Window
		prev, seen := s.last[key]
		if seen && prev.Utilization == reading.Utilization && prev.ResetAt == reading.ResetAt && observedMs-prev.ObservedMs < claudeQuotaSampleHeartbeatMs {
			continue
		}
		s.last[key] = claudeQuotaSampleState{Utilization: reading.Utilization, ResetAt: reading.ResetAt, ObservedMs: observedMs}
		due = append(due, reading)
	}
	s.mu.Unlock()
	for _, reading := range due {
		_, err := db.ExecContext(ctx, `
INSERT INTO claude_quota_samples (account, auth_index, auth_id, quota_window, observed_at_ms, utilization, reset_at, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			account, trim(rec.AuthIndex), trim(rec.AuthID), reading.Window, observedMs, reading.Utilization, reading.ResetAt, trim(reading.Status))
		s.mu.Lock()
		if err != nil {
			// Forget the reading so the next record retries it.
			delete(s.last, account+"\x00"+reading.Window)
			s.lastError = sanitizeTriggerError(err.Error())
		} else {
			s.stored++
		}
		s.mu.Unlock()
	}
}

type claudeQuotaReport struct {
	GeneratedAt  string                   `json:"generated_at"`
	LookbackDays int                      `json:"lookback_days"`
	Sampler      claudeQuotaSamplerStatus `json:"sampler"`
	Accounts     []claudeQuotaAccount     `json:"accounts"`
}

type claudeQuotaAccount struct {
	Account string              `json:"account"`
	Windows []claudeQuotaWindow `json:"windows"`
}

type claudeQuotaWindow struct {
	Window               string                  `json:"window"`
	Scope                string                  `json:"scope,omitempty"`
	DurationSeconds      int64                   `json:"duration_seconds"`
	Utilization          float64                 `json:"utilization"`
	Status               string                  `json:"status,omitempty"`
	ResetAt              int64                   `json:"reset_at"`
	ObservedAt           int64                   `json:"observed_at"`
	ResetPassed          bool                    `json:"reset_passed"`
	Estimated            bool                    `json:"estimated"`
	Note                 string                  `json:"note,omitempty"`
	CycleStart           int64                   `json:"cycle_start,omitempty"`
	CycleBaseCost        float64                 `json:"cycle_base_cost"`
	CycleCacheReadCost   float64                 `json:"cycle_cache_read_cost"`
	CycleCacheReadTokens int64                   `json:"cycle_cache_read_tokens"`
	UnpricedTokens       int64                   `json:"unpriced_tokens"`
	Samples              int                     `json:"samples"`
	Cycles               int                     `json:"cycles"`
	Points               int                     `json:"points"`
	ScreenedSteps        int                     `json:"screened_steps"`
	Change               *claudeQuotaChange      `json:"change,omitempty"`
	API                  claudeQuotaTier         `json:"api"`
	Fitted               claudeQuotaTier         `json:"fitted"`
	Models               []claudeQuotaModelPrice `json:"models,omitempty"`
}

// claudeQuotaChange is the most recent change in how fast requests use up a
// window. Both tiers fit only the steps from At on. RateRatio is how much of
// the window the steps since At used under the fit after the change, as a
// share of what the fit before it gives for the same requests.
type claudeQuotaChange struct {
	At                      int64    `json:"at"`
	DroppedPoints           int      `json:"dropped_points"`
	RateRatio               float64  `json:"rate_ratio"`
	PreviousCachePriceRatio *float64 `json:"previous_cache_price_ratio,omitempty"`
}

// claudeQuotaTier is one way of pricing cache hits. CachePriceRatio is k, the
// cache-hit price as a share of the API cache-read price. OtherUSD is the part
// of the current cycle's utilization that CPA's requests do not account for:
// usage of the subscription outside CPA.
type claudeQuotaTier struct {
	Available         bool    `json:"available"`
	Reason            string  `json:"reason,omitempty"`
	Method            string  `json:"method,omitempty"`
	CachePriceRatio   float64 `json:"cache_price_ratio"`
	CachePriceRatioSE float64 `json:"cache_price_ratio_se,omitempty"`
	Clamped           bool    `json:"clamped,omitempty"`
	Confidence        string  `json:"confidence,omitempty"`
	LimitUSD          float64 `json:"limit_usd"`
	LimitUSDSE        float64 `json:"limit_usd_se,omitempty"`
	CycleCostUSD      float64 `json:"cycle_cost_usd"`
	RemainingUSD      float64 `json:"remaining_usd"`
	OtherUSD          float64 `json:"other_usd"`
	Points            int     `json:"points"`
	RMSE              float64 `json:"rmse,omitempty"`
}

type claudeQuotaModelPrice struct {
	Model               string  `json:"model"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	InputPerM           float64 `json:"input_per_m"`
	APICacheReadPerM    float64 `json:"api_cache_read_per_m"`
	FittedCacheReadPerM float64 `json:"fitted_cache_read_per_m,omitempty"`
}

type claudeQuotaSample struct {
	ObservedMs  int64
	Utilization float64
	ResetAt     int64
	Status      string
}

type claudeCostEvent struct {
	DoneMs          int64
	Model           string
	Base            float64
	CacheRead       float64
	CacheReadTokens int64
	Unpriced        int64
}

// claudeCostSeries holds one account's priced requests in completion order
// with prefix sums, so the cost of any time range is two binary searches.
type claudeCostSeries struct {
	events   []claudeCostEvent
	cumBase  []float64
	cumCache []float64
}

func newClaudeCostSeries(events []claudeCostEvent) *claudeCostSeries {
	s := &claudeCostSeries{events: events, cumBase: make([]float64, len(events)+1), cumCache: make([]float64, len(events)+1)}
	for i, event := range events {
		s.cumBase[i+1] = s.cumBase[i] + event.Base
		s.cumCache[i+1] = s.cumCache[i] + event.CacheRead
	}
	return s
}

func (s *claudeCostSeries) index(ms int64) int {
	return sort.Search(len(s.events), func(i int) bool { return s.events[i].DoneMs >= ms })
}

// sums returns the base and cache-read cost of requests completed in [from, to).
func (s *claudeCostSeries) sums(fromMs, toMs int64) (float64, float64) {
	from, to := s.index(fromMs), s.index(toMs)
	if to <= from {
		return 0, 0
	}
	return s.cumBase[to] - s.cumBase[from], s.cumCache[to] - s.cumCache[from]
}

type claudeQuotaEstimator struct {
	mu     sync.Mutex
	at     time.Time
	report claudeQuotaReport
}

var globalClaudeQuotaEstimator = &claudeQuotaEstimator{}

func (e *claudeQuotaEstimator) get(ctx context.Context, refresh bool) (claudeQuotaReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !refresh && !e.at.IsZero() && time.Since(e.at) < claudeQuotaReportTTL {
		return e.report, nil
	}
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return claudeQuotaReport{}, err
	}
	report, err := computeClaudeQuotaReport(ctx, db, time.Now(), defaultModelPrices())
	if err != nil {
		return claudeQuotaReport{}, err
	}
	report.Sampler = globalClaudeQuota.status()
	e.report, e.at = report, time.Now()
	return report, nil
}

func handleClaudeQuotaManagement(req managementRequest) (resp managementResponse) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp = jsonResponse(http.StatusInternalServerError, map[string]any{"error": "claude_quota_failed", "message": fmt.Sprintf("panic: %v", recovered)})
		}
	}()
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeQuotaComputeTimeout)
	defer cancel()
	report, err := globalClaudeQuotaEstimator.get(ctx, parseBoolString(firstQuery(req.Query, "refresh", "false"), false))
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "claude_quota_failed", "message": sanitizeTriggerError(err.Error())})
	}
	return jsonResponse(http.StatusOK, report)
}

func computeClaudeQuotaReport(ctx context.Context, db *sql.DB, now time.Time, prices map[string]modelPrice) (claudeQuotaReport, error) {
	report := claudeQuotaReport{
		GeneratedAt:  now.Format(time.RFC3339),
		LookbackDays: int(claudeQuotaFitLookback / (24 * time.Hour)),
		Accounts:     []claudeQuotaAccount{},
	}
	nowMs := now.UnixMilli()
	rows, err := db.QueryContext(ctx, `
SELECT account, quota_window, observed_at_ms, utilization, reset_at, status
FROM claude_quota_samples
WHERE observed_at_ms >= ?
ORDER BY observed_at_ms, id`, now.Add(-claudeQuotaFitLookback).UnixMilli())
	if err != nil {
		return report, err
	}
	grouped := map[string]map[string][]claudeQuotaSample{}
	earliestMs := nowMs
	for rows.Next() {
		var account, window string
		var sample claudeQuotaSample
		if err := rows.Scan(&account, &window, &sample.ObservedMs, &sample.Utilization, &sample.ResetAt, &sample.Status); err != nil {
			_ = rows.Close()
			return report, err
		}
		duration, _, ok := claudeWindowSpec(window)
		if !ok {
			continue
		}
		if grouped[account] == nil {
			grouped[account] = map[string][]claudeQuotaSample{}
		}
		grouped[account][window] = append(grouped[account][window], sample)
		start := sample.ObservedMs
		if sample.ResetAt > 0 {
			start = (sample.ResetAt - duration) * 1000
		}
		if start < earliestMs {
			earliestMs = start
		}
	}
	if err := rows.Close(); err != nil {
		return report, err
	}
	if len(grouped) == 0 {
		return report, nil
	}
	events, err := loadClaudeCostEvents(ctx, db, earliestMs/1000-60, prices)
	if err != nil {
		return report, err
	}
	for account, windows := range grouped {
		seriesByFamily := map[string]*claudeCostSeries{}
		series := func(family string) *claudeCostSeries {
			if cached, ok := seriesByFamily[family]; ok {
				return cached
			}
			var filtered []claudeCostEvent
			for _, event := range events[account] {
				if family == "" || strings.Contains(normalizeModelName(event.Model), family) {
					filtered = append(filtered, event)
				}
			}
			built := newClaudeCostSeries(filtered)
			seriesByFamily[family] = built
			return built
		}
		out := claudeQuotaAccount{Account: account}
		// The shortest unscoped window goes first: it finds the steps that
		// usage outside CPA fell in, for the other windows to leave out too.
		reference, referenceSeconds := "", int64(0)
		for window := range windows {
			if duration, scope, _ := claudeWindowSpec(window); scope == "" && (reference == "" || duration < referenceSeconds) {
				reference, referenceSeconds = window, duration
			}
		}
		var screen *claudeQuotaScreen
		if reference != "" {
			var built claudeQuotaWindow
			built, screen = buildClaudeQuotaWindow(reference, windows[reference], series, prices, nowMs, nil)
			out.Windows = append(out.Windows, built)
		}
		for window, samples := range windows {
			if window != reference {
				built, _ := buildClaudeQuotaWindow(window, samples, series, prices, nowMs, screen)
				out.Windows = append(out.Windows, built)
			}
		}
		sort.Slice(out.Windows, func(i, j int) bool {
			if out.Windows[i].DurationSeconds != out.Windows[j].DurationSeconds {
				return out.Windows[i].DurationSeconds < out.Windows[j].DurationSeconds
			}
			return out.Windows[i].Window < out.Windows[j].Window
		})
		report.Accounts = append(report.Accounts, out)
	}
	sort.Slice(report.Accounts, func(i, j int) bool { return report.Accounts[i].Account < report.Accounts[j].Account })
	return report, nil
}

// loadClaudeCostEvents prices every Claude subscription request since the
// given unix second, grouped by account and sorted by completion time.
func loadClaudeCostEvents(ctx context.Context, db *sql.DB, since int64, prices map[string]modelPrice) (map[string][]claudeCostEvent, error) {
	rows, err := db.QueryContext(ctx, `
SELECT requested_at, latency_ms, provider, auth_type, source, auth_index, auth_id, model, alias, service_tier,
  input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens
FROM usage_events
WHERE provider = 'claude' AND requested_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type resolved struct {
		price modelPrice
		ok    bool
	}
	priceCache := map[string]resolved{}
	out := map[string][]claudeCostEvent{}
	for rows.Next() {
		var requestedAt, latencyMs int64
		var authType, source, authIndex, authID string
		var row costTokenRow
		if err := rows.Scan(&requestedAt, &latencyMs, &row.Provider, &authType, &source, &authIndex, &authID,
			&row.Model, &row.Alias, &row.ServiceTier, &row.InputTokens, &row.OutputTokens, &row.CachedTokens,
			&row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens); err != nil {
			return nil, err
		}
		if !isClaudeSubscriptionUsage(row.Provider, authType) {
			continue
		}
		account := claudeQuotaAccountKey(source, authIndex, authID)
		if account == "" {
			continue
		}
		key := modelCostKey(row.Model, row.Alias, row.Provider)
		hit, cached := priceCache[key]
		if !cached {
			hit.price, hit.ok = resolveModelPrice(row, prices)
			priceCache[key] = hit
		}
		event := claudeCostEvent{
			DoneMs:          requestedAt*1000 + maxInt64(latencyMs, 0),
			Model:           firstNonEmptyString(row.Model, row.Alias),
			CacheReadTokens: normalizeCacheTokens(row.CachedTokens, row.CacheReadTokens, row.CacheCreationTokens).Read,
		}
		if hit.ok {
			event.Base, event.CacheRead = costComponentsWithPrice(row, hit.price)
		} else if usageTokenInputRequiresPricing(row) {
			event.Unpriced = row.TotalTokens
		}
		out[account] = append(out[account], event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for account := range out {
		events := out[account]
		sort.SliceStable(events, func(i, j int) bool { return events[i].DoneMs < events[j].DoneMs })
	}
	return out, nil
}

type claudeQuotaCycle struct {
	ResetAt int64
	StartMs int64
	Samples []claudeQuotaSample
}

// buildClaudeQuotaCycles groups time-ordered samples by reset time. A cycle
// starts one window before its reset, but never before the previous reset:
// the 5h window starts at the first request after the last one expired.
func buildClaudeQuotaCycles(samples []claudeQuotaSample, durationSeconds int64) []claudeQuotaCycle {
	byKey := map[int64]int{}
	var cycles []claudeQuotaCycle
	for _, sample := range samples {
		if sample.ResetAt <= 0 {
			continue
		}
		key := (sample.ResetAt + 30) / 60
		i, ok := byKey[key]
		if !ok {
			i = len(cycles)
			byKey[key] = i
			cycles = append(cycles, claudeQuotaCycle{ResetAt: sample.ResetAt})
		}
		cycles[i].Samples = append(cycles[i].Samples, sample)
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i].ResetAt < cycles[j].ResetAt })
	for i := range cycles {
		cycles[i].StartMs = (cycles[i].ResetAt - durationSeconds) * 1000
		if i > 0 && cycles[i-1].ResetAt*1000 > cycles[i].StartMs {
			cycles[i].StartMs = cycles[i-1].ResetAt * 1000
		}
	}
	return cycles
}

// claudeCycleCrossings returns the first observation of each new highest
// value in a cycle: the moments the reported utilization stepped up. The
// cycle's first sample is skipped because when it was reached is unknown.
func claudeCycleCrossings(cycle claudeQuotaCycle) []claudeQuotaSample {
	var out []claudeQuotaSample
	highest := math.Inf(-1)
	for i, sample := range cycle.Samples {
		if i > 0 && sample.Utilization > highest {
			out = append(out, sample)
		}
		if sample.Utilization > highest {
			highest = sample.Utilization
		}
	}
	return out
}

// claudeQuotaPoint is one step up of the utilization. Points of one Segment
// share an intercept: a segment is a cycle, or the part of one between steps
// that were left out.
type claudeQuotaPoint struct {
	Cycle       int
	Segment     int
	ObservedMs  int64
	Utilization float64
	Base        float64
	CacheRead   float64
}

type claudeQuotaFit struct {
	OK     bool
	A      float64 // utilization points per USD of base cost; limit = 100/A
	VarA   float64
	K      float64 // cache-hit price as a share of the API price
	KSE    float64 // standard error of K from the unconstrained fit
	RMSE   float64 // percentage points
	Points int
	// Clamped reports that the unconstrained fit gave cache hits a negative
	// price, so A comes from a refit with cache hits free.
	Clamped bool
}

// fitClaudeQuota regresses utilization on cumulative cost with one intercept
// per segment (within-segment demeaning). The intercepts absorb each cycle's
// unknown start offset, the rounding of the reported utilization and the
// usage outside CPA in left-out steps. With fitCache false, base and
// cache-read cost share one slope (k fixed to 1).
func fitClaudeQuota(points []claudeQuotaPoint, fitCache bool) claudeQuotaFit {
	m := claudeQuotaMomentsOf(points)
	failed := claudeQuotaFit{Points: m.rows}
	beta, inv, rss, ok := m.solve(claudeQuotaColumns(fitCache)...)
	if !ok {
		return failed
	}
	dof := m.rows - m.segments - len(beta)
	if dof < 1 || beta[0] <= 0 {
		return failed
	}
	sigma2 := rss / float64(dof)
	fit := claudeQuotaFit{OK: true, A: beta[0], VarA: sigma2 * inv[0][0], K: 1, Points: m.rows}
	if fitCache {
		a, b := beta[0], beta[1]
		varK := sigma2*inv[1][1]/(a*a) + b*b*sigma2*inv[0][0]/math.Pow(a, 4) - 2*b*sigma2*inv[0][1]/math.Pow(a, 3)
		fit.K, fit.KSE = b/a, math.Sqrt(math.Max(varK, 0))
		if fit.K < 0 {
			beta, inv, rss, ok = m.solve(claudeQuotaBaseCost)
			if !ok || beta[0] <= 0 {
				return failed
			}
			sigma2 = rss / float64(m.rows-m.segments-1)
			fit.A, fit.VarA, fit.K, fit.Clamped = beta[0], sigma2*inv[0][0], 0, true
		}
	}
	fit.RMSE = math.Sqrt(rss / float64(m.rows))
	return fit
}

// Regression columns, as weights on base and cache-read cost.
var (
	claudeQuotaBaseCost  = [2]float64{1, 0}
	claudeQuotaCacheCost = [2]float64{0, 1}
	claudeQuotaTotalCost = [2]float64{1, 1}
)

// claudeQuotaColumns prices cache hits freely or, with fitCache false, at
// the API price (k fixed to 1).
func claudeQuotaColumns(fitCache bool) [][2]float64 {
	if fitCache {
		return [][2]float64{claudeQuotaBaseCost, claudeQuotaCacheCost}
	}
	return [][2]float64{claudeQuotaTotalCost}
}

// claudeQuotaSums are running sums over steps of one segment, taken relative
// to an origin step of that segment so that demeaning keeps its precision.
type claudeQuotaSums struct {
	n     int
	x, xy [2]float64
	xx    [2][2]float64
	y, yy float64
}

func (s *claudeQuotaSums) add(point, origin claudeQuotaPoint) {
	x := [2]float64{point.Base - origin.Base, point.CacheRead - origin.CacheRead}
	y := point.Utilization - origin.Utilization
	s.n++
	s.y += y
	s.yy += y * y
	for i := range x {
		s.x[i] += x[i]
		s.xy[i] += x[i] * y
		for j := range x {
			s.xx[i][j] += x[i] * x[j]
		}
	}
}

func (s claudeQuotaSums) minus(o claudeQuotaSums) claudeQuotaSums {
	s.n -= o.n
	s.y -= o.y
	s.yy -= o.yy
	for i := range s.x {
		s.x[i] -= o.x[i]
		s.xy[i] -= o.xy[i]
		for j := range s.x {
			s.xx[i][j] -= o.xx[i][j]
		}
	}
	return s
}

// moments demeans the sums, which gives their steps an intercept of their
// own. A single step carries no information.
func (s claudeQuotaSums) moments() claudeQuotaMoments {
	if s.n < 2 {
		return claudeQuotaMoments{}
	}
	n := float64(s.n)
	m := claudeQuotaMoments{yy: s.yy - s.y*s.y/n, rows: s.n, segments: 1}
	for i := range s.x {
		m.xy[i] = s.xy[i] - s.x[i]*s.y/n
		for j := range s.x {
			m.xx[i][j] = s.xx[i][j] - s.x[i]*s.x[j]/n
		}
	}
	return m
}

// claudeQuotaMoments are the cross products of base cost, cache-read cost
// and utilization after demeaning within each segment, which stands in for
// one intercept per segment. They add across segments. rows counts the steps
// in segments with at least two steps, and segments counts those segments.
type claudeQuotaMoments struct {
	xx             [2][2]float64
	xy             [2]float64
	yy             float64
	rows, segments int
}

func (m claudeQuotaMoments) plus(o claudeQuotaMoments) claudeQuotaMoments {
	m.yy += o.yy
	m.rows += o.rows
	m.segments += o.segments
	for i := range m.xy {
		m.xy[i] += o.xy[i]
		for j := range m.xy {
			m.xx[i][j] += o.xx[i][j]
		}
	}
	return m
}

func claudeQuotaMomentsOf(points []claudeQuotaPoint) claudeQuotaMoments {
	origins := map[int]claudeQuotaPoint{}
	sums := map[int]*claudeQuotaSums{}
	var order []int
	for _, point := range points {
		s, ok := sums[point.Segment]
		if !ok {
			s = &claudeQuotaSums{}
			sums[point.Segment], origins[point.Segment] = s, point
			order = append(order, point.Segment)
		}
		s.add(point, origins[point.Segment])
	}
	var m claudeQuotaMoments
	for _, segment := range order {
		m = m.plus(sums[segment].moments())
	}
	return m
}

// solve regresses utilization on the given cost columns by least squares and
// returns the slopes, (X'X)^-1 and the residual sum of squares.
func (m claudeQuotaMoments) solve(columns ...[2]float64) ([]float64, [][]float64, float64, bool) {
	p := len(columns)
	if m.rows <= p {
		return nil, nil, 0, false
	}
	xtx := make([][]float64, p)
	xty := make([]float64, p)
	for i, u := range columns {
		xtx[i] = make([]float64, p)
		for a := range u {
			xty[i] += u[a] * m.xy[a]
			for j, v := range columns {
				for b := range v {
					xtx[i][j] += u[a] * m.xx[a][b] * v[b]
				}
			}
		}
	}
	inv, ok := invertMatrix(xtx)
	if !ok {
		return nil, nil, 0, false
	}
	beta := make([]float64, p)
	rss := m.yy
	for i := range beta {
		for j := range beta {
			beta[i] += inv[i][j] * xty[j]
		}
		rss -= beta[i] * xty[i]
	}
	return beta, inv, math.Max(rss, 0), true
}

// claudeQuotaRecentPoints drops the steps before the most recent change in
// how fast cost uses up the window. The change is looked for with the cache
// price free when it can be fitted: with k fixed, a shift in the cache mix
// alone would look like a change.
func claudeQuotaRecentPoints(points []claudeQuotaPoint) ([]claudeQuotaPoint, *claudeQuotaChange) {
	_, _, _, fitCache := claudeQuotaMomentsOf(points).solve(claudeQuotaColumns(true)...)
	var change *claudeQuotaChange
	previous, start := 0, 0
	for {
		j, rate := findClaudeQuotaChange(points[start:], fitCache)
		if j == 0 {
			break
		}
		previous, start = start, start+j
		change = &claudeQuotaChange{At: points[start].ObservedMs / 1000, DroppedPoints: start, RateRatio: rate}
	}
	if change != nil && fitCache {
		if fit := fitClaudeQuota(points[previous:start], true); fit.OK && fit.Points >= claudeQuotaMinFitPoints {
			change.PreviousCachePriceRatio = &fit.K
		}
	}
	return points[start:], change
}

// findClaudeQuotaChange looks for one change in time-ordered steps. It
// counts when the Wald statistic of the best split reaches
// claudeQuotaChangeMinWald and its rate ratio is at least
// claudeQuotaChangeMinShift away from 1, so that slow drift in the traffic is
// not taken for a change. It returns the index of the first step after the
// change, or 0, and the rate ratio.
func findClaudeQuotaChange(points []claudeQuotaPoint, fitCache bool) (int, float64) {
	j, wald, rate := bestClaudeQuotaSplit(points, fitCache)
	if wald < claudeQuotaChangeMinWald || math.Abs(rate-1) < claudeQuotaChangeMinShift {
		return 0, 0
	}
	return j, rate
}

// bestClaudeQuotaSplit tries as the first step after a change every step that
// leaves claudeQuotaChangeMinPoints steps on each side, so a change can be
// found once that many steps have followed it, and returns the split with the
// largest Wald statistic (a sup-Wald test) and its rate ratio.
func bestClaudeQuotaSplit(points []claudeQuotaPoint, fitCache bool) (int, float64, float64) {
	lo, hi := claudeQuotaChangeMinPoints, len(points)-claudeQuotaChangeMinPoints
	if hi < lo {
		return 0, 0, 0
	}
	columns := claudeQuotaColumns(fitCache)
	// Runs of consecutive steps from one segment, with the moments and the cost
	// of the runs after each, so that every split costs the same.
	type run struct {
		start, end int
		sums       claudeQuotaSums
	}
	var runs []run
	for i := 0; i < len(points); {
		r := run{start: i}
		for ; i < len(points) && points[i].Segment == points[r.start].Segment; i++ {
			r.sums.add(points[i], points[r.start])
		}
		r.end = i
		runs = append(runs, r)
	}
	after := make([]claudeQuotaMoments, len(runs)+1)
	afterCost := make([][2]float64, len(runs)+1)
	for r := len(runs) - 1; r >= 0; r-- {
		first, last := points[runs[r].start], points[runs[r].end-1]
		after[r] = after[r+1].plus(runs[r].sums.moments())
		afterCost[r] = [2]float64{afterCost[r+1][0] + last.Base - first.Base, afterCost[r+1][1] + last.CacheRead - first.CacheRead}
	}
	best, bestWald, bestRate := 0, 0.0, 0.0
	var before claudeQuotaMoments
	for r, run := range runs {
		var head claudeQuotaSums
		last := points[run.end-1]
		for j := run.start; j < run.end; j++ {
			if j >= lo && j <= hi {
				pre := before.plus(head.moments())
				post := after[r+1].plus(run.sums.minus(head).moments())
				cost := [2]float64{afterCost[r+1][0] + last.Base - points[j].Base, afterCost[r+1][1] + last.CacheRead - points[j].CacheRead}
				if wald, rate, ok := claudeQuotaSplit(pre, post, cost, columns); ok && wald > bestWald {
					best, bestWald, bestRate = j, wald, rate
				}
			}
			head.add(points[j], points[run.start])
		}
		before = before.plus(run.sums.moments())
	}
	return best, bestWald, bestRate
}

// claudeQuotaSplit compares separate fits of the steps before and after a
// split with one shared fit, in which the segment that spans the split keeps an
// intercept on each side. It returns the Wald statistic and the rate ratio:
// the window that cost, the cost of the steps after the split, used under the
// later fit, over what the earlier fit gives for it.
func claudeQuotaSplit(pre, post claudeQuotaMoments, cost [2]float64, columns [][2]float64) (float64, float64, bool) {
	betaPre, _, rssPre, okPre := pre.solve(columns...)
	betaPost, _, rssPost, okPost := post.solve(columns...)
	_, _, rssPooled, okPooled := pre.plus(post).solve(columns...)
	if !okPre || !okPost || !okPooled || betaPre[0] <= 0 || betaPost[0] <= 0 {
		return 0, 0, false
	}
	dof := pre.rows - pre.segments + post.rows - post.segments - 2*len(columns)
	if dof < 1 {
		return 0, 0, false
	}
	var before, after float64
	for i, column := range columns {
		x := column[0]*cost[0] + column[1]*cost[1]
		before += betaPre[i] * x
		after += betaPost[i] * x
	}
	if before <= 0 || after <= 0 {
		return 0, 0, false
	}
	rss := rssPre + rssPost
	wald := math.Inf(1)
	if rss > 0 {
		wald = (rssPooled - rss) / (rss / float64(dof))
	}
	return wald, after / before, true
}

// invertMatrix inverts a small square matrix by Gauss-Jordan elimination with
// partial pivoting.
func invertMatrix(m [][]float64) ([][]float64, bool) {
	n := len(m)
	a := make([][]float64, n)
	scale := 0.0
	for i := range m {
		a[i] = make([]float64, 2*n)
		copy(a[i], m[i])
		a[i][n+i] = 1
		scale = math.Max(scale, math.Abs(m[i][i]))
	}
	if scale == 0 {
		return nil, false
	}
	for col := 0; col < n; col++ {
		pivot := col
		for row := col + 1; row < n; row++ {
			if math.Abs(a[row][col]) > math.Abs(a[pivot][col]) {
				pivot = row
			}
		}
		if math.Abs(a[pivot][col]) <= 1e-12*scale {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		div := a[col][col]
		for j := range a[col] {
			a[col][j] /= div
		}
		for row := 0; row < n; row++ {
			if row == col || a[row][col] == 0 {
				continue
			}
			factor := a[row][col]
			for j := range a[row] {
				a[row][j] -= factor * a[col][j]
			}
		}
	}
	inv := make([][]float64, n)
	for i := range a {
		inv[i] = append([]float64(nil), a[i][n:]...)
	}
	return inv, true
}

// claudeQuotaSegments gives each run of kept steps an intercept of its own: a
// segment starts with each cycle and at each point whose step drop leaves
// out. A point alone in its segment carries no information and is dropped.
func claudeQuotaSegments(points []claudeQuotaPoint, drop []bool) []claudeQuotaPoint {
	out := make([]claudeQuotaPoint, 0, len(points))
	segment, start := -1, 0
	for i, point := range points {
		if i == 0 || point.Cycle != points[i-1].Cycle || drop[i] {
			if len(out)-start == 1 {
				out = out[:start]
			}
			segment, start = segment+1, len(out)
		}
		point.Segment = segment
		out = append(out, point)
	}
	if len(out)-start == 1 {
		out = out[:start]
	}
	return out
}

// screenClaudeQuotaSteps finds the steps in which the utilization rose more
// than the requests through CPA explain, because the subscription was used
// elsewhere too. drop[i] reports that the step ending at points[i] is left
// out; ok is false when there was too little to fit.
//
// Usage outside CPA only ever adds to the utilization. The steps are fitted in
// blocks of claudeQuotaScreenBlockSteps within a cycle, by claudeQuotaBlockFit:
// a block that rose further above the median block than
// claudeQuotaScreenSigmas times the spread of the blocks, and at least
// claudeQuotaScreenMinExcess, is left out and the rest refitted until nothing
// changes. A stretch at the start or end in which claudeQuotaScreenRunShare of
// the blocks are left out, and that lasts claudeQuotaScreenPersist and
// claudeQuotaChangeMinPoints steps, is kept: usage outside CPA comes and goes,
// and a rate that held that long is more likely a change in the limit or the
// price, which the change detection then looks for.
func screenClaudeQuotaSteps(points []claudeQuotaPoint) ([]bool, bool) {
	var blocks [][2]int // first and last point; the block's steps end at first+1..last
	for i := 1; i < len(points); i++ {
		if points[i].Cycle != points[i-1].Cycle {
			continue
		}
		if n := len(blocks); n > 0 && blocks[n-1][1] == i-1 && blocks[n-1][1]-blocks[n-1][0] < claudeQuotaScreenBlockSteps {
			blocks[n-1][1] = i
		} else {
			blocks = append(blocks, [2]int{i - 1, i})
		}
	}
	// The short block a cycle ends with joins the one before it, so that it
	// rises as far above the fit as the others when its steps do.
	merged := blocks[:0]
	for _, block := range blocks {
		if n := len(merged); n > 0 && merged[n-1][1] == block[0] && block[1]-block[0] < claudeQuotaScreenBlockSteps {
			merged[n-1][1] = block[1]
			continue
		}
		merged = append(merged, block)
	}
	blocks = merged
	// The cache price is fitted when the cost mix varies enough, as in the
	// change detection.
	_, _, _, fitCache := claudeQuotaMomentsOf(claudeQuotaSegments(points, make([]bool, len(points)))).solve(claudeQuotaColumns(true)...)
	keptBy := func(drop []bool) func(int) bool { return func(b int) bool { return !drop[blocks[b][1]] } }
	drop := make([]bool, len(points))
	a, k, ok := claudeQuotaBlockFit(points, blocks, keptBy(drop), fitCache)
	if !ok {
		return drop, false
	}
	for round := 0; round < claudeQuotaScreenRounds; round++ {
		// Residuals per block, centred on the median block: a fit pulled up by
		// left-in outside usage lowers every block in proportion to its steps.
		residuals := make([]float64, len(blocks))
		perStep := make([]float64, len(blocks))
		for b, block := range blocks {
			first, last := points[block[0]], points[block[1]]
			cost := last.Base - first.Base + k*(last.CacheRead-first.CacheRead)
			residuals[b] = last.Utilization - first.Utilization - a*cost
			perStep[b] = residuals[b] / float64(block[1]-block[0])
		}
		center := claudeQuotaMedian(perStep)
		deviations := make([]float64, len(blocks))
		for b, block := range blocks {
			residuals[b] -= center * float64(block[1]-block[0])
			deviations[b] = math.Abs(residuals[b])
		}
		// 1.4826 × the median absolute deviation estimates the spread.
		threshold := math.Max(claudeQuotaScreenMinExcess, claudeQuotaScreenSigmas*1.4826*claudeQuotaMedian(deviations))
		next := make([]bool, len(points))
		for b, block := range blocks {
			for i := block[0] + 1; i <= block[1]; i++ {
				next[i] = residuals[b] > threshold
			}
		}
		if slices.Equal(next, drop) {
			break
		}
		nextA, nextK, fitted := claudeQuotaBlockFit(points, blocks, keptBy(next), fitCache)
		if !fitted {
			break
		}
		drop, a, k = next, nextA, nextK
	}
	dropped := func(b int) bool { return drop[blocks[b][1]] }
	// endRun walks inward from one end and returns the length of the longest
	// stretch there in which claudeQuotaScreenRunShare of the blocks are left
	// out, ending with one that is.
	endRun := func(at func(n int) int) int {
		left, length := 0, 0
		for n := range blocks {
			if dropped(at(n)) {
				left++
				if float64(left) >= claudeQuotaScreenRunShare*float64(n+1) {
					length = n + 1
				}
			}
		}
		return length
	}
	lead := endRun(func(n int) int { return n })
	trail := endRun(func(n int) int { return len(blocks) - 1 - n })
	keep := func(from, to int) {
		steps := 0
		for b := from; b <= to; b++ {
			steps += blocks[b][1] - blocks[b][0]
		}
		lasted := points[blocks[to][1]].ObservedMs - points[blocks[from][0]].ObservedMs
		if steps < claudeQuotaChangeMinPoints || lasted < claudeQuotaScreenPersist.Milliseconds() {
			return
		}
		for i := blocks[from][0] + 1; i <= blocks[to][1]; i++ {
			drop[i] = false
		}
	}
	if lead > 0 {
		keep(0, lead-1)
	}
	if trail > 0 {
		keep(len(blocks)-trail, len(blocks)-1)
	}
	return drop, true
}

// claudeQuotaBlockFit fits how far the kept blocks rose to what they cost, by
// least absolute deviations: blocks with usage outside CPA pull it far less
// than they pull least squares while they are a minority. It returns the
// utilization points per USD of base cost and k, the cache price.
func claudeQuotaBlockFit(points []claudeQuotaPoint, blocks [][2]int, kept func(b int) bool, fitCache bool) (float64, float64, bool) {
	type row struct {
		x [2]float64
		y float64
	}
	var rows []row
	for b, block := range blocks {
		if kept(b) {
			first, last := points[block[0]], points[block[1]]
			rows = append(rows, row{x: [2]float64{last.Base - first.Base, last.CacheRead - first.CacheRead}, y: last.Utilization - first.Utilization})
		}
	}
	// Iteratively reweighted least squares, each row weighted by the inverse
	// of its last residual.
	solve := func(columns [][2]float64) ([]float64, bool) {
		p := len(columns)
		if len(rows) < claudeQuotaMinRegressionPoints || len(rows) <= p {
			return nil, false
		}
		weights := make([]float64, len(rows))
		for i := range weights {
			weights[i] = 1
		}
		z := make([]float64, p)
		var beta []float64
		for iteration := 0; iteration < claudeQuotaScreenFitIterations; iteration++ {
			xtx := make([][]float64, p)
			for i := range xtx {
				xtx[i] = make([]float64, p)
			}
			xty := make([]float64, p)
			for r, row := range rows {
				for c, column := range columns {
					z[c] = column[0]*row.x[0] + column[1]*row.x[1]
				}
				for i := range z {
					xty[i] += weights[r] * z[i] * row.y
					for j := range z {
						xtx[i][j] += weights[r] * z[i] * z[j]
					}
				}
			}
			inv, ok := invertMatrix(xtx)
			if !ok {
				return nil, false
			}
			beta = make([]float64, p)
			for i := range beta {
				for j := range beta {
					beta[i] += inv[i][j] * xty[j]
				}
			}
			for r, row := range rows {
				fitted := 0.0
				for c, column := range columns {
					fitted += beta[c] * (column[0]*row.x[0] + column[1]*row.x[1])
				}
				weights[r] = 1 / math.Max(math.Abs(row.y-fitted), 0.01)
			}
		}
		return beta, beta[0] > 0
	}
	beta, ok := solve(claudeQuotaColumns(fitCache))
	if !ok {
		return 0, 0, false
	}
	if !fitCache {
		return beta[0], 1, true
	}
	if k := beta[1] / beta[0]; k >= 0 {
		return beta[0], k, true
	}
	// Cache hits came out with a negative price; price them at 0, as
	// fitClaudeQuota does.
	if beta, ok = solve([][2]float64{claudeQuotaBaseCost}); !ok {
		return 0, 0, false
	}
	return beta[0], 0, true
}

// claudeQuotaMedian returns the median of values, which it sorts; 0 if empty.
func claudeQuotaMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[len(values)/2]
}

// claudeQuotaSpan is the time a step took: between two consecutive points of
// one cycle.
type claudeQuotaSpan struct {
	FromMs, ToMs int64
}

// claudeQuotaScreen hands the steps of an account's shortest unscoped window
// to its other windows, whose own steps are too coarse to screen. In time
// order.
type claudeQuotaScreen struct {
	steps   []claudeQuotaSpan
	dropped []claudeQuotaSpan
}

func newClaudeQuotaScreen(points []claudeQuotaPoint, drop []bool) *claudeQuotaScreen {
	s := &claudeQuotaScreen{}
	for i := 1; i < len(points); i++ {
		if points[i].Cycle != points[i-1].Cycle {
			continue
		}
		span := claudeQuotaSpan{FromMs: points[i-1].ObservedMs, ToMs: points[i].ObservedMs}
		s.steps = append(s.steps, span)
		if drop[i] {
			s.dropped = append(s.dropped, span)
		}
	}
	return s
}

// keeps reports whether a step from fromMs to toMs can be fitted: no
// left-out step overlaps it, and steps cover all of it, so that usage outside
// CPA during it would have shown.
func (s *claudeQuotaScreen) keeps(fromMs, toMs int64) bool {
	i := sort.Search(len(s.dropped), func(i int) bool { return s.dropped[i].ToMs > fromMs })
	if i < len(s.dropped) && s.dropped[i].FromMs < toMs {
		return false
	}
	at := fromMs
	for j := sort.Search(len(s.steps), func(j int) bool { return s.steps[j].ToMs > fromMs }); j < len(s.steps) && at < toMs; j++ {
		if s.steps[j].FromMs > at {
			return false
		}
		at = s.steps[j].ToMs
	}
	return at >= toMs
}

// buildClaudeQuotaWindow estimates one window. With screen nil the window
// screens its own steps and, if it could, returns them as the screen for the
// account's other windows; otherwise it leaves out the steps screen does not
// keep.
func buildClaudeQuotaWindow(window string, samples []claudeQuotaSample, series func(string) *claudeCostSeries, prices map[string]modelPrice, nowMs int64, screen *claudeQuotaScreen) (claudeQuotaWindow, *claudeQuotaScreen) {
	duration, scope, _ := claudeWindowSpec(window)
	latest := samples[len(samples)-1]
	out := claudeQuotaWindow{
		Window:          window,
		Scope:           scope,
		DurationSeconds: duration,
		Utilization:     latest.Utilization,
		Status:          latest.Status,
		ResetAt:         latest.ResetAt,
		ObservedAt:      latest.ObservedMs / 1000,
		ResetPassed:     latest.ResetAt > 0 && nowMs >= latest.ResetAt*1000,
		Samples:         len(samples),
	}
	family, estimable := claudeScopeModelFamily(scope)
	if !estimable {
		out.Note = "unscoped_window"
		return out, nil
	}
	if latest.ResetAt <= 0 {
		out.Note = "no_reset_header"
		return out, nil
	}
	out.Estimated = true
	costs := series(family)
	cycles := buildClaudeQuotaCycles(samples, duration)
	out.Cycles = len(cycles)
	var points []claudeQuotaPoint
	for i, cycle := range cycles {
		for _, crossing := range claudeCycleCrossings(cycle) {
			base, cacheRead := costs.sums(cycle.StartMs, crossing.ObservedMs)
			points = append(points, claudeQuotaPoint{Cycle: i, ObservedMs: crossing.ObservedMs, Utilization: crossing.Utilization, Base: base, CacheRead: cacheRead})
		}
	}
	out.Points = len(points)
	sort.SliceStable(points, func(i, j int) bool { return points[i].ObservedMs < points[j].ObservedMs })

	var drop []bool
	var next *claudeQuotaScreen
	if screen != nil {
		drop = make([]bool, len(points))
		for i := 1; i < len(points); i++ {
			drop[i] = points[i].Cycle == points[i-1].Cycle && !screen.keeps(points[i-1].ObservedMs, points[i].ObservedMs)
		}
	} else {
		var screened bool
		if drop, screened = screenClaudeQuotaSteps(points); screened {
			next = newClaudeQuotaScreen(points, drop)
		}
	}
	var recent []claudeQuotaPoint
	recent, out.Change = claudeQuotaRecentPoints(claudeQuotaSegments(points, drop))
	if out.Change != nil && screen == nil {
		// The steps were screened against a fit across the change; screen the
		// ones after it against their own.
		from := sort.Search(len(points), func(i int) bool { return points[i].ObservedMs >= recent[0].ObservedMs })
		if again, ok := screenClaudeQuotaSteps(points[from:]); ok {
			copy(drop[from+1:], again[1:])
			recent = claudeQuotaSegments(points[from:], again)
			next = newClaudeQuotaScreen(points, drop)
		}
	}
	for _, left := range drop {
		if left {
			out.ScreenedSteps++
		}
	}
	points = recent

	current := cycles[len(cycles)-1]
	out.CycleStart = current.StartMs / 1000
	cycleEndMs := nowMs
	if out.ResetPassed {
		cycleEndMs = current.ResetAt * 1000
	}
	out.CycleBaseCost, out.CycleCacheReadCost = costs.sums(current.StartMs, cycleEndMs)
	modelTokens := map[string]int64{}
	for _, event := range costs.events[costs.index(current.StartMs):costs.index(cycleEndMs)] {
		out.CycleCacheReadTokens += event.CacheReadTokens
		out.UnpricedTokens += event.Unpriced
		modelTokens[event.Model] += event.CacheReadTokens
	}
	remainingShare := math.Max(0, 1-out.Utilization/100)
	if out.ResetPassed {
		remainingShare = 1
	}
	// otherUSD is what the latest reading shows used beyond the cost of the
	// requests before it.
	seenBase, seenCacheRead := costs.sums(current.StartMs, latest.ObservedMs)
	otherUSD := func(limit, k float64) float64 {
		if out.ResetPassed {
			return 0
		}
		return math.Max(0, limit*latest.Utilization/100-seenBase-k*seenCacheRead)
	}

	out.API = claudeQuotaTier{CachePriceRatio: 1, CycleCostUSD: out.CycleBaseCost + out.CycleCacheReadCost}
	if fit := fitClaudeQuota(points, false); fit.OK && fit.Points >= claudeQuotaMinRegressionPoints {
		out.API.Available = true
		out.API.Method = "regression"
		out.API.LimitUSD = 100 / fit.A
		out.API.LimitUSDSE = 100 * math.Sqrt(math.Max(fit.VarA, 0)) / (fit.A * fit.A)
		out.API.Points = fit.Points
		out.API.RMSE = fit.RMSE
	} else {
		if latest.Utilization >= 1 && seenBase+seenCacheRead > 0 {
			out.API.Available = true
			out.API.Method = "ratio"
			out.API.LimitUSD = (seenBase + seenCacheRead) / (latest.Utilization / 100)
		} else {
			out.API.Reason = "insufficient_usage"
		}
	}
	if out.API.Available {
		out.API.RemainingUSD = out.API.LimitUSD * remainingShare
		out.API.OtherUSD = otherUSD(out.API.LimitUSD, 1)
	}

	fit := fitClaudeQuota(points, true)
	switch {
	case fit.Points < claudeQuotaMinFitPoints:
		out.Fitted.Reason = "insufficient_points"
	case !fit.OK:
		out.Fitted.Reason = "fit_failed"
	default:
		out.Fitted = claudeQuotaTier{
			Available:         true,
			Method:            "regression",
			CachePriceRatio:   fit.K,
			CachePriceRatioSE: fit.KSE,
			Clamped:           fit.Clamped,
			LimitUSD:          100 / fit.A,
			LimitUSDSE:        100 * math.Sqrt(math.Max(fit.VarA, 0)) / (fit.A * fit.A),
			CycleCostUSD:      out.CycleBaseCost + fit.K*out.CycleCacheReadCost,
			RemainingUSD:      100 / fit.A * remainingShare,
			OtherUSD:          otherUSD(100/fit.A, fit.K),
			Points:            fit.Points,
			RMSE:              fit.RMSE,
			Confidence:        "low",
		}
		switch {
		case fit.KSE < 0.1:
			out.Fitted.Confidence = "high"
		case fit.KSE < 0.3:
			out.Fitted.Confidence = "medium"
		}
	}
	out.Models = claudeQuotaModelPrices(modelTokens, prices, out.Fitted)
	return out, next
}

func claudeQuotaModelPrices(modelTokens map[string]int64, prices map[string]modelPrice, fitted claudeQuotaTier) []claudeQuotaModelPrice {
	var out []claudeQuotaModelPrice
	for model, tokens := range modelTokens {
		price, ok := resolveModelPrice(costTokenRow{Model: model, Provider: "claude"}, prices)
		if !ok || tokens <= 0 {
			continue
		}
		price = effectiveModelPriceForServiceTier(price, model, "")
		row := claudeQuotaModelPrice{
			Model:            model,
			CacheReadTokens:  tokens,
			InputPerM:        price.Prompt,
			APICacheReadPerM: effectiveCacheReadPrice(price),
		}
		if fitted.Available {
			row.FittedCacheReadPerM = fitted.CachePriceRatio * row.APICacheReadPerM
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CacheReadTokens > out[j].CacheReadTokens })
	if len(out) > claudeQuotaTopModels {
		out = out[:claudeQuotaTopModels]
	}
	return out
}

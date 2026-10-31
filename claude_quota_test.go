package main

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestParseClaudeUnifiedWindows(t *testing.T) {
	readings := parseClaudeUnifiedWindows(map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":        {"0.69"},
		"Anthropic-Ratelimit-Unified-5h-Reset":              {"1793491200"},
		"Anthropic-Ratelimit-Unified-5h-Status":             {"allowed"},
		"anthropic-ratelimit-unified-7d-utilization":        {"0.755"},
		"anthropic-ratelimit-unified-7d-reset":              {"1793836800000"},
		"Anthropic-Ratelimit-Unified-7d_sonnet-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-Overage-Utilization":   {"0.5"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":     {"bogus"},
		"Anthropic-Ratelimit-Unified-Status":                {"allowed"},
	})
	if len(readings) != 3 {
		t.Fatalf("readings = %+v, want 5h, 7d and 7d_sonnet", readings)
	}
	got := map[string]claudeWindowReading{}
	for _, reading := range readings {
		got[reading.Window] = reading
	}
	if r := got["5h"]; r.Utilization != 69 || r.ResetAt != 1793491200 || r.Status != "allowed" {
		t.Fatalf("5h = %+v", r)
	}
	if r := got["7d"]; r.Utilization != 75.5 || r.ResetAt != 1793836800 {
		t.Fatalf("7d = %+v, want 75.5%% and a reset normalized to seconds", r)
	}
	if r := got["7d_sonnet"]; r.Utilization != 10 || r.ResetAt != 0 {
		t.Fatalf("7d_sonnet = %+v", r)
	}
}

func TestClaudeWindowSpec(t *testing.T) {
	for _, tc := range []struct {
		window   string
		duration int64
		scope    string
		ok       bool
	}{
		{"5h", 5 * 3600, "", true},
		{"7d", 7 * 86400, "", true},
		{"7d_sonnet", 7 * 86400, "sonnet", true},
		{"30m", 1800, "", true},
		{"overage", 0, "", false},
		{"0d", 0, "", false},
		{"d", 0, "", false},
	} {
		duration, scope, ok := claudeWindowSpec(tc.window)
		if duration != tc.duration || scope != tc.scope || ok != tc.ok {
			t.Fatalf("claudeWindowSpec(%q) = %d, %q, %v", tc.window, duration, scope, ok)
		}
	}
}

func claudeQuotaTestRecord(at time.Time, headers map[string][]string) usageRecord {
	return usageRecord{
		Provider:        "claude",
		AuthType:        "oauth",
		AuthIndex:       "idx-1",
		AuthID:          "claude-user.json",
		Source:          "User@Example.com",
		RequestedAt:     at,
		ResponseHeaders: headers,
	}
}

func claudeQuotaTestHeaders(fiveHour, sevenDay string) map[string][]string {
	return map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {fiveHour},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1793491200"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {sevenDay},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"1793836800"},
	}
}

func TestClaudeQuotaSamplerStoresOnlyChangesAndHeartbeats(t *testing.T) {
	db := newProtectionTestDB(t)
	ctx := context.Background()
	s := &claudeQuotaSampler{enabled: true}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claude_quota_samples`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t0 := time.Unix(1793480000, 0)
	s.record(ctx, db, claudeQuotaTestRecord(t0, claudeQuotaTestHeaders("0.25", "0.50")))
	if got := count(); got != 2 {
		t.Fatalf("first record stored %d samples, want 2", got)
	}
	s.record(ctx, db, claudeQuotaTestRecord(t0.Add(time.Minute), claudeQuotaTestHeaders("0.25", "0.50")))
	if got := count(); got != 2 {
		t.Fatalf("unchanged record stored a sample: %d", got)
	}
	s.record(ctx, db, claudeQuotaTestRecord(t0.Add(2*time.Minute), claudeQuotaTestHeaders("0.26", "0.50")))
	if got := count(); got != 3 {
		t.Fatalf("5h change stored %d samples, want 3", got)
	}
	s.record(ctx, db, claudeQuotaTestRecord(t0.Add(13*time.Minute), claudeQuotaTestHeaders("0.26", "0.50")))
	if got := count(); got != 5 {
		t.Fatalf("heartbeat stored %d samples, want 5", got)
	}

	apiKey := claudeQuotaTestRecord(t0.Add(14*time.Minute), claudeQuotaTestHeaders("0.90", "0.90"))
	apiKey.AuthType = "apikey"
	s.record(ctx, db, apiKey)
	other := claudeQuotaTestRecord(t0.Add(14*time.Minute), claudeQuotaTestHeaders("0.90", "0.90"))
	other.Provider = "antigravity"
	s.record(ctx, db, other)
	if got := count(); got != 5 {
		t.Fatalf("API-key or non-Claude records stored samples: %d", got)
	}

	s.configure(pluginConfig{ClaudeQuotaEnabled: false})
	s.record(ctx, db, claudeQuotaTestRecord(t0.Add(15*time.Minute), claudeQuotaTestHeaders("0.95", "0.95")))
	if got := count(); got != 5 {
		t.Fatalf("disabled sampler stored samples: %d", got)
	}

	var account string
	var utilization float64
	if err := db.QueryRow(`SELECT account, utilization FROM claude_quota_samples WHERE quota_window='5h' ORDER BY id DESC LIMIT 1`).Scan(&account, &utilization); err != nil {
		t.Fatal(err)
	}
	if account != "user@example.com" || utilization != 26 {
		t.Fatalf("latest 5h sample = %q %.2f", account, utilization)
	}
	if status := s.status(); status.Stored != 5 || status.LastError != "" {
		t.Fatalf("sampler status = %+v", status)
	}
}

func TestClaudeQuotaSamplerSurvivesClosedDatabase(t *testing.T) {
	db := newProtectionTestDB(t)
	_ = db.Close()
	s := &claudeQuotaSampler{enabled: true}
	s.record(context.Background(), db, claudeQuotaTestRecord(time.Unix(1793480000, 0), claudeQuotaTestHeaders("0.25", "0.50")))
	if s.status().LastError == "" {
		t.Fatal("insert error was not kept in the sampler status")
	}
	s.record(context.Background(), nil, claudeQuotaTestRecord(time.Unix(1793480000, 0), claudeQuotaTestHeaders("0.25", "0.50")))
}

// claudeQuotaPlan is what a simulated subscription charges: the window limit
// and the price of cache hits as a share of the API price.
type claudeQuotaPlan struct {
	limitUSD   float64
	cacheShare float64
}

func simulateClaudeQuota(limitUSD, cacheShare float64, cycles int, duration int64, varyMix bool) ([]claudeQuotaSample, []claudeCostEvent, int64) {
	plan := claudeQuotaPlan{limitUSD: limitUSD, cacheShare: cacheShare}
	return simulateClaudeQuotaPlans(7, plan, plan, 0, cycles, duration, varyMix)
}

// simulateClaudeQuotaPlans replays requests against a subscription that
// switches from plan before to plan after at changeMs, or never when it is 0.
// The reported utilization is the charge so far over the current limit,
// floored to whole percent as Anthropic does, and each request's header shows
// the usage of requests completed before it.
func simulateClaudeQuotaPlans(seed int64, before, after claudeQuotaPlan, changeMs int64, cycles int, duration int64, varyMix bool) ([]claudeQuotaSample, []claudeCostEvent, int64) {
	rng := rand.New(rand.NewSource(seed))
	var samples []claudeQuotaSample
	var events []claudeCostEvent
	reset := int64(1793000000)
	for c := 0; c < cycles; c++ {
		reset += duration
		startMs := (reset - duration) * 1000
		charged := 0.0
		mix := 1.0
		for i := 0; ; i++ {
			at := startMs + int64(i)*60_000 + int64(rng.Intn(20_000))
			plan := before
			if changeMs > 0 && at >= changeMs {
				plan = after
			}
			used := charged / plan.limitUSD * 100
			if at+20_000 >= reset*1000 || used >= 98 {
				break
			}
			samples = append(samples, claudeQuotaSample{ObservedMs: at + 500, Utilization: math.Floor(used), ResetAt: reset})
			if varyMix && i%25 == 0 {
				mix = 0.2 + rng.Float64()*2.8
			}
			base := 1.5 + rng.Float64()*3
			cacheRead := base * mix
			if !varyMix {
				cacheRead = base * 1.2
			}
			events = append(events, claudeCostEvent{DoneMs: at + 20_000, Model: "model-a", Base: base, CacheRead: cacheRead, CacheReadTokens: int64(cacheRead * 1e6)})
			charged += base + plan.cacheShare*cacheRead
		}
	}
	return samples, events, reset*1000 - 60_000
}

func claudeQuotaTestWindow(samples []claudeQuotaSample, events []claudeCostEvent, nowMs int64) claudeQuotaWindow {
	series := newClaudeCostSeries(events)
	w, _ := buildClaudeQuotaWindow("5h", samples, func(string) *claudeCostSeries { return series }, map[string]modelPrice{}, nowMs, nil)
	return w
}

func TestClaudeQuotaFitRecoversLimitAndCachePrice(t *testing.T) {
	const limit, share = 1500.0, 0.3
	samples, events, nowMs := simulateClaudeQuota(limit, share, 3, 5*3600, true)
	w := claudeQuotaTestWindow(samples, events, nowMs)
	if !w.Fitted.Available {
		t.Fatalf("fitted tier unavailable: %+v", w.Fitted)
	}
	if math.Abs(w.Fitted.CachePriceRatio-share) > 0.05 {
		t.Fatalf("cache share = %.3f ± %.3f, want %.2f", w.Fitted.CachePriceRatio, w.Fitted.CachePriceRatioSE, share)
	}
	if math.Abs(w.Fitted.LimitUSD-limit)/limit > 0.03 {
		t.Fatalf("fitted limit = %.1f, want %.0f", w.Fitted.LimitUSD, limit)
	}
	if w.Fitted.Confidence != "high" {
		t.Fatalf("confidence = %q, SE %.3f", w.Fitted.Confidence, w.Fitted.CachePriceRatioSE)
	}

	var base, cacheRead float64
	for _, event := range events {
		base += event.Base
		cacheRead += event.CacheRead
	}
	wantAPI := limit * (base + cacheRead) / (base + share*cacheRead)
	if !w.API.Available || w.API.Method != "regression" || math.Abs(w.API.LimitUSD-wantAPI)/wantAPI > 0.08 {
		t.Fatalf("api tier = %+v, want limit near %.0f", w.API, wantAPI)
	}
	if w.Cycles != 3 || w.Points < 100 {
		t.Fatalf("cycles = %d, points = %d", w.Cycles, w.Points)
	}
	if w.Change != nil {
		t.Fatalf("an unchanged plan was taken for a change: %+v", w.Change)
	}
	wantRemaining := w.Fitted.LimitUSD * (1 - w.Utilization/100)
	if math.Abs(w.Fitted.RemainingUSD-wantRemaining) > 1e-6 {
		t.Fatalf("remaining = %.2f, want %.2f", w.Fitted.RemainingUSD, wantRemaining)
	}
}

func TestClaudeQuotaFitErrorShowsCyclesThatDisagree(t *testing.T) {
	// Each cycle charges cache hits at a share of its own, 0.3 on average but
	// too close for a change. Every step of a cycle carries that cycle's
	// error, so the error of the pooled k has to show how far apart the cycles
	// are, not only how well each one fits.
	const duration = 5 * 3600
	var samples []claudeQuotaSample
	var events []claudeCostEvent
	var nowMs int64
	for c, share := range []float64{0.27, 0.33, 0.27, 0.33, 0.27, 0.33} {
		plan := claudeQuotaPlan{1500, share}
		cycleSamples, cycleEvents, end := simulateClaudeQuotaPlans(int64(c+1), plan, plan, 0, 1, duration, true)
		shift := int64(c) * duration
		for _, sample := range cycleSamples {
			sample.ObservedMs += shift * 1000
			sample.ResetAt += shift
			samples = append(samples, sample)
		}
		for _, event := range cycleEvents {
			event.DoneMs += shift * 1000
			events = append(events, event)
		}
		nowMs = end + shift*1000
	}
	w := claudeQuotaTestWindow(samples, events, nowMs)
	if w.Change != nil {
		t.Fatalf("the cycles were taken for a change: %+v", w.Change)
	}
	if f := w.Fitted; !f.Available || math.Abs(f.CachePriceRatio-0.3) > 2*f.CachePriceRatioSE {
		t.Fatalf("cache share = %.3f ± %.3f, want 0.3 within two errors", f.CachePriceRatio, f.CachePriceRatioSE)
	}
}

func TestClaudeQuotaFitClampsNegativeCachePrice(t *testing.T) {
	samples, events, nowMs := simulateClaudeQuota(1500, 0, 3, 5*3600, true)
	w := claudeQuotaTestWindow(samples, events, nowMs)
	if !w.Fitted.Available || w.Fitted.CachePriceRatio < 0 || w.Fitted.CachePriceRatio > 0.05 {
		t.Fatalf("fitted tier = %+v, want a cache share near zero", w.Fitted)
	}
	if math.Abs(w.Fitted.LimitUSD-1500)/1500 > 0.03 {
		t.Fatalf("fitted limit = %.1f", w.Fitted.LimitUSD)
	}
}

func TestClaudeQuotaFitFollowsACachePriceChange(t *testing.T) {
	changeMs := int64(1793000000+2*5*3600+5*3600/2) * 1000 // halfway through the third cycle
	samples, events, nowMs := simulateClaudeQuotaPlans(7, claudeQuotaPlan{1500, 0.3}, claudeQuotaPlan{1500, 1}, changeMs, 6, 5*3600, true)
	w := claudeQuotaTestWindow(samples, events, nowMs)
	c := w.Change
	if c == nil || math.Abs(float64(c.At*1000-changeMs)) > 30*60_000 {
		t.Fatalf("change = %+v, want one near %d", c, changeMs/1000)
	}
	if c.PreviousCachePriceRatio == nil || math.Abs(*c.PreviousCachePriceRatio-0.3) > 0.05 || c.RateRatio < 1.5 {
		t.Fatalf("change = %+v, want the cache share up from 0.3", c)
	}
	if !w.Fitted.Available || math.Abs(w.Fitted.CachePriceRatio-1) > 0.05 || math.Abs(w.Fitted.LimitUSD-1500)/1500 > 0.03 {
		t.Fatalf("fitted tier = %+v, want the new cache share and the same limit", w.Fitted)
	}
	// Cache hits now cost the API price, so the API tier is exact again.
	if !w.API.Available || math.Abs(w.API.LimitUSD-1500)/1500 > 0.03 {
		t.Fatalf("api tier = %+v", w.API)
	}
}

func TestClaudeQuotaFitFollowsALimitChange(t *testing.T) {
	changeMs := int64(1793000000+3*5*3600) * 1000 // the fourth cycle starts
	before := claudeQuotaPlan{1500, 0.3}
	for _, varyMix := range []bool{true, false} {
		samples, events, nowMs := simulateClaudeQuotaPlans(7, before, claudeQuotaPlan{3000, 0.3}, changeMs, 6, 5*3600, varyMix)
		w := claudeQuotaTestWindow(samples, events, nowMs)
		if w.Change == nil || math.Abs(w.Change.RateRatio-0.5) > 0.03 {
			t.Fatalf("varyMix=%v: change = %+v, want the rate halved", varyMix, w.Change)
		}
		if varyMix && (!w.Fitted.Available || math.Abs(w.Fitted.LimitUSD-3000)/3000 > 0.03) {
			t.Fatalf("fitted tier = %+v, want limit 3000", w.Fitted)
		}
		// Without a varying cache mix the change is found with k fixed to 1:
		// cache reads are 1.2 × base, so $3000 of charge is $4853 at API prices.
		if want := 3000 * 2.2 / 1.36; !varyMix && (w.Change.PreviousCachePriceRatio != nil || math.Abs(w.API.LimitUSD-want)/want > 0.03) {
			t.Fatalf("api tier = %+v, change = %+v, want limit %.0f", w.API, w.Change, want)
		}

		samples, events, nowMs = simulateClaudeQuotaPlans(7, before, claudeQuotaPlan{1575, 0.3}, changeMs, 6, 5*3600, varyMix)
		if w := claudeQuotaTestWindow(samples, events, nowMs); w.Change != nil {
			t.Fatalf("varyMix=%v: a 5%% limit change was taken for a change: %+v", varyMix, w.Change)
		}
	}
}

func TestClaudeQuotaFitNeedsVaryingCacheMix(t *testing.T) {
	samples, events, nowMs := simulateClaudeQuota(1500, 0.3, 2, 5*3600, false)
	w := claudeQuotaTestWindow(samples, events, nowMs)
	if w.Fitted.Available {
		t.Fatalf("fitted tier should be unavailable when cache share never changes: %+v", w.Fitted)
	}
	if !w.API.Available {
		t.Fatalf("api tier should still be available: %+v", w.API)
	}
}

type claudeQuotaOutsideSim struct {
	hours        int
	limit5h      float64
	limit5hAfter float64 // from changeHour on, when set
	changeHour   float64
	limit7d      float64
	cacheShare   float64
	steadyMix    bool // cache reads always 1.2 × base, so k cannot be fitted
	// outside charges the subscription per minute for usage CPA never sees,
	// and can keep CPA idle meanwhile.
	outside func(hour float64) (charge float64, cpaIdle bool)
}

// simulateClaudeQuotaOutside replays a request through CPA every minute
// against one subscription's 5h and 7d windows, which also count the usage
// outside CPA. It returns the readings of both, the requests, a time just
// before the end, and the outside usage in the last 5h cycle.
func simulateClaudeQuotaOutside(sim claudeQuotaOutsideSim) (fiveHour, sevenDay []claudeQuotaSample, events []claudeCostEvent, nowMs int64, lastCycleOutside float64) {
	rng := rand.New(rand.NewSource(11))
	start := int64(1793000000)
	weekReset := start + 7*86400
	var charged5, charged7 float64
	cycle, mix := -1, 1.0
	for minute := 0; minute < sim.hours*60; minute++ {
		hour := float64(minute) / 60
		if c := minute / 300; c != cycle {
			cycle, charged5, lastCycleOutside = c, 0, 0
		}
		charge, idle := sim.outside(hour)
		charged5 += charge
		charged7 += charge
		lastCycleOutside += charge
		if idle {
			continue
		}
		limit5 := sim.limit5h
		if sim.limit5hAfter > 0 && hour >= sim.changeHour {
			limit5 = sim.limit5hAfter
		}
		at := start*1000 + int64(minute)*60_000 + int64(rng.Intn(20_000))
		fiveHour = append(fiveHour, claudeQuotaSample{ObservedMs: at + 500, Utilization: math.Floor(charged5 / limit5 * 100), ResetAt: start + int64(cycle+1)*5*3600})
		sevenDay = append(sevenDay, claudeQuotaSample{ObservedMs: at + 500, Utilization: math.Floor(charged7 / sim.limit7d * 100), ResetAt: weekReset})
		if minute%25 == 0 {
			mix = 0.2 + rng.Float64()*2.8
		}
		if sim.steadyMix {
			mix = 1.2
		}
		base := 1.5 + rng.Float64()*3
		events = append(events, claudeCostEvent{DoneMs: at + 20_000, Model: "model-a", Base: base, CacheRead: base * mix})
		charged5 += base + sim.cacheShare*base*mix
		charged7 += base + sim.cacheShare*base*mix
	}
	return fiveHour, sevenDay, events, (start+int64(sim.hours)*3600)*1000 - 60_000, lastCycleOutside
}

func TestClaudeQuotaScreensUsageOutsideCPA(t *testing.T) {
	sim := claudeQuotaOutsideSim{hours: 40, limit5h: 3000, limit7d: 25000, cacheShare: 0.3, outside: func(hour float64) (float64, bool) {
		switch {
		case hour >= 6 && hour < 9, hour >= 37.5:
			return 4, false
		case hour >= 15 && hour < 16:
			return 6, true // used only outside CPA for an hour
		case hour >= 22 && hour < 24.5:
			return 3, false
		}
		return 0, false
	}}
	fiveSamples, weekSamples, events, nowMs, lastCycleOutside := simulateClaudeQuotaOutside(sim)
	series := newClaudeCostSeries(events)
	costs := func(string) *claudeCostSeries { return series }

	five, screen := buildClaudeQuotaWindow("5h", fiveSamples, costs, map[string]modelPrice{}, nowMs, nil)
	if screen == nil || five.ScreenedSteps == 0 {
		t.Fatalf("5h screened %d steps, screen built %v", five.ScreenedSteps, screen != nil)
	}
	// The last burst is still going on; it is too short to be taken for a change.
	if five.Change != nil {
		t.Fatalf("usage outside CPA was taken for a change: %+v", five.Change)
	}
	if !five.Fitted.Available || math.Abs(five.Fitted.LimitUSD-3000)/3000 > 0.03 || math.Abs(five.Fitted.CachePriceRatio-0.3) > 0.05 {
		t.Fatalf("5h fitted tier = %+v, want limit 3000 and cache share 0.3", five.Fitted)
	}
	if math.Abs(five.Fitted.OtherUSD-lastCycleOutside) > 0.03*3000 {
		t.Fatalf("5h other usage = %.0f, want %.0f", five.Fitted.OtherUSD, lastCycleOutside)
	}

	var base, cacheRead float64
	for _, event := range events {
		base += event.Base
		cacheRead += event.CacheRead
	}
	wantAPI := sim.limit7d * (base + cacheRead) / (base + sim.cacheShare*cacheRead)
	week, next := buildClaudeQuotaWindow("7d", weekSamples, costs, map[string]modelPrice{}, nowMs, screen)
	if next != nil || week.ScreenedSteps == 0 || week.Change != nil {
		t.Fatalf("7d screened %d steps, change %+v", week.ScreenedSteps, week.Change)
	}
	if !week.API.Available || math.Abs(week.API.LimitUSD-wantAPI)/wantAPI > 0.08 {
		t.Fatalf("7d api tier = %+v, want limit near %.0f", week.API, wantAPI)
	}
	// The 7d window prices cache hits at the 5h window's k.
	if f := week.Fitted; !f.Available || f.CachePriceFrom != "5h" || f.CachePriceRatio != five.Fitted.CachePriceRatio ||
		f.CachePriceRatioSE != five.Fitted.CachePriceRatioSE || math.Abs(f.LimitUSD-sim.limit7d)/sim.limit7d > 0.03 {
		t.Fatalf("7d fitted tier = %+v, want the 5h cache price and limit %.0f", f, sim.limit7d)
	}
	// Kept whole, the 7d steps take the outside usage for the requests' own.
	keepAll := &claudeQuotaScreen{steps: []claudeQuotaSpan{{FromMs: 0, ToMs: math.MaxInt64}}}
	if blind, _ := buildClaudeQuotaWindow("7d", weekSamples, costs, map[string]modelPrice{}, nowMs, keepAll); blind.API.LimitUSD > 0.9*wantAPI {
		t.Fatalf("7d api limit without screening = %.0f, want it well below %.0f", blind.API.LimitUSD, wantAPI)
	}
}

func TestClaudeQuotaScreenLeavesALastingRateChangeToTheChangeDetection(t *testing.T) {
	// The limit drops for the last 15 hours. With a steady cache mix the cache
	// price cannot take up the change, so every step since rises above the
	// fit, as if all of them were used outside CPA too.
	sim := claudeQuotaOutsideSim{hours: 40, limit5h: 3000, limit5hAfter: 2000, changeHour: 25, limit7d: 25000, cacheShare: 0.3, steadyMix: true,
		outside: func(float64) (float64, bool) { return 0, false }}
	fiveSamples, _, events, nowMs, _ := simulateClaudeQuotaOutside(sim)
	series := newClaudeCostSeries(events)
	five, _ := buildClaudeQuotaWindow("5h", fiveSamples, func(string) *claudeCostSeries { return series }, map[string]modelPrice{}, nowMs, nil)
	if five.Change == nil || math.Abs(five.Change.RateRatio-1.5) > 0.1 {
		t.Fatalf("change = %+v, want the rate up 1.5×", five.Change)
	}
	// $2000 of charge is 2000 × 2.2 / 1.36 at API prices.
	if want := 2000 * 2.2 / 1.36; !five.API.Available || math.Abs(five.API.LimitUSD-want)/want > 0.03 {
		t.Fatalf("api tier = %+v, want the new limit %.0f", five.API, want)
	}
}

func TestClaudeQuotaScreenKeepsOnlyCoveredCleanSteps(t *testing.T) {
	s := &claudeQuotaScreen{
		steps:   []claudeQuotaSpan{{0, 10}, {10, 20}, {20, 30}, {40, 50}},
		dropped: []claudeQuotaSpan{{10, 20}},
	}
	for _, tc := range []struct {
		from, to int64
		want     bool
	}{
		{2, 8, true},
		{0, 10, true},
		{5, 12, false},  // overlaps a left-out step
		{20, 30, true},  // touches one but does not overlap it
		{25, 45, false}, // nothing covers 30..40
		{42, 50, true},
		{45, 55, false}, // runs past the last step
	} {
		if got := s.keeps(tc.from, tc.to); got != tc.want {
			t.Errorf("keeps(%d, %d) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestClaudeQuotaFallsBackToRatioWithoutCrossings(t *testing.T) {
	reset := int64(1793836800)
	start := (reset - 7*86400) * 1000
	events := []claudeCostEvent{
		{DoneMs: start + 1000, Base: 100, CacheRead: 100},
		{DoneMs: start + 2000, Base: 200, CacheRead: 400},
	}
	samples := []claudeQuotaSample{{ObservedMs: start + 3000, Utilization: 40, ResetAt: reset}}
	series := newClaudeCostSeries(events)
	w, _ := buildClaudeQuotaWindow("7d", samples, func(string) *claudeCostSeries { return series }, map[string]modelPrice{}, start+4000, nil)
	if !w.API.Available || w.API.Method != "ratio" || math.Abs(w.API.LimitUSD-2000) > 1e-6 {
		t.Fatalf("api tier = %+v, want ratio limit 2000", w.API)
	}
	if math.Abs(w.API.RemainingUSD-1200) > 1e-6 {
		t.Fatalf("remaining = %.2f, want 1200", w.API.RemainingUSD)
	}
	if w.Fitted.Available || w.Fitted.Reason != "insufficient_points" {
		t.Fatalf("fitted tier = %+v", w.Fitted)
	}
}

func TestComputeClaudeQuotaReportFromDatabase(t *testing.T) {
	db := newProtectionTestDB(t)
	ctx := context.Background()
	now := time.Unix(1793480000, 0)
	for i, model := range []string{"model-a", "sonnet-test", "model-a"} {
		if _, err := db.Exec(`
INSERT INTO usage_events (requested_at, provider, model, auth_type, source, auth_index, auth_id, latency_ms,
  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, total_tokens)
VALUES (?, 'claude', ?, 'oauth', 'user@example.com', 'idx-1', 'claude-user.json', 1000, 1000, 1000, 1000000, 1000, 1003000)`,
			now.Add(time.Duration(-10+i)*time.Minute).Unix(), model); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
INSERT INTO usage_events (requested_at, provider, model, auth_type, source, latency_ms, input_tokens, output_tokens, total_tokens)
VALUES (?, 'claude', 'model-a', 'apikey', 'sk-test', 1000, 1000000, 1000000, 2000000)`, now.Add(-5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	headers := map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":        {"0.10"},
		"Anthropic-Ratelimit-Unified-5h-Reset":              {"1793490000"},
		"Anthropic-Ratelimit-Unified-7d_sonnet-Utilization": {"0.02"},
		"Anthropic-Ratelimit-Unified-7d_sonnet-Reset":       {"1793836800"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":     {"0.30"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":           {"1793836800"},
	}
	s := &claudeQuotaSampler{enabled: true}
	s.record(ctx, db, claudeQuotaTestRecord(now.Add(-time.Minute), headers))

	prices := map[string]modelPrice{
		"model-a":     {Prompt: 10, Completion: 50, CacheRead: 1, CacheReadSet: true, CacheCreation: 12.5, CacheCreateSet: true},
		"sonnet-test": {Prompt: 3, Completion: 15, CacheRead: 0.3, CacheReadSet: true, CacheCreation: 3.75, CacheCreateSet: true},
	}
	report, err := computeClaudeQuotaReport(ctx, db, now, prices)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Account != "user@example.com" {
		t.Fatalf("accounts = %+v", report.Accounts)
	}
	windows := report.Accounts[0].Windows
	if len(windows) != 3 || windows[0].Window != "5h" || windows[1].Window != "7d_oi" || windows[2].Window != "7d_sonnet" {
		t.Fatalf("windows = %+v", windows)
	}
	fiveHour := windows[0]
	// model-a: 1000 in × $10 + 1000 out × $50 + 1000 write × $12.5 = $0.0725 base, 1M read × $1 = $1 cache.
	wantBase := 2*0.0725 + (1000*3+1000*15+1000*3.75)/1e6
	wantCache := 2*1.0 + 0.3
	if math.Abs(fiveHour.CycleBaseCost-wantBase) > 1e-9 || math.Abs(fiveHour.CycleCacheReadCost-wantCache) > 1e-9 {
		t.Fatalf("5h cycle costs = %.6f / %.6f, want %.6f / %.6f (API-key usage must not count)", fiveHour.CycleBaseCost, fiveHour.CycleCacheReadCost, wantBase, wantCache)
	}
	if !fiveHour.API.Available || fiveHour.API.Method != "ratio" || math.Abs(fiveHour.API.LimitUSD-(wantBase+wantCache)/0.10) > 1e-6 {
		t.Fatalf("5h api tier = %+v", fiveHour.API)
	}
	if len(fiveHour.Models) != 2 || fiveHour.Models[0].Model != "model-a" || fiveHour.Models[0].APICacheReadPerM != 1 || fiveHour.Models[0].InputPerM != 10 {
		t.Fatalf("5h models = %+v", fiveHour.Models)
	}
	if oi := windows[1]; oi.Estimated || oi.Note != "unscoped_window" || oi.Utilization != 30 {
		t.Fatalf("7d_oi = %+v, want shown but not estimated", oi)
	}
	if sonnet := windows[2]; !sonnet.Estimated || math.Abs(sonnet.CycleCacheReadCost-0.3) > 1e-9 {
		t.Fatalf("7d_sonnet = %+v, want only sonnet usage", sonnet)
	}
}

func TestClaudeQuotaManagementRejectsWrites(t *testing.T) {
	resp := handleClaudeQuotaManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginID + "/claude-quota"})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSummaryClaudePoolCountsSubscriptionLoginsOnly(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, row := range []struct{ provider, authType, source string }{
		{"claude", "oauth", "user@example.com"},
		{"claude", "oauth", "user@example.com"},
		{"claude", "apikey", "sk-ant-test"},
		{"codex", "oauth", "codex@example.com"},
	} {
		if _, err := db.Exec(`INSERT INTO usage_events (requested_at, provider, auth_type, source, auth_index, model, input_tokens, output_tokens, total_tokens)
VALUES (?, ?, ?, ?, ?, 'model-a', 10, 10, 20)`, now-60, row.provider, row.authType, row.source, row.source); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.summaryOnce(context.Background(), "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	accounts := data["claude_accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].Source != "user@example.com" || accounts[0].Requests != 2 {
		t.Fatalf("claude accounts = %+v, want the one subscription login", accounts)
	}
	if totals := data["claude_totals"].(totalsRow); totals.Requests != 2 {
		t.Fatalf("claude totals = %d requests, want 2 (API keys excluded)", totals.Requests)
	}
	if totals := data["provider_totals"].(totalsRow); totals.Requests != 1 {
		t.Fatalf("provider totals = %d requests, want only the Claude API key (logins move to the pool like Codex)", totals.Requests)
	}
}

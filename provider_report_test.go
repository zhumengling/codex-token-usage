package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestProviderReportFiltersBeforePagination(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte(`api-keys:
  codex:
  - {name: Small, base-url: 'https://small.example', keys: [{api-key: fixture-small}]}
  - {name: Large, base-url: 'https://large.example', keys: [{api-key: fixture-large}]}
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_CONFIG_PATH", config)
	entries := readConfiguredProviderEntries()
	if len(entries) != 2 {
		t.Fatalf("expected two configured providers, got %d", len(entries))
	}
	ctx, usageStore := context.Background(), newTestStore(t)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 31; index++ {
		event := usageRecord{Provider: "codex", AuthType: "apikey", AuthID: entries[0].AuthID,
			RequestedAt: now.Add(-2*time.Hour + time.Duration(index)*time.Second), Model: "model-" + string(rune('A'+index%21)),
			Detail: usageDetail{InputTokens: 1, TotalTokens: 1}}
		if index < 7 {
			event.Failure.StatusCode = []int{401, 402, 403, 429, 500, 503, 418}[index]
			event.Failed = true
		}
		if err := usageStore.recordUsage(ctx, event); err != nil {
			t.Fatal(err)
		}
		event.Model, event.AuthID, event.RequestedAt = "large-model", entries[1].AuthID, now
		event.Detail.InputTokens, event.Detail.TotalTokens = 1000, 1000
		if err := usageStore.recordUsage(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	db, _, err := usageStore.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := queryProviders(ctx, db, now.Add(-24*time.Hour).Unix(), 1, "other")
	if err != nil || len(providers) != 2 || providers[1].Provider != entries[0].Name || providers[1].Requests != 31 {
		t.Fatalf("provider beyond the limit lost its usage: %+v, %v", providers, err)
	}
	first, err := queryProviderReport(ctx, db, entries[0].Name, "24h", 1, 1, now, entries, nil, providerBudgetConfig{})
	if err != nil || first.Totals.Requests != 31 || first.Totals.Failed != 7 || first.Totals.UnpricedTokens != 31 ||
		len(first.Trend) != 1 || first.Trend[0].Requests != 31 || !first.MoreModels || !first.MoreRecent || len(first.Models) != 20 || len(first.Recent) != 30 {
		t.Fatalf("first report page: %+v, %v", first, err)
	}
	for status, want := range map[string]int64{"401": 1, "402": 1, "403": 1, "429": 1, "5xx": 2, "other": 1, "ok": 24} {
		if first.Statuses[status] != want {
			t.Errorf("status %s = %d, want %d", status, first.Statuses[status], want)
		}
	}
	second, err := queryProviderReport(ctx, db, entries[0].Name, "24h", 2, 2, now, entries, nil, providerBudgetConfig{})
	if err != nil || second.MoreModels || second.MoreRecent || len(second.Models) != 1 || len(second.Recent) != 1 || second.Recent[0].StatusCode != 401 || second.Totals != first.Totals {
		t.Fatalf("second report page: %+v, %v", second, err)
	}
}

func TestProviderBudgetCalendarMonthAndUnpricedUsage(t *testing.T) {
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	ctx, usageStore := context.Background(), newTestStore(t)
	zone := time.FixedZone("fixture", 8*3600)
	month := time.Date(2026, time.November, 1, 0, 0, 0, 0, zone)
	now := month.AddDate(0, 0, 3)
	const provider = "vendor-fixture"
	for _, event := range []struct {
		model string
		at    time.Time
		input int64
	}{{"fixture-priced", month.Add(-time.Second), 100}, {"fixture-priced", month, 80},
		{"fixture-priced", now, 20}, {"fixture-unpriced", now, 5}, {"fixture-priced", now.Add(time.Second), 100}} {
		if err := usageStore.recordUsage(ctx, usageRecord{Provider: provider, Model: event.model, RequestedAt: event.at,
			Detail: usageDetail{InputTokens: event.input, TotalTokens: event.input}}); err != nil {
			t.Fatal(err)
		}
	}
	db, _, err := usageStore.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]modelPrice{"fixture-priced": {Prompt: 1e6, Completion: 1e6}}
	for _, fixture := range []struct {
		limit  float64
		status string
	}{{125, "warning"}, {100, "over"}, {200, "incomplete"}} {
		report, err := queryProviderReport(ctx, db, provider, "24h", 1, 1, now, nil, prices, providerBudgetConfig{Budgets: map[string]float64{provider: fixture.limit}})
		if err != nil || math.Abs(report.Budget.CostUSD-100) > 1e-10 || report.Budget.Status != fixture.status || report.Budget.UnpricedTokens != 5 ||
			report.Budget.MonthStart != month.Unix() || report.Budget.MonthEnd != month.AddDate(0, 1, 0).Unix() || report.Totals.CostUSD != 20 {
			t.Fatalf("budget %s: budget=%+v totals=%+v err=%v", fixture.status, report.Budget, report.Totals, err)
		}
	}
	for _, raw := range []string{"provider_budgets_usd: [1]", "provider_budgets_usd: {fixture: .inf}", "provider_budgets_usd: {fixture: -1}", "provider_budgets_usd: {fixture: nope}"} {
		if got := parseProviderBudgets([]byte(raw)); !got.Invalid || len(got.Budgets) != 0 {
			t.Errorf("malformed budget accepted: %q -> %+v", raw, got)
		}
	}
	if got := parseProviderBudgets([]byte("priority: 120\nprovider_budgets_usd: {'Codex · A': 20.5}")); got.Invalid || got.Budgets["Codex · A"] != 20.5 {
		t.Fatalf("valid budget config not parsed: %+v", got)
	}
}

func TestUsageHandleDistinguishesWriteAndStateFailures(t *testing.T) {
	ctx := context.Background()
	previous := globalStore
	globalStore = newTestStore(t)
	t.Cleanup(func() { globalStore = previous })
	db, _, err := globalStore.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(statement string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	call := func(wantCode string) {
		t.Helper()
		raw, err := handleMethod("usage.handle", []byte(`{"provider":"codex","model":"fixture","detail":{"total_tokens":1}}`))
		var result envelope
		if err != nil || json.Unmarshal(raw, &result) != nil || (wantCode == "" && !result.OK) ||
			(wantCode != "" && (result.OK || result.Error == nil || result.Error.Code != wantCode)) {
			t.Fatalf("usage RPC: want error %q, got %s, %v", wantCode, raw, err)
		}
	}
	exec(`CREATE TRIGGER reject_fixture BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT, 'fixture insert rejection'); END`)
	call("usage_write_failed")
	exec(`DROP TRIGGER reject_fixture`)
	exec(`DROP TABLE account_protection_reservations`)
	call("")
	var rows int
	status := globalStore.ingestion.status()
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_events").Scan(&rows); err != nil || rows != 1 ||
		status.WriteFailures != 1 || status.StateUpdateFailures != 1 || status.LastWriteFailureAt == 0 || status.LastStateFailureAt == 0 {
		t.Fatalf("usage is not persisted exactly once: rows=%d diagnostics=%+v error=%v", rows, status, err)
	}
}

func TestUsageFilesAndCSVProtection(t *testing.T) {
	for value, want := range map[string]string{"=1+1": "'=1+1", "\t@SUM(1)": "'\t@SUM(1)", "  ＋1": "'  ＋1", "normal": "normal", "-42": "'-42"} {
		if got := safeCSVCell("provider", value); got != want {
			t.Errorf("CSV cell %q = %q, want %q", value, got, want)
		}
	}
	if got := safeCSVCell("cost_usd", "-0.0125"); got != "-0.0125" {
		t.Errorf("numeric CSV cell changed: %q", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("CPA_TOKEN_USAGE_DIR", filepath.Join(t.TempDir(), "private"))
	path, err := usageDBPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(name, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := secureUsageFiles(path); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600, path + "-wal": 0600, path + "-shm": 0600} {
		if info, err := os.Stat(name); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: want mode %o, got %v, %v", name, mode, info, err)
		}
	}
}

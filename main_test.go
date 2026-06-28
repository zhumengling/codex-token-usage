package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSanitizeTriggerErrorKeepsErrorText(t *testing.T) {
	if got := sanitizeTriggerError(errors.New("context canceled")); got != "context canceled" {
		t.Fatalf("sanitizeTriggerError(error) = %q, want context canceled", got)
	}
	if got := sanitizeTriggerError(map[string]any{}); got == "{}" {
		t.Fatalf("sanitizeTriggerError(empty map) = %q, want no raw JSON object noise", got)
	}
}

func TestCodex429AutobanFiltersSchedulerCandidate(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(time.Hour).Unix()
	err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "auth-banned",
		AuthIndex:   "idx-banned",
		Source:      "banned@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-primary-window-minutes": {"300"},
		},
	})
	if err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-banned", Provider: "codex", Priority: 100},
			{ID: "auth-ok", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "auth-ok" {
		t.Fatalf("pickAuth response = %+v, want handled auth-ok", resp)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans, ok := data["autobans"].([]autobanRow)
	if !ok || len(bans) != 1 {
		t.Fatalf("summary autobans = %#v, want one active ban", data["autobans"])
	}
	if bans[0].AuthID != "auth-banned" || bans[0].Window != "5h" {
		t.Fatalf("ban = %+v, want auth-banned 5h", bans[0])
	}
}

func TestExpiredAutobanWithMillisecondResetIsClearedFromSummary(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	ctx := context.Background()
	store := &store{}
	defer store.close()

	expiredResetMS := time.Now().Add(-time.Minute).UnixMilli()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "expired-ban@example.com",
		AuthIndex:   "expired-ban",
		Source:      "expired-ban@example.com",
		RequestedAt: time.Now().Add(-2 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {strconv.FormatInt(expiredResetMS, 10)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 0 {
		t.Fatalf("autobans = %#v, want expired millisecond reset ban cleared", bans)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].PrimaryUsedPercent != nil || accounts[0].PrimaryResetAt != nil {
		t.Fatalf("accounts = %#v, want expired quota snapshot cleared", accounts)
	}
}

func TestAutobanSummaryUsesLatestQuotaSnapshotForDisplay(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	banResetAt := time.Now().Add(7 * 24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "quota-display@example.com",
		AuthIndex:   "quota-display",
		Source:      "quota-display@example.com",
		RequestedAt: time.Now().Add(-30 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(time.Now().Add(2 * time.Hour).Unix())},
			"x-codex-secondary-used-percent": {"100"},
			"x-codex-secondary-reset-at":     {intToString(banResetAt)},
		},
	}); err != nil {
		t.Fatalf("record ban usage returned error: %v", err)
	}

	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	latestPrimary := 30.0
	latestSecondary := 100.0
	primaryResetAt := time.Now().Add(4 * time.Hour).Unix()
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "quota-display@example.com",
		AuthIndex:            "quota-display",
		Source:               "quota-display@example.com",
		Provider:             "codex",
		Mode:                 "quota",
		Status:               "success",
		StartedAt:            time.Now().Add(-5 * time.Minute).Unix(),
		FinishedAt:           time.Now().Add(-4 * time.Minute).Unix(),
		PrimaryUsedPercent:   &latestPrimary,
		PrimaryResetAt:       &primaryResetAt,
		SecondaryUsedPercent: &latestSecondary,
		SecondaryResetAt:     &banResetAt,
	}); err != nil {
		t.Fatalf("record quota trigger run: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 {
		t.Fatalf("autobans = %#v, want one active weekly ban", bans)
	}
	if bans[0].PrimaryUsedPercent == nil || math.Abs(*bans[0].PrimaryUsedPercent-30.0) > 0.000001 {
		t.Fatalf("autoban primary percent = %v, want latest 30", bans[0].PrimaryUsedPercent)
	}
	if bans[0].SecondaryUsedPercent == nil || math.Abs(*bans[0].SecondaryUsedPercent-100.0) > 0.000001 {
		t.Fatalf("autoban secondary percent = %v, want latest 100 to keep weekly ban active", bans[0].SecondaryUsedPercent)
	}
}

func TestCodex401InvalidAuthFiltersUntilAuthFileReplaced(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "broken.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":         "broken@example.com",
		"name":          "Broken",
		"type":          "codex",
		"access_token":  "old-secret",
		"refresh_token": "old-refresh",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	oldMod := time.Now().Add(-time.Hour)
	if err := os.Chtimes(authFile, oldMod, oldMod); err != nil {
		t.Fatalf("chtimes old auth file: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "broken@example.com",
		AuthIndex:   "broken.cpa.json",
		Source:      "broken@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "broken@example.com", Provider: "codex", Priority: 100},
			{ID: "healthy@example.com", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "healthy@example.com" {
		t.Fatalf("pickAuth response = %+v, want healthy account", resp)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	invalids, ok := data["invalid_auths"].([]invalidAuthRow)
	if !ok || len(invalids) != 1 {
		t.Fatalf("invalid_auths = %#v, want one invalid auth", data["invalid_auths"])
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || !accounts[0].InvalidAuth {
		t.Fatalf("accounts = %#v, want invalid auth marked", accounts)
	}

	newMod := time.Now().Add(time.Hour)
	if err := os.WriteFile(authFile, []byte(`{"email":"broken@example.com","type":"codex","access_token":"new-secret"}`), 0600); err != nil {
		t.Fatalf("replace auth file: %v", err)
	}
	if err := os.Chtimes(authFile, newMod, newMod); err != nil {
		t.Fatalf("chtimes new auth file: %v", err)
	}

	resp, err = store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "broken@example.com", Provider: "codex", Priority: 100},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth after replace returned error: %v", err)
	}
	if resp.Handled {
		t.Fatalf("pickAuth after replace = %+v, want unhandled so CPA keeps configured scheduler", resp)
	}
}

func TestSchedulerKeepsHostFillFirstWhenNoFilteringNeeded(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	resp, err := store.pickAuth(context.Background(), schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-a", Provider: "codex", Priority: 100},
			{ID: "auth-b", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if resp.Handled || resp.DelegateBuiltin != "" {
		t.Fatalf("pickAuth = %+v, want unhandled to preserve CPA fill-first/round-robin setting", resp)
	}
}

func TestSchedulerDoesNotHandleNonCodexRoute(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	resp, err := store.pickAuth(context.Background(), schedulerPickRequest{
		Provider: "claude",
		Candidates: []schedulerAuthCandidate{
			{ID: "claude-a", Provider: "claude", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if resp.Handled {
		t.Fatalf("non-codex pickAuth response = %+v, want unhandled", resp)
	}
}

func TestSummaryMergesConfiguredCodexAccountsWithoutLeakingTokens(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)

	ctx := context.Background()
	store := &store{}
	defer store.close()

	for i := 1; i <= 12; i++ {
		email := fmt.Sprintf("account%02d@example.com", i)
		authFile := filepath.Join(authDir, email+".cpa.json")
		raw, err := json.Marshal(map[string]any{
			"email":         email,
			"name":          fmt.Sprintf("Account %02d", i),
			"type":          "codex",
			"plan_type":     "plus",
			"disabled":      i == 12,
			"expired":       false,
			"access_token":  "secret-access-token",
			"refresh_token": "secret-refresh-token",
			"id_token":      "secret-id-token",
		})
		if err != nil {
			t.Fatalf("marshal auth file: %v", err)
		}
		if err := os.WriteFile(authFile, raw, 0600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}
		if i <= 9 {
			if err := store.recordUsage(ctx, usageRecord{
				Provider:    "codex",
				AuthID:      email,
				AuthIndex:   fmt.Sprintf("%016x", i),
				Source:      email,
				RequestedAt: time.Now(),
				Detail: usageDetail{
					InputTokens:  100,
					OutputTokens: 50,
				},
			}); err != nil {
				t.Fatalf("recordUsage %d returned error: %v", i, err)
			}
		}
	}

	data, err := store.summary(ctx, "24h", 2000)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts, ok := data["accounts"].([]accountRow)
	if !ok {
		t.Fatalf("summary accounts = %#v, want []accountRow", data["accounts"])
	}
	if len(accounts) != 12 {
		t.Fatalf("summary accounts len = %d, want 12", len(accounts))
	}
	configured := 0
	zeroUsage := 0
	disabled := 0
	for _, account := range accounts {
		if account.Configured {
			configured++
		}
		if account.Requests == 0 {
			zeroUsage++
		}
		if account.Disabled {
			disabled++
		}
	}
	if configured != 12 || zeroUsage != 3 || disabled != 1 {
		t.Fatalf("configured=%d zeroUsage=%d disabled=%d, want 12/3/1", configured, zeroUsage, disabled)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if text := string(raw); strings.Contains(text, "secret-access-token") || strings.Contains(text, "secret-refresh-token") || strings.Contains(text, "secret-id-token") {
		t.Fatalf("summary leaked token material: %s", text)
	}
}

func TestSummaryFlagsQuotaDropWithoutLocalUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(4 * time.Hour).Unix()
	account := "shared@example.com"
	first := time.Now().Add(-20 * time.Minute)
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      account,
			AuthIndex:   "shared-account",
			Source:      account,
			RequestedAt: first,
			ResponseHeaders: map[string][]string{
				"x-codex-primary-used-percent": {"12"},
				"x-codex-primary-reset-at":     {intToString(resetAt)},
			},
		},
		{
			Provider:    "codex",
			AuthID:      account,
			AuthIndex:   "shared-account",
			Source:      account,
			RequestedAt: first.Add(15 * time.Minute),
			ResponseHeaders: map[string][]string{
				"x-codex-primary-used-percent": {"18.5"},
				"x-codex-primary-reset-at":     {intToString(resetAt)},
			},
		},
	}
	for i, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage %d returned error: %v", i, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	alerts, ok := data["external_use_alerts"].([]externalUseAlert)
	if !ok || len(alerts) != 1 {
		t.Fatalf("external_use_alerts = %#v, want one alert", data["external_use_alerts"])
	}
	if alerts[0].Window != "5h" || alerts[0].DeltaPercent != 6.5 || alerts[0].LocalTokens != 0 {
		t.Fatalf("alert = %+v, want 5h delta 6.5 local 0", alerts[0])
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || !accounts[0].ExternalUseSuspected || accounts[0].ExternalUseWindow != "5h" {
		t.Fatalf("accounts = %#v, want external use suspected on 5h", accounts)
	}
}

func TestSummaryEstimatesSecondaryQuotaCapacity(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "quota@example.com",
		AuthIndex:   "quota-account",
		Source:      "quota@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  200,
			OutputTokens: 50,
			TotalTokens:  250,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"25"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 750 {
		t.Fatalf("account quota estimates = total %d remaining %d, want 1000/750", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
	totals := data["totals"].(totalsRow)
	if totals.SecondaryQuotaTotalEstimate != 1000 || totals.SecondaryQuotaRemainingEstimate != 750 || totals.SecondaryQuotaEstimatedAccounts != 1 {
		t.Fatalf("total quota estimates = total %d remaining %d accounts %d, want 1000/750/1", totals.SecondaryQuotaTotalEstimate, totals.SecondaryQuotaRemainingEstimate, totals.SecondaryQuotaEstimatedAccounts)
	}
}

func TestSecondaryQuotaEstimateAdjustsTriggerRemainingWithLaterUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	resetAt := time.Now().Add(48 * time.Hour).Unix()
	finishedAt := time.Now().Add(-2 * time.Minute).Unix()
	limit := int64(1000)
	remaining := int64(775)
	secondaryPct := 22.5
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "paid@example.com",
		AuthIndex:            "paid-account",
		Source:               "paid@example.com",
		Provider:             "codex",
		Mode:                 "quota",
		Status:               "success",
		StartedAt:            finishedAt - 1,
		FinishedAt:           finishedAt,
		SecondaryUsedPercent: &secondaryPct,
		SecondaryResetAt:     &resetAt,
		SecondaryLimit:       &limit,
		SecondaryRemaining:   &remaining,
	}); err != nil {
		t.Fatalf("record quota trigger run: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "paid@example.com",
		AuthIndex:   "paid-account",
		Source:      "paid@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  100,
			OutputTokens: 25,
			TotalTokens:  125,
		},
	}); err != nil {
		t.Fatalf("record later usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 650 {
		t.Fatalf("secondary quota estimate = total %d remaining %d, want trigger total 1000 and adjusted remaining 650", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
}

func TestFreeAccountUsesMonthlyQuotaWindowIndependentOfSummaryWindow(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "free.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "free@example.com",
		"type":         "codex",
		"plan_type":    "free",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	resetAt := time.Now().Add(20 * 24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "free@example.com",
		AuthIndex:   "free.cpa.json",
		Source:      "free@example.com",
		RequestedAt: time.Now().Add(-10 * 24 * time.Hour),
		Detail: usageDetail{
			InputTokens:  240,
			OutputTokens: 60,
			TotalTokens:  300,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"30"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want configured free account", len(accounts))
	}
	if accounts[0].SecondaryQuotaWindow != "month" {
		t.Fatalf("secondary quota window = %q, want month", accounts[0].SecondaryQuotaWindow)
	}
	if accounts[0].SecondaryWindowTokens != 300 || accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 700 {
		t.Fatalf("monthly quota = window tokens %d total %d remaining %d, want 300/1000/700", accounts[0].SecondaryWindowTokens, accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
}

func TestSummaryClearsExpiredQuotaSnapshots(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(-time.Minute).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "expired-quota@example.com",
		AuthIndex:   "expired-quota",
		Source:      "expired-quota@example.com",
		RequestedAt: time.Now().Add(-10 * time.Minute),
		Detail: usageDetail{
			InputTokens:  800,
			OutputTokens: 200,
			TotalTokens:  1000,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"85"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-secondary-used-percent": {"90"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].PrimaryUsedPercent != nil || accounts[0].SecondaryUsedPercent != nil {
		t.Fatalf("quota percent = primary %v secondary %v, want cleared", accounts[0].PrimaryUsedPercent, accounts[0].SecondaryUsedPercent)
	}
	if accounts[0].PrimaryWindowTokens != 0 || accounts[0].SecondaryWindowTokens != 0 {
		t.Fatalf("window tokens = primary %d secondary %d, want 0/0", accounts[0].PrimaryWindowTokens, accounts[0].SecondaryWindowTokens)
	}
	totals := data["totals"].(totalsRow)
	if totals.SecondaryQuotaTotalEstimate != 0 || totals.SecondaryQuotaRemainingEstimate != 0 || totals.SecondaryQuotaEstimatedAccounts != 0 {
		t.Fatalf("total quota estimates = total %d remaining %d accounts %d, want 0/0/0", totals.SecondaryQuotaTotalEstimate, totals.SecondaryQuotaRemainingEstimate, totals.SecondaryQuotaEstimatedAccounts)
	}
}

func TestSummaryUsesLatestQuotaSnapshotInsteadOfMaxPercent(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      "latest-quota@example.com",
			AuthIndex:   "latest-quota",
			Source:      "latest-quota@example.com",
			RequestedAt: time.Now().Add(-30 * time.Minute),
			Detail:      usageDetail{InputTokens: 100, TotalTokens: 100},
			ResponseHeaders: map[string][]string{
				"x-codex-secondary-used-percent": {"80"},
				"x-codex-secondary-reset-at":     {intToString(resetAt)},
			},
		},
		{
			Provider:    "codex",
			AuthID:      "latest-quota@example.com",
			AuthIndex:   "latest-quota",
			Source:      "latest-quota@example.com",
			RequestedAt: time.Now().Add(-5 * time.Minute),
			Detail:      usageDetail{InputTokens: 50, TotalTokens: 50},
			ResponseHeaders: map[string][]string{
				"x-codex-secondary-used-percent": {"10"},
				"x-codex-secondary-reset-at":     {intToString(resetAt)},
			},
		},
	}
	for i, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage %d returned error: %v", i, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].SecondaryUsedPercent == nil || math.Abs(*accounts[0].SecondaryUsedPercent-10) > 0.000001 {
		t.Fatalf("accounts = %#v, want latest secondary percent 10", accounts)
	}
}

func TestQuotaTriggerDefaultConfigIsDisabled(t *testing.T) {
	cfg := normalizePluginConfig(defaultPluginConfig())
	if cfg.QuotaTriggerEnabled {
		t.Fatalf("default quota trigger enabled = true, want false")
	}
	if cfg.QuotaTriggerMode != "probe" || cfg.QuotaTriggerIntervalMinutes != 10 || cfg.QuotaTriggerMinAccountCooldownMinutes != 10 {
		t.Fatalf("default config = %+v, want probe/10m/10m", cfg)
	}
	decoded := parsePluginConfigYAML([]byte("quota_trigger_enabled: true\nquota_trigger_mode: probe\nquota_trigger_interval_minutes: 5\n"), defaultPluginConfig())
	decoded = normalizePluginConfig(decoded)
	if !decoded.QuotaTriggerEnabled || decoded.QuotaTriggerMode != "probe" || decoded.QuotaTriggerIntervalMinutes != 5 {
		t.Fatalf("decoded config = %+v, want enabled probe 5m", decoded)
	}
	chinese := parsePluginConfigYAML([]byte("开启定时额度触发: true\n触发模式: 探测请求\n触发间隔分钟: 6\n最大并发账号数: 2\n单账号超时秒数: 12\n单账号最小冷却分钟: 7\n"), defaultPluginConfig())
	chinese = normalizePluginConfig(chinese)
	if !chinese.QuotaTriggerEnabled ||
		chinese.QuotaTriggerMode != "probe" ||
		chinese.QuotaTriggerIntervalMinutes != 6 ||
		chinese.QuotaTriggerMaxConcurrency != 2 ||
		chinese.QuotaTriggerTimeoutSeconds != 12 ||
		chinese.QuotaTriggerMinAccountCooldownMinutes != 7 {
		t.Fatalf("chinese config = %+v, want enabled probe 6m/2/12s/7m", chinese)
	}
}

func TestQuotaTriggerQuotaModeUpdatesSnapshotAndCooldown(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "quota-trigger.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "quota-trigger@example.com",
		"type":         "codex",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-access-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode probe body: %v", err)
		}
		if req["model"] != codexProbeModel || req["stream"] != false {
			t.Fatalf("probe body = %#v, want model %s and non-stream", req, codexProbeModel)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":12.5,"reset_at":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":22.5,"reset_at":%d,"limit_window_seconds":604800,"remaining_tokens":775,"limit_tokens":1000}}}`, resetAt, resetAt)))
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 1 || failed != 0 || skipped != 0 || candidates != 1 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want 1/0/0/1", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].PrimaryUsedPercent == nil || math.Abs(*accounts[0].PrimaryUsedPercent-12.5) > 0.000001 {
		t.Fatalf("accounts = %#v, want primary quota 12.5 from trigger", accounts)
	}
	if accounts[0].QuotaTriggerStatus != "success" || accounts[0].QuotaTriggerLastAt == "" {
		t.Fatalf("quota trigger account status = %+v, want success with time", accounts[0])
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 775 {
		t.Fatalf("secondary quota estimate = total %d remaining %d, want trigger absolute 1000/775", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}

	success, failed, skipped, candidates, err = store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("second runQuotaTriggerRound returned error: %v", err)
	}
	if success != 0 || failed != 0 || skipped != 1 || candidates != 0 {
		t.Fatalf("second round = success %d failed %d skipped %d candidates %d, want cooldown skip 0/0/1/0", success, failed, skipped, candidates)
	}
}

func TestQuotaTriggerRefreshesAndReleasesActiveAutoban(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "autoban-refresh.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "autoban-refresh@example.com",
		"type":         "codex",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	banResetAt := time.Now().Add(2 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "autoban-refresh@example.com",
		AuthIndex:   "autoban-refresh.cpa.json",
		Source:      "autoban-refresh@example.com",
		RequestedAt: time.Now().Add(-10 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {intToString(banResetAt)},
		},
	}); err != nil {
		t.Fatalf("record ban usage returned error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-access-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":45,"reset_at":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":12,"reset_at":%d,"limit_window_seconds":604800}}}`, banResetAt, time.Now().Add(6*24*time.Hour).Unix())))
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 1 || failed != 0 || skipped != 0 || candidates != 1 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want active ban refreshed 1/0/0/1", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 0 {
		t.Fatalf("autobans = %#v, want 5h ban released after quota refresh", bans)
	}
}

func TestExternalUseUsesQuotaTriggerSnapshotsWithFivePercentThreshold(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	resetAt := time.Now().Add(6 * 24 * time.Hour).Unix()
	first := time.Now().Add(-30 * time.Minute).Unix()
	second := time.Now().Add(-10 * time.Minute).Unix()
	p20, p26 := 20.0, 26.0
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "shared-trigger@example.com",
		AuthIndex:            "shared-trigger.cpa.json",
		Source:               "shared-trigger@example.com",
		Provider:             "codex",
		AuthFile:             "shared-trigger.cpa.json",
		Mode:                 "quota",
		Status:               "success",
		HTTPStatus:           200,
		StartedAt:            first,
		FinishedAt:           first,
		SecondaryUsedPercent: &p20,
		SecondaryResetAt:     &resetAt,
	}); err != nil {
		t.Fatalf("record first trigger: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "quota-trigger",
		Model:        "quota-trigger",
		Alias:        "quota-trigger",
		AuthID:       "shared-trigger@example.com",
		AuthIndex:    "shared-trigger.cpa.json",
		Source:       "shared-trigger@example.com",
		RequestedAt:  time.Unix(first+600, 0),
		Detail:       usageDetail{TotalTokens: 999999},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"25"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("record quota-trigger usage: %v", err)
	}
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "shared-trigger@example.com",
		AuthIndex:            "shared-trigger.cpa.json",
		Source:               "shared-trigger@example.com",
		Provider:             "codex",
		AuthFile:             "shared-trigger.cpa.json",
		Mode:                 "quota",
		Status:               "success",
		HTTPStatus:           200,
		StartedAt:            second,
		FinishedAt:           second,
		SecondaryUsedPercent: &p26,
		SecondaryResetAt:     &resetAt,
	}); err != nil {
		t.Fatalf("record second trigger: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	alerts := data["external_use_alerts"].([]externalUseAlert)
	if len(alerts) != 1 {
		t.Fatalf("external_use_alerts = %#v, want one alert", alerts)
	}
	if alerts[0].Window != "7d" || alerts[0].DeltaPercent != 6 || alerts[0].LocalTokens != 0 {
		t.Fatalf("alert = %+v, want 7d delta 6 local 0", alerts[0])
	}
}

func TestQuotaTriggerFiltersBadAccountsAndRecords401429(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	fixtures := []struct {
		name     string
		email    string
		token    string
		disabled bool
		expired  bool
	}{
		{name: "invalid.cpa.json", email: "invalid@example.com", token: "invalid-token"},
		{name: "limited.cpa.json", email: "limited@example.com", token: "limited-token"},
		{name: "disabled.cpa.json", email: "disabled@example.com", token: "disabled-token", disabled: true},
		{name: "expired.cpa.json", email: "expired@example.com", token: "expired-token", expired: true},
	}
	for _, fixture := range fixtures {
		raw, err := json.Marshal(map[string]any{
			"email":        fixture.email,
			"type":         "codex",
			"access_token": fixture.token,
			"disabled":     fixture.disabled,
			"expired":      fixture.expired,
		})
		if err != nil {
			t.Fatalf("marshal auth: %v", err)
		}
		if err := os.WriteFile(filepath.Join(authDir, fixture.name), raw, 0600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
	}

	resetAt := time.Now().Add(time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer invalid-token":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer limited-token":
			w.Header().Set("x-codex-primary-used-percent", "100")
			w.Header().Set("x-codex-primary-reset-at", strconv.FormatInt(resetAt, 10))
			w.Header().Set("x-codex-primary-window-minutes", "300")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Fatalf("unexpected trigger token: %s", r.Header.Get("Authorization"))
		}
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 0 || failed != 2 || skipped != 2 || candidates != 2 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want 0/2/2/2", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	invalids := data["invalid_auths"].([]invalidAuthRow)
	if len(invalids) != 1 || invalids[0].AuthID != "invalid@example.com" {
		t.Fatalf("invalid_auths = %#v, want invalid@example.com", invalids)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 || bans[0].AuthID != "limited@example.com" {
		t.Fatalf("autobans = %#v, want limited@example.com", bans)
	}
	rawSummary, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(rawSummary), "invalid-token") || strings.Contains(string(rawSummary), "limited-token") {
		t.Fatalf("summary leaked trigger token material: %s", rawSummary)
	}
}

func TestSummaryCalculatesModelCosts(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "priced@example.com",
		AuthIndex:   "priced-account",
		Source:      "priced@example.com",
		Model:       "gpt-5.5",
		Alias:       "gpt-5.5",
		ServiceTier: "default",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  1_000_000,
			OutputTokens: 500_000,
			CachedTokens: 200_000,
			TotalTokens:  1_500_000,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	totals := data["totals"].(totalsRow)
	const wantCost = 19.1
	if math.Abs(totals.CostUSD-wantCost) > 0.000001 || !totals.CostAvailable {
		t.Fatalf("totals cost = %.8f available=%v, want %.2f true", totals.CostUSD, totals.CostAvailable, wantCost)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || math.Abs(accounts[0].CostUSD-wantCost) > 0.000001 || !accounts[0].CostAvailable {
		t.Fatalf("accounts = %#v, want one priced account cost %.2f", accounts, wantCost)
	}
	models := data["models"].([]modelRow)
	if len(models) != 1 || math.Abs(models[0].CostUSD-wantCost) > 0.000001 || !models[0].CostAvailable {
		t.Fatalf("models = %#v, want one priced model cost %.2f", models, wantCost)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	if math.Abs(recent[0].CostUSD-wantCost) > 0.000001 || !recent[0].CostAvailable || recent[0].PriceDetail == "" {
		t.Fatalf("recent cost = %.8f available=%v price=%q, want %.2f true with price detail", recent[0].CostUSD, recent[0].CostAvailable, recent[0].PriceDetail, wantCost)
	}
}

func TestRecentRequestsExposeLatencyAndCacheBreakdown(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "latency@example.com",
		AuthIndex:   "latency-account",
		Source:      "latency@example.com",
		Model:       "gpt-5.5",
		Alias:       "gpt-5.5",
		ServiceTier: "standard",
		Latency:     20_000_000_000,
		TTFT:        4_800_000_000,
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:         85_168,
			OutputTokens:        796,
			CachedTokens:        84_352,
			CacheReadTokens:     84_352,
			CacheCreationTokens: 0,
			TotalTokens:         85_964,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	row := recent[0]
	if row.LatencyMs != 20_000 || row.TTFTMs != 4_800 {
		t.Fatalf("latency fields = %d/%d, want 20000/4800", row.LatencyMs, row.TTFTMs)
	}
	if row.InputTokens != 85_168 || row.OutputTokens != 796 || row.CachedTokens != 84_352 || row.CacheReadTokens != 84_352 {
		t.Fatalf("token breakdown = %+v, want input/output/cache/read populated", row)
	}
	if !row.CostAvailable || row.CostUSD <= 0 || !strings.Contains(row.PriceDetail, "$5 / $30/M") {
		t.Fatalf("recent pricing = cost %.8f available=%v detail=%q, want gpt-5.5 pricing", row.CostUSD, row.CostAvailable, row.PriceDetail)
	}
}

func TestLatencyIsClampedToTTFTForThroughput(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "latency-clamp@example.com",
		AuthIndex:   "latency-clamp",
		Source:      "latency-clamp@example.com",
		Model:       "gpt-5.5",
		Latency:     1,
		TTFT:        1_800_000_000,
		RequestedAt: time.Now(),
		Detail: usageDetail{
			OutputTokens: 900,
			TotalTokens:  900,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	if recent[0].LatencyMs != 1800 || recent[0].TTFTMs != 1800 {
		t.Fatalf("latency fields = %d/%d, want clamped to TTFT 1800/1800", recent[0].LatencyMs, recent[0].TTFTMs)
	}
	totals := data["totals"].(totalsRow)
	if totals.OutputTokensPerSecond > 600 {
		t.Fatalf("throughput = %.2f, want reasonable value after latency clamp", totals.OutputTokensPerSecond)
	}
}

func TestThroughputUsesWeightedReliableSamples(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	now := time.Now()
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      "throughput@example.com",
			AuthIndex:   "throughput",
			Source:      "throughput@example.com",
			Model:       "gpt-5.5",
			Latency:     int64(1300 * time.Millisecond),
			TTFT:        int64(1300 * time.Millisecond),
			RequestedAt: now,
			Detail: usageDetail{
				OutputTokens: 4000,
				TotalTokens:  4000,
			},
		},
		{
			Provider:    "codex",
			AuthID:      "throughput@example.com",
			AuthIndex:   "throughput",
			Source:      "throughput@example.com",
			Model:       "gpt-5.5",
			Latency:     int64(20 * time.Second),
			TTFT:        int64(1500 * time.Millisecond),
			RequestedAt: now.Add(time.Second),
			Detail: usageDetail{
				OutputTokens: 1000,
				TotalTokens:  1000,
			},
		},
	}
	for _, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage returned error: %v", err)
		}
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	totals := data["totals"].(totalsRow)
	if totals.OutputTokensPerSecond < 49 || totals.OutputTokensPerSecond > 51 {
		t.Fatalf("throughput = %.2f, want weighted reliable throughput near 50", totals.OutputTokensPerSecond)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one account", accounts)
	}
	if accounts[0].OutputTokensPerSecond < 49 || accounts[0].OutputTokensPerSecond > 51 {
		t.Fatalf("account throughput = %.2f, want weighted reliable throughput near 50", accounts[0].OutputTokensPerSecond)
	}
}

func TestSummaryCalculatesOpenAICompatibleProviderCostsFromPriceFile(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	priceFile := filepath.Join(t.TempDir(), "model_prices.json")
	t.Setenv("CPA_MODEL_PRICE_FILE", priceFile)
	raw := []byte(`{
		"openrouter/anthropic/claude-sonnet-4.5": {
			"litellm_provider": "openrouter",
			"input_cost_per_token": 0.000003,
			"output_cost_per_token": 0.000015,
			"cache_read_input_token_cost": 0.0000003,
			"cache_creation_input_token_cost": 0.00000375
		}
	}`)
	if err := os.WriteFile(priceFile, raw, 0600); err != nil {
		t.Fatalf("write price file: %v", err)
	}
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "openai-compatible",
		AuthID:      "openai-compatibility:openrouter:upstream-key",
		AuthIndex:   "upstream-account",
		Source:      "openrouter",
		Model:       "anthropic/claude-sonnet-4.5",
		Alias:       "claude-sonnet",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:         1_000_000,
			OutputTokens:        500_000,
			CachedTokens:        200_000,
			CacheReadTokens:     200_000,
			CacheCreationTokens: 100_000,
			TotalTokens:         1_800_000,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	const wantCost = 10.335
	totals := data["totals"].(totalsRow)
	if totals.TotalTokens != 0 || totals.CostUSD != 0 {
		t.Fatalf("codex totals = %#v, want other provider usage excluded", totals)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if math.Abs(providerTotals.CostUSD-wantCost) > 0.000001 || !providerTotals.CostAvailable {
		t.Fatalf("provider totals cost = %.8f available=%v, want %.3f true", providerTotals.CostUSD, providerTotals.CostAvailable, wantCost)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 1 || providers[0].Provider != "openrouter" || math.Abs(providers[0].CostUSD-wantCost) > 0.000001 {
		t.Fatalf("providers = %#v, want openrouter cost %.3f", providers, wantCost)
	}
	models := data["models"].([]modelRow)
	if len(models) != 0 {
		t.Fatalf("codex models = %#v, want other provider models excluded", models)
	}
	providerModels := data["provider_models"].([]modelRow)
	if len(providerModels) != 1 || providerModels[0].Provider != "openrouter" || math.Abs(providerModels[0].CostUSD-wantCost) > 0.000001 {
		t.Fatalf("provider_models = %#v, want openrouter cost %.3f", providerModels, wantCost)
	}
}

func TestUnconfiguredCodexAPIKeyProviderDoesNotPolluteStats(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	ctx := context.Background()
	store := &store{}
	defer store.close()

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "apikey",
		AuthID:       "codex:apikey:b575a2ab1607",
		AuthIndex:    "e88eaa4c2018a1fa",
		Source:       "sk-provider-secret",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  120,
			OutputTokens: 30,
			TotalTokens:  150,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "oauth",
		AuthID:       "real-account@example.com.cpa.json",
		AuthIndex:    "real-account",
		Source:       "real-account@example.com",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  200,
			OutputTokens: 50,
			TotalTokens:  250,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want only oauth Codex account", accounts)
	}
	if strings.Contains(accounts[0].Source, "sk-") || accounts[0].AuthID == "codex:apikey:b575a2ab1607" {
		t.Fatalf("Codex account pool leaked API key provider row: %+v", accounts[0])
	}
	totals := data["totals"].(totalsRow)
	if totals.TotalTokens != 250 {
		t.Fatalf("codex totals = %d, want only oauth tokens 250", totals.TotalTokens)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if providerTotals.TotalTokens != 0 {
		t.Fatalf("provider totals = %d, want unconfigured API-key Codex provider excluded", providerTotals.TotalTokens)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 0 {
		t.Fatalf("providers = %#v, want unconfigured API-key Codex provider hidden", providers)
	}
	providerRecent := data["provider_recent"].([]recentRow)
	if len(providerRecent) != 0 {
		t.Fatalf("provider_recent = %#v, want unconfigured API-key Codex requests hidden", providerRecent)
	}
}

func TestCodexAPIKeyProviderUsesConfiguredEndpointName(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", configPath)
	if err := os.WriteFile(configPath, []byte(`
codex-api-key:
  - api-key: sk-provider-secret
    base-url: https://api.kmoon.site/v1
    models:
      - name: gpt-5.5
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "apikey",
		AuthID:       "codex:apikey:b575a2ab1607",
		AuthIndex:    "e88eaa4c2018a1fa",
		Source:       "sk-provider-secret",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  120,
			OutputTokens: 30,
			TotalTokens:  150,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if providerTotals.TotalTokens != 150 {
		t.Fatalf("provider totals = %d, want configured Codex endpoint tokens 150", providerTotals.TotalTokens)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 1 || providers[0].Provider != "Codex · api.kmoon.site" || providers[0].TotalTokens != 150 {
		t.Fatalf("providers = %#v, want one configured Codex endpoint row", providers)
	}
	providerModels := data["provider_models"].([]modelRow)
	if len(providerModels) != 1 || providerModels[0].Provider != "Codex · api.kmoon.site" {
		t.Fatalf("provider_models = %#v, want configured Codex endpoint name", providerModels)
	}
}

func TestProviderRecentKeepsOlderEndpointRows(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
openai-compatibility:
  - name: 字节
    api-key: sk-byte-secret
codex-api-key:
  - name: Codex · api.kmoon.site
    api-key: sk-codex-secret
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()
	base := time.Now().Add(-time.Hour)
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "openai-compatible-字节",
		AuthID:      "openai-compatibility:字节:test",
		AuthType:    "api_key",
		Source:      "ark-byte-key",
		Model:       "deepseek-v4-pro",
		RequestedAt: base,
		Detail:      usageDetail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
	}); err != nil {
		t.Fatalf("record byte usage: %v", err)
	}
	for i := 0; i < 60; i++ {
		if err := store.recordUsage(ctx, usageRecord{
			Provider:    "codex",
			AuthID:      "codex:apikey:test",
			AuthType:    "apikey",
			Source:      "sk-codex-secret",
			Model:       "gpt-5.5",
			RequestedAt: base.Add(time.Duration(i+1) * time.Minute),
			Failed:      true,
			Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		}); err != nil {
			t.Fatalf("record codex api key usage %d: %v", i, err)
		}
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["provider_recent"].([]recentRow)
	var foundByte bool
	var leakedKey bool
	for _, row := range recent {
		if row.Provider == "字节" {
			foundByte = true
		}
		if strings.Contains(row.Source, "sk-codex-secret") {
			leakedKey = true
		}
	}
	if !foundByte {
		t.Fatalf("provider_recent len=%d missing older 字节 row after newer Codex endpoint rows", len(recent))
	}
	if leakedKey {
		t.Fatalf("provider_recent leaked raw API key: %#v", recent)
	}
}

func TestConfiguredProviderNamesFromYAMLReadsOpenAICompatibilityNames(t *testing.T) {
	raw := `
openai-compatibility:
  - api-key-entries:
      - api-key: sk-redacted
    name: deepseek
  - base-url: http://example.invalid
    name: maas
  - name: '字节'
claude-api-key: []
codex-api-key:
  - api-key: sk-codex-redacted
    base-url: https://api.kmoon.site/v1
    models:
      - name: gpt-5.5
gemini-api-key: []
`
	got := configuredProviderNamesFromYAML(raw)
	want := []string{"deepseek", "maas", "字节", "Codex · api.kmoon.site"}
	if len(got) != len(want) {
		t.Fatalf("names = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %#v, want %#v", got, want)
		}
	}
}

func TestConfiguredProviderEntriesFromYAMLReadsAllCommonProviderEndpoints(t *testing.T) {
	raw := `
openai-compatibility:
  - api-key-entries:
      - api-key: sk-random-openai-compat
    base-url: https://compat-random.example/v1
    name: random-compat-a
openai-compatible:
  - api-key-entries:
      - api-key: sk-random-openai-compatible
    base-url: https://compatible-random.example/v1
    name: random-compat-b
codex-api-key:
  - api-key: sk-random-codex
    base-url: https://codex-random.example/v1
claude-api-key:
  - api-key: sk-random-claude
    base-url: https://claude-random.example/v1
anthropic-api-key:
  - api-key: sk-random-anthropic
    base-url: https://anthropic-random.example/v1
gemini-api-key:
  - api-key: sk-random-gemini
    base-url: https://gemini-random.example/v1
antigravity-api-key:
  - api-key: sk-random-antigravity
    base-url: https://antigravity-random.example/v1
anthropic-oauth:
  - name: random-anthropic-oauth
antigravity-oauth:
  - name: random-antigravity-oauth
`
	entries := configuredProviderEntriesFromYAML(raw)
	got := map[string]providerConfigEntry{}
	for _, entry := range entries {
		got[entry.Name] = entry
	}
	want := map[string]string{
		"random-compat-a":                          "OpenAI",
		"random-compat-b":                          "OpenAI",
		"Codex · codex-random.example":             "Codex",
		"Claude · claude-random.example":           "Claude",
		"Claude · anthropic-random.example":        "Claude",
		"Gemini · gemini-random.example":           "Gemini",
		"Antigravity · antigravity-random.example": "Antigravity",
		"random-anthropic-oauth":                   "Claude",
		"random-antigravity-oauth":                 "Antigravity",
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %#v, want %d common provider endpoints", entries, len(want))
	}
	for name, provider := range want {
		entry, ok := got[name]
		if !ok {
			t.Fatalf("missing provider endpoint %q in %#v", name, entries)
		}
		if entry.Provider != provider {
			t.Fatalf("provider for %q = %q, want %q", name, entry.Provider, provider)
		}
	}
	if got["random-compat-a"].APIKey != "sk-random-openai-compat" {
		t.Fatalf("nested openai-compatible api key was not read: %+v", got["random-compat-a"])
	}
	if got["Codex · codex-random.example"].APIKey != "sk-random-codex" {
		t.Fatalf("codex api key was not read: %+v", got["Codex · codex-random.example"])
	}
}

func TestConfiguredAuthFilesReadCodexAnthropicAndAntigravityOAuth(t *testing.T) {
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	writeAuth := func(name, raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(raw), 0600); err != nil {
			t.Fatalf("write auth %s: %v", name, err)
		}
	}
	writeAuth("codex-random.cpa.json", `{"provider":"codex","email":"codex-random@example.com","access_token":"redacted"}`)
	writeAuth("anthropic-random.json", `{"provider":"anthropic","email":"anthropic-random@example.com","refresh_token":"redacted"}`)
	writeAuth("antigravity-random.json", `{"platform":"antigravity","email":"antigravity-random@example.com","refresh_token":"redacted"}`)

	files := readConfiguredAuthFiles()
	if len(files) != 3 {
		t.Fatalf("auth files = %#v, want codex/anthropic/antigravity", files)
	}
	providers := map[string]string{}
	for _, file := range files {
		providers[file.Email] = file.Provider
	}
	if providers["codex-random@example.com"] != "codex" || providers["anthropic-random@example.com"] != "anthropic" || providers["antigravity-random@example.com"] != "antigravity" {
		t.Fatalf("auth file providers = %#v", providers)
	}
	codexAccounts := readConfiguredAuthAccounts()
	if len(codexAccounts) != 1 || codexAccounts[0].Email != "codex-random@example.com" {
		t.Fatalf("codex account pool auth files = %#v, want only codex OAuth file", codexAccounts)
	}
	triggerAccounts := readTriggerAuthAccounts()
	if len(triggerAccounts) != 1 || triggerAccounts[0].Email != "codex-random@example.com" || triggerAccounts[0].AccessToken != "redacted" {
		t.Fatalf("trigger auth accounts = %#v, want only Codex with access token", triggerAccounts)
	}
}

func TestRetentionConfigParsesChineseAndEnglishFields(t *testing.T) {
	cfg := parsePluginConfigYAML([]byte(`
usage_retention_days: 120
额度触发记录保留天数: 45
请求明细保留天数: 20
`), defaultPluginConfig())
	cfg = normalizePluginConfig(cfg)
	if cfg.UsageRetentionDays != 120 || cfg.QuotaTriggerRetentionDays != 45 || cfg.RequestDetailRetentionDays != 20 {
		t.Fatalf("retention config = %+v", cfg)
	}
}

func TestSummaryIncludesDiagnosticsAndAlerts(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "broken@example.com",
		AuthIndex:   "broken.json",
		Source:      "broken@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized, Body: "unauthorized sk-secret-should-not-leak"},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	diagnostics, ok := data["diagnostics"].(diagnosticsSummary)
	if !ok {
		t.Fatalf("diagnostics = %#v, want diagnosticsSummary", data["diagnostics"])
	}
	if diagnostics.Database.UsageEvents != 1 || diagnostics.AuthFiles.Invalid401 != 1 {
		t.Fatalf("diagnostics = %+v, want one usage event and one invalid auth", diagnostics)
	}
	alerts, ok := data["alerts"].([]dashboardAlert)
	if !ok || len(alerts) == 0 {
		t.Fatalf("alerts = %#v, want at least one alert", data["alerts"])
	}
}

func TestExportRecordsDoNotLeakSecrets(t *testing.T) {
	data := map[string]any{
		"accounts": []accountRow{{
			AuthIndex:   "auth.json",
			AuthID:      "acct@example.com",
			Source:      "sk-secret-should-not-export",
			Provider:    "codex",
			Requests:    1,
			TotalTokens: 42,
			CostUSD:     0.01,
		}},
		"provider_recent": []recentRow{{
			Time:        time.Now().Format(time.RFC3339),
			Provider:    "Codex · api.example.com",
			Source:      "sk-secret-should-not-export",
			Model:       "test-model",
			TotalTokens: 42,
			CostUSD:     0.01,
		}},
	}
	records, headers := exportRecords(data, "accounts")
	body, err := recordsToCSV(headers, records)
	if err != nil {
		t.Fatalf("recordsToCSV returned error: %v", err)
	}
	if strings.Contains(string(body), "sk-secret-should-not-export") {
		t.Fatalf("account export leaked secret: %s", body)
	}
	records, headers = exportRecords(data, "recent")
	body, err = recordsToCSV(headers, records)
	if err != nil {
		t.Fatalf("recordsToCSV recent returned error: %v", err)
	}
	if strings.Contains(string(body), "sk-secret-should-not-export") {
		t.Fatalf("recent export leaked secret: %s", body)
	}
}

func TestMergeConfiguredProvidersReflectsCurrentConfig(t *testing.T) {
	rows := []providerRow{{Provider: "deepseek", TotalTokens: 100}}
	withCodex := mergeConfiguredProviders(rows, []string{"deepseek", "Codex · api.kmoon.site"})
	if len(withCodex) != 2 || withCodex[1].Provider != "Codex · api.kmoon.site" {
		t.Fatalf("providers with codex config = %#v", withCodex)
	}
	withoutCodex := mergeConfiguredProviders(rows, []string{"deepseek"})
	if len(withoutCodex) != 1 || withoutCodex[0].Provider != "deepseek" {
		t.Fatalf("providers after config removal = %#v, want only deepseek", withoutCodex)
	}
}

func TestPriceNameCandidatesMatchAliasesAndDateSuffixes(t *testing.T) {
	candidates := priceNameCandidates("deepseek-v4-pro-260425 OpenAI")
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate] = true
	}
	for _, want := range []string{"deepseek-v4-pro-260425 openai", "deepseek-v4-pro-260425", "deepseek-v4-pro"} {
		if !seen[want] {
			t.Fatalf("priceNameCandidates missing %q in %#v", want, candidates)
		}
	}
}

func TestProviderSpecificPricesDoNotOverrideGenericFallback(t *testing.T) {
	prices := map[string]modelPrice{}
	generic := modelPrice{Prompt: 0.435, Completion: 0.87}
	azure := modelPrice{Prompt: 1.74, Completion: 3.48}
	registerPriceCandidate(prices, "deepseek-v4-pro", generic)
	registerPriceCandidate(prices, "azure_ai/deepseek-v4-pro", azure)

	price, ok := resolveModelPrice(costTokenRow{Provider: "字节", Model: "deepseek-v4-pro-260425"}, prices)
	if !ok {
		t.Fatalf("resolve generic fallback returned no price")
	}
	if price.Prompt != generic.Prompt || price.Completion != generic.Completion {
		t.Fatalf("unknown provider price = %+v, want generic %+v", price, generic)
	}

	price, ok = resolveModelPrice(costTokenRow{Provider: "azure_ai", Model: "deepseek-v4-pro"}, prices)
	if !ok {
		t.Fatalf("resolve azure provider returned no price")
	}
	if price.Prompt != azure.Prompt || price.Completion != azure.Completion {
		t.Fatalf("azure provider price = %+v, want provider-specific %+v", price, azure)
	}
}

func TestModelPriceUpdateConfigParsesChineseFields(t *testing.T) {
	cfg := parsePluginConfigYAML([]byte(`
自动更新模型价格表: false
模型价格更新间隔小时: 12
模型价格表地址: https://example.test/model_prices.json
模型价格更新超时秒数: 9
`), defaultPluginConfig())
	cfg = normalizePluginConfig(cfg)
	if cfg.ModelPriceAutoUpdateEnabled {
		t.Fatalf("ModelPriceAutoUpdateEnabled = true, want false")
	}
	if cfg.ModelPriceUpdateIntervalHours != 12 {
		t.Fatalf("ModelPriceUpdateIntervalHours = %d, want 12", cfg.ModelPriceUpdateIntervalHours)
	}
	if cfg.ModelPriceUpdateURL != "https://example.test/model_prices.json" {
		t.Fatalf("ModelPriceUpdateURL = %q", cfg.ModelPriceUpdateURL)
	}
	if cfg.ModelPriceUpdateTimeoutSeconds != 9 {
		t.Fatalf("ModelPriceUpdateTimeoutSeconds = %d, want 9", cfg.ModelPriceUpdateTimeoutSeconds)
	}
}

func TestDownloadModelPricesValidatesAndWritesFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"openai/test-model": {
				"input_cost_per_token": 0.000001,
				"output_cost_per_token": 0.000002,
				"litellm_provider": "openai"
			}
		}`))
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "model_prices.json")
	entries, loaded, size, err := downloadModelPrices(context.Background(), server.URL, target)
	if err != nil {
		t.Fatalf("downloadModelPrices returned error: %v", err)
	}
	if entries != 1 || loaded != 1 || size <= 0 {
		t.Fatalf("entries=%d loaded=%d size=%d, want one loaded price", entries, loaded, size)
	}
	prices := readPricesFromPathForTest(t, target)
	price, ok := prices["openai/test-model"]
	if !ok {
		t.Fatalf("downloaded prices = %#v, want openai/test-model", prices)
	}
	if price.Prompt != 1 || price.Completion != 2 {
		t.Fatalf("price = %+v, want per-token values converted to per-million", price)
	}
}

func readPricesFromPathForTest(t *testing.T, path string) map[string]modelPrice {
	t.Helper()
	t.Setenv("CPA_MODEL_PRICE_FILE", path)
	old := globalModelPriceUpdater
	globalModelPriceUpdater = &modelPriceUpdateManager{}
	t.Cleanup(func() { globalModelPriceUpdater = old })
	return readModelPricesFromFile()
}

func withCodexQuotaURLForTest(t *testing.T, url string) {
	t.Helper()
	old := codexQuotaURLOverrideForTest
	oldResponses := codexResponsesURLOverrideForTest
	codexQuotaURLOverrideForTest = url
	codexResponsesURLOverrideForTest = url
	t.Cleanup(func() {
		codexQuotaURLOverrideForTest = old
		codexResponsesURLOverrideForTest = oldResponses
	})
}

func intToString(v int64) string {
	return stringFromAny(v)
}

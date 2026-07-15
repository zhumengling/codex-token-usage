package main

import (
	"context"
	"testing"
	"time"
)

func TestSummaryMaintenanceLightEligibilityTracksAuthFiles(t *testing.T) {
	manager := &summaryMaintenanceManager{state: summaryMaintenanceState{
		LastRevision:                   "previous",
		LastProcessedUsageEventID:      10,
		LastProcessedQuotaTriggerID:    20,
		LastProcessedAuthFilesRevision: "auth-a",
	}}
	base := storeRevision{UsageMaxID: 10, QuotaMaxID: 20, AuthFilesRevision: "auth-a"}
	if !manager.lightMaintenanceEnough(base) {
		t.Fatal("unchanged auth revision should allow light maintenance")
	}
	usageChanged := base
	usageChanged.UsageMaxID++
	if !manager.lightMaintenanceEnough(usageChanged) {
		t.Fatal("new usage events should use incremental light maintenance")
	}
	quotaChanged := base
	quotaChanged.QuotaMaxID++
	if !manager.lightMaintenanceEnough(quotaChanged) {
		t.Fatal("new quota trigger runs should use incremental light maintenance")
	}
	authChanged := base
	authChanged.AuthFilesRevision = "auth-b"
	if manager.lightMaintenanceEnough(authChanged) {
		t.Fatal("changed auth files must trigger full maintenance")
	}
}

func TestLightMaintenanceBackfillsAutobanFromUsage(t *testing.T) {
	now := time.Now().Unix()
	withCodexHostAuthSource(t, issue12HostAuthCaller("alice.json", "alice-index", "alice@example.com", now-300))
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO usage_events (
  requested_at, provider, auth_id, auth_index, source, failed, status_code,
  primary_used_percent, primary_reset_at
) VALUES (?, 'codex', 'alice.json', 'alice-index', 'alice@example.com', 1, 429, 100, ?)`, now-60, now+3600); err != nil {
		t.Fatal(err)
	}
	if err := s.runSummaryMaintenanceMode(context.Background(), "light"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM autoban_bans WHERE active=1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active autobans=%d, want light maintenance to backfill one", active)
	}
}

func TestStoreRevisionResetDueIncludesXAI(t *testing.T) {
	now := time.Now().Unix()
	if !storeRevisionResetDue(storeRevision{NextXAIResetAt: now - 1}, now) {
		t.Fatal("expired xAI state did not make maintenance due")
	}
	if storeRevisionResetDue(storeRevision{NextXAIResetAt: now + 60, NextBanResetAt: now + 60}, now) {
		t.Fatal("future reset was reported as due")
	}
}

func TestLightMaintenanceExpiresXAIStateRows(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO xai_account_states
  (state_key, provider, state, reason, observed_at, reset_at, active)
VALUES ('expired-xai', 'xai', 'rate_limited', 'test', ?, ?, 1)`, now-60, now-1); err != nil {
		t.Fatal(err)
	}
	if err := s.runSummaryMaintenanceMode(context.Background(), "light"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM xai_account_states WHERE state_key='expired-xai'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("expired xAI state active=%d, want 0", active)
	}
}

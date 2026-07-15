package main

import (
	"context"
	"testing"
	"time"
)

func TestSummaryMaintenanceUsesFullModeForNewUsageOrQuota(t *testing.T) {
	manager := &summaryMaintenanceManager{state: summaryMaintenanceState{
		LastRevision:                   "previous",
		LastProcessedUsageEventID:      10,
		LastProcessedQuotaTriggerID:    20,
		LastProcessedAuthFilesRevision: "auth-a",
	}}
	base := storeRevision{UsageMaxID: 10, QuotaMaxID: 20, AuthFilesRevision: "auth-a"}
	if !manager.lightMaintenanceEnough(base) {
		t.Fatal("unchanged usage, quota and auth revisions should allow light maintenance")
	}
	usageChanged := base
	usageChanged.UsageMaxID++
	if manager.lightMaintenanceEnough(usageChanged) {
		t.Fatal("new usage events must trigger full maintenance")
	}
	quotaChanged := base
	quotaChanged.QuotaMaxID++
	if manager.lightMaintenanceEnough(quotaChanged) {
		t.Fatal("new quota trigger runs must trigger full maintenance")
	}
	authChanged := base
	authChanged.AuthFilesRevision = "auth-b"
	if manager.lightMaintenanceEnough(authChanged) {
		t.Fatal("changed auth files must trigger full maintenance")
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

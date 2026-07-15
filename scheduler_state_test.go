package main

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerStateFastPathAvoidsOpeningDatabase(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	globalSchedulerState.setRestricted("codex", false)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = false
	globalAccountProtection.configure(cfg)

	s := &store{}
	globalStore = s
	t.Cleanup(func() {
		s.close()
		globalStore = &store{}
	})
	resp, err := s.pickAuthOnce(context.Background(), schedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5.5",
		Candidates: []schedulerAuthCandidate{
			{ID: "alice", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatalf("response=%+v, want native scheduler delegation", resp)
	}
	if s.db != nil {
		t.Fatal("healthy fast path opened SQLite")
	}
}

func TestSchedulerStateRefreshTracksActiveRestrictions(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO autoban_bans
		(auth_id, provider, window, reason, banned_at, reset_at, active)
		VALUES ('alice', 'codex', '5h', 'test', ?, ?, 1)`, now, now+3600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO xai_account_states
		(state_key, provider, state, reason, observed_at, reset_at, active)
		VALUES ('grok', 'xai', 'rate_limited', 'test', ?, ?, 1)`, now, now+60); err != nil {
		t.Fatal(err)
	}
	if err := globalSchedulerState.refresh(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !globalSchedulerState.needsDatabase("codex", false) {
		t.Fatal("active Codex ban was not cached")
	}
	if !globalSchedulerState.needsDatabase("xai", false) {
		t.Fatal("active xAI state was not cached")
	}
}

func TestQueryActiveAutobansDoesNotRewriteUsageHistory(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_events
		(requested_at, provider, primary_reset_at, total_tokens)
		VALUES (?, 'codex', ?, 1)`, time.Now().Unix(), int64(1_800_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if _, err := queryActiveAutobans(context.Background(), db, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var resetAt int64
	if err := db.QueryRow(`SELECT primary_reset_at FROM usage_events LIMIT 1`).Scan(&resetAt); err != nil {
		t.Fatal(err)
	}
	if resetAt != 1_800_000_000_000 {
		t.Fatalf("queryActiveAutobans rewrote usage history: %d", resetAt)
	}
}

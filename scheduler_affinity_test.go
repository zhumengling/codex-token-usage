package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func affinityTestRequest(sessionID string) schedulerPickRequest {
	return schedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-test",
		Options: schedulerOptions{
			Headers: map[string][]string{"Session-Id": {sessionID}},
		},
	}
}

func affinityTestCandidates() []schedulerAuthCandidate {
	return []schedulerAuthCandidate{
		{ID: "a", Provider: "codex", Priority: 1},
		{ID: "b", Provider: "codex", Priority: 1},
	}
}

func resetSchedulerSelectionState(t *testing.T) {
	t.Helper()
	globalSchedulerRotation.reset()
	globalSchedulerAffinity.reset()
	t.Cleanup(func() {
		globalSchedulerRotation.reset()
		globalSchedulerAffinity.reset()
	})
}

func TestSchedulerAffinityKeepsSameSessionOnSameAuth(t *testing.T) {
	resetSchedulerSelectionState(t)
	candidates := affinityTestCandidates()
	request := affinityTestRequest("session-one")
	rotationKey := schedulerRotationKey(request, "codex")
	affinityKey := schedulerAffinityKey(request, "codex")
	if affinityKey == "" || strings.Contains(affinityKey, "session-one") {
		t.Fatalf("affinity key must be non-empty and must not expose the raw session: %q", affinityKey)
	}
	first := pickSchedulerCandidate(rotationKey, affinityKey, candidates)
	second := pickSchedulerCandidate(rotationKey, affinityKey, candidates)
	if first.ID != "a" || second.ID != "a" {
		t.Fatalf("same session picks = %q, %q; want a, a", first.ID, second.ID)
	}

	otherRequest := affinityTestRequest("session-two")
	other := pickSchedulerCandidate(rotationKey, schedulerAffinityKey(otherRequest, "codex"), candidates)
	if other.ID != "b" {
		t.Fatalf("new session pick = %q, want b for balanced initial placement", other.ID)
	}
}

func TestSchedulerAffinityFallsBackWhenBoundAuthIsFiltered(t *testing.T) {
	resetSchedulerSelectionState(t)
	request := affinityTestRequest("session-filtered")
	rotationKey := schedulerRotationKey(request, "codex")
	affinityKey := schedulerAffinityKey(request, "codex")
	if got := pickSchedulerCandidate(rotationKey, affinityKey, affinityTestCandidates()); got.ID != "a" {
		t.Fatalf("initial pick = %q, want a", got.ID)
	}
	onlyB := []schedulerAuthCandidate{{ID: "b", Provider: "codex", Priority: 1}}
	if got := pickSchedulerCandidate(rotationKey, affinityKey, onlyB); got.ID != "b" {
		t.Fatalf("failover pick = %q, want b", got.ID)
	}
	if got := pickSchedulerCandidate(rotationKey, affinityKey, affinityTestCandidates()); got.ID != "b" {
		t.Fatalf("rebound pick = %q, want b", got.ID)
	}
}

func TestSchedulerWithoutSessionKeepsRoundRobin(t *testing.T) {
	resetSchedulerSelectionState(t)
	request := schedulerPickRequest{Provider: "codex", Model: "gpt-test"}
	rotationKey := schedulerRotationKey(request, "codex")
	if key := schedulerAffinityKey(request, "codex"); key != "" {
		t.Fatalf("affinity key = %q, want empty without a session header", key)
	}
	first := pickSchedulerCandidate(rotationKey, "", affinityTestCandidates())
	second := pickSchedulerCandidate(rotationKey, "", affinityTestCandidates())
	if first.ID != "a" || second.ID != "b" {
		t.Fatalf("round-robin picks = %q, %q; want a, b", first.ID, second.ID)
	}
}

func TestProtectionAffinityPreservesSessionUntilConcurrencyLimit(t *testing.T) {
	resetSchedulerSelectionState(t)
	db := newProtectionTestDB(t)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	cfg.AccountProtectionFreeConcurrency = 2
	request := affinityTestRequest("protected-session")
	affinityKey := schedulerAffinityKey(request, "codex")
	candidates := []schedulerAuthCandidate{
		protectionTestCandidate("a", "free", 10),
		protectionTestCandidate("b", "free", 1),
	}
	s := &store{}
	for _, want := range []string{"a", "a", "b"} {
		got, err := s.pickProtectedAuth(context.Background(), db, candidates, cfg, schedulerRotationKey(request, "codex"), affinityKey)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want {
			t.Fatalf("picked %q, want %q", got.ID, want)
		}
	}
}

func TestProtectionTokenDemotionDoesNotMoveExistingSession(t *testing.T) {
	resetSchedulerSelectionState(t)
	db := newProtectionTestDB(t)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	cfg.AccountProtectionFreeTokenLimit = 100
	request := affinityTestRequest("token-sticky-session")
	affinityKey := schedulerAffinityKey(request, "codex")
	globalSchedulerAffinity.bind(affinityKey, "a")
	if _, err := db.Exec(`INSERT INTO usage_events (requested_at, provider, auth_id, auth_index, total_tokens) VALUES (?, 'codex', 'a', 'a', 100)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	candidates := []schedulerAuthCandidate{
		protectionTestCandidate("a", "free", 1),
		protectionTestCandidate("b", "free", 1),
	}
	got, err := (&store{}).pickProtectedAuth(context.Background(), db, candidates, cfg, schedulerRotationKey(request, "codex"), affinityKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a" {
		t.Fatalf("picked %q, want existing session to remain on token-demoted account a", got.ID)
	}
}

func TestSchedulerAffinityExpiresStaleBinding(t *testing.T) {
	resetSchedulerSelectionState(t)
	request := affinityTestRequest("expired-session")
	key := schedulerAffinityKey(request, "codex")
	globalSchedulerAffinity.bindings = map[string]schedulerAffinityBinding{
		key: {AuthID: "b", ExpiresAt: time.Now().Add(-time.Second)},
	}
	got := pickSchedulerCandidate(schedulerRotationKey(request, "codex"), key, affinityTestCandidates())
	if got.ID != "a" {
		t.Fatalf("expired affinity pick = %q, want fresh round-robin pick a", got.ID)
	}
}

func TestSchedulerAffinityCacheIsBounded(t *testing.T) {
	var manager schedulerAffinityManager
	for i := 0; i <= schedulerAffinityMaxBindings; i++ {
		manager.bind(strconv.Itoa(i), "auth")
	}
	if got := len(manager.bindings); got != schedulerAffinityMaxBindings {
		t.Fatalf("binding count = %d, want %d", got, schedulerAffinityMaxBindings)
	}
}

func TestCodexSchedulerKeepsSessionAfterFilteringUnavailableCandidate(t *testing.T) {
	resetSchedulerSelectionState(t)
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	oldCfg := globalAccountProtection.config()
	globalAccountProtection.configure(defaultPluginConfig())
	t.Cleanup(func() { globalAccountProtection.configure(oldCfg) })
	s := &store{}
	t.Cleanup(s.close)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO autoban_bans (auth_id, auth_index, provider, window, reason, banned_at, reset_at, active, last_status_code)
VALUES ('blocked', 'blocked', 'codex', '5h', 'test', ?, ?, 1, 429)`, now, now+3600); err != nil {
		t.Fatal(err)
	}
	request := affinityTestRequest("filtered-session")
	request.Candidates = []schedulerAuthCandidate{
		{ID: "blocked", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "blocked"}},
		{ID: "a", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "a"}},
		{ID: "b", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "b"}},
	}
	first, err := s.pickAuth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.pickAuth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Handled || !second.Handled || first.AuthID != "a" || second.AuthID != "a" {
		t.Fatalf("same-session responses = %+v, %+v; want a, a", first, second)
	}

	request.Options.Headers["Session-Id"] = []string{"another-session"}
	third, err := s.pickAuth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Handled || third.AuthID != "b" {
		t.Fatalf("new-session response = %+v; want b", third)
	}
}

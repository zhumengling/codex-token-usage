package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func newProtectionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func protectionTestCandidate(id, plan string, priority int) schedulerAuthCandidate {
	return schedulerAuthCandidate{
		ID:       id,
		Provider: "codex",
		Priority: priority,
		Attributes: map[string]string{
			"auth_index": id,
			"plan_type":  plan,
		},
	}
}

func TestNormalizedProtectionPlan(t *testing.T) {
	for input, want := range map[string]string{
		"free": "free", "chatgpt plus": "plus", "K12": "k12", "education": "k12", "team": "team", "pro": "pro", "": "plus",
	} {
		if got := normalizedProtectionPlan(input); got != want {
			t.Fatalf("normalizedProtectionPlan(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProtectionConcurrencySwitchesCandidate(t *testing.T) {
	globalSchedulerRotation.reset()
	db := newProtectionTestDB(t)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	cfg.AccountProtectionFreeConcurrency = 2
	s := &store{}
	ctx := context.Background()
	candidates := []schedulerAuthCandidate{protectionTestCandidate("a", "free", 10), protectionTestCandidate("b", "free", 1)}
	for _, want := range []string{"a", "a", "b"} {
		got, err := s.pickProtectedAuth(ctx, db, candidates, cfg, "codex\x00test")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want {
			t.Fatalf("picked %q, want %q", got.ID, want)
		}
	}
}

func TestProtectionTokenDemotionPrefersLowerUsageCandidate(t *testing.T) {
	globalSchedulerRotation.reset()
	db := newProtectionTestDB(t)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	cfg.AccountProtectionFreeTokenLimit = 2_000_000
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO usage_events (requested_at, provider, auth_id, auth_index, total_tokens) VALUES (?, 'codex', 'a', 'a', ?)`, now, 2_000_000); err != nil {
		t.Fatal(err)
	}
	got, err := (&store{}).pickProtectedAuth(context.Background(), db, []schedulerAuthCandidate{
		protectionTestCandidate("a", "free", 10), protectionTestCandidate("b", "free", 1),
	}, cfg, "codex\x00test")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("picked %q, want lower-token candidate b", got.ID)
	}
}

func TestProtectionSaturationUsesLeastInFlightCandidate(t *testing.T) {
	globalSchedulerRotation.reset()
	db := newProtectionTestDB(t)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	cfg.AccountProtectionFreeConcurrency = 1
	now := time.Now().Unix()
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES ('a', 'a', '', 'free', ?, ?)`, now, now+900); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES ('b', 'b', '', 'free', ?, ?)`, now, now+900); err != nil {
		t.Fatal(err)
	}
	got, err := (&store{}).pickProtectedAuth(context.Background(), db, []schedulerAuthCandidate{
		protectionTestCandidate("a", "free", 10), protectionTestCandidate("b", "free", 1),
	}, cfg, "codex\x00test")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("picked %q, want least-in-flight candidate b", got.ID)
	}
}

func TestProtectionSaturationUsesPriorityBeforeTokenDemotion(t *testing.T) {
	globalSchedulerRotation.reset()
	states := []protectionCandidate{
		{
			Candidate: schedulerAuthCandidate{ID: "high", Priority: 10},
			InFlight:  1,
			Limit:     1,
			Tokens:    100,
			Threshold: 100,
		},
		{
			Candidate: schedulerAuthCandidate{ID: "low", Priority: 1},
			InFlight:  1,
			Limit:     1,
			Tokens:    0,
			Threshold: 100,
		},
	}
	if got := chooseProtectedCandidate(states, "test"); got.Candidate.ID != "high" {
		t.Fatalf("picked %q, want higher-priority saturated candidate", got.Candidate.ID)
	}
}

func TestProtectionRoundRobinsWithinSamePriority(t *testing.T) {
	globalSchedulerRotation.reset()
	states := []protectionCandidate{
		{Candidate: schedulerAuthCandidate{ID: "z-account", Priority: 1}, Limit: 5},
		{Candidate: schedulerAuthCandidate{ID: "a-account", Priority: 1}, Limit: 5},
	}
	if got := chooseProtectedCandidate(states, "test"); got.Candidate.ID != "a-account" {
		t.Fatalf("first pick = %q, want a-account", got.Candidate.ID)
	}
	if got := chooseProtectedCandidate(states, "test"); got.Candidate.ID != "z-account" {
		t.Fatalf("second pick = %q, want z-account", got.Candidate.ID)
	}
}

func TestSchedulerRotationUsesHighestPriorityAndStableOrder(t *testing.T) {
	var rotation schedulerRotationManager
	candidates := []schedulerAuthCandidate{
		{ID: "z-high", Priority: 9},
		{ID: "low", Priority: 1},
		{ID: "a-high", Priority: 9},
	}
	if got := rotation.pick("codex\x00model", candidates); got.ID != "a-high" {
		t.Fatalf("first pick = %q, want a-high", got.ID)
	}
	reordered := []schedulerAuthCandidate{candidates[2], candidates[0], candidates[1]}
	if got := rotation.pick("codex\x00model", reordered); got.ID != "z-high" {
		t.Fatalf("second pick = %q, want z-high", got.ID)
	}
	if got := rotation.pick("codex\x00model", candidates); got.ID != "a-high" {
		t.Fatalf("third pick = %q, want a-high", got.ID)
	}
}

func TestProtectionReservationExpiresAndReleasesOnUsage(t *testing.T) {
	db := newProtectionTestDB(t)
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES ('expired', 'expired', '', 'plus', ?, ?)`, now-1000, now-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES ('active', 'active', '', 'plus', ?, ?)`, now, now+900); err != nil {
		t.Fatal(err)
	}
	cfg := defaultPluginConfig()
	_, err := (&store{}).pickProtectedAuth(context.Background(), db, []schedulerAuthCandidate{protectionTestCandidate("other", "plus", 1)}, cfg, "codex\x00test")
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseProtectionReservation(context.Background(), db, usageRecord{AuthID: "active", AuthIndex: "active"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM account_protection_reservations WHERE auth_id IN ('expired','active')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("reservation count = %d, want 0", count)
	}
}

func TestProtectionCandidateAliasesExcludeSharedWorkspaceID(t *testing.T) {
	candidates := []schedulerAuthCandidate{
		{ID: "shared-workspace", Attributes: map[string]string{"auth_index": "shared-workspace", "source": "a@example.com", "auth_file": "a.json"}},
		{ID: "shared-workspace", Attributes: map[string]string{"auth_index": "shared-workspace", "source": "b@example.com", "auth_file": "b.json"}},
	}
	sets := protectionCandidateAliasSets(candidates)
	if len(sets) != 2 {
		t.Fatalf("alias sets = %+v", sets)
	}
	for i, aliases := range sets {
		if containsAlias(aliases, "shared-workspace") {
			t.Fatalf("candidate %d retained shared workspace alias: %+v", i, aliases)
		}
	}
	if !containsAlias(sets[0], "a.json") || !containsAlias(sets[0], "a@example.com") {
		t.Fatalf("candidate A aliases = %+v", sets[0])
	}
	if !containsAlias(sets[1], "b.json") || !containsAlias(sets[1], "b@example.com") {
		t.Fatalf("candidate B aliases = %+v", sets[1])
	}
}

func TestConfiguredProtectionPlanIndexIgnoresSharedAliases(t *testing.T) {
	index := configuredProtectionPlanIndex([]configuredAccount{
		{AuthID: "shared", AuthIndex: "shared", Email: "a@example.com", AuthFile: "a.json", PlanType: "free"},
		{AuthID: "shared", AuthIndex: "shared", Email: "b@example.com", AuthFile: "b.json", PlanType: "team"},
	})
	if index["shared"] != "" {
		t.Fatalf("shared alias retained plan %q", index["shared"])
	}
	if index["a@example.com"] != "free" || index["b@example.com"] != "team" {
		t.Fatalf("unique plan index = %+v", index)
	}
}

func TestProtectionSnapshotBatchesReservationAndTokenMetrics(t *testing.T) {
	db := newProtectionTestDB(t)
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES ('shared', 'shared', 'a@example.com', 'k12', ?, ?)`, now, now+900); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_events (requested_at, provider, auth_id, auth_index, source, total_tokens) VALUES (?, 'codex', 'shared', 'shared', 'a@example.com', 100), (?, 'codex', 'shared', 'shared', 'b@example.com', 200)`, now, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadProtectionSnapshot(context.Background(), db, now-300, now)
	if err != nil {
		t.Fatal(err)
	}
	inFlight, tokens := snapshot.metrics([]string{"a@example.com"})
	if inFlight != 1 || tokens != 100 {
		t.Fatalf("account A metrics = %d/%d, want 1/100", inFlight, tokens)
	}
	inFlight, tokens = snapshot.metrics([]string{"b@example.com"})
	if inFlight != 0 || tokens != 200 {
		t.Fatalf("account B metrics = %d/%d, want 0/200", inFlight, tokens)
	}
}

func TestProtectionSnapshotAggregatesUsageBeforeLoading(t *testing.T) {
	db := newProtectionTestDB(t)
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO usage_events (requested_at, provider, auth_id, auth_index, source, total_tokens) VALUES
		(?, 'codex', 'a', 'a', 'a@example.com', 100),
		(?, 'CODEX', 'a', 'a', 'a@example.com', 200),
		(?, 'codex', 'b', 'b', 'b@example.com', 400)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadProtectionSnapshot(context.Background(), db, now-300, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Usage) != 2 {
		t.Fatalf("usage groups = %d, want 2", len(snapshot.Usage))
	}
	_, tokens := snapshot.metrics([]string{"a@example.com"})
	if tokens != 300 {
		t.Fatalf("account A tokens = %d, want 300", tokens)
	}
}

func TestProtectionSnapshotCountsGroupedReservations(t *testing.T) {
	db := newProtectionTestDB(t)
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO account_protection_reservations (auth_id, auth_index, source, plan_type, created_at, expires_at) VALUES
		('a', 'a', '', 'plus', ?, ?),
		('a', 'a', '', 'plus', ?, ?),
		('a', 'a', '', 'plus', ?, ?)`, now, now+900, now, now+900, now, now+900); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadProtectionSnapshot(context.Background(), db, now-300, now)
	if err != nil {
		t.Fatal(err)
	}
	inFlight, _ := snapshot.metrics([]string{"a"})
	if inFlight != 3 {
		t.Fatalf("in-flight = %d, want 3", inFlight)
	}
}

func TestProtectionSnapshotDoesNotDoubleCountSharedAliases(t *testing.T) {
	snapshot := newProtectionSnapshot(
		[]protectionReservationSample{{Aliases: []string{"account", "account@example.com"}, Count: 2}},
		[]protectionUsageSample{{Aliases: []string{"account", "account@example.com"}, Tokens: 300}},
	)
	inFlight, tokens := snapshot.metrics([]string{"account", "account@example.com"})
	if inFlight != 2 || tokens != 300 {
		t.Fatalf("metrics = %d/%d, want 2/300", inFlight, tokens)
	}
}

func TestProtectionRotationHandlesDuplicateCandidateIDs(t *testing.T) {
	globalSchedulerRotation.reset()
	states := []protectionCandidate{
		{Candidate: schedulerAuthCandidate{ID: "shared", Priority: 1, Attributes: map[string]string{"auth_file": "a.json"}}, AuthIndex: "a", Limit: 5},
		{Candidate: schedulerAuthCandidate{ID: "shared", Priority: 1, Attributes: map[string]string{"auth_file": "b.json"}}, AuthIndex: "b", Limit: 5},
	}
	first := chooseProtectedCandidate(states, "duplicate")
	second := chooseProtectedCandidate(states, "duplicate")
	if first.AuthIndex == second.AuthIndex {
		t.Fatalf("duplicate-ID rotation chose %q twice", first.AuthIndex)
	}
}

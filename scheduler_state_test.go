package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestSchedulerStateRefreshDoesNotOverwriteConcurrentRestriction(t *testing.T) {
	var cache schedulerStateCache
	cache.setRestricted("codex", false)

	loaderStarted := make(chan struct{})
	allowLoaderReturn := make(chan struct{})
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- cache.refreshWithLoader(func() (schedulerRestrictionState, error) {
			close(loaderStarted)
			<-allowLoaderReturn
			return schedulerRestrictionState{}, nil
		})
	}()

	<-loaderStarted
	cache.setRestricted("codex", true)
	close(allowLoaderReturn)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if !cache.needsDatabase("codex", false) {
		t.Fatal("stale refresh result overwrote a concurrent Codex restriction")
	}
	if cache.needsDatabase("xai", false) {
		t.Fatal("unchanged xAI generation did not accept the refresh result")
	}
}

func TestCodexRestrictionWritesMarkSchedulerState(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	globalSchedulerState.setRestricted("codex", false)
	if err := recordInvalidAuthIfNeeded(context.Background(), db, usageRecord{
		Provider:    "codex",
		AuthID:      "invalid-account",
		RequestedAt: time.Now(),
	}, http.StatusUnauthorized); err != nil {
		t.Fatal(err)
	}
	if !globalSchedulerState.needsDatabase("codex", false) {
		t.Fatal("successful invalid-auth write did not restrict the scheduler cache")
	}

	globalSchedulerState.setRestricted("codex", false)
	if err := recordAutobanIfNeeded(context.Background(), db, usageRecord{
		Provider:    "codex",
		AuthID:      "rate-limited-account",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
	}, http.StatusTooManyRequests, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !globalSchedulerState.needsDatabase("codex", false) {
		t.Fatal("successful autoban write did not restrict the scheduler cache")
	}
}

func TestQuotaProbeRestrictionWritesMarkSchedulerState(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetSchedulerStateForTest()
			t.Cleanup(resetSchedulerStateForTest)
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			codexResponsesURLOverrideForTest = server.URL
			t.Cleanup(func() { codexResponsesURLOverrideForTest = "" })

			globalSchedulerState.setRestricted("codex", false)
			cfg := defaultPluginConfig()
			run := executeQuotaProbeRequest(context.Background(), db, triggerAuthAccount{
				configuredAccount: configuredAccount{
					AuthID:    "probe-account",
					AuthIndex: "probe-account.json",
					Provider:  "codex",
					Source:    "probe@example.com",
				},
				AccessToken: "test-token",
			}, cfg)
			if run.HTTPStatus != status {
				t.Fatalf("HTTP status=%d, want %d", run.HTTPStatus, status)
			}
			if !globalSchedulerState.needsDatabase("codex", false) {
				t.Fatalf("quota probe %d write did not restrict the scheduler cache", status)
			}
		})
	}
}

func TestEmptySchedulerSnapshotDoesNotClearRestrictionCache(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			resetSchedulerStateForTest()
			t.Cleanup(resetSchedulerStateForTest)
			s := newTestStore(t)
			previousStore := globalStore
			previousProtectionConfig := globalAccountProtection.config()
			globalStore = s
			t.Cleanup(func() {
				globalStore = previousStore
				globalAccountProtection.configure(previousProtectionConfig)
			})
			if provider == "codex" {
				cfg := defaultPluginConfig()
				cfg.AccountProtectionEnabled = false
				globalAccountProtection.configure(cfg)
			}

			globalSchedulerState.setRestricted(provider, true)
			resp, err := s.pickAuthOnce(context.Background(), schedulerPickRequest{
				Provider: provider,
				Candidates: []schedulerAuthCandidate{
					{ID: "account", Provider: provider, Priority: 1},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Handled {
				t.Fatalf("response=%+v, want native scheduler delegation", resp)
			}
			if !globalSchedulerState.needsDatabase(provider, false) {
				t.Fatal("request-path database snapshot cleared a possibly newer restriction")
			}
		})
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

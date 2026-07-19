package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "invalid-account", AuthIndex: "invalid-index", Name: "invalid-account.json", Provider: "codex",
		}}})
	})
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
		AuthIndex:   "invalid-index",
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
	tests := []struct {
		status int
		runs   int
	}{
		{status: http.StatusUnauthorized, runs: 1},
		{status: http.StatusPaymentRequired, runs: 1},
		{status: http.StatusForbidden, runs: forbiddenInvalidAuthThreshold},
		{status: http.StatusTooManyRequests, runs: 1},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
				if method != "host.auth.list" {
					return nil, os.ErrNotExist
				}
				return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
					ID: "probe-account", AuthIndex: "stable-probe-index", Name: "probe-account.json", Provider: "codex", Source: "file",
				}}})
			})
			resetSchedulerStateForTest()
			t.Cleanup(resetSchedulerStateForTest)
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.status == http.StatusTooManyRequests {
					w.Header().Set("x-codex-primary-reset-at", "4102444800")
					w.Header().Set("x-codex-primary-window-minutes", "300")
				}
				w.WriteHeader(test.status)
				if test.status == http.StatusPaymentRequired {
					_, _ = w.Write([]byte(`{"error":{"code":"payment_required"}}`))
				}
			}))
			t.Cleanup(server.Close)
			codexResponsesURLOverrideForTest = server.URL
			t.Cleanup(func() { codexResponsesURLOverrideForTest = "" })

			globalSchedulerState.setRestricted("codex", false)
			cfg := defaultPluginConfig()
			for i := 0; i < test.runs; i++ {
				run := executeQuotaProbeRequest(context.Background(), db, triggerAuthAccount{
					configuredAccount: configuredAccount{
						AuthID:    "probe-account",
						AuthIndex: "probe-account.json",
						Provider:  "codex",
						Source:    "probe@example.com",
					},
					AccessToken: "test-token",
				}, cfg)
				if run.HTTPStatus != test.status {
					t.Fatalf("HTTP status=%d, want %d", run.HTTPStatus, test.status)
				}
				if err := recordQuotaTriggerRun(context.Background(), db, run); err != nil {
					t.Fatal(err)
				}
				if err := applyQuotaTriggerAccountState(context.Background(), db, run); err != nil {
					t.Fatal(err)
				}
				if test.status == http.StatusForbidden && i+1 < forbiddenInvalidAuthThreshold && globalSchedulerState.needsDatabase("codex", false) {
					t.Fatalf("403 probe restricted scheduler after only %d failures", i+1)
				}
			}
			if !globalSchedulerState.needsDatabase("codex", false) {
				t.Fatalf("quota probe %d write did not restrict the scheduler cache", test.status)
			}
			var storedStatus int
			query := `SELECT last_status_code FROM invalid_auths WHERE active=1 AND auth_id='probe-account'`
			if test.status == http.StatusTooManyRequests {
				var resetAt int64
				var window string
				if err := db.QueryRow(`SELECT last_status_code,reset_at,window FROM autoban_bans WHERE active=1 AND auth_id='probe-account'`).Scan(&storedStatus, &resetAt, &window); err != nil {
					t.Fatal(err)
				}
				if resetAt != 4102444800 || window != "5h" {
					t.Fatalf("429 ban reset/window=%d/%q, want 4102444800/5h", resetAt, window)
				}
			} else if err := db.QueryRow(query).Scan(&storedStatus); err != nil {
				t.Fatal(err)
			}
			if storedStatus != test.status {
				t.Fatalf("stored status=%d, want %d", storedStatus, test.status)
			}
			bans, err := queryActiveAutobans(context.Background(), db, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			invalids, err := queryActiveInvalidAuths(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			available, filtered, _, _, _ := filterCodexSchedulerCandidates([]schedulerAuthCandidate{
				{ID: "probe-account", Provider: "codex", Priority: 1, Attributes: map[string]string{
					"auth_index": "probe-account.json",
					"auth_file":  "probe-account.json",
				}},
				{ID: "healthy-account", Provider: "codex", Priority: 1},
			}, bans, invalids)
			if !filtered || len(available) != 1 || available[0].ID != "healthy-account" {
				t.Fatalf("filtered=%v available=%+v, want only healthy-account", filtered, available)
			}
		})
	}
}

func TestSuccessfulQuotaProbeClearsRecoveredRestriction(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "probe-account", AuthIndex: "stable-probe-index", Name: "probe-account.json", Provider: "codex", Source: "file",
		}}})
	})
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := recordInvalidAuthIfNeeded(context.Background(), db, usageRecord{
		Provider: "codex", AuthID: "probe-account", AuthIndex: "probe-account.json", RequestedAt: time.Now(),
	}, http.StatusUnauthorized); err != nil {
		t.Fatal(err)
	}
	run := quotaTriggerRun{
		AuthID: "probe-account", AuthIndex: "probe-account.json", AuthFile: "probe-account.json", Provider: "codex", Status: "success", HTTPStatus: http.StatusOK,
		StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix(),
	}
	if err := recordQuotaTriggerRun(context.Background(), db, run); err != nil {
		t.Fatal(err)
	}
	if err := applyQuotaTriggerAccountState(context.Background(), db, run); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='probe-account'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("successful quota probe left invalid auth active=%d", active)
	}
	limited := quotaTriggerRun{
		AuthID: "probe-account", AuthIndex: "probe-account.json", AuthFile: "probe-account.json", Provider: "codex", Status: "failed", HTTPStatus: http.StatusTooManyRequests,
		StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix(),
	}
	if err := recordQuotaTriggerRun(context.Background(), db, limited); err != nil {
		t.Fatal(err)
	}
	if err := applyQuotaTriggerAccountState(context.Background(), db, limited); err != nil {
		t.Fatal(err)
	}
	if err := recordQuotaTriggerRun(context.Background(), db, run); err != nil {
		t.Fatal(err)
	}
	if err := applyQuotaTriggerAccountState(context.Background(), db, run); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM autoban_bans WHERE auth_id='probe-account.json'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("successful quota probe left 429 autoban active=%d", active)
	}
}

func TestQuotaProbeSkipsInvalidAccountsDespiteFullQuotaSnapshot(t *testing.T) {
	authFile := "probe-account.json"
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := newTestStore(t)
			authDir := os.Getenv("CPA_AUTH_DIR")
			if err := os.MkdirAll(authDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(authDir, authFile), []byte(`{
  "type": "codex",
  "email": "probe@example.com",
  "access_token": "test-token"
}`), 0o600); err != nil {
				t.Fatal(err)
			}
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_source_kind
) VALUES (?, ?, ?, 'codex', 'probe status', ?, 1, ?, ?, ?)`,
				"probe@example.com", authFile, "probe@example.com", now-7200, status, authFile, authSourceKindFile,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`
INSERT INTO quota_trigger_runs (
  auth_id, auth_index, source, provider, auth_file, mode, status, http_status,
  started_at, finished_at, primary_used_percent, primary_reset_at
) VALUES (?, ?, ?, 'codex', ?, 'probe', 'success', 200, ?, ?, 100, ?)`,
				"probe@example.com", authFile, "probe@example.com", authFile, now-7200, now-7200, now+3600,
			); err != nil {
				t.Fatal(err)
			}

			candidates, skipped, err := selectQuotaTriggerCandidates(context.Background(), db, defaultPluginConfig())
			if err != nil {
				t.Fatal(err)
			}
			if skipped != 1 || len(candidates) != 0 {
				t.Fatalf("status=%d skipped=%d candidates=%+v, want unavailable account skipped", status, skipped, candidates)
			}
		})
	}
}

func TestQuotaProbeSkipsActive429UntilReset(t *testing.T) {
	s := newTestStore(t)
	authDir := os.Getenv("CPA_AUTH_DIR")
	authFile := "rate-limited.json"
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, authFile), []byte(`{
  "type": "codex",
  "email": "rate-limited@example.com",
  "access_token": "test-token"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active, last_status_code, auth_file
) VALUES (?, ?, ?, 'codex', '14d', 'quota trigger', ?, ?, 1, ?, ?)`,
		authFile, authFile, "rate-limited@example.com", now-30, now+3600, http.StatusTooManyRequests, authFile); err != nil {
		t.Fatal(err)
	}
	candidates, skipped, err := selectQuotaTriggerCandidates(context.Background(), db, defaultPluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(candidates) != 0 {
		t.Fatalf("active 429 skipped=%d candidates=%+v, want skipped until reset", skipped, candidates)
	}
	generation := globalSchedulerState.generation("codex")
	if err := expireAutobans(context.Background(), db, now+3601); err != nil {
		t.Fatal(err)
	}
	if globalSchedulerState.generation("codex") <= generation {
		t.Fatal("expired 429 did not invalidate scheduler state")
	}
	var active int
	var releaseReason string
	if err := db.QueryRow(`SELECT active,release_reason FROM autoban_bans WHERE auth_id=?`, authFile).Scan(&active, &releaseReason); err != nil {
		t.Fatal(err)
	}
	if active != 0 || releaseReason != "reset_at reached" {
		t.Fatalf("expired 429 active=%d release_reason=%q", active, releaseReason)
	}
	candidates, skipped, err = selectQuotaTriggerCandidates(context.Background(), db, defaultPluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(candidates) != 1 || candidates[0].AuthFile != authFile {
		t.Fatalf("expired 429 skipped=%d candidates=%+v, want one candidate after reset", skipped, candidates)
	}
}

func TestEmptySchedulerSnapshotClearsMatchingRestrictionGeneration(t *testing.T) {
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
			if globalSchedulerState.needsDatabase(provider, false) {
				t.Fatal("empty database snapshot did not clear its matching restriction generation")
			}
		})
	}
}

func TestSchedulerStateGenerationClearRejectsNewRestriction(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			var cache schedulerStateCache
			cache.setRestricted(provider, true)
			staleGeneration := cache.generation(provider)
			cache.setRestricted(provider, true)
			if cache.clearRestrictedIfGeneration(provider, staleGeneration) {
				t.Fatal("stale generation cleared a newer restriction")
			}
			if !cache.needsDatabase(provider, false) {
				t.Fatal("newer restriction was lost after stale clear attempt")
			}
			if !cache.clearRestrictedIfGeneration(provider, cache.generation(provider)) {
				t.Fatal("matching generation did not clear restriction")
			}
			if cache.needsDatabase(provider, false) {
				t.Fatal("matching generation clear left restriction active")
			}
		})
	}
}

func TestSchedulerStatePendingWriteBlocksRefreshAndGenerationClear(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			var cache schedulerStateCache
			cache.setRestricted(provider, false)
			cache.beginRestrictionWrite(provider)
			pendingGeneration := cache.generation(provider)
			if cache.clearRestrictedIfGeneration(provider, pendingGeneration) {
				t.Fatal("pending restriction write was cleared")
			}
			if err := cache.refreshWithLoader(func() (schedulerRestrictionState, error) {
				return schedulerRestrictionState{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if !cache.needsDatabase(provider, false) {
				t.Fatal("empty refresh cleared a pending restriction write")
			}
			cache.finishRestrictionWrite(provider)
			if cache.clearRestrictedIfGeneration(provider, pendingGeneration) {
				t.Fatal("generation captured during pending write cleared committed restriction")
			}
			if !cache.needsDatabase(provider, false) {
				t.Fatal("finished restriction write was not retained")
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

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func freshActivationQuota() *quotaActivationQuota {
	now := time.Now().Unix()
	primaryDuration := int64((7 * 24 * time.Hour).Seconds())
	secondaryDuration := int64((3 * time.Hour).Seconds())
	primaryResetAfter := primaryDuration
	secondaryResetAfter := secondaryDuration
	resetPrimary := now + primaryDuration
	resetSecondary := now + secondaryDuration
	primaryPct, secondaryPct := 0.0, 0.0
	primaryUsed, secondaryUsed := int64(0), int64(0)
	primaryLimit, secondaryLimit := int64(1000), int64(1000)
	primaryRemaining, secondaryRemaining := primaryLimit, secondaryLimit
	return &quotaActivationQuota{
		ObservedAt: now,
		Primary: quotaActivationWindow{
			Presence: quotaWindowPresent, UsedPercent: &primaryPct, ResetAt: &resetPrimary,
			UsedTokens: &primaryUsed, RemainingTokens: &primaryRemaining, LimitTokens: &primaryLimit,
			LimitWindowSeconds: &primaryDuration, ResetAfterSeconds: &primaryResetAfter,
		},
		Secondary: quotaActivationWindow{
			Presence: quotaWindowPresent, UsedPercent: &secondaryPct, ResetAt: &resetSecondary,
			UsedTokens: &secondaryUsed, RemainingTokens: &secondaryRemaining, LimitTokens: &secondaryLimit,
			LimitWindowSeconds: &secondaryDuration, ResetAfterSeconds: &secondaryResetAfter,
		},
	}
}

func healthyActivationAccount() configuredAccount {
	return configuredAccount{AuthIndex: "auth-index-1", AuthID: "auth-id-1", AuthFile: "seat-1.json", Provider: "codex"}
}

func TestEvaluateQuotaActivation(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*configuredAccount, *quotaActivationQuota)
		credential bool
		quotaOK    bool
		force      bool
		eligible   bool
		reason     quotaActivationReason
	}{
		{name: "both reported windows fresh", credential: true, quotaOK: true, eligible: true, reason: activationEligible},
		{name: "one reported fresh and secondary explicitly absent", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
		}, eligible: true, reason: activationEligible},
		{name: "primary used", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			*quota.Primary.UsedPercent = 1
			*quota.Primary.UsedTokens = 10
			*quota.Primary.RemainingTokens = 990
		}, reason: activationPrimaryNotFresh},
		{name: "secondary used", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			*quota.Secondary.UsedPercent = 1
			*quota.Secondary.UsedTokens = 10
			*quota.Secondary.RemainingTokens = 990
		}, reason: activationSecondaryNotFresh},
		{name: "rounded percent with tokens", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			*quota.Primary.UsedTokens = 1
			*quota.Primary.RemainingTokens = 999
		}, reason: activationPrimaryNotFresh},
		{name: "rounded percent with active countdown", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			*quota.Primary.ResetAfterSeconds = *quota.Primary.LimitWindowSeconds - 1
		}, reason: activationPrimaryNotFresh},
		{name: "contradictory remaining", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { *quota.Primary.RemainingTokens = 999 }, reason: activationUnknownQuota},
		{name: "contradictory reset countdown", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { *quota.Primary.ResetAt -= 120 }, reason: activationUnknownQuota},
		{name: "positive percent overrides rounded token fields", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { *quota.Primary.UsedPercent = 1 }, reason: activationPrimaryNotFresh},
		{name: "omitted secondary presence", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { quota.Secondary.Presence = "" }, reason: activationUnknownQuota},
		{name: "zero reported windows", credential: true, quotaOK: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) {
			quota.Primary = quotaActivationWindow{Presence: quotaWindowAbsent}
			quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
		}, reason: activationUnknownQuota},
		{name: "wrong provider", credential: true, quotaOK: true, mutate: func(account *configuredAccount, _ *quotaActivationQuota) { account.Provider = "anthropic" }, reason: activationWrongProvider},
		{name: "expired", credential: true, quotaOK: true, mutate: func(account *configuredAccount, _ *quotaActivationQuota) { account.Expired = true }, reason: activationExpired},
		{name: "unstable identity", credential: true, quotaOK: true, mutate: func(account *configuredAccount, _ *quotaActivationQuota) { account.AuthIndex = "" }, reason: activationUnstableIdentity},
		{name: "disabled force remains unsafe", credential: true, quotaOK: true, force: true, mutate: func(account *configuredAccount, _ *quotaActivationQuota) { account.Disabled = true }, reason: activationDisabled},
		{name: "missing credential", quotaOK: true, reason: activationMissingCredential},
		{name: "force bypasses freshness", credential: true, quotaOK: true, force: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { *quota.Primary.UsedPercent = 50 }, eligible: true, reason: activationEligible},
		{name: "force does not bypass unknown presence", credential: true, quotaOK: true, force: true, mutate: func(_ *configuredAccount, quota *quotaActivationQuota) { quota.Secondary.Presence = "" }, reason: activationUnknownQuota},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := healthyActivationAccount()
			quota := freshActivationQuota()
			if test.mutate != nil {
				test.mutate(&account, quota)
			}
			decision := evaluateQuotaActivation(account, test.credential, quota, test.quotaOK, test.force)
			if decision.Eligible != test.eligible || decision.Reason != test.reason {
				t.Fatalf("decision=%+v, want eligible=%v reason=%s", decision, test.eligible, test.reason)
			}
		})
	}
}

func TestMergeCodexQuotaPayloadPreservesPresenceDurationCountdownAndZeroTokens(t *testing.T) {
	headers := map[string][]string{}
	mergeCodexQuotaPayload(headers, []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800,"reset_at":4102444800,"used_tokens":0,"remaining_tokens":1000,"limit_tokens":1000},"secondary_window":null}}`))
	for key, want := range map[string]string{
		"x-codex-primary-window-presence":      "present",
		"x-codex-primary-used-percent":         "0",
		"x-codex-primary-used-tokens":          "0",
		"x-codex-primary-remaining-tokens":     "1000",
		"x-codex-primary-limit-tokens":         "1000",
		"x-codex-primary-limit-window-seconds": "604800",
		"x-codex-primary-reset-after-seconds":  "604800",
		"x-codex-secondary-window-presence":    "absent",
	} {
		if got := headerValue(headers, key); got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
	run := quotaTriggerRun{}
	populateQuotaTriggerRunWindows(&run, headers)
	quota := quotaActivationQuotaFromRun(run)
	if quota.Primary.Presence != quotaWindowPresent || quota.Secondary.Presence != quotaWindowAbsent || pointerInt64Value(quota.Primary.LimitWindowSeconds) != 604800 || pointerInt64Value(quota.Primary.ResetAfterSeconds) != 604800 {
		t.Fatalf("activation projection lost window metadata: %+v", quota)
	}
}

func TestMergeCodexQuotaPayloadKeepsBackwardCompatibleCamelCaseParsing(t *testing.T) {
	headers := map[string][]string{}
	mergeCodexQuotaPayload(headers, []byte(`{"body":{"rateLimit":{"primaryWindow":{"usedPercent":0,"limitWindowSeconds":3600,"resetAfterSeconds":3600,"resetAt":4102444800},"secondaryWindow":null}}}`))
	if got := headerValue(headers, "x-codex-primary-window-presence"); got != "present" {
		t.Fatalf("primary presence=%q", got)
	}
	if got := headerValue(headers, "x-codex-secondary-window-presence"); got != "absent" {
		t.Fatalf("secondary presence=%q", got)
	}
	if got := headerValue(headers, "x-codex-primary-limit-window-seconds"); got != "3600" {
		t.Fatalf("primary duration=%q", got)
	}
}

func TestMergeCodexQuotaPayloadPresenceRejectsOmittedAndMalformedWindows(t *testing.T) {
	tests := []struct {
		name string
		body string
		want quotaWindowPresence
	}{
		{name: "object", body: `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":4102444800}}}`, want: quotaWindowPresent},
		{name: "null", body: `{"rate_limit":{"primary_window":null}}`, want: quotaWindowAbsent},
		{name: "omitted", body: `{"rate_limit":{}}`, want: quotaWindowUnknown},
		{name: "scalar", body: `{"rate_limit":{"primary_window":"bad"}}`, want: quotaWindowUnknown},
		{name: "malformed reset does not fall back", body: `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":"bad","limit_window_seconds":3600,"reset_after_seconds":3600}}}`, want: quotaWindowUnknown},
		{name: "fractional duration", body: `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":4102444800,"limit_window_seconds":3600.5,"reset_after_seconds":3600}}}`, want: quotaWindowUnknown},
		{name: "contradictory aliases", body: `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":4102444800},"primaryWindow":null}}`, want: quotaWindowUnknown},
		{name: "contradictory numeric aliases", body: `{"rate_limit":{"primary_window":{"used_percent":0,"usedPercent":1,"reset_at":4102444800}}}`, want: quotaWindowUnknown},
		{name: "contradictory nested and root values", body: `{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":4102444800}},"primary_window":{"used_percent":5,"reset_at":4102444800}}`, want: quotaWindowUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := map[string][]string{}
			mergeCodexQuotaPayload(headers, []byte(test.body))
			got := parseQuotaWindowPresence(headerValue(headers, "x-codex-primary-window-presence"))
			if got != test.want {
				t.Fatalf("presence=%q, want %q; headers=%v", got, test.want, headers)
			}
		})
	}
}

func TestRecordQuotaTriggerRunPersistsWindowPresenceDurationAndCountdown(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := quotaTriggerRun{
		AuthIndex: "index-a", Provider: "codex", Status: "success", HTTPStatus: http.StatusOK,
		PrimaryWindowPresence: quotaWindowPresent, PrimaryLimitWindowSeconds: int64Pointer(604800), PrimaryResetAfterSeconds: int64Pointer(604799),
		SecondaryWindowPresence: quotaWindowAbsent,
	}
	if err := recordQuotaTriggerRun(context.Background(), db, run); err != nil {
		t.Fatal(err)
	}
	var primaryPresence, secondaryPresence string
	var primaryDuration, primaryCountdown int64
	var secondaryDuration, secondaryCountdown any
	if err := db.QueryRow(`SELECT primary_window_presence,primary_limit_window_seconds,primary_reset_after_seconds,secondary_window_presence,secondary_limit_window_seconds,secondary_reset_after_seconds FROM quota_trigger_runs`).Scan(
		&primaryPresence, &primaryDuration, &primaryCountdown, &secondaryPresence, &secondaryDuration, &secondaryCountdown,
	); err != nil {
		t.Fatal(err)
	}
	if primaryPresence != "present" || primaryDuration != 604800 || primaryCountdown != 604799 || secondaryPresence != "absent" || secondaryDuration != nil || secondaryCountdown != nil {
		t.Fatalf("persisted presence/duration/countdown=%q/%d/%d %q/%v/%v", primaryPresence, primaryDuration, primaryCountdown, secondaryPresence, secondaryDuration, secondaryCountdown)
	}
}

func TestActivationAccountIdentityKeepsSameEmailSeatsSeparate(t *testing.T) {
	left := healthyActivationAccount()
	left.Email = "shared@example.com"
	right := left
	right.AuthIndex = "auth-index-2"
	right.AuthID = "auth-id-2"
	right.AuthFile = "seat-2.json"
	if activationAccountKey(left) == activationAccountKey(right) {
		t.Fatal("distinct auth indexes were deduplicated by shared email")
	}
}

func TestActivationAPIIdentityDoesNotExposeAuthFile(t *testing.T) {
	account := healthyActivationAccount()
	account.AuthID = account.AuthFile // Host auth IDs may themselves be file names.
	row := activationAccountResultFromConfigured(account)
	row.CycleKey = "fixture-raw-cycle-key"
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), account.AuthFile) || strings.Contains(row.AccountKey, account.AuthFile) || strings.Contains(string(raw), "auth_id") || strings.Contains(string(raw), row.CycleKey) || strings.Contains(string(raw), "cycle_key") {
		t.Fatalf("activation API identity exposed internal auth/file/cycle identity: %s", raw)
	}
	if len(row.AccountKey) != sha256.Size*2 {
		t.Fatalf("account key length=%d, want opaque SHA-256 hex", len(row.AccountKey))
	}
}

func TestStableActivationAuthIndexesRejectsDuplicates(t *testing.T) {
	left := healthyActivationAccount()
	right := left
	right.AuthID = "auth-id-2"
	right.AuthFile = "seat-2.json"
	stable := stableActivationAuthIndexes([]configuredAccount{left, right})
	if _, ok := stable[normalizeAccountAlias(left.AuthIndex)]; ok {
		t.Fatal("duplicate auth index was treated as a stable identity")
	}
}

func TestQuotaActivationForcePreviewRequiresExplicitSelection(t *testing.T) {
	_, status, err := (&quotaActivationManager{}).startPreview(context.Background(), quotaActivationPreviewRequest{Force: true})
	if status != http.StatusBadRequest || err == nil || !strings.Contains(err.Error(), "explicit auth_indexes") {
		t.Fatalf("force-all preview status=%d err=%v", status, err)
	}
}

func TestQuotaActivationSchemaMigratesExistingCycleTable(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE quota_activation_cycles (account_key TEXT NOT NULL,cycle_key TEXT NOT NULL,run_id TEXT NOT NULL,status TEXT NOT NULL,reserved_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(account_key,cycle_key))`); err != nil {
		t.Fatal(err)
	}
	if err := initializeSQLiteStore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`PRAGMA table_info(quota_activation_cycles)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		found = found || name == "next_cycle_after"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("next_cycle_after migration was not applied")
	}
}

func TestQuotaActivationSchemaMigratesObservationMarkers(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE quota_activation_cycles (account_key TEXT NOT NULL,cycle_key TEXT NOT NULL,run_id TEXT NOT NULL,status TEXT NOT NULL,reserved_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,next_cycle_after INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account_key,cycle_key))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at) VALUES ('existing','cycle','run','sent_unknown',100,100)`); err != nil {
		t.Fatal(err)
	}
	if err := initializeSQLiteStore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := initializeSQLiteStore(context.Background(), db); err != nil {
		t.Fatalf("idempotent initialization: %v", err)
	}
	columns := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(quota_activation_cycles)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"active_observed_at", "refresh_observed_at"} {
		if !columns[name] {
			t.Fatalf("%s migration was not applied", name)
		}
	}
	var activeAt, refreshAt int64
	if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key='existing'`).Scan(&activeAt, &refreshAt); err != nil {
		t.Fatal(err)
	}
	if activeAt != 0 || refreshAt != 0 {
		t.Fatalf("migrated observation defaults=%d/%d, want 0/0", activeAt, refreshAt)
	}
}

func TestActivationQuotaObservationState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*quotaActivationQuota)
		want   activationQuotaObservationState
	}{
		{name: "fresh", want: activationObservationFresh},
		{name: "positive percent active", mutate: func(quota *quotaActivationQuota) { *quota.Primary.UsedPercent = 1 }, want: activationObservationActive},
		{name: "positive tokens active", mutate: func(quota *quotaActivationQuota) { *quota.Primary.UsedTokens = 1; *quota.Primary.RemainingTokens = 999 }, want: activationObservationActive},
		{name: "short countdown active", mutate: func(quota *quotaActivationQuota) { *quota.Primary.ResetAfterSeconds--; *quota.Primary.ResetAt-- }, want: activationObservationActive},
		{name: "unknown presence", mutate: func(quota *quotaActivationQuota) { quota.Secondary.Presence = quotaWindowUnknown }, want: activationObservationInvalid},
		{name: "absent only", mutate: func(quota *quotaActivationQuota) {
			quota.Primary = quotaActivationWindow{Presence: quotaWindowAbsent}
			quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
		}, want: activationObservationInvalid},
		{name: "contradictory countdown", mutate: func(quota *quotaActivationQuota) {
			*quota.Primary.ResetAfterSeconds = *quota.Primary.LimitWindowSeconds + 1
		}, want: activationObservationInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quota := freshActivationQuota()
			if test.mutate != nil {
				test.mutate(quota)
			}
			if got := classifyActivationQuotaObservation(*quota); got != test.want {
				t.Fatalf("observation state=%q, want %q", got, test.want)
			}
		})
	}
}

func TestActivationCycleDecisionRecognizesObservedReset(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		boundary int64
		wantKind activationCycleDecisionKind
	}{
		{name: "future boundary sent unknown", status: "sent_unknown", boundary: time.Now().Add(7 * 24 * time.Hour).Unix(), wantKind: activationCycleObservedResetSuccessor},
		{name: "no boundary partial", status: "partial", boundary: 0, wantKind: activationCycleObservedResetSuccessor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			account := healthyActivationAccount()
			fresh := *freshActivationQuota()
			fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
			reservedAt := fresh.ObservedAt - 20
			predecessorKey := activationCycleKey(account, fresh)
			if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(account), predecessorKey, "run-old", test.status, reservedAt, reservedAt, test.boundary); err != nil {
				t.Fatal(err)
			}
			active := fresh
			active.ObservedAt = reservedAt + 5
			shorter := *active.Primary.LimitWindowSeconds - 5
			active.Primary.ResetAfterSeconds = &shorter
			activeReset := active.ObservedAt + shorter
			active.Primary.ResetAt = &activeReset
			insertActivationHistory(t, db, "history-active", activationAccountKey(account), &active, nil)

			fresh.ObservedAt = reservedAt + 10
			freshReset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
			fresh.Primary.ResetAt = &freshReset
			decision, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Kind != test.wantKind || decision.CycleKey == predecessorKey {
				t.Fatalf("decision=%+v, predecessor=%q", decision, predecessorKey)
			}
			var activeAt, refreshAt int64
			if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=? AND cycle_key=?`, activationAccountKey(account), predecessorKey).Scan(&activeAt, &refreshAt); err != nil {
				t.Fatal(err)
			}
			if activeAt != active.ObservedAt || refreshAt != fresh.ObservedAt {
				t.Fatalf("markers=%d/%d, want %d/%d", activeAt, refreshAt, active.ObservedAt, fresh.ObservedAt)
			}
			repeated, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
			if err != nil || repeated != decision {
				t.Fatalf("repeated decision=%+v err=%v, want %+v", repeated, err, decision)
			}
		})
	}
}

func TestActivationCycleDecisionBlocksFreshWithoutActiveEvidence(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	predecessorKey := activationCycleKey(account, fresh)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(account), predecessorKey, "run-old", "sent_unknown", fresh.ObservedAt-10, fresh.ObservedAt-10, fresh.ObservedAt+1000); err != nil {
		t.Fatal(err)
	}
	decision, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != activationCycleSameCycleBlocked || decision.CycleKey != predecessorKey {
		t.Fatalf("decision=%+v, want blocked predecessor", decision)
	}
	var activeAt, refreshAt int64
	if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=?`, activationAccountKey(account)).Scan(&activeAt, &refreshAt); err != nil {
		t.Fatal(err)
	}
	if activeAt != 0 || refreshAt != 0 {
		t.Fatalf("fresh-only observation advanced markers=%d/%d", activeAt, refreshAt)
	}
}

func TestActivationCycleDecisionRejectsMalformedPersistedObservationMarkers(t *testing.T) {
	tests := []struct {
		name      string
		activeAt  int64
		refreshAt int64
	}{
		{name: "negative active", activeAt: -1, refreshAt: 0},
		{name: "negative refresh", activeAt: 105, refreshAt: -1},
		{name: "refresh without active", activeAt: 0, refreshAt: 110},
		{name: "active before reservation", activeAt: 99, refreshAt: 110},
		{name: "refresh equals active", activeAt: 105, refreshAt: 105},
		{name: "refresh precedes active", activeAt: 110, refreshAt: 105},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			account := healthyActivationAccount()
			fresh := *freshActivationQuota()
			fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
			fresh.ObservedAt = 120
			resetAt := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
			fresh.Primary.ResetAt = &resetAt
			accountKey := activationAccountKey(account)
			if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after,active_observed_at,refresh_observed_at) VALUES (?,?,?,?,?,?,?,?,?)`, accountKey, "malformed-predecessor", "run-old", "sent_unknown", 100, 100, 0, test.activeAt, test.refreshAt); err != nil {
				t.Fatal(err)
			}
			decision, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Kind != activationCycleSameCycleBlocked || decision.CycleKey != "malformed-predecessor" {
				t.Fatalf("malformed markers produced decision=%+v", decision)
			}
			var activeAt, refreshAt int64
			if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=?`, accountKey).Scan(&activeAt, &refreshAt); err != nil {
				t.Fatal(err)
			}
			if activeAt != test.activeAt || refreshAt != test.refreshAt {
				t.Fatalf("malformed markers were moved from %d/%d to %d/%d", test.activeAt, test.refreshAt, activeAt, refreshAt)
			}
		})
	}
}

func TestActivationCycleHistoryIsScopedToExactAccount(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	other := account
	other.AuthIndex = "other-index"
	other.AuthID = "other-id"
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	reservedAt := fresh.ObservedAt - 20
	accountKey := activationAccountKey(account)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,0)`, accountKey, "account-predecessor", "run-old", "sent_unknown", reservedAt, reservedAt); err != nil {
		t.Fatal(err)
	}
	active := fresh
	active.ObservedAt = reservedAt + 5
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	insertActivationHistory(t, db, "other-account-history", activationAccountKey(other), &active, nil)
	fresh.ObservedAt = reservedAt + 10
	freshReset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
	fresh.Primary.ResetAt = &freshReset
	decision, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != activationCycleSameCycleBlocked || decision.CycleKey != "account-predecessor" {
		t.Fatalf("other account history advanced decision=%+v", decision)
	}
	var activeAt, refreshAt int64
	if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=?`, accountKey).Scan(&activeAt, &refreshAt); err != nil {
		t.Fatal(err)
	}
	if activeAt != 0 || refreshAt != 0 {
		t.Fatalf("other account history advanced markers=%d/%d", activeAt, refreshAt)
	}
}

func TestActivationCycleDecisionDoesNotAdvancePastBoundaryWithoutFreshQuota(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	active := *freshActivationQuota()
	active.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	boundary := active.ObservedAt - 10
	reservedAt := boundary - 10
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	predecessorKey := "boundary-predecessor"
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(account), predecessorKey, "run-old", "sent_unknown", reservedAt, reservedAt, boundary); err != nil {
		t.Fatal(err)
	}
	decision, err := activationCycleDecisionForQuota(context.Background(), db, account, active)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != activationCycleSameCycleBlocked || decision.CycleKey != predecessorKey {
		t.Fatalf("active quota after boundary decision=%+v, want blocked predecessor", decision)
	}
}

func TestActivationCycleDecisionRequiresInterveningActiveObservationForEachGeneration(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	accountKey := activationAccountKey(account)
	predecessorKey := activationCycleKey(account, fresh)
	reservedAt := fresh.ObservedAt - 20
	activeAt := reservedAt + 5
	refreshAt := reservedAt + 10
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after,active_observed_at,refresh_observed_at) VALUES (?,?,?,?,?,?,?,?,?)`, accountKey, predecessorKey, "run-old", "sent_unknown", reservedAt, reservedAt, 0, activeAt, refreshAt); err != nil {
		t.Fatal(err)
	}
	fresh.ObservedAt = refreshAt
	freshReset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
	fresh.Primary.ResetAt = &freshReset
	first, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil || first.Kind != activationCycleObservedResetSuccessor {
		t.Fatalf("first successor=%+v err=%v", first, err)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, accountKey, first.CycleKey, "run-successor"); err != nil || !reserved {
		t.Fatalf("reserve successor=%v err=%v", reserved, err)
	}
	repeated := fresh
	repeated.ObservedAt++
	repeatedReset := repeated.ObservedAt + *repeated.Primary.LimitWindowSeconds
	repeated.Primary.ResetAt = &repeatedReset
	blocked, err := activationCycleDecisionForQuota(context.Background(), db, account, repeated)
	if err != nil || blocked.Kind != activationCycleSameCycleBlocked || blocked.CycleKey != first.CycleKey {
		t.Fatalf("repeated fresh decision=%+v err=%v", blocked, err)
	}

	active := repeated
	if err := db.QueryRow(`SELECT reserved_at FROM quota_activation_cycles WHERE account_key=? AND cycle_key=?`, accountKey, first.CycleKey).Scan(&active.ObservedAt); err != nil {
		t.Fatal(err)
	}
	active.ObservedAt++
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	if decision, err := activationCycleDecisionForQuota(context.Background(), db, account, active); err != nil || decision.Kind != activationCycleSameCycleBlocked {
		t.Fatalf("active successor decision=%+v err=%v", decision, err)
	}
	nextFresh := repeated
	nextFresh.ObservedAt = active.ObservedAt + 1
	nextReset := nextFresh.ObservedAt + *nextFresh.Primary.LimitWindowSeconds
	nextFresh.Primary.ResetAt = &nextReset
	next, err := activationCycleDecisionForQuota(context.Background(), db, account, nextFresh)
	if err != nil || next.Kind != activationCycleObservedResetSuccessor || next.CycleKey == first.CycleKey {
		t.Fatalf("next generation=%+v err=%v", next, err)
	}
}

func TestObservedResetSuccessorKeyStableAcrossRestartAndFailedBeforeSendRetry(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	reservedAt := fresh.ObservedAt - 20
	activeAt := reservedAt + 5
	refreshAt := reservedAt + 10
	accountKey := activationAccountKey(account)
	predecessorKey := activationCycleKey(account, fresh)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after,active_observed_at,refresh_observed_at) VALUES (?,?,?,?,?,?,?,?,?)`, accountKey, predecessorKey, "run-old", "sent_unknown", reservedAt, reservedAt, 0, activeAt, refreshAt); err != nil {
		t.Fatal(err)
	}
	fresh.ObservedAt = refreshAt
	reset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
	fresh.Primary.ResetAt = &reset
	first, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil || first.Kind != activationCycleObservedResetSuccessor {
		t.Fatalf("first decision=%+v err=%v", first, err)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, accountKey, first.CycleKey, "run-failed"); err != nil || !reserved {
		t.Fatalf("first reservation=%v err=%v", reserved, err)
	}
	if err := updateActivationCycle(context.Background(), db, accountKey, first.CycleKey, "failed_before_send"); err != nil {
		t.Fatal(err)
	}
	s.close()
	db, _, err = s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil || afterRestart != first {
		t.Fatalf("restart decision=%+v err=%v, want %+v", afterRestart, err, first)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, accountKey, afterRestart.CycleKey, "run-retry"); err != nil || !reserved {
		t.Fatalf("failed-before-send retry=%v err=%v", reserved, err)
	}
	blocked, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
	if err != nil || blocked.Kind != activationCycleSameCycleBlocked || blocked.CycleKey != first.CycleKey {
		t.Fatalf("post-retry repeated fresh=%+v err=%v", blocked, err)
	}
}

func TestActivationCycleHistoryBackfillsThreeAmbiguousAndOnePartialPredecessor(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statuses := []string{"sent_unknown", "sent_unknown", "sent_unknown", "partial"}
	for index, status := range statuses {
		account := healthyActivationAccount()
		account.AuthIndex = "auth-index-" + strconv.Itoa(index+1)
		account.AuthID = "auth-id-" + strconv.Itoa(index+1)
		fresh := *freshActivationQuota()
		fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
		reservedAt := fresh.ObservedAt - 20
		accountKey := activationAccountKey(account)
		predecessorKey := activationCycleKey(account, fresh)
		boundary := int64(0)
		if status == "sent_unknown" {
			boundary = fresh.ObservedAt + int64((7 * 24 * time.Hour).Seconds())
		}
		if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, accountKey, predecessorKey, "run-old-"+strconv.Itoa(index), status, reservedAt, reservedAt, boundary); err != nil {
			t.Fatal(err)
		}
		active := fresh
		active.ObservedAt = reservedAt + 5
		shorter := *active.Primary.LimitWindowSeconds - 5
		active.Primary.ResetAfterSeconds = &shorter
		activeReset := active.ObservedAt + shorter
		active.Primary.ResetAt = &activeReset
		insertActivationHistory(t, db, "history-"+strconv.Itoa(index), accountKey, &active, nil)
		fresh.ObservedAt = reservedAt + 10
		freshReset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
		fresh.Primary.ResetAt = &freshReset
		decision, err := activationCycleDecisionForQuota(context.Background(), db, account, fresh)
		if err != nil || decision.Kind != activationCycleObservedResetSuccessor || decision.CycleKey == predecessorKey {
			t.Fatalf("account %d status=%s decision=%+v err=%v", index, status, decision, err)
		}
	}
}

func TestActivationCycleHistoryRejectsMalformedAndOutOfOrderSnapshots(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	reservedAt := fresh.ObservedAt - 20
	predecessorKey := activationCycleKey(account, fresh)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,0)`, activationAccountKey(account), predecessorKey, "run-old", "partial", reservedAt, reservedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_jobs(job_id,job_type,state,created_at,updated_at) VALUES ('malformed','preview','completed',?,?),('out-of-order','run','completed',?,?)`, reservedAt, reservedAt, reservedAt, reservedAt); err != nil {
		t.Fatal(err)
	}
	historyFresh := fresh
	historyFresh.ObservedAt = reservedAt + 1
	historyFreshReset := historyFresh.ObservedAt + *historyFresh.Primary.LimitWindowSeconds
	historyFresh.Primary.ResetAt = &historyFreshReset
	active := fresh
	active.ObservedAt = reservedAt + 5
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	if _, err := db.Exec(`INSERT INTO quota_activation_job_accounts(job_id,account_key,before_quota_json,after_quota_json) VALUES ('malformed',?,'{bad',''),('out-of-order',?,?,?)`, activationAccountKey(account), activationAccountKey(account), marshalActivationQuota(&historyFresh), marshalActivationQuota(&active)); err != nil {
		t.Fatal(err)
	}
	// The fresh "before" snapshot precedes the active "after" snapshot, so it
	// cannot prove an active-to-fresh transition.
	currentActive := active
	currentActive.ObservedAt++
	currentReset := currentActive.ObservedAt + shorter
	currentActive.Primary.ResetAt = &currentReset
	decision, err := activationCycleDecisionForQuota(context.Background(), db, account, currentActive)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != activationCycleSameCycleBlocked {
		t.Fatalf("out-of-order history decision=%+v, want blocked", decision)
	}
}

func insertActivationHistory(t *testing.T, db *sql.DB, jobID, accountKey string, before, after *quotaActivationQuota) {
	t.Helper()
	observedAt := int64(1)
	if before != nil {
		observedAt = before.ObservedAt
	} else if after != nil {
		observedAt = after.ObservedAt
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_jobs(job_id,job_type,state,created_at,updated_at) VALUES (?,'preview','completed',?,?)`, jobID, observedAt, observedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_job_accounts(job_id,account_key,before_quota_json,after_quota_json) VALUES (?,?,?,?)`, jobID, accountKey, marshalActivationQuota(before), marshalActivationQuota(after)); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaActivationSchemaAndRestartRecovery(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO quota_activation_jobs(job_id,job_type,state,created_at,updated_at,total_accounts) VALUES ('run-1','run','running',?,?,2)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_job_accounts(job_id,account_key,auth_index,status) VALUES ('run-1','a','a','reserved'),('run-1','b','b','revalidating')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at) VALUES ('a','cycle-a','run-1','dispatch_intent',?,?),('b','cycle-b','run-1','dispatch_intent',?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedQuotaActivationJobs(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var first, second string
	if err := db.QueryRow(`SELECT status FROM quota_activation_job_accounts WHERE account_key='a'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM quota_activation_job_accounts WHERE account_key='b'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != "sent_unknown" || second != "failed_before_send" {
		t.Fatalf("recovered states=%q/%q", first, second)
	}
	var firstCycle, secondCycle string
	if err := db.QueryRow(`SELECT status FROM quota_activation_cycles WHERE account_key='a'`).Scan(&firstCycle); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM quota_activation_cycles WHERE account_key='b'`).Scan(&secondCycle); err != nil {
		t.Fatal(err)
	}
	if firstCycle != "sent_unknown" || secondCycle != "failed_before_send" {
		t.Fatalf("recovered cycles=%q/%q", firstCycle, secondCycle)
	}
}

func TestReserveActivationCycleRowsAffectedStatusContract(t *testing.T) {
	tests := []struct {
		status       string
		wantReserved bool
	}{
		{status: "failed_before_send", wantReserved: true},
		{status: "dispatch_intent", wantReserved: false},
		{status: "verified", wantReserved: false},
		{status: "partial", wantReserved: false},
		{status: "sent_unknown", wantReserved: false},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if reserved, err := reserveActivationCycle(context.Background(), db, "account", "cycle", "run-1"); err != nil || !reserved {
				t.Fatalf("first reservation=%v err=%v", reserved, err)
			}
			if err := updateActivationCycle(context.Background(), db, "account", "cycle", test.status); err != nil {
				t.Fatal(err)
			}
			reserved, err := reserveActivationCycle(context.Background(), db, "account", "cycle", "run-2")
			if err != nil {
				t.Fatal(err)
			}
			if reserved != test.wantReserved {
				t.Fatalf("reservation for %s=%v, want %v", test.status, reserved, test.wantReserved)
			}
			var runID, status string
			if err := db.QueryRow(`SELECT run_id,status FROM quota_activation_cycles WHERE account_key='account' AND cycle_key='cycle'`).Scan(&runID, &status); err != nil {
				t.Fatal(err)
			}
			if test.wantReserved {
				if runID != "run-2" || status != "dispatch_intent" {
					t.Fatalf("reused row=%q/%q, want run-2/dispatch_intent", runID, status)
				}
			} else if runID != "run-1" || status != test.status {
				t.Fatalf("blocked row changed to %q/%q, want run-1/%s", runID, status, test.status)
			}
		})
	}
}

func TestReserveActivationCycleReusesOnlyDefinitePreSendFailure(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, "account", "cycle", "run-1"); err != nil || !reserved {
		t.Fatalf("initial reservation=%v err=%v", reserved, err)
	}
	if err := updateActivationCycle(context.Background(), db, "account", "cycle", "failed_before_send"); err != nil {
		t.Fatal(err)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, "account", "cycle", "run-2"); err != nil || !reserved {
		t.Fatalf("definite pre-send retry=%v err=%v", reserved, err)
	}
	var runID, status string
	if err := db.QueryRow(`SELECT run_id,status FROM quota_activation_cycles WHERE account_key='account' AND cycle_key='cycle'`).Scan(&runID, &status); err != nil {
		t.Fatal(err)
	}
	if runID != "run-2" || status != "dispatch_intent" {
		t.Fatalf("retried reservation=%q/%q, want run-2/dispatch_intent", runID, status)
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, "account", "cycle", "run-3"); err != nil || reserved {
		t.Fatalf("active dispatch retry=%v err=%v, want blocked", reserved, err)
	}
}

func TestReserveActivationCycleWaitsForConcurrentAccountPersistence(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const accountCount = 4
	// One connection holds the write lock while each account reservation owns a
	// separate connection blocked inside SQLite's busy handler.
	db.SetMaxOpenConns(accountCount + 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	blocker, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = blocker.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	type reservationResult struct {
		account  string
		reserved bool
		err      error
	}
	results := make(chan reservationResult, accountCount)
	var started sync.WaitGroup
	started.Add(accountCount)
	for i := 0; i < accountCount; i++ {
		account := "account-" + strconv.Itoa(i)
		go func() {
			started.Done()
			reserved, err := reserveActivationCycle(ctx, db, account, "cycle", "run")
			results <- reservationResult{account: account, reserved: reserved, err: err}
		}()
	}
	started.Wait()

	// Do not release the lock until all four writes are demonstrably in flight.
	// A launch barrier plus a fixed sleep could pass without exercising SQLite
	// contention if one or more workers had not entered ExecContext yet.
	waitUntil := time.Now().Add(2 * time.Second)
	for db.Stats().InUse != accountCount+1 && time.Now().Before(waitUntil) {
		time.Sleep(time.Millisecond)
	}
	if inUse := db.Stats().InUse; inUse != accountCount+1 {
		t.Fatalf("SQLite connections in use=%d, want blocker plus %d waiting reservations", inUse, accountCount)
	}
	if _, err := blocker.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	committed = true

	for i := 0; i < accountCount; i++ {
		result := <-results
		if result.err != nil || !result.reserved {
			t.Fatalf("reservation for %s=%v err=%v", result.account, result.reserved, result.err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_activation_cycles WHERE run_id='run' AND status='dispatch_intent'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != accountCount {
		t.Fatalf("dispatch intents=%d, want %d", count, accountCount)
	}
}

func TestQuotaActivationVerification(t *testing.T) {
	before := *freshActivationQuota()
	after := before
	primaryUsed := 0.1
	secondaryUsed := 0.2
	after.Primary.UsedPercent = &primaryUsed
	after.Secondary.UsedPercent = &secondaryUsed
	if got := classifyActivationVerification(before, after); got != "verified" {
		t.Fatalf("verification=%q, want verified", got)
	}
	after.Secondary.UsedPercent = before.Secondary.UsedPercent
	if got := classifyActivationVerification(before, after); got != "partial" {
		t.Fatalf("verification=%q, want partial", got)
	}

	before = *freshActivationQuota()
	before.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	after = before
	shorter := *before.Primary.LimitWindowSeconds - 5
	anchoredReset := after.ObservedAt + shorter
	after.Primary.ResetAfterSeconds = &shorter
	after.Primary.ResetAt = &anchoredReset
	if got := classifyActivationVerification(before, after); got != "verified" {
		t.Fatalf("single reported countdown transition=%q, want verified", got)
	}

	before = *freshActivationQuota()
	before.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	after = before
	floatingReset := *before.Primary.ResetAt + 60
	after.Primary.ResetAt = &floatingReset
	if got := classifyActivationVerification(before, after); got != "sent_unknown" {
		t.Fatalf("floating reset movement=%q, want sent_unknown", got)
	}

	beforeUsed := 25.0
	before.Primary.UsedPercent = &beforeUsed
	after = before
	if got := classifyActivationVerification(before, after); got != "sent_unknown" {
		t.Fatalf("unchanged force-mode verification=%q, want sent_unknown", got)
	}

	before = *freshActivationQuota()
	before.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	after = before
	positive := 1.0
	after.Primary.UsedPercent = &positive
	contradictoryCountdown := *after.Primary.LimitWindowSeconds + 1
	after.Primary.ResetAfterSeconds = &contradictoryCountdown
	if got := classifyActivationVerification(before, after); got != "sent_unknown" {
		t.Fatalf("contradictory post-state verification=%q, want sent_unknown", got)
	}
}

func TestActivationCycleKeyIgnoresFloatingFreshResetAt(t *testing.T) {
	account := healthyActivationAccount()
	first := *freshActivationQuota()
	first.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	second := first
	moved := *first.Primary.ResetAt + 90
	second.Primary.ResetAt = &moved
	second.ObservedAt += 90
	if left, right := activationCycleKey(account, first), activationCycleKey(account, second); left != right {
		t.Fatalf("fresh floating reset changed cycle key: %s != %s", left, right)
	}

	shorter := *second.Primary.LimitWindowSeconds - 10
	anchored := second.ObservedAt + shorter
	second.Primary.ResetAfterSeconds = &shorter
	second.Primary.ResetAt = &anchored
	if activationCycleKey(account, first) == activationCycleKey(account, second) {
		t.Fatal("active anchored window reused the fresh cycle key")
	}
	later := second
	laterCountdown := shorter - 5
	later.Primary.ResetAfterSeconds = &laterCountdown
	later.ObservedAt += 5
	if activationCycleKey(account, second) != activationCycleKey(account, later) {
		t.Fatal("active anchored cycle key changed as its countdown advanced")
	}
}

func TestActivationCycleReservationAdvancesOnlyAfterRecordedBoundary(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	first := *freshActivationQuota()
	first.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	baseKey := activationCycleKey(account, first)
	boundary := *first.Primary.ResetAt
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(account), baseKey, "run-1", "sent_unknown", first.ObservedAt, first.ObservedAt, boundary); err != nil {
		t.Fatal(err)
	}

	beforeBoundary := first
	beforeBoundary.ObservedAt = boundary - 1
	beforeReset := beforeBoundary.ObservedAt + *beforeBoundary.Primary.LimitWindowSeconds
	beforeBoundary.Primary.ResetAt = &beforeReset
	key, err := activationCycleKeyForReservation(context.Background(), db, account, beforeBoundary)
	if err != nil {
		t.Fatal(err)
	}
	if key != baseKey {
		t.Fatal("moving fresh reset created a new key before the prior boundary")
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, activationAccountKey(account), key, "run-duplicate"); err != nil || reserved {
		t.Fatalf("same-cycle sent_unknown reservation=%v err=%v, want blocked", reserved, err)
	}

	afterBoundary := first
	afterBoundary.ObservedAt = boundary + 1
	afterReset := afterBoundary.ObservedAt + *afterBoundary.Primary.LimitWindowSeconds
	afterBoundary.Primary.ResetAt = &afterReset
	nextKey, err := activationCycleKeyForReservation(context.Background(), db, account, afterBoundary)
	if err != nil {
		t.Fatal(err)
	}
	if nextKey == baseKey {
		t.Fatal("genuinely later fresh cycle collided with the prior cycle")
	}
	if reserved, err := reserveActivationCycle(context.Background(), db, activationAccountKey(account), nextKey, "run-2"); err != nil || !reserved {
		t.Fatalf("later-cycle reservation=%v err=%v, want reserved", reserved, err)
	}
	if repeated, err := activationCycleKeyForReservation(context.Background(), db, account, afterBoundary); err != nil || repeated != nextKey {
		t.Fatalf("later fresh opportunity key=%q err=%v, want stable %q", repeated, err, nextKey)
	}
}

func TestActivationCycleReservationWithoutSafeBoundaryRemainsBlocked(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	quota := *freshActivationQuota()
	quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	baseKey := activationCycleKey(account, quota)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,0)`, activationAccountKey(account), baseKey, "run-ambiguous", "sent_unknown", quota.ObservedAt, quota.ObservedAt); err != nil {
		t.Fatal(err)
	}
	quota.ObservedAt += int64((30 * 24 * time.Hour).Seconds())
	resetAt := quota.ObservedAt + *quota.Primary.LimitWindowSeconds
	quota.Primary.ResetAt = &resetAt
	key, err := activationCycleKeyForReservation(context.Background(), db, account, quota)
	if err != nil {
		t.Fatal(err)
	}
	if key != baseKey {
		t.Fatal("ambiguous send without a safe post-send boundary became retryable")
	}
	quota.Secondary = quotaActivationWindow{Presence: quotaWindowPresent, UsedPercent: float64Pointer(0), ResetAt: int64Pointer(resetAt), LimitWindowSeconds: int64Pointer(3600), ResetAfterSeconds: int64Pointer(3600)}
	changedShapeKey, err := activationCycleKeyForReservation(context.Background(), db, account, quota)
	if err != nil {
		t.Fatal(err)
	}
	if changedShapeKey != baseKey {
		t.Fatal("changed window shape bypassed the account-level sent_unknown guard")
	}
}

func TestActivationCycleReservationBlockingStatusesGuardAccountWide(t *testing.T) {
	for _, status := range []string{"dispatch_intent", "verified", "partial", "sent_unknown"} {
		t.Run(status, func(t *testing.T) {
			s := newTestStore(t)
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			account := healthyActivationAccount()
			quota := *freshActivationQuota()
			quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
			accountKey := activationAccountKey(account)
			if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,0)`, accountKey, "guard-key", "run-guard", status, quota.ObservedAt, quota.ObservedAt); err != nil {
				t.Fatal(err)
			}
			// A changed window shape produces a different base key, but an account-wide
			// blocker without a trustworthy boundary must still win.
			quota.Secondary = quotaActivationWindow{Presence: quotaWindowPresent, UsedPercent: float64Pointer(0), ResetAt: int64Pointer(quota.ObservedAt + 3600), LimitWindowSeconds: int64Pointer(3600), ResetAfterSeconds: int64Pointer(3600)}
			key, err := activationCycleKeyForReservation(context.Background(), db, account, quota)
			if err != nil {
				t.Fatal(err)
			}
			if key != "guard-key" {
				t.Fatalf("status %s returned key %q, want account-wide guard", status, key)
			}
		})
	}
}

func TestActivationCycleReservationUsesLatestAccountWideBlocker(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account := healthyActivationAccount()
	quota := *freshActivationQuota()
	quota.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	accountKey := activationAccountKey(account)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,0),(?,?,?,?,?,?,0)`,
		accountKey, "older", "run-old", "verified", quota.ObservedAt-1, quota.ObservedAt+10,
		accountKey, "latest", "run-latest", "partial", quota.ObservedAt, quota.ObservedAt); err != nil {
		t.Fatal(err)
	}
	key, err := activationCycleKeyForReservation(context.Background(), db, account, quota)
	if err != nil {
		t.Fatal(err)
	}
	if key != "latest" {
		t.Fatalf("account-wide guard=%q, want latest reservation", key)
	}
}

func TestQuotaActivationPruningKeepsLatestAmbiguousCycleGuard(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at) VALUES ('account','old','run-old','verified',?,?),('account','latest','run-latest','sent_unknown',?,?)`, old, old, old+1, old+1); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneQuotaActivationState(context.Background(), db, time.Now().Add(-30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM quota_activation_cycles WHERE account_key='account'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining cycle guards=%d, want latest guard only", count)
	}
	var key, status string
	if err := db.QueryRow(`SELECT cycle_key,status FROM quota_activation_cycles WHERE account_key='account'`).Scan(&key, &status); err != nil {
		t.Fatal(err)
	}
	if key != "latest" || status != "sent_unknown" {
		t.Fatalf("kept cycle=%q/%q, want latest sent_unknown", key, status)
	}
}

func TestQuotaActivationPruningKeepsGuardBeforeFailedSuccessor(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after,active_observed_at,refresh_observed_at) VALUES ('account','predecessor','run-old','sent_unknown',?,?,0,0,0),('account','failed-successor','run-failed','failed_before_send',?,?,0,0,0)`, old, old, old+3, old+3); err != nil {
		t.Fatal(err)
	}
	active := *freshActivationQuota()
	active.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	active.ObservedAt = old + 1
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	fresh := *freshActivationQuota()
	fresh.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	fresh.ObservedAt = old + 2
	freshReset := fresh.ObservedAt + *fresh.Primary.LimitWindowSeconds
	fresh.Primary.ResetAt = &freshReset
	insertActivationHistory(t, db, "retained-evidence", "account", &active, &fresh)
	if _, err := pruneQuotaActivationState(context.Background(), db, time.Now().Add(-30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	var key, status string
	var activeAt, refreshAt int64
	if err := db.QueryRow(`SELECT cycle_key,status,active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key='account'`).Scan(&key, &status, &activeAt, &refreshAt); err != nil {
		t.Fatal(err)
	}
	if key != "predecessor" || status != "sent_unknown" || activeAt != old+1 || refreshAt != old+2 {
		t.Fatalf("retained guard=%q/%q markers=%d/%d", key, status, activeAt, refreshAt)
	}
	var historyRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM quota_activation_job_accounts WHERE account_key='account'`).Scan(&historyRows); err != nil {
		t.Fatal(err)
	}
	if historyRows != 0 {
		t.Fatalf("retained historical job rows=%d, want evidence compacted into cycle markers", historyRows)
	}
}

func TestQuotaProbeGateRejectsOverlappingRounds(t *testing.T) {
	if !tryAcquireQuotaProbeGate() {
		t.Fatal("shared quota probe gate was unexpectedly busy")
	}
	defer releaseQuotaProbeGate()
	if tryAcquireQuotaProbeGate() {
		t.Fatal("shared quota probe gate allowed an overlapping round")
	}
}

func TestQuotaProbeFailureActivationStatus(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 429} {
		if got := quotaProbeFailureActivationStatus(status); got != "failed_before_send" {
			t.Fatalf("status %d classified as %q", status, got)
		}
	}
	for _, status := range []int{0, 301, 408, 409, 425, 499, 500, 503} {
		if got := quotaProbeFailureActivationStatus(status); got != "sent_unknown" {
			t.Fatalf("status %d classified as %q", status, got)
		}
	}
}

func TestQuotaActivationProbeAllowsOnlyDefiniteCompatibilityRetry(t *testing.T) {
	oldResponsesURL := codexResponsesURLOverrideForTest
	defer func() { codexResponsesURLOverrideForTest = oldResponsesURL }()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"unknown_parameter","param":"stream"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	codexResponsesURLOverrideForTest = server.URL
	account := healthyActivationAccount()
	run := executeQuotaProbeRequest(context.Background(), nil, triggerAuthAccount{configuredAccount: account, AccessToken: "fixture-access"}, defaultPluginConfig())
	if run.Status != "success" || run.HTTPStatus != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("compatibility probe status=%q http=%d calls=%d", run.Status, run.HTTPStatus, calls.Load())
	}
}

func TestQuotaActivationManagementFlowUsesFixturesOnly(t *testing.T) {
	oldStore := globalStore
	oldManager := globalQuotaActivation
	oldGrace := quotaActivationPropagationGrace
	oldQuotaURL := codexQuotaURLOverrideForTest
	oldResponsesURL := codexResponsesURLOverrideForTest
	globalStore = newTestStore(t)
	globalQuotaActivation = &quotaActivationManager{}
	quotaActivationPropagationGrace = 0
	t.Cleanup(func() {
		globalStore.close()
		globalStore = oldStore
		globalQuotaActivation = oldManager
		quotaActivationPropagationGrace = oldGrace
		codexQuotaURLOverrideForTest = oldQuotaURL
		codexResponsesURLOverrideForTest = oldResponsesURL
	})

	authDir := os.Getenv("CPA_AUTH_DIR")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authJSON := `{"provider":"codex","email":"shared@example.com","access_token":"fixture-access","chatgpt_account_id":"fixture-account-a"}`
	authJSONB := `{"provider":"codex","email":"shared@example.com","access_token":"fixture-access","chatgpt_account_id":"fixture-account-b"}`
	if err := os.WriteFile(filepath.Join(authDir, "seat-a.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "seat-b.json"), []byte(authJSONB), 0o600); err != nil {
		t.Fatal(err)
	}
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{
				{ID: "seat-a.json", AuthIndex: "index-a", Name: "seat-a.json", Provider: "codex", Email: "shared@example.com", Source: "file"},
				{ID: "seat-b.json", AuthIndex: "index-b", Name: "seat-b.json", Provider: "codex", Email: "shared@example.com", Source: "file"},
			}})
		case "host.auth.get":
			return json.Marshal(hostAuthGetResponse{AuthIndex: "index-a", Name: "seat-a.json", JSON: json.RawMessage(authJSON)})
		default:
			return nil, os.ErrNotExist
		}
	})

	var compactCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Errorf("unexpected authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			db, _, err := globalStore.open(context.Background())
			if err != nil {
				t.Errorf("open store at dispatch: %v", err)
			} else {
				var cycleStatus string
				if err := db.QueryRow(`SELECT status FROM quota_activation_cycles ORDER BY rowid DESC LIMIT 1`).Scan(&cycleStatus); err != nil {
					t.Errorf("read cycle at dispatch: %v", err)
				} else if cycleStatus != "dispatch_intent" {
					t.Errorf("cycle status at network dispatch=%q, want dispatch_intent", cycleStatus)
				}
			}
			compactCalls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		windowSeconds := int64((7 * 24 * time.Hour).Seconds())
		resetAfter := windowSeconds
		if compactCalls.Load() > 0 {
			resetAfter -= 5
		}
		resetAt := time.Now().Unix() + resetAfter
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":` + strconv.FormatInt(windowSeconds, 10) + `,"reset_after_seconds":` + strconv.FormatInt(resetAfter, 10) + `,"reset_at":` + strconv.FormatInt(resetAt, 10) + `},"secondary_window":null}}`))
	}))
	defer server.Close()
	codexQuotaURLOverrideForTest = server.URL
	codexResponsesURLOverrideForTest = server.URL
	globalQuotaActivation.configure(defaultPluginConfig())

	// Seed the predecessor shape observed in live data: an ambiguous HTTP 200
	// cycle with durable active evidence and a future scheduled boundary. The
	// preview's one fresh quota read must reconcile that evidence and derive the
	// same stable successor that run revalidation uses.
	inventory, _, err := activationInventory()
	if err != nil || len(inventory) != 2 {
		t.Fatalf("fixture inventory=%d err=%v", len(inventory), err)
	}
	var account, blockedAccount configuredAccount
	for _, item := range inventory {
		switch item.AuthIndex {
		case "index-a":
			account = item
		case "index-b":
			blockedAccount = item
		}
	}
	if account.AuthIndex == "" || blockedAccount.AuthIndex == "" {
		t.Fatal("fixture accounts were not retained as exact seats")
	}
	db, _, err := globalStore.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reservedAt := time.Now().Unix() - 100
	predecessorKey := "fixture-predecessor-cycle"
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(account), predecessorKey, "fixture-predecessor-run", "sent_unknown", reservedAt, reservedAt, time.Now().Add(7*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	active := *freshActivationQuota()
	active.Secondary = quotaActivationWindow{Presence: quotaWindowAbsent}
	active.ObservedAt = reservedAt + 10
	shorter := *active.Primary.LimitWindowSeconds - 5
	active.Primary.ResetAfterSeconds = &shorter
	activeReset := active.ObservedAt + shorter
	active.Primary.ResetAt = &activeReset
	insertActivationHistory(t, db, "fixture-active-history", activationAccountKey(account), &active, nil)
	if _, err := db.Exec(`INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at,next_cycle_after) VALUES (?,?,?,?,?,?,?)`, activationAccountKey(blockedAccount), "fixture-blocked-cycle", "fixture-blocked-run", "sent_unknown", reservedAt, reservedAt, time.Now().Add(7*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	previewResponse := handleQuotaActivationManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginID + "/quota-activation/preview", Body: []byte(`{"force":false,"auth_indexes":[]}`)})
	if previewResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("preview status=%d body=%s", previewResponse.StatusCode, previewResponse.Body)
	}
	var started quotaActivationStartResponse
	if err := json.Unmarshal(previewResponse.Body, &started); err != nil {
		t.Fatal(err)
	}
	if started.PreviewID == "" || started.RunID != "" {
		t.Fatalf("preview start response=%+v", started)
	}
	preview := waitActivationJob(t, "preview", started.PreviewID)
	if preview.State != "completed" || preview.ExpiresAt == "" || preview.ConfirmationToken == "" || len(preview.Accounts) != 2 || preview.EligibleAccounts != 1 {
		t.Fatalf("preview=%+v accounts=%+v", preview, preview.Accounts)
	}
	var eligibleRow, blockedRow *quotaActivationAccountResult
	for index := range preview.Accounts {
		switch preview.Accounts[index].AuthIndex {
		case "index-a":
			eligibleRow = &preview.Accounts[index]
		case "index-b":
			blockedRow = &preview.Accounts[index]
		}
	}
	if eligibleRow == nil || !eligibleRow.Eligible || blockedRow == nil || blockedRow.Eligible || blockedRow.Reason != activationDuplicateCycle {
		t.Fatalf("preview did not share observed-reset/duplicate decisions: %+v", preview.Accounts)
	}
	if compactCalls.Load() != 0 {
		t.Fatal("preview sent a model request")
	}
	var activeObservedAt, refreshObservedAt int64
	if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=? AND cycle_key=?`, activationAccountKey(account), predecessorKey).Scan(&activeObservedAt, &refreshObservedAt); err != nil {
		t.Fatal(err)
	}
	if activeObservedAt != active.ObservedAt || refreshObservedAt <= activeObservedAt {
		t.Fatalf("preview markers=%d/%d, want active=%d followed by fresh", activeObservedAt, refreshObservedAt, active.ObservedAt)
	}

	runBody, _ := json.Marshal(quotaActivationRunRequest{PreviewID: preview.ID, ConfirmationToken: preview.ConfirmationToken, AuthIndexes: []string{"index-a"}})
	runResponse := handleQuotaActivationManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginID + "/quota-activation/run", Body: runBody})
	if runResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("run status=%d body=%s", runResponse.StatusCode, runResponse.Body)
	}
	if reused := handleQuotaActivationManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginID + "/quota-activation/run", Body: runBody}); reused.StatusCode != http.StatusConflict {
		t.Fatalf("reused confirmation status=%d, want 409", reused.StatusCode)
	}
	started = quotaActivationStartResponse{}
	if err := json.Unmarshal(runResponse.Body, &started); err != nil {
		t.Fatal(err)
	}
	if started.RunID == "" || started.PreviewID != "" {
		t.Fatalf("run start response=%+v", started)
	}
	run := waitActivationJob(t, "run", started.RunID)
	if len(run.Accounts) != 1 || run.Accounts[0].Status != "verified" || compactCalls.Load() != 1 {
		t.Fatalf("run=%+v accounts=%+v calls=%d", run, run.Accounts, compactCalls.Load())
	}
	if run.Accounts[0].CycleKey == "" || run.Accounts[0].CycleKey == predecessorKey {
		t.Fatalf("run cycle key did not use the observed-reset successor")
	}
	var successorActiveAt, successorRefreshAt int64
	if err := db.QueryRow(`SELECT active_observed_at,refresh_observed_at FROM quota_activation_cycles WHERE account_key=? AND cycle_key=?`, activationAccountKey(account), run.Accounts[0].CycleKey).Scan(&successorActiveAt, &successorRefreshAt); err != nil {
		t.Fatal(err)
	}
	if successorActiveAt == 0 || successorRefreshAt != 0 {
		t.Fatalf("successor markers=%d/%d, want active with no refresh", successorActiveAt, successorRefreshAt)
	}
	raw, _ := json.Marshal(run)
	for _, secret := range []string{"fixture-access", "Bearer ", "Authorization"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("API result disclosed %q", secret)
		}
	}
}

func waitActivationJob(t *testing.T, jobType, id string) quotaActivationJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		db, _, err := globalStore.open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		job, err := loadActivationJob(context.Background(), db, jobType, id)
		if err == nil && job.State != "queued" && job.State != "running" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s job %s", jobType, id)
	return quotaActivationJob{}
}

func TestSanitizeTriggerErrorRedactsCredentialMaterial(t *testing.T) {
	for _, value := range []string{
		"Authorization: Bearer fixture-secret",
		"authorization: bearer fixture-secret",
		"cookie: session=fixture-secret",
		"refresh_token=fixture-secret",
		"upstream rejected sk-fixture-secret",
		"upstream rejected eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJmaXh0dXJlIn0.fixture-signature",
	} {
		if got := sanitizeTriggerError(value); got != "trigger failed" {
			t.Fatalf("sanitizeTriggerError(%q)=%q", value, got)
		}
	}
}

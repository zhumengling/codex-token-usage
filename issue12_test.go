package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func issue12HostAuthCaller(authFile, authIndex, email string, modTime int64) hostCallFunc {
	return func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, errors.New("unsupported host callback")
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID:        authFile,
			AuthIndex: authIndex,
			Name:      authFile,
			Path:      "/auth/" + authFile,
			Provider:  "codex",
			Email:     email,
			ModTime:   time.Unix(modTime, 0).UTC().Format(time.RFC3339),
		}}})
	}
}

func issue12SchedulerRequest(authFile string) schedulerPickRequest {
	return schedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-test",
		Candidates: []schedulerAuthCandidate{{
			ID:       authFile,
			Provider: "codex",
			Priority: 1,
			Attributes: map[string]string{
				"path": "/auth/" + authFile,
			},
		}},
	}
}

func TestReplacedCodexAuthClearsOldAutobanBeforeScheduling(t *testing.T) {
	now := time.Now().Unix()
	withCodexHostAuthSource(t, issue12HostAuthCaller("foo.json", "index-new", "new@example.com", now-10))
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES ('foo.json', 'index-old', 'old@example.com', 'codex', '5h', 'old account exhausted', ?, ?, 1, 429, 'foo.json', ?)`, now-100, now+3600, now-200); err != nil {
		t.Fatal(err)
	}
	response, err := s.pickAuth(context.Background(), issue12SchedulerRequest("foo.json"))
	if err != nil {
		t.Fatalf("replacement auth remained blocked: %v", err)
	}
	if response.Handled {
		t.Fatalf("response = %+v, want plugin to delegate after clearing the only stale ban", response)
	}
	var active int
	var reason string
	if err := db.QueryRow(`SELECT active, release_reason FROM autoban_bans WHERE auth_id='foo.json'`).Scan(&active, &reason); err != nil {
		t.Fatal(err)
	}
	if active != 0 || reason != "auth file replaced" {
		t.Fatalf("autoban active=%d reason=%q, want released replacement", active, reason)
	}
}

func TestUnchangedCodexAuthRemainsAutobanned(t *testing.T) {
	now := time.Now().Unix()
	withCodexHostAuthSource(t, issue12HostAuthCaller("foo.json", "index-old", "same@example.com", now-200))
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES ('foo.json', 'index-old', 'same@example.com', 'codex', '5h', 'account exhausted', ?, ?, 1, 429, 'foo.json', ?)`, now-100, now+3600, now-200); err != nil {
		t.Fatal(err)
	}
	_, err = s.pickAuth(context.Background(), issue12SchedulerRequest("foo.json"))
	var reject *schedulerRejectError
	if !errors.As(err, &reject) || reject.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("pick error = %v, want unchanged auth to remain unavailable", err)
	}
}

func TestTokenRefreshDoesNotClearAutobanForSameAccount(t *testing.T) {
	now := time.Now().Unix()
	withCodexHostAuthSource(t, issue12HostAuthCaller("foo.json", "index-old", "same@example.com", now-10))
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES ('foo.json', 'index-old', 'same@example.com', 'codex', '5h', 'account exhausted', ?, ?, 1, 429, 'foo.json', ?)`, now-100, now+3600, now-200); err != nil {
		t.Fatal(err)
	}
	_, err = s.pickAuth(context.Background(), issue12SchedulerRequest("foo.json"))
	var reject *schedulerRejectError
	if !errors.As(err, &reject) {
		t.Fatalf("pick error = %v, want token refresh for same identity to preserve ban", err)
	}
}

func TestDeleteThenReaddSameJSONDoesNotRestoreOldRestriction(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_source_kind
) VALUES ('foo.json', 'foo.json', 'same@example.com', 'codex', '401 unauthorized', ?, 1, 401, 'foo.json', 'file')`, now-100); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES ('foo.json', 'foo.json', 'same@example.com', 'codex', '14d', 'quota exhausted', ?, ?, 1, 429, 'foo.json', ?)`, now-100, now+3600, now-200); err != nil {
		t.Fatal(err)
	}
	generation := globalSchedulerState.generation("codex")
	if err := clearMissingConfiguredAuthState(context.Background(), db, nil, true); err != nil {
		t.Fatal(err)
	}
	if globalSchedulerState.generation("codex") <= generation {
		t.Fatal("deleting the old JSON did not invalidate scheduler state")
	}
	// Re-adding the same filename must not reactivate historical database rows.
	if err := clearMissingConfiguredAuthState(context.Background(), db, []configuredAccount{{
		AuthID: "new-stable-id", AuthIndex: "foo.json", AuthFile: "foo.json", Source: "same@example.com", Provider: "codex", AuthSourceKind: authSourceKindFile,
	}}, true); err != nil {
		t.Fatal(err)
	}
	var invalidActive, banActive int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='foo.json'`).Scan(&invalidActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM autoban_bans WHERE auth_id='foo.json'`).Scan(&banActive); err != nil {
		t.Fatal(err)
	}
	if invalidActive != 0 || banActive != 0 {
		t.Fatalf("re-added JSON restored old state: invalid=%d ban=%d", invalidActive, banActive)
	}
}

func TestAllFilteredSchedulingForcesFreshHostAuthList(t *testing.T) {
	now := time.Now().Unix()
	email := "old@example.com"
	modTime := now - 200
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		return issue12HostAuthCaller("foo.json", "index-stable", email, modTime)(method, payload)
	})
	s := newTestStore(t)
	if accounts := readConfiguredAuthAccounts(); len(accounts) != 1 || accounts[0].Email != "old@example.com" {
		t.Fatalf("primed accounts = %+v", accounts)
	}
	email = "new@example.com"
	modTime = now - 10
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES ('foo.json', 'index-stable', 'old@example.com', 'codex', '5h', 'old account exhausted', ?, ?, 1, 429, 'foo.json', ?)`, now-100, now+3600, now-200); err != nil {
		t.Fatal(err)
	}
	response, err := s.pickAuth(context.Background(), issue12SchedulerRequest("foo.json"))
	if err != nil || response.Handled {
		t.Fatalf("response=%+v err=%v, want forced refresh to release stale ban and delegate", response, err)
	}
}

func TestAutobanBackfillSkipsUsageFromReplacedAuthFile(t *testing.T) {
	now := time.Now().Unix()
	requestAt := now - 100
	withCodexHostAuthSource(t, issue12HostAuthCaller("foo.json", "index-new", "new@example.com", now-10))
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO usage_events (
  requested_at, provider, auth_id, auth_index, source, failed, status_code,
  primary_used_percent, primary_reset_at
) VALUES (?, 'codex', 'foo.json', 'index-old', 'old@example.com', 1, 429, 100, ?)`, requestAt, now+3600); err != nil {
		t.Fatal(err)
	}
	if err := backfillAutobansFromUsage(context.Background(), db, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM autoban_bans WHERE active=1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active autobans = %d, want replaced historical 429 to stay released", count)
	}
}

func TestAutobanSchemaUpgradeAddsAuthFileVersionColumnsIdempotently(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE autoban_bans (
auth_id TEXT PRIMARY KEY, active INTEGER NOT NULL DEFAULT 1,
released_at INTEGER NOT NULL DEFAULT 0, release_reason TEXT NOT NULL DEFAULT ''
)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureAutobanBanColumns(context.Background(), db); err != nil {
			t.Fatalf("upgrade pass %d: %v", i+1, err)
		}
	}
	columns := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(autoban_bans)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["auth_file"] || !columns["auth_file_mtime"] {
		t.Fatalf("columns = %+v, want auth_file and auth_file_mtime", columns)
	}
}

package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestOAuthUsageWithCPAAccessKeyIsNotExternalAPIKey(t *testing.T) {
	rec := usageRecord{
		Provider:  "codex",
		APIKey:    "sk-cpa-access-key",
		AuthID:    "codex-user@example.com-plus.json",
		AuthIndex: "stable-oauth-index",
		AuthType:  "oauth",
		Source:    "user@example.com",
	}
	if isCodexAPIKeyUsageRecord(rec) {
		t.Fatal("OAuth usage was misclassified as an external Codex API-key request")
	}
	rec.AuthType = "apikey"
	if !isCodexAPIKeyUsageRecord(rec) {
		t.Fatal("explicit API-key usage was not classified as an external Codex API-key request")
	}
}

func TestRecordAutobanCanonicalizesTrafficAndQuotaTriggerIdentity(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	resetAt := now + 3600
	percent := 100.0
	traffic := usageRecord{
		Provider:    "codex",
		APIKey:      "sk-cpa-access-key",
		AuthID:      "codex-user@example.com-plus.json",
		AuthIndex:   "stable-oauth-index",
		AuthType:    "oauth",
		AuthFile:    "codex-user@example.com-plus.json",
		Source:      "user@example.com",
		RequestedAt: time.Unix(now, 0),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
	}
	if err := recordAutobanIfNeeded(context.Background(), db, traffic, http.StatusTooManyRequests, &percent, &resetAt, nil, nil); err != nil {
		t.Fatal(err)
	}
	quota := traffic
	quota.APIKey = ""
	quota.AuthID = "user@example.com"
	quota.AuthIndex = "codex-user@example.com-plus.json"
	quota.AuthType = "codex"
	if err := recordAutobanIfNeeded(context.Background(), db, quota, http.StatusTooManyRequests, &percent, &resetAt, nil, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM autoban_bans WHERE active=1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active autoban rows=%d, want one canonical row", count)
	}
	var authID string
	if err := db.QueryRow(`SELECT auth_id FROM autoban_bans WHERE active=1`).Scan(&authID); err != nil {
		t.Fatal(err)
	}
	if authID != "codex-user@example.com-plus.json" {
		t.Fatalf("canonical autoban auth_id=%q, want auth file", authID)
	}
}

func TestMergeAutobanIdentityDuplicatesKeepsNewestCanonicalRow(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, auth_file, auth_file_mtime
) VALUES
  ('codex-user@example.com-plus.json', 'stable-oauth-index', 'user@example.com', 'codex', '5h', 'backfilled', ?, ?, 1, 429, 100, 'codex-user@example.com-plus.json', 10),
  ('user@example.com', 'codex-user@example.com-plus.json', 'user@example.com', 'codex', '5h', 'quota trigger', ?, ?, 1, 429, 100, 'codex-user@example.com-plus.json', 20)`,
		now-60, now+1800, now, now+3600); err != nil {
		t.Fatal(err)
	}
	if err := mergeAutobanIdentityDuplicates(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int
	var authID, reason string
	var resetAt int64
	if err := db.QueryRow(`SELECT COUNT(*), auth_id, reason, reset_at FROM autoban_bans WHERE active=1`).Scan(&count, &authID, &reason, &resetAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || authID != "codex-user@example.com-plus.json" || reason != "quota trigger" || resetAt != now+3600 {
		t.Fatalf("merged row count=%d auth_id=%q reason=%q reset_at=%d", count, authID, reason, resetAt)
	}
}

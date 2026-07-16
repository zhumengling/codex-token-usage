package main

import (
	"context"
	"testing"
	"time"
)

func insertQuotaSnapshotForTest(t *testing.T, s *store, authID, authIndex, source, authFile string, authFileMTime int64, primary, secondary float64) {
	t.Helper()
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO quota_trigger_runs (
  auth_id, auth_index, source, provider, auth_file, auth_file_mtime, mode, status, http_status,
  started_at, finished_at, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
) VALUES (?, ?, ?, 'codex', ?, ?, 'probe', 'success', 200, ?, ?, ?, ?, ?, ?)`,
		authID, authIndex, source, authFile, authFileMTime, now-2, now-1, primary, now+4*3600, secondary, now+6*24*3600,
	); err != nil {
		t.Fatal(err)
	}
}

func TestUnusedAccountDoesNotInheritAnotherAccountsQuota(t *testing.T) {
	s := newTestStore(t)
	insertQuotaSnapshotForTest(t, s, "stable-a", "index-a", "shared@example.com", "a.json", 101, 27, 4)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{
		{AuthID: "stable-a", AuthIndex: "index-a", Source: "shared@example.com", Email: "shared@example.com", AuthFile: "a.json", AuthFileMTime: 101, Requests: 1},
		{AuthID: "shared@example.com", AuthIndex: "b.json", Source: "shared@example.com", Email: "shared@example.com", AuthFile: "b.json", AuthFileMTime: 202},
	}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, time.Now().Add(-24*time.Hour).Unix())
	if accounts[0].PrimaryUsedPercent == nil || *accounts[0].PrimaryUsedPercent != 27 || accounts[0].SecondaryUsedPercent == nil || *accounts[0].SecondaryUsedPercent != 4 {
		t.Fatalf("account A quota = %+v", accounts[0])
	}
	if accounts[1].PrimaryUsedPercent != nil || accounts[1].SecondaryUsedPercent != nil || accounts[1].PrimaryWindowTokens != 0 || accounts[1].SecondaryWindowTokens != 0 {
		t.Fatalf("unused account inherited quota: %+v", accounts[1])
	}
}

func TestUnusedAccountDoesNotDisplayItsOwnQuotaProbe(t *testing.T) {
	s := newTestStore(t)
	insertQuotaSnapshotForTest(t, s, "user@example.com", "user.json", "user@example.com", "user.json", 303, 12, 3)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{
		AuthID: "user@example.com", AuthIndex: "user.json", Source: "user@example.com",
		Email: "user@example.com", AuthFile: "user.json", AuthFileMTime: 303, Requests: 0,
	}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, time.Now().Add(-24*time.Hour).Unix())
	if accounts[0].PrimaryUsedPercent != nil || accounts[0].SecondaryUsedPercent != nil {
		t.Fatalf("unused account displayed probe quota: %+v", accounts[0])
	}
}

func TestUsedAccountShowsItsOwnQuotaProbe(t *testing.T) {
	s := newTestStore(t)
	insertQuotaSnapshotForTest(t, s, "user@example.com", "user.json", "user@example.com", "user.json", 303, 12, 3)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{
		AuthID: "user@example.com", AuthIndex: "user.json", Source: "user@example.com",
		Email: "user@example.com", AuthFile: "user.json", AuthFileMTime: 303, Requests: 1,
	}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, time.Now().Add(-24*time.Hour).Unix())
	if accounts[0].PrimaryUsedPercent == nil || *accounts[0].PrimaryUsedPercent != 12 || accounts[0].SecondaryUsedPercent == nil || *accounts[0].SecondaryUsedPercent != 3 {
		t.Fatalf("used account did not display its own quota probe: %+v", accounts[0])
	}
}

func TestReplacedAuthFileDoesNotReuseOldQuotaWithoutStableIdentity(t *testing.T) {
	s := newTestStore(t)
	insertQuotaSnapshotForTest(t, s, "old@example.com", "same.json", "old@example.com", "same.json", 404, 27, 4)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{
		AuthID: "new@example.com", AuthIndex: "same.json", Source: "new@example.com",
		Email: "new@example.com", AuthFile: "same.json", AuthFileMTime: 505,
	}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, time.Now().Add(-24*time.Hour).Unix())
	if accounts[0].PrimaryUsedPercent != nil || accounts[0].SecondaryUsedPercent != nil {
		t.Fatalf("replacement inherited old quota: %+v", accounts[0])
	}
}

func TestStableAccountIdentitySurvivesFileReplacement(t *testing.T) {
	s := newTestStore(t)
	insertQuotaSnapshotForTest(t, s, "stable-account-id", "old-index", "old@example.com", "same.json", 606, 18, 2)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{
		AuthID: "stable-account-id", AuthIndex: "new-index", Source: "new@example.com",
		Email: "new@example.com", AuthFile: "same.json", AuthFileMTime: 707, Requests: 1,
	}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, time.Now().Add(-24*time.Hour).Unix())
	if accounts[0].PrimaryUsedPercent == nil || *accounts[0].PrimaryUsedPercent != 18 {
		t.Fatalf("stable identity quota was lost: %+v", accounts[0])
	}
}

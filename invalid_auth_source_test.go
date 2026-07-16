package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestEnsureInvalidAuthColumnsMigratesAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE invalid_auths (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  invalidated_at INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_status_code INTEGER NOT NULL DEFAULT 401,
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0
);
INSERT INTO invalid_auths(auth_id,auth_index,source,provider,reason,invalidated_at,auth_file)
VALUES
  ('file-id','file-index','user@example.com','codex','401',1,'account.json'),
  ('runtime-id','runtime-index','memory','codex','401',1,''),
  ('old-id','old-index','old@example.com','codex','401',1,''),
  ('runtime-looking.json','runtime-looking.json','runtime@example.com','codex','401',1,'');`); err != nil {
		t.Fatal(err)
	}

	if err := ensureInvalidAuthColumns(context.Background(), db); err != nil {
		t.Fatalf("migration pass 1: %v", err)
	}
	if _, err := db.Exec(`
UPDATE invalid_auths SET auth_source_kind=' FILE ' WHERE auth_id='file-id';
UPDATE invalid_auths SET auth_source_kind='Runtime_Only' WHERE auth_id='runtime-id';
UPDATE invalid_auths SET auth_source_kind='unknown' WHERE auth_id='old-id';`); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		if err := ensureInvalidAuthColumns(context.Background(), db); err != nil {
			t.Fatalf("migration pass %d: %v", i, err)
		}
	}

	want := map[string]string{
		"file-id":              authSourceKindFile,
		"runtime-id":           authSourceKindRuntimeOnly,
		"old-id":               authSourceKindLegacy,
		"runtime-looking.json": authSourceKindLegacy,
	}
	rows, err := db.Query(`SELECT auth_id, auth_source_kind FROM invalid_auths`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var authID, kind string
		if err := rows.Scan(&authID, &kind); err != nil {
			t.Fatal(err)
		}
		if kind != want[authID] {
			t.Fatalf("auth %q kind = %q, want %q", authID, kind, want[authID])
		}
		delete(want, authID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Fatalf("missing migrated rows: %+v", want)
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_invalid_auths_source_kind_active'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatalf("source-kind index count = %d, want 1", indexes)
	}
}

func TestRuntimeInvalidAuthDoesNotAffectSameEmailSibling(t *testing.T) {
	invalid := invalidAuthRow{
		AuthID: "runtime-id", AuthIndex: "runtime-index", Source: "same@example.com",
		AuthSourceKind: authSourceKindRuntimeOnly, LastStatusCode: http.StatusUnauthorized,
	}
	accounts := []accountRow{
		{AuthID: "file-id", AuthIndex: "file-index", AuthFile: "file.json", Email: "same@example.com"},
		{AuthID: "runtime-id", AuthIndex: "runtime-index", Email: "same@example.com"},
	}
	applyInvalidAuths(accounts, []invalidAuthRow{invalid})
	if accounts[0].InvalidAuth {
		t.Fatalf("same-email physical sibling was marked invalid: %+v", accounts[0])
	}
	if !accounts[1].InvalidAuth {
		t.Fatalf("matching runtime account was not marked invalid: %+v", accounts[1])
	}

	physical := configuredAccount{AuthID: "file-id", AuthIndex: "file-index", AuthFile: "file.json", Email: "same@example.com"}
	runtime := configuredAccount{AuthID: "runtime-id", AuthIndex: "runtime-index", Email: "same@example.com", AuthSourceKind: authSourceKindRuntimeOnly}
	if configuredMatchesInvalidAuth(physical, []invalidAuthRow{invalid}) {
		t.Fatal("quota trigger treated same-email physical sibling as invalid")
	}
	if !configuredMatchesInvalidAuth(runtime, []invalidAuthRow{invalid}) {
		t.Fatal("quota trigger did not match the exact runtime identity")
	}

	physicalCandidate := schedulerAuthCandidate{ID: "file-id", Provider: "codex", Attributes: map[string]string{"auth_index": "file-index", "auth_file": "file.json", "email": "same@example.com"}}
	runtimeCandidate := schedulerAuthCandidate{ID: "runtime-id", Provider: "codex", Attributes: map[string]string{"auth_index": "runtime-index", "email": "same@example.com"}}
	if candidateMatchesInvalidAuth(physicalCandidate, []invalidAuthRow{invalid}) {
		t.Fatal("scheduler blocked same-email physical sibling")
	}
	if !candidateMatchesInvalidAuth(runtimeCandidate, []invalidAuthRow{invalid}) {
		t.Fatal("scheduler did not block the exact runtime identity")
	}
	available, filtered, filteredCount, _, matchedInvalids := filterCodexSchedulerCandidates(
		[]schedulerAuthCandidate{physicalCandidate, runtimeCandidate}, nil, []invalidAuthRow{invalid},
	)
	if !filtered || filteredCount != 1 || len(available) != 1 || available[0].ID != "file-id" || len(matchedInvalids) != 1 {
		t.Fatalf("scheduler filter result available=%+v filtered=%v count=%d matched=%v", available, filtered, filteredCount, matchedInvalids)
	}
}

func TestFileInvalidAuthMatchesFilesystemShapedCandidateByAuthFile(t *testing.T) {
	invalid := invalidAuthRow{
		AuthID: "stable-host-id", AuthIndex: "opaque-host-index", AuthFile: "file.json",
		AuthSourceKind: authSourceKindFile, LastStatusCode: http.StatusUnauthorized,
	}
	filesystem := configuredAccount{
		AuthID: "same@example.com", AuthIndex: "file.json", AuthFile: "file.json", Email: "same@example.com", AuthSourceKind: authSourceKindFile,
	}
	if !configuredMatchesInvalidAuth(filesystem, []invalidAuthRow{invalid}) {
		t.Fatal("quota trigger did not match host-recorded 401 to filesystem-shaped credential")
	}
	mixedInventory := []configuredAccount{
		filesystem,
		{AuthID: "unrelated-stable-id", AuthIndex: "unrelated-index", AuthFile: "other.json", AuthSourceKind: authSourceKindFile},
	}
	match, ok := matchCodexHostAuthInventoryExact(invalid, mixedInventory)
	if !ok || match.AuthFile != "file.json" {
		t.Fatalf("mixed filesystem/stable inventory match = %+v, %v", match, ok)
	}
	accounts := []accountRow{
		{AuthID: "same@example.com", AuthIndex: "file.json", AuthFile: "file.json", Email: "same@example.com"},
		{AuthID: "unrelated-stable-id", AuthIndex: "unrelated-index", AuthFile: "other.json"},
	}
	applyInvalidAuths(accounts, []invalidAuthRow{invalid})
	if !accounts[0].InvalidAuth {
		t.Fatal("summary did not apply host-recorded 401 to filesystem-shaped account")
	}
	if accounts[1].InvalidAuth {
		t.Fatal("summary applied file 401 to unrelated stable sibling")
	}
}

func TestFileInvalidAuthMatchesStableSchedulerIDWithFilenameAuthIndex(t *testing.T) {
	invalid := invalidAuthRow{
		AuthID: "stable-host-id", AuthIndex: "stable-host-index", AuthFile: "file.json",
		AuthSourceKind: authSourceKindFile, LastStatusCode: http.StatusUnauthorized,
	}
	candidate := schedulerAuthCandidate{
		ID: "stable-host-id", Provider: "codex", Attributes: map[string]string{
			"auth_index": "file.json",
			"auth_file":  "file.json",
		},
	}
	if !candidateMatchesInvalidAuth(candidate, []invalidAuthRow{invalid}) {
		t.Fatal("scheduler candidate did not match the same stable ID and physical file")
	}
}

func TestClassifyAndFilterInvalidAuthRowsUsesExactHostInventory(t *testing.T) {
	inventory := []configuredAccount{
		{AuthID: "file-id", AuthIndex: "file-index", AuthFile: "file.json", AuthSourceKind: authSourceKindFile},
		{AuthID: "runtime-id", AuthIndex: "runtime-index", AuthSourceKind: authSourceKindRuntimeOnly},
	}
	rows := []invalidAuthRow{
		{AuthID: "file-id", AuthIndex: "file-index", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "runtime-id", AuthIndex: "runtime-index", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "hidden-runtime-id", AuthIndex: "hidden-runtime-index", AuthSourceKind: authSourceKindRuntimeOnly, LastStatusCode: http.StatusUnauthorized},
		{AuthID: "missing-id", AuthFile: "missing.json", LastStatusCode: http.StatusUnauthorized},
		{Source: "duplicate@example.com", LastStatusCode: http.StatusUnauthorized},
	}
	rows = classifyInvalidAuthRows(rows, inventory)
	if rows[0].AuthSourceKind != authSourceKindFile || rows[1].AuthSourceKind != authSourceKindRuntimeOnly {
		t.Fatalf("classified rows = %+v", rows)
	}
	if rows[2].AuthSourceKind != authSourceKindRuntimeOnly || rows[3].AuthSourceKind != authSourceKindLegacy || rows[4].AuthSourceKind != authSourceKindLegacy {
		t.Fatalf("inferred stale rows = %+v", rows)
	}
	filtered := filterMissingInvalidAuthRows(rows, inventory, true)
	if len(filtered) != 3 || filtered[0].AuthID != "file-id" || filtered[1].AuthID != "runtime-id" || filtered[2].AuthID != "hidden-runtime-id" {
		t.Fatalf("filtered rows = %+v, want exact file plus visible and hidden runtime records", filtered)
	}
}

func TestInvalidAuthSummaryFilters401And403Separately(t *testing.T) {
	rows := []invalidAuthRow{
		{AuthID: "unauthorized", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "payment", LastStatusCode: http.StatusPaymentRequired},
		{AuthID: "forbidden", LastStatusCode: http.StatusForbidden},
		{AuthID: "unknown", LastStatusCode: 0},
	}
	unauthorized := filterUnauthorizedInvalidAuths(rows)
	if len(unauthorized) != 1 || unauthorized[0].AuthID != "unauthorized" {
		t.Fatalf("401 rows = %+v", unauthorized)
	}
	forbidden := filterForbiddenInvalidAuths(rows)
	if len(forbidden) != 1 || forbidden[0].AuthID != "forbidden" {
		t.Fatalf("403 rows = %+v", forbidden)
	}
	var account accountRow
	applyInvalidAuthToAccount(&account, forbidden[0])
	if !account.InvalidAuth || account.InvalidAuthStatusCode != http.StatusForbidden {
		t.Fatalf("403 account state = %+v", account)
	}
}

func TestActiveInvalidAuthQueryAndSchedulerCoverPoolsLargerThanTwoThousand(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`
INSERT INTO invalid_auths (
  auth_id,auth_index,source,provider,reason,invalidated_at,active,
  last_status_code,auth_file,auth_source_kind
) VALUES (?,?,?,?,?,?,1,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2105; i++ {
		authID := fmt.Sprintf("inactive-%04d", i)
		authIndex := fmt.Sprintf("inactive-index-%04d", i)
		authFile := fmt.Sprintf("inactive-%04d.json", i)
		if i == 0 {
			authID = "target-id"
			authIndex = "target-host-index"
			authFile = "target.json"
		}
		if _, err := stmt.Exec(authID, authIndex, authFile, "codex", "401", int64(i), http.StatusUnauthorized, authFile, authSourceKindFile); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	invalids, err := queryActiveInvalidAuths(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalids) != 2105 {
		t.Fatalf("active invalid auth count = %d, want 2105", len(invalids))
	}
	available, filtered, filteredCount, _, matched := filterCodexSchedulerCandidates([]schedulerAuthCandidate{{
		ID: "target-id", Provider: "codex", Attributes: map[string]string{
			"auth_index": "target.json",
			"auth_file":  "target.json",
		},
	}}, nil, invalids)
	matchedTarget := false
	for index := range matched {
		if index >= 0 && index < len(invalids) && invalids[index].AuthID == "target-id" {
			matchedTarget = true
		}
	}
	if !filtered || filteredCount != 1 || len(available) != 0 || len(matched) != 1 || !matchedTarget {
		t.Fatalf("large-pool scheduler result available=%+v filtered=%v count=%d matched=%+v", available, filtered, filteredCount, matched)
	}
}

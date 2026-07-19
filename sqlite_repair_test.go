package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummaryAutoRepairsInvalidIndexRootpage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db, dbPath, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO quota_activation_jobs (
		job_id, job_type, state, created_at, updated_at
	) VALUES ('rootpage-regression', 'probe', 'pending', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	s.close()

	corruptSQLiteIndexRootpage(t, dbPath, "idx_quota_activation_jobs_state")
	assertInvalidRootpage(t, dbPath, "idx_quota_activation_jobs_state")

	manager := &summaryPrecomputeManager{}
	data, err := manager.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatalf("Summary did not recover from invalid index rootpage: %v", err)
	}
	if data == nil {
		t.Fatal("Summary returned nil data after automatic repair")
	}

	repairedDB, _, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	problems, err := sqliteIntegrityProblems(ctx, repairedDB, 0)
	if err != nil {
		t.Fatalf("integrity_check after automatic repair: %v", err)
	}
	if !sqliteIntegrityOK(problems) {
		t.Fatalf("integrity_check after automatic repair = %v, want [ok]", problems)
	}
	var rootpage, pageCount int64
	if err := repairedDB.QueryRowContext(ctx, `
SELECT rootpage FROM sqlite_schema
WHERE type='index' AND name='idx_quota_activation_jobs_state'`).Scan(&rootpage); err != nil {
		t.Fatalf("recreated index missing: %v", err)
	}
	if err := repairedDB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if rootpage <= 0 || rootpage > pageCount {
		t.Fatalf("recreated index rootpage=%d, page_count=%d", rootpage, pageCount)
	}

	backups, err := filepath.Glob(dbPath + ".bak-auto-repair-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("automatic repair backups=%v, want exactly one", backups)
	}
	assertInvalidRootpage(t, backups[0], "idx_quota_activation_jobs_state")
}

func corruptSQLiteIndexRootpage(t *testing.T, dbPath string, indexName string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	var pageCount, schemaVersion int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA writable_schema=ON`); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`UPDATE sqlite_schema SET rootpage=? WHERE type='index' AND name=?`, pageCount+1000, indexName)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("corrupt index rootpage affected %d rows, want 1", affected)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA schema_version=%d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA writable_schema=OFF`); err != nil {
		t.Fatal(err)
	}
}

func assertInvalidRootpage(t *testing.T, dbPath string, indexName string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = sqliteIntegrityProblems(context.Background(), db, 0)
	if err == nil {
		t.Fatalf("database %q did not report invalid rootpage", dbPath)
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, strings.ToLower(indexName)) || !strings.Contains(message, "invalid rootpage") {
		t.Fatalf("database %q corruption error=%q, want index %q and invalid rootpage", dbPath, err, indexName)
	}
}

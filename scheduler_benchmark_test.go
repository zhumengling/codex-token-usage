package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func benchmarkSchedulerCandidates(count int) []schedulerAuthCandidate {
	candidates := make([]schedulerAuthCandidate, count)
	for i := range candidates {
		id := fmt.Sprintf("account-%04d", i)
		candidates[i] = schedulerAuthCandidate{
			ID:         id,
			Provider:   "codex",
			Priority:   1,
			Attributes: map[string]string{"auth_index": id, "source": id},
		}
	}
	return candidates
}

func BenchmarkSchedulerHealthyFastPath100Accounts(b *testing.B) {
	resetSchedulerStateForTest()
	globalSchedulerState.setRestricted("codex", false)
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = false
	globalAccountProtection.configure(cfg)
	s := &store{}
	globalStore = s
	b.Cleanup(func() {
		s.close()
		globalStore = &store{}
		resetSchedulerStateForTest()
	})
	req := schedulerPickRequest{Provider: "codex", Model: "gpt-5.5", Candidates: benchmarkSchedulerCandidates(100)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.pickAuthOnce(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProtectedPick100Accounts50kEvents(b *testing.B) {
	dir := b.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("CPA_TOKEN_USAGE_DIR", dir)
	b.Setenv("CPA_AUTH_DIR", authDir)
	s := &store{}
	b.Cleanup(s.close)
	db, _, err := s.open(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	candidates := benchmarkSchedulerCandidates(100)
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_events
		(requested_at, provider, auth_id, auth_index, source, total_tokens)
		VALUES (?, 'codex', ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 50_000; i++ {
		account := candidates[i%len(candidates)].ID
		if _, err := stmt.Exec(now-int64(i%300), account, account, account, 1000); err != nil {
			b.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	cfg := defaultPluginConfig()
	cfg.AccountProtectionEnabled = true
	globalAccountProtection.configure(cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.pickProtectedAuth(context.Background(), db, candidates, cfg, "benchmark"); err != nil {
			b.Fatal(err)
		}
	}
}

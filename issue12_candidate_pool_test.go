package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCountMissingHealthySchedulerAccounts(t *testing.T) {
	now := time.Now().Unix()
	candidates := []schedulerAuthCandidate{{ID: "blocked.json", Provider: "codex", Attributes: map[string]string{"path": "/auth/blocked.json"}}}
	inventory := []configuredAccount{
		{AuthID: "blocked.json", AuthIndex: "blocked", AuthFile: "blocked.json", Provider: "codex", AuthSourceKind: authSourceKindFile, RuntimeRegistered: true},
		{AuthID: "healthy.json", AuthIndex: "healthy", AuthFile: "healthy.json", Provider: "codex", Email: "same@example.com", AuthSourceKind: authSourceKindFile, RuntimeRegistered: true},
		{AuthID: "candidate-email.json", AuthIndex: "candidate-email", AuthFile: "candidate-email.json", Provider: "codex", Email: "same@example.com", AuthSourceKind: authSourceKindFile, RuntimeRegistered: true},
		{AuthID: "disabled.json", AuthIndex: "disabled", AuthFile: "disabled.json", Provider: "codex", AuthSourceKind: authSourceKindFile, Disabled: true, RuntimeRegistered: true},
	}
	candidates = append(candidates, schedulerAuthCandidate{ID: "candidate-email.json", Provider: "codex", Attributes: map[string]string{"path": "/auth/candidate-email.json", "email": "same@example.com"}})
	bans := []autobanRow{{AuthID: "blocked.json", AuthIndex: "blocked", AuthFile: "blocked.json", Provider: "codex", Active: true, ResetAt: now + 3600}}
	if got := countMissingHealthySchedulerAccounts(candidates, inventory, bans, nil); got != 1 {
		t.Fatalf("missing healthy accounts=%d, want 1", got)
	}
}

func TestCodexSchedulerReportsStaleCandidatePoolWithoutSelectingUnknownAuth(t *testing.T) {
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, errors.New("unsupported host callback")
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{
			{ID: "blocked.json", AuthIndex: "blocked", Name: "blocked.json", Path: "/auth/blocked.json", Provider: "codex"},
			{ID: "healthy.json", AuthIndex: "healthy", Name: "healthy.json", Path: "/auth/healthy.json", Provider: "codex"},
		}})
	})
	oldDiagnostics := globalSchedulerDiagnostics
	globalSchedulerDiagnostics = &schedulerDiagnosticsTracker{}
	t.Cleanup(func() { globalSchedulerDiagnostics = oldDiagnostics })
	globalSchedulerAffinity.reset()
	t.Cleanup(globalSchedulerAffinity.reset)
	oldProtection := globalAccountProtection.config()
	globalAccountProtection.configure(defaultPluginConfig())
	t.Cleanup(func() { globalAccountProtection.configure(oldProtection) })
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO autoban_bans (auth_id, auth_index, provider, window, reason, banned_at, reset_at, active, last_status_code, auth_file)
VALUES ('blocked.json', 'blocked', 'codex', '5h', 'test', ?, ?, 1, 429, 'blocked.json')`, now, now+3600); err != nil {
		t.Fatal(err)
	}
	request := schedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-test",
		Options: schedulerOptions{Headers: map[string][]string{
			"Session-Id": {"stale-pool-session"},
		}},
		Candidates: []schedulerAuthCandidate{{
			ID: "blocked.json", Provider: "codex", Priority: 10,
			Attributes: map[string]string{"path": "/auth/blocked.json"},
		}},
	}
	affinityKey := schedulerAffinityKey(request, "codex")
	globalSchedulerAffinity.bind(affinityKey, "blocked.json")
	_, err = s.pickAuth(context.Background(), request)
	var reject *schedulerRejectError
	if !errors.As(err, &reject) || reject.Code != "auth_unavailable" {
		t.Fatalf("pick error=%v, want auth_unavailable", err)
	}
	if !strings.Contains(reject.Message, "omitted 1 healthy registered Codex account") {
		t.Fatalf("reject message=%q, want stale candidate pool detail", reject.Message)
	}
	status := globalSchedulerDiagnostics.status(1)
	if !status.CandidatePoolStale || status.RequestCandidates != 1 || status.MissingHealthyAccounts != 1 || status.CandidateHighestPriority != 10 {
		t.Fatalf("scheduler diagnostics=%+v, want stale candidate pool", status)
	}
	if _, ok := globalSchedulerAffinity.pick(affinityKey, request.Candidates); ok {
		t.Fatal("stale blocked Session affinity remained bound after every candidate was filtered")
	}
}

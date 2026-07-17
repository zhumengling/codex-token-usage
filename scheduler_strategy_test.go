package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCPASchedulerStrategy(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want cpaSchedulerStrategy
	}{
		{name: "fill first", raw: "routing:\n  strategy: fill-first\n", want: cpaSchedulerFillFirst},
		{name: "round robin", raw: "routing:\n  strategy: round-robin\n", want: cpaSchedulerRoundRobin},
		{name: "quoted with comment", raw: "routing:\n  strategy: 'fill-first' # configured by CPA\n", want: cpaSchedulerFillFirst},
		{name: "flat compatibility", raw: "routing.strategy: fill_first\n", want: cpaSchedulerFillFirst},
		{name: "unknown fallback", raw: "routing:\n  strategy: custom\n", want: cpaSchedulerRoundRobin},
		{name: "missing fallback", raw: "port: 8317\n", want: cpaSchedulerRoundRobin},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseCPASchedulerStrategy(test.raw); got != test.want {
				t.Fatalf("strategy=%q, want %q", got, test.want)
			}
		})
	}
}

func TestCurrentCPASchedulerStrategyReloadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", path)
	resetCPASchedulerStrategyCache()
	t.Cleanup(resetCPASchedulerStrategyCache)
	if err := os.WriteFile(path, []byte("routing:\n  strategy: fill-first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := currentCPASchedulerStrategy(); got != cpaSchedulerFillFirst {
		t.Fatalf("initial strategy=%q, want fill-first", got)
	}
	if err := os.WriteFile(path, []byte("routing:\n  strategy: round-robin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got := currentCPASchedulerStrategy(); got != cpaSchedulerRoundRobin {
		t.Fatalf("reloaded strategy=%q, want round-robin", got)
	}
}

func TestSchedulerSelectionFollowsCPAStrategy(t *testing.T) {
	resetSchedulerSelectionState(t)
	candidates := []schedulerAuthCandidate{
		{ID: "z", Provider: "codex", Priority: 1},
		{ID: "a", Provider: "codex", Priority: 1},
	}
	if first := pickSchedulerCandidateWithStrategy("fill", "", cpaSchedulerFillFirst, candidates); first.ID != "a" {
		t.Fatalf("fill-first first=%q, want a", first.ID)
	}
	if second := pickSchedulerCandidateWithStrategy("fill", "", cpaSchedulerFillFirst, candidates); second.ID != "a" {
		t.Fatalf("fill-first second=%q, want a", second.ID)
	}
	if first := pickSchedulerCandidateWithStrategy("round", "", cpaSchedulerRoundRobin, candidates); first.ID != "a" {
		t.Fatalf("round-robin first=%q, want a", first.ID)
	}
	if second := pickSchedulerCandidateWithStrategy("round", "", cpaSchedulerRoundRobin, candidates); second.ID != "z" {
		t.Fatalf("round-robin second=%q, want z", second.ID)
	}
}

func TestSessionAffinityOverridesFillFirstWhileBindingIsAvailable(t *testing.T) {
	resetSchedulerSelectionState(t)
	request := affinityTestRequest("fill-affinity")
	key := schedulerAffinityKey(request, "codex")
	globalSchedulerAffinity.bind(key, "b")
	chosen := pickSchedulerCandidateWithStrategy(
		schedulerRotationKey(request, "codex"),
		key,
		cpaSchedulerFillFirst,
		affinityTestCandidates(),
	)
	if chosen.ID != "b" {
		t.Fatalf("affinity fill-first choice=%q, want b", chosen.ID)
	}
}

func TestCodexRestrictedSchedulerUsesFillFirstWhenAffinityDisabled(t *testing.T) {
	resetSchedulerSelectionState(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("routing:\n  strategy: fill-first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_TOKEN_USAGE_DIR", dir)
	t.Setenv("CPA_CONFIG_PATH", configPath)
	resetCPASchedulerStrategyCache()
	t.Cleanup(resetCPASchedulerStrategyCache)
	oldCfg := globalAccountProtection.config()
	cfg := defaultPluginConfig()
	cfg.SchedulerSessionAffinityEnabled = false
	globalAccountProtection.configure(cfg)
	t.Cleanup(func() { globalAccountProtection.configure(oldCfg) })
	s := &store{}
	t.Cleanup(s.close)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO autoban_bans (auth_id, auth_index, provider, window, reason, banned_at, reset_at, active, last_status_code)
VALUES ('blocked', 'blocked', 'codex', '5h', 'test', ?, ?, 1, 429)`, now, now+3600); err != nil {
		t.Fatal(err)
	}
	request := affinityTestRequest("ignored-session")
	request.Candidates = []schedulerAuthCandidate{
		{ID: "blocked", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "blocked"}},
		{ID: "b", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "b"}},
		{ID: "a", Provider: "codex", Priority: 1, Attributes: map[string]string{"auth_index": "a"}},
	}
	for i := 0; i < 2; i++ {
		response, err := s.pickAuth(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if !response.Handled || response.AuthID != "a" {
			t.Fatalf("fill-first response[%d]=%+v, want a", i, response)
		}
	}
}

func TestProtectedSelectionUsesFillFirstWithinEligibleTier(t *testing.T) {
	resetSchedulerSelectionState(t)
	states := []protectionCandidate{
		{Candidate: schedulerAuthCandidate{ID: "z", Priority: 1}, Limit: 5},
		{Candidate: schedulerAuthCandidate{ID: "a", Priority: 1}, Limit: 5},
	}
	for i := 0; i < 2; i++ {
		chosen, ok := chooseProtectedCandidateWithStrategy(states, "protected-fill", "", cpaSchedulerFillFirst)
		if !ok || chosen.Candidate.ID != "a" {
			t.Fatalf("protected fill-first[%d]=%+v ok=%v, want a", i, chosen, ok)
		}
	}
}

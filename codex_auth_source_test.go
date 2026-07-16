package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withCodexHostAuthSource(t *testing.T, caller hostCallFunc) {
	t.Helper()
	oldCaller := hostAuthCaller
	oldCodexSource := globalCodexAuthSource
	oldXAISource := globalXAIAuthSource
	hostAuthCaller = caller
	globalCodexAuthSource = &codexAuthSourceManager{}
	globalXAIAuthSource = &xaiAuthSourceManager{}
	t.Cleanup(func() {
		hostAuthCaller = oldCaller
		globalCodexAuthSource = oldCodexSource
		globalXAIAuthSource = oldXAISource
	})
}

func TestCodexHostAuthSourceShowsUnusedAccountsInSummary(t *testing.T) {
	updatedAt := "2026-07-15T08:00:00Z"
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{
			{ID: "a.json", AuthIndex: "index-a", Name: "a.json", Provider: "codex", Email: "a@example.com", ModTime: updatedAt},
			{ID: "b.json", AuthIndex: "index-b", Name: "b.json", Provider: "codex", Email: "b@example.com", ModTime: updatedAt},
			{ID: "xai.json", AuthIndex: "index-x", Name: "xai.json", Provider: "xai", Email: "x@example.com", ModTime: updatedAt},
		}})
	})
	s := newTestStore(t)
	data, err := s.summaryOnce(context.Background(), "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	accounts, ok := data["accounts"].([]accountRow)
	if !ok {
		t.Fatalf("accounts type = %T", data["accounts"])
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want two unused Codex accounts", accounts)
	}
	for _, account := range accounts {
		if !account.Configured || account.Requests != 0 || account.Provider != "codex" {
			t.Fatalf("unused host account = %+v, want configured zero-usage Codex account", account)
		}
	}
	status := globalCodexAuthSource.status()
	if !status.Authoritative || status.Source != "host_callback" || status.Accounts != 2 {
		t.Fatalf("Codex auth source status = %+v", status)
	}
}

func TestCodexHostAuthSourceKeepsInventoryButOnlyReturnsFileAccounts(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{
			{ID: "file-id", AuthIndex: "file-index", Name: "file.json", Provider: "codex", Email: "same@example.com"},
			{ID: "runtime-id", AuthIndex: "runtime-index", Provider: "codex", Email: "same@example.com", Source: "memory"},
			{ID: "legacy-id", AuthIndex: "legacy-index", Provider: "codex", Email: "legacy@example.com"},
			{ID: "xai-id", AuthIndex: "xai-index", Name: "xai.json", Provider: "xai"},
		}})
	})
	t.Setenv("CPA_AUTH_DIR", filepath.Join(t.TempDir(), "missing"))

	accounts := readConfiguredAuthAccounts()
	if len(accounts) != 1 || accounts[0].AuthID != "file-id" || accounts[0].AuthSourceKind != authSourceKindFile {
		t.Fatalf("configured accounts = %+v, want only the file-backed Codex account", accounts)
	}
	inventory, err := readCodexHostAuthInventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 3 {
		t.Fatalf("inventory = %+v, want all three Codex host entries", inventory)
	}
	kinds := map[string]string{}
	for _, account := range inventory {
		kinds[account.AuthID] = account.AuthSourceKind
	}
	if kinds["file-id"] != authSourceKindFile || kinds["runtime-id"] != authSourceKindRuntimeOnly || kinds["legacy-id"] != authSourceKindLegacy {
		t.Fatalf("inventory kinds = %+v", kinds)
	}
}

func TestCodexAuthSourceInvalidateRejectsInFlightStaleRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		calls++
		if calls == 1 {
			close(started)
			<-release
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
				ID: "old-id", AuthIndex: "old-index", Name: "old.json", Provider: "codex", Source: "file",
			}}})
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "new-id", AuthIndex: "new-index", Name: "new.json", Provider: "codex", Source: "file",
		}}})
	})
	manager := &codexAuthSourceManager{}
	type result struct {
		accounts []configuredAccount
		err      error
	}
	done := make(chan result, 1)
	go func() {
		accounts, err := manager.hostInventory()
		done <- result{accounts: accounts, err: err}
	}()
	<-started
	manager.invalidate()
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if calls < 2 || len(got.accounts) != 1 || got.accounts[0].AuthID != "new-id" {
		t.Fatalf("refresh calls=%d accounts=%+v, want fresh new-id inventory", calls, got.accounts)
	}
}

func TestMatchCodexHostAuthInventoryExactDoesNotGuessByEmail(t *testing.T) {
	inventory := []configuredAccount{
		{AuthID: "id-a", AuthIndex: "index-a", AuthFile: "a.json", Email: "same@example.com", AuthSourceKind: authSourceKindFile},
		{AuthID: "id-b", AuthIndex: "index-b", AuthFile: "b.json", Email: "same@example.com", AuthSourceKind: authSourceKindFile},
	}
	if _, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{Source: "same@example.com"}, inventory); ok {
		t.Fatal("email-only invalid auth unexpectedly matched a host credential")
	}
	match, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "id-b", Source: "same@example.com"}, inventory)
	if !ok || match.AuthFile != "b.json" {
		t.Fatalf("stable ID match = %+v, %v, want b.json", match, ok)
	}
	match, ok = matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "a.json"}, inventory)
	if !ok || match.AuthID != "id-a" {
		t.Fatalf("file identity match = %+v, %v, want id-a", match, ok)
	}
	match, ok = matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "same@example.com", AuthIndex: "index-b"}, inventory)
	if !ok || match.AuthID != "id-b" {
		t.Fatalf("email auth_id plus stable index match = %+v, %v, want id-b", match, ok)
	}
	if _, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "id-a", AuthIndex: "index-b"}, inventory); ok {
		t.Fatal("conflicting stable auth_id/auth_index unexpectedly matched a credential")
	}
	if _, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "stale-id", AuthFile: "b.json"}, inventory); ok {
		t.Fatal("stale stable auth_id unexpectedly fell back to a replacement file")
	}
}

func TestMatchCodexHostAuthInventoryExactAllowsFileFallbackWithoutStableHostID(t *testing.T) {
	inventory := []configuredAccount{{
		AuthID: "same@example.com", AuthIndex: "b.json", AuthFile: "b.json", Email: "same@example.com", AuthSourceKind: authSourceKindFile,
	}}
	match, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "unavailable-host-id", AuthIndex: "b.json", AuthFile: "b.json"}, inventory)
	if !ok || match.AuthFile != "b.json" {
		t.Fatalf("filesystem fallback match = %+v, %v, want b.json", match, ok)
	}
}

func TestMatchCodexHostAuthInventoryExactMapsFilesystemProbeToStableHostIndex(t *testing.T) {
	inventory := []configuredAccount{{
		AuthID: "host-file-id", AuthIndex: "stable-host-index", AuthFile: "probe-account.json", Email: "probe@example.com", AuthSourceKind: authSourceKindFile,
	}}
	match, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{
		AuthID: "probe@example.com", AuthIndex: "probe-account.json", AuthFile: "probe-account.json", Source: "probe@example.com",
	}, inventory)
	if !ok || match.AuthID != "host-file-id" || match.AuthIndex != "stable-host-index" {
		t.Fatalf("filesystem probe match = %+v, %v, want authoritative host identity", match, ok)
	}
}

func TestConfiguredCodexFileInventoryMatchesHostRecordedFileByFileName(t *testing.T) {
	inventory := configuredCodexFileInventory([]configuredAccount{{
		AuthID: "same@example.com", AuthIndex: "b.json", AuthFile: "b.json", Email: "same@example.com", Provider: "codex",
	}})
	match, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{
		AuthID: "stable-host-id", AuthIndex: "opaque-host-index", AuthFile: "b.json", AuthSourceKind: authSourceKindFile,
	}, inventory)
	if !ok || match.AuthFile != "b.json" {
		t.Fatalf("filesystem-only identity match = %+v, %v, want b.json", match, ok)
	}
}

func TestMatchCodexHostAuthInventoryExactAllowsRuntimeJSONStableIdentity(t *testing.T) {
	inventory := []configuredAccount{
		{AuthID: "file-id", AuthIndex: "file-index", AuthFile: "physical.json", AuthSourceKind: authSourceKindFile},
		{AuthID: "runtime.json", AuthIndex: "runtime-index", AuthSourceKind: authSourceKindRuntimeOnly},
	}
	match, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{
		AuthID: "runtime.json", AuthIndex: "runtime-index", AuthSourceKind: authSourceKindRuntimeOnly,
	}, inventory)
	if !ok || match.AuthSourceKind != authSourceKindRuntimeOnly {
		t.Fatalf("runtime .json stable identity match = %+v, %v", match, ok)
	}
	match, ok = matchCodexHostAuthInventoryExact(invalidAuthRow{
		AuthID: "runtime.json", AuthIndex: "runtime-index", AuthSourceKind: authSourceKindLegacy,
	}, inventory)
	if !ok || match.AuthSourceKind != authSourceKindRuntimeOnly {
		t.Fatalf("legacy runtime .json identity match = %+v, %v", match, ok)
	}
}

func TestMatchCodexHostAuthInventoryExactRequiresUniqueCandidate(t *testing.T) {
	inventory := []configuredAccount{
		{AuthID: "duplicate", AuthIndex: "same", AuthSourceKind: authSourceKindRuntimeOnly},
		{AuthID: "duplicate", AuthIndex: "same", AuthSourceKind: authSourceKindRuntimeOnly},
	}
	if _, ok := matchCodexHostAuthInventoryExact(invalidAuthRow{AuthID: "duplicate", AuthIndex: "same"}, inventory); ok {
		t.Fatal("duplicate stable identities unexpectedly produced a unique match")
	}
}

func TestCodexHostAuthSourceEmptyListRemovesHistoricalAccounts(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{})
	})
	s := newTestStore(t)
	if err := s.recordUsage(context.Background(), usageRecord{
		Provider: "codex", AuthID: "removed.json", AuthIndex: "removed", Source: "removed.json",
		RequestedAt: time.Now(), Detail: usageDetail{TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := s.summaryOnce(context.Background(), "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	if accounts := data["accounts"].([]accountRow); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want authoritative empty host list to remove history", accounts)
	}
}

func TestCodexHostAuthSourceFallsBackToFilesystem(t *testing.T) {
	withCodexHostAuthSource(t, func(string, any) (json.RawMessage, error) {
		return nil, errors.New("host callback unavailable")
	})
	dir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "fallback.json"), []byte(`{"type":"codex","email":"fallback@example.com"}`), 0600); err != nil {
		t.Fatal(err)
	}
	accounts := readConfiguredAuthAccounts()
	if len(accounts) != 1 || accounts[0].AuthFile != "fallback.json" {
		t.Fatalf("accounts = %+v, want filesystem fallback", accounts)
	}
	status := globalCodexAuthSource.status()
	if !status.Authoritative || status.Source != "filesystem_fallback" {
		t.Fatalf("Codex auth source status = %+v", status)
	}
}

func TestCodexHostAuthListChangeInvalidatesSummaryRevisionWithoutUsage(t *testing.T) {
	files := []hostAuthFileEntry{{ID: "a.json", AuthIndex: "a", Name: "a.json", Provider: "codex"}}
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: files})
	})
	t.Setenv("CPA_AUTH_DIR", filepath.Join(t.TempDir(), "missing"))
	first := authFilesRevision()
	files = append(files, hostAuthFileEntry{ID: "b.json", AuthIndex: "b", Name: "b.json", Provider: "codex"})
	globalCodexAuthSource.mu.Lock()
	globalCodexAuthSource.fetchedAt = time.Time{}
	globalCodexAuthSource.mu.Unlock()
	second := authFilesRevision()
	if first == second {
		t.Fatalf("auth revision stayed %q after an unused host account was added", first)
	}
}

func TestConfiguredAuthDirUsesDefaultAndExpandsHome(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_AUTH_DIR", "")
	t.Setenv("CPA_CONFIG_PATH", configPath)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".cli-proxy-api")
	if got := configuredAuthDir(); got != want {
		t.Fatalf("configuredAuthDir() = %q, want %q", got, want)
	}
}

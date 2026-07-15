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

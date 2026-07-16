package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func withUserHomeDir(t *testing.T, home string, err error) {
	t.Helper()
	previous := userHomeDir
	userHomeDir = func() (string, error) {
		return home, err
	}
	t.Cleanup(func() {
		userHomeDir = previous
	})
}

func withCurrentWorkingDir(t *testing.T, dir string, err error) {
	t.Helper()
	previous := currentWorkingDir
	currentWorkingDir = func() (string, error) {
		return dir, err
	}
	t.Cleanup(func() {
		currentWorkingDir = previous
	})
}

func clearPathOverrides(t *testing.T) {
	t.Helper()
	t.Setenv("CPA_TOKEN_USAGE_DIR", "")
	t.Setenv("CPA_MODEL_PRICE_FILE", "")
	t.Setenv("CPA_CONFIG_PATH", "")
	t.Setenv("CPA_CONFIG_FILE", "")
}

func TestDefaultPathsUseCurrentUserHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "non-root-user")
	withUserHomeDir(t, home, nil)
	clearPathOverrides(t)

	dataDir := filepath.Join(home, ".cli-proxy-api", "plugins", "codex-token-usage")
	dbPath, err := usageDBPath()
	if err != nil {
		t.Fatalf("usageDBPath() error = %v", err)
	}
	if want := filepath.Join(dataDir, "usage.db"); dbPath != want {
		t.Fatalf("usageDBPath() = %q, want %q", dbPath, want)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("plugin data directory was not created: info=%v err=%v", info, err)
	}
	if want := filepath.Join(dataDir, "model_prices.json"); modelPriceFilePath() != want {
		t.Fatalf("modelPriceFilePath() = %q, want %q", modelPriceFilePath(), want)
	}
	if want := filepath.Join(home, ".cli-proxy-api", "config.yaml"); configuredConfigPath() != want {
		t.Fatalf("configuredConfigPath() = %q, want %q", configuredConfigPath(), want)
	}
}

func TestStoreOpenInitializesSQLiteUnderCurrentUserHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "service-user")
	withUserHomeDir(t, home, nil)
	clearPathOverrides(t)

	s := &store{}
	t.Cleanup(s.close)
	db, path, err := s.open(context.Background())
	if err != nil {
		t.Fatalf("store.open() error = %v", err)
	}
	want := filepath.Join(home, ".cli-proxy-api", "plugins", "codex-token-usage", "usage.db")
	if path != want {
		t.Fatalf("store.open() path = %q, want %q", path, want)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("SQLite ping failed: %v", err)
	}
	if info, err := os.Stat(want); err != nil || info.IsDir() {
		t.Fatalf("SQLite file was not created: info=%v err=%v", info, err)
	}
}

func TestPathEnvironmentOverridesTakePrecedence(t *testing.T) {
	withUserHomeDir(t, filepath.Join(t.TempDir(), "home"), nil)
	dataDir := filepath.Join(t.TempDir(), "custom-data")
	pricePath := filepath.Join(t.TempDir(), "custom-prices.json")
	configPath := filepath.Join(t.TempDir(), "custom-config.yaml")
	t.Setenv("CPA_TOKEN_USAGE_DIR", dataDir)
	t.Setenv("CPA_MODEL_PRICE_FILE", pricePath)
	t.Setenv("CPA_CONFIG_FILE", filepath.Join(t.TempDir(), "legacy-config.yaml"))
	t.Setenv("CPA_CONFIG_PATH", configPath)

	dbPath, err := usageDBPath()
	if err != nil {
		t.Fatalf("usageDBPath() error = %v", err)
	}
	if want := filepath.Join(dataDir, "usage.db"); dbPath != want {
		t.Fatalf("usageDBPath() = %q, want %q", dbPath, want)
	}
	if got := modelPriceFilePath(); got != pricePath {
		t.Fatalf("modelPriceFilePath() = %q, want %q", got, pricePath)
	}
	if got := configuredConfigPath(); got != configPath {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, configPath)
	}
}

func TestDefaultModelPricePathFollowsPluginDataOverride(t *testing.T) {
	withUserHomeDir(t, filepath.Join(t.TempDir(), "home"), nil)
	dataDir := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv("CPA_TOKEN_USAGE_DIR", dataDir)
	t.Setenv("CPA_MODEL_PRICE_FILE", "")

	if want := filepath.Join(dataDir, "model_prices.json"); modelPriceFilePath() != want {
		t.Fatalf("modelPriceFilePath() = %q, want %q", modelPriceFilePath(), want)
	}
}

func TestConfiguredConfigPathSupportsLegacyEnvironmentOverride(t *testing.T) {
	withUserHomeDir(t, filepath.Join(t.TempDir(), "home"), nil)
	legacyPath := filepath.Join(t.TempDir(), "legacy-config.yaml")
	t.Setenv("CPA_CONFIG_PATH", "")
	t.Setenv("CPA_CONFIG_FILE", legacyPath)

	if got := configuredConfigPath(); got != legacyPath {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, legacyPath)
	}
}

func TestConfiguredConfigPathUsesCPAProcessConfigFlag(t *testing.T) {
	home := t.TempDir()
	withUserHomeDir(t, home, nil)
	clearPathOverrides(t)

	configPath := filepath.Join(t.TempDir(), "custom", "config.yaml")
	for _, args := range [][]string{
		{"-config", configPath},
		{"--config", configPath},
		{"-config=" + configPath},
		{"--config=" + configPath},
	} {
		if got := configuredConfigPathForArgs(args); got != configPath {
			t.Fatalf("configuredConfigPathForArgs(%q) = %q, want %q", args, got, configPath)
		}
	}
}

func TestConfiguredConfigPathEnvironmentOverrideWinsOverProcessFlag(t *testing.T) {
	withUserHomeDir(t, t.TempDir(), nil)
	overridePath := filepath.Join(t.TempDir(), "override.yaml")
	processPath := filepath.Join(t.TempDir(), "process.yaml")
	t.Setenv("CPA_CONFIG_PATH", overridePath)
	t.Setenv("CPA_CONFIG_FILE", "")

	if got := configuredConfigPathForArgs([]string{"-config", processPath}); got != overridePath {
		t.Fatalf("configuredConfigPathForArgs() = %q, want environment override %q", got, overridePath)
	}
}

func TestConfiguredConfigPathPrefersCPAWorkingDirectoryConfig(t *testing.T) {
	home := t.TempDir()
	workingDir := t.TempDir()
	withUserHomeDir(t, home, nil)
	withCurrentWorkingDir(t, workingDir, nil)
	clearPathOverrides(t)

	canonicalConfig := filepath.Join(home, ".cli-proxy-api", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(canonicalConfig), 0755); err != nil {
		t.Fatalf("create canonical config directory: %v", err)
	}
	if err := os.WriteFile(canonicalConfig, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatalf("write canonical config: %v", err)
	}
	legacyConfig := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(legacyConfig, []byte("port: 8318\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	workingConfig := filepath.Join(workingDir, "config.yaml")
	if err := os.WriteFile(workingConfig, []byte("port: 8319\n"), 0600); err != nil {
		t.Fatalf("write working-directory config: %v", err)
	}

	if got := configuredConfigPath(); got != workingConfig {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, workingConfig)
	}
}

func TestConfiguredConfigPathPrefersCanonicalCPAConfigWithoutWorkingConfig(t *testing.T) {
	home := t.TempDir()
	withUserHomeDir(t, home, nil)
	withCurrentWorkingDir(t, t.TempDir(), nil)
	clearPathOverrides(t)

	canonicalConfig := filepath.Join(home, ".cli-proxy-api", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(canonicalConfig), 0755); err != nil {
		t.Fatalf("create canonical config directory: %v", err)
	}
	if err := os.WriteFile(canonicalConfig, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatalf("write canonical config: %v", err)
	}

	if got := configuredConfigPath(); got != canonicalConfig {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, canonicalConfig)
	}
}

func TestCommandLineConfigPathStopsAtDoubleDash(t *testing.T) {
	before := filepath.Join(t.TempDir(), "before.yaml")
	after := filepath.Join(t.TempDir(), "after.yaml")

	if got := commandLineConfigPath([]string{"--config", before, "--", "--config", after}); got != before {
		t.Fatalf("commandLineConfigPath() = %q, want %q", got, before)
	}
	if got := commandLineConfigPath([]string{"--", "--config", after}); got != "" {
		t.Fatalf("commandLineConfigPath() = %q, want empty path after --", got)
	}
}

func TestConfiguredConfigPathFallsBackToExistingHomeConfig(t *testing.T) {
	home := t.TempDir()
	withUserHomeDir(t, home, nil)
	withCurrentWorkingDir(t, t.TempDir(), nil)
	clearPathOverrides(t)

	legacyConfig := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(legacyConfig, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	if got := configuredConfigPath(); got != legacyConfig {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, legacyConfig)
	}
}

func TestConfiguredConfigPathFallsBackToExistingWorkingDirectoryConfig(t *testing.T) {
	home := t.TempDir()
	workingDir := t.TempDir()
	withUserHomeDir(t, home, nil)
	withCurrentWorkingDir(t, workingDir, nil)
	clearPathOverrides(t)

	workingConfig := filepath.Join(workingDir, "config.yaml")
	if err := os.WriteFile(workingConfig, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatalf("write working-directory config: %v", err)
	}
	if got := configuredConfigPath(); got != workingConfig {
		t.Fatalf("configuredConfigPath() = %q, want %q", got, workingConfig)
	}
}

func TestUsageDBPathReportsHomeResolutionFailure(t *testing.T) {
	withUserHomeDir(t, "", errors.New("home unavailable"))
	clearPathOverrides(t)

	if _, err := usageDBPath(); err == nil {
		t.Fatal("usageDBPath() error = nil, want home resolution failure")
	}
	if got := modelPriceFilePath(); got != "" {
		t.Fatalf("modelPriceFilePath() = %q, want empty path after home resolution failure", got)
	}
	if got := configuredConfigPath(); got != "" {
		t.Fatalf("configuredConfigPath() = %q, want empty path after home resolution failure", got)
	}
}

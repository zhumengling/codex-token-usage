package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyModelPriceFileMigratesToCache(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CPA_TOKEN_USAGE_DIR", dataDir)
	t.Setenv("CPA_MODEL_PRICE_FILE", "")
	legacyPath := filepath.Join(dataDir, legacyModelPriceJSONFileName)
	want := []byte(`{"gpt-test":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`)
	if err := os.WriteFile(legacyPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyModelPriceFile(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, modelPriceCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("migrated content = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy JSON still exists: %v", err)
	}
}

func TestLegacyModelPriceMigrationDoesNotTouchExplicitPath(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "custom-prices.json")
	t.Setenv("CPA_TOKEN_USAGE_DIR", dataDir)
	t.Setenv("CPA_MODEL_PRICE_FILE", explicit)
	legacyPath := filepath.Join(dataDir, legacyModelPriceJSONFileName)
	if err := os.WriteFile(legacyPath, []byte(`{"legacy":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyModelPriceFile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("explicit override unexpectedly changed plugin legacy file: %v", err)
	}
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Fatalf("explicit path unexpectedly created: %v", err)
	}
}

func TestLegacyModelPriceMigrationRemovesDuplicateJSON(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CPA_TOKEN_USAGE_DIR", dataDir)
	t.Setenv("CPA_MODEL_PRICE_FILE", "")
	legacyPath := filepath.Join(dataDir, legacyModelPriceJSONFileName)
	cachePath := filepath.Join(dataDir, modelPriceCacheFileName)
	if err := os.WriteFile(legacyPath, []byte(`{"old":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"new":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyModelPriceFile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate legacy JSON still exists: %v", err)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil || string(got) != `{"new":{}}` {
		t.Fatalf("cache was not preserved: content=%q err=%v", got, err)
	}
}

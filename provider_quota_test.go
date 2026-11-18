package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderInventoryPrefersCPA8Groups(t *testing.T) {
	inventory := parseProviderInventory([]byte(`
config-version: 8
auth-dir: /legacy/auth
oauth: {auth-dir: /current/auth}
codex-api-key: [{base-url: 'https://deprecated.example', api-key: obsolete}]
api-keys:
  codex:
  - name: Codex Supplier
    base-url: https://upstream.example:8080/api
    proxy-url: direct
    prefix: /dev/
    headers: {X-First: 'first # value'}
    connection-pool: {enabled: true, idle-conn-timeout: 1m}
    keys:
    - api-key: key-shared
      connection-pool: {max-idle-conns-per-host: 3}
    - api-key: key-shared
      proxy-url: different
  openai-compatibility:
  - name: Existing OpenAI
    base-url: https://compatible.example
    keys: [{api-key: compatible-key}]
  - name: Hidden
    disabled: true
    keys: []
`))
	if inventory.status != "ok" || inventory.authDir != "/current/auth" || len(inventory.entries) != 3 {
		t.Fatalf("status=%s directory=%s endpoints=%d", inventory.status, inventory.authDir, len(inventory.entries))
	}
	first, second := inventory.entries[1], inventory.entries[2]
	if first.Name != "Codex · Codex Supplier" || second.Name != first.Name+" #2" || first.AuthID == second.AuthID || first.AuthIndex == second.AuthIndex {
		t.Fatal("Codex credentials with a shared endpoint label were not separated")
	}
	flat := parseProviderInventory([]byte(`codex-api-key:
  - {api-key: key-shared, base-url: 'https://upstream.example:8080/api', prefix: dev, proxy-url: direct,
     headers: {X-First: 'first # value'}, connection-pool: {enabled: true, idle-conn-timeout: 1m, max-idle-conns-per-host: 3}}
`))
	if len(flat.entries) != 1 || flat.entries[0].AuthID != first.AuthID || flat.entries[0].AuthIndex != first.AuthIndex {
		t.Fatal("grouped and flattened credentials resolve to different identities")
	}
	legacy := parseProviderInventory([]byte("api-keys: [inbound]\ncodex-api-key:\n- {api-key: legacy, base-url: 'https://old.example'}\n"))
	if legacy.status != "ok" || len(legacy.entries) != 1 || legacy.entries[0].Name != "Codex · old.example" {
		t.Fatal("legacy flat profile was not recognized")
	}
}

func TestProviderQuotaAndUsageStayOnExactCredential(t *testing.T) {
	const sharedKey, clientKey = "shared-upstream-fixture-key", "fixture-inbound-secret"
	config := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(config, []byte(`
api-keys:
  codex:
  - {name: First, base-url: 'https://first.example', keys: [{api-key: shared-upstream-fixture-key}]}
  - {name: Second, base-url: 'https://second.example', keys: [{api-key: shared-upstream-fixture-key}]}
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_CONFIG_PATH", config)
	entries := readConfiguredProviderEntries()
	if len(entries) != 2 {
		t.Fatalf("expected two configured Codex credentials, got %d", len(entries))
	}
	first, second := entries[0], entries[1]
	ctx, usageStore := context.Background(), newTestStore(t)
	now := time.Now().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	base := usageRecord{Provider: "codex", AuthType: "apikey", Source: sharedKey, APIKey: clientKey, RequestedAt: now, Model: "gpt-5.5"}
	primary, secondary, missing, ambiguous, exactHistory, oauth, payment, oldIdentity := base, base, base, base, base, base, base, base
	primary.AuthID, primary.AuthIndex = first.AuthID, second.AuthIndex
	primary.RequestedAt = old
	primary.Detail = usageDetail{InputTokens: 5, CacheCreation5mTokens: 3, CacheCreation1hTokens: 4, TotalTokens: 5}
	primary.ResponseHeaders = map[string][]string{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Reset-After-Seconds": {"60"}}
	secondary.AuthID, secondary.Failed, secondary.Failure.StatusCode = first.AuthID, true, http.StatusTooManyRequests
	secondary.ResponseHeaders = map[string][]string{"x-codex-secondary-used-percent": {"98.5"}}
	missing.AuthID, missing.Detail.TotalTokens = second.AuthID, 9
	exactHistory.AuthIndex, exactHistory.Detail.TotalTokens = second.AuthIndex, 17
	ambiguous.Detail.TotalTokens = 7
	oldIdentity.AuthID, oldIdentity.Detail.TotalTokens = "codex:apikey:retired", 12
	oauth.AuthType, oauth.AuthID, oauth.Source, oauth.Detail.TotalTokens = "oauth", "codex-oauth.json", "fixture@example.test", 13
	oauth.ResponseHeaders = map[string][]string{"x-codex-primary-used-percent": {"10"}}
	payment.AuthID, payment.Failed, payment.Failure.StatusCode = first.AuthID, true, http.StatusPaymentRequired
	payment.ResponseHeaders = map[string][]string{"x-codex-primary-used-percent": {"50"}}
	for _, rec := range []usageRecord{primary, secondary, missing, ambiguous, exactHistory, oauth, payment, oldIdentity} {
		if err := usageStore.recordUsage(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	db, _, err := usageStore.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := queryProviders(ctx, db, old.Unix(), 100, "other")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]providerRow{}
	for _, row := range providers {
		byName[row.Provider] = row
	}
	if len(byName) != 3 || byName[first.Name].Requests != 3 || byName[first.Name].CacheCreationTokens != 7 ||
		byName[first.Name].RateLimited != 1 || byName[second.Name].TotalTokens != 26 || byName[unmatchedCodexProvider].TotalTokens != 19 {
		t.Fatalf("unexpected provider counts: %+v", providers)
	}
	if totals, err := queryOneTotals(ctx, db, old.Unix(), "codex"); err != nil || totals.Requests != 1 || totals.TotalTokens != 13 {
		t.Fatalf("OAuth usage contaminated: %+v; err=%v", totals, err)
	}
	quotas, err := queryProviderQuotas(ctx, db, entries)
	if err != nil || len(quotas) != 2 {
		t.Fatalf("quotas=%+v; err=%v", quotas, err)
	}
	if got := quotas[0]; got.Primary == nil || got.Primary.UsedPercent != 0 || got.Primary.ResetAt != old.Unix()+60 || got.Primary.ExpiresAt != old.Unix()+60 ||
		got.Secondary == nil || got.Secondary.UsedPercent != 98.5 || got.Secondary.ResetAt != 0 || got.Secondary.ExpiresAt != now.Add(24*time.Hour).Unix() {
		t.Fatalf("first credential observations: %+v", got)
	}
	if got := quotas[1]; got.Primary != nil || got.Secondary != nil {
		t.Fatal("missing headers or another credential were treated as a quota observation")
	}
	if exported := providerExportRows([]providerRow{byName[first.Name], byName[second.Name]}, quotas); exported[0]["primary_used_percent"] != "0" || exported[1]["primary_used_percent"] != "" {
		t.Fatal("provider export lost the distinction between zero and missing")
	}
	logs, _, err := exportLogRecords(ctx, db, logExportFilter{Scope: "providers", Provider: first.Name, Window: "7d", Status: "402"}, nil)
	if err != nil || len(logs) != 1 || logs[0]["provider"] != first.Name {
		t.Fatalf("provider 402 filter found %d rows; err=%v", len(logs), err)
	}
	data, err := usageStore.summary(ctx, "7d", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := data["provider_quotas"].([]providerQuotaRow); len(got) != 2 || got[0].Primary == nil {
		t.Fatal("summary did not include upstream quota")
	}
	data["test_log_export"] = logs
	for section, contents := range data {
		encoded, err := json.Marshal(contents)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{sharedKey, clientKey} {
			if strings.Contains(string(encoded), key) {
				t.Errorf("%s included an unmasked secret", section)
			}
		}
	}
}

func TestProviderQuotaRejectsInvalidOrConflictingHeaders(t *testing.T) {
	for _, values := range [][]string{{"NaN"}, {"+Inf"}, {"101"}, {"-1"}, {""}, {"0", "100"}} {
		if amount, _ := providerQuotaHeaders(usageRecord{ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": values}}, 200, "primary"); amount != nil {
			t.Fatalf("accepted invalid observation %v", values)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCPAProviderIntegration(t *testing.T) {
	binaries := filepath.SplitList(os.Getenv("CPA_TEST_BINARIES"))
	plugin := os.Getenv("CPA_TEST_PLUGIN")
	if len(binaries) == 0 || plugin == "" {
		t.Skip("set CPA_TEST_BINARIES and CPA_TEST_PLUGIN to run native host integration")
	}
	for _, binary := range binaries {
		t.Run(filepath.Base(filepath.Dir(binary)), func(t *testing.T) {
			testCPAProviderHost(t, binary, plugin)
		})
	}
}

func testCPAProviderHost(t *testing.T, binary, plugin string) {
	const managementSecret = "fixture-management-secret"
	const accessSecret = "fixture-access-secret"
	const withQuotaSecret = "fixture-supplier-quota"
	const withoutQuotaSecret = "fixture-supplier-no-quota"
	reset := time.Now().Add(time.Hour).Unix()
	var firstStatus, secondStatus atomic.Int32
	upstream := func(secret string, quota bool, status *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/responses" || request.Header.Get("Authorization") != "Bearer "+secret {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if failure := int(status.Load()); failure != 0 {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(failure)
				_, _ = io.WriteString(writer, `{"error":{"message":"fixture upstream rejected the request"}}`)
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			if quota {
				writer.Header().Set("X-Codex-Primary-Used-Percent", "0")
				writer.Header().Set("X-Codex-Primary-Reset-At", strconv.FormatInt(reset, 10))
			}
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n")
		}))
	}
	withQuota, withoutQuota := upstream(withQuotaSecret, true, &firstStatus), upstream(withoutQuotaSecret, false, &secondStatus)
	t.Cleanup(withQuota.Close)
	t.Cleanup(withoutQuota.Close)
	parent := t.TempDir()
	pluginDir := filepath.Join(parent, "plugins", runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	pluginBytes, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, pluginID+filepath.Ext(plugin)), pluginBytes, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	configPath := filepath.Join(parent, "host-config.yaml")
	config := fmt.Sprintf(`config-version: 8
server: {host: '127.0.0.1', port: %d}
management: {allow-remote: false, secret-key: %q, disable-control-panel: true}
access: {api-keys: [%q]}
oauth: {auth-dir: %q}
observability: {usage: {usage-statistics-enabled: true}}
routing: {retry: {request-retry: 0}}
api-keys:
  codex:
  - name: Fixture A
    base-url: %q
    models: [{name: gpt-5.5, alias: fixture-a}]
    keys: [{api-key: %q}]
  - name: Fixture B
    base-url: %q
    models: [{name: gpt-5.5, alias: fixture-b}]
    keys: [{api-key: %q}]
plugins:
  enabled: true
  dir: %q
  configs:
    codex-token-usage:
      enabled: true
      model_price_auto_update_enabled: false
      summary_precompute_enabled: false
`, port, managementSecret, accessSecret, filepath.Join(parent, "auth"), withQuota.URL, withQuotaSecret, withoutQuota.URL, withoutQuotaSecret, filepath.Join(parent, "plugins"))
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(parent, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(binary, "-config", configPath)
	cmd.Dir = parent
	cmd.Env = append(os.Environ(), "HOME="+parent, "USERPROFILE="+parent, "CPA_TOKEN_USAGE_DIR="+filepath.Join(parent, "data"), "CPA_CONFIG_PATH=", "CPA_CONFIG_FILE=")
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "CPA_CONFIG_PATH="+configPath)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			if raw, err := os.ReadFile(logFile.Name()); err == nil {
				t.Logf("fixture CPA log:\n%s", raw)
			}
		}
	}()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 5 * time.Second}
	observer := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	t.Cleanup(observer.CloseIdleConnections)
	requestFrom := func(client *http.Client, method, path, key, body string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(context.Background(), method, base+path, strings.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		return raw, response.StatusCode, err
	}
	request := func(method, path, key, body string) ([]byte, int, error) {
		return requestFrom(client, method, path, key, body)
	}
	const summaryPath = "/v0/management/plugins/codex-token-usage/summary?window=24h&limit=50"
	const reportPath = "/v0/management/plugins/codex-token-usage/provider-report?window=24h&provider="
	const configEndpoint = "/v0/management/plugins/codex-token-usage/config"
	var summary struct {
		Version   string             `json:"version"`
		Providers []providerRow      `json:"providers"`
		Quotas    []providerQuotaRow `json:"provider_quotas"`
		Totals    totalsRow          `json:"provider_totals"`
	}
	loadSummary := func() bool {
		raw, status, err := request(http.MethodGet, summaryPath, managementSecret, "")
		if err != nil || status != http.StatusOK || json.Unmarshal(raw, &summary) != nil {
			return false
		}
		for _, secret := range []string{accessSecret, withQuotaSecret, withoutQuotaSecret} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("fixture summary contained an unmasked secret")
			}
		}
		return true
	}
	waitFor := func(label string, test func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if test() {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s: %+v", label, summary)
	}
	waitFor("plug-in runtime, custom config and both endpoint rows", func() bool {
		return loadSummary() && summary.Version == pluginVersion && len(summary.Quotas) == 2
	})
	if _, code, err := request(http.MethodGet, summaryPath, "", ""); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("management authorization: status=%d, err=%v", code, err)
	}
	if _, code, err := request(http.MethodGet, reportPath+url.QueryEscape("Codex · Fixture A"), "", ""); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("provider report authorization: status=%d, err=%v", code, err)
	}
	if body, code, err := request(http.MethodGet, "/v0/resource/plugins/codex-token-usage/dashboard", managementSecret, ""); err != nil || code != http.StatusOK || !strings.Contains(string(body), "provider_quotas") {
		t.Fatalf("dashboard resource: status=%d, err=%v", code, err)
	}
	for _, pattern := range []struct {
		alias  string
		stream bool
	}{{"fixture-a", false}, {"fixture-b", true}} {
		body := fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"mock request"}],"stream":%t}`, pattern.alias, pattern.stream)
		if response, code, err := request(http.MethodPost, "/v1/responses", accessSecret, body); err != nil || code != http.StatusOK {
			t.Fatalf("fixture %s: status=%d err=%v, body=%s", pattern.alias, code, err, response)
		}
	}
	waitFor("usage delivery to both endpoint rows", func() bool {
		return loadSummary() && summary.Totals.Requests == 2 && len(summary.Providers) == 2
	})
	for _, provider := range summary.Providers {
		if provider.Requests != 1 || provider.TotalTokens != 13 || provider.Failed != 0 {
			t.Fatalf("wrong provider counters: %+v", provider)
		}
	}
	first, second := summary.Quotas[0], summary.Quotas[1]
	if first.Provider != "Codex · Fixture A" || first.Primary == nil || first.Primary.UsedPercent != 0 || first.Primary.ResetAt != reset ||
		second.Provider != "Codex · Fixture B" || second.Primary != nil || second.Secondary != nil {
		t.Fatalf("wrong upstream observations: %+v / %+v", first, second)
	}
	if body, code, err := request(http.MethodGet, "/v0/management/plugins/codex-token-usage/export?window=24h&type=providers&format=csv", managementSecret, ""); err != nil || code != http.StatusOK || !strings.Contains(string(body), "primary_used_percent") {
		t.Fatalf("provider CSV export: status=%d, err=%v", code, err)
	}
	firstStatus.Store(http.StatusTooManyRequests)
	secondStatus.Store(http.StatusPaymentRequired)
	for _, failure := range []struct {
		alias string
		code  int
	}{{"fixture-a", http.StatusTooManyRequests}, {"fixture-b", http.StatusPaymentRequired}} {
		body := fmt.Sprintf(`{"model":%q,"input":"fixture failure"}`, failure.alias)
		if _, code, err := request(http.MethodPost, "/v1/responses", accessSecret, body); err != nil || code != failure.code {
			t.Fatalf("fixture %s: got status=%d, want %d, err=%v", failure.alias, code, failure.code, err)
		}
	}
	waitFor("supplier failure ingestion", func() bool {
		return loadSummary() && summary.Totals.Requests == 4 && summary.Totals.Failed == 2 && summary.Totals.RateLimited == 1
	})
	if snapshot := summary.Quotas[0].Primary; snapshot == nil || snapshot.UsedPercent != 0 || summary.Quotas[1].Primary != nil {
		t.Fatal("upstream failures without quota headers changed the quota observations")
	}
	for _, fixture := range []struct{ provider, status string }{{first.Provider, "429"}, {second.Provider, "402"}} {
		raw, code, err := request(http.MethodGet, reportPath+url.QueryEscape(fixture.provider), managementSecret, "")
		var report providerReport
		if err != nil || code != http.StatusOK || json.Unmarshal(raw, &report) != nil || report.Provider != fixture.provider ||
			report.Totals.Requests != 2 || report.Totals.TotalTokens != 13 || report.Statuses[fixture.status] != 1 || report.Statuses["ok"] != 1 || len(report.Recent) != 2 {
			t.Fatalf("provider report for %s: status=%d, err=%v, body=%s", fixture.provider, code, err, raw)
		}
	}
	patch := fmt.Sprintf(`{"provider_budgets_usd":{%q:12.5,%q:7}}`, first.Provider, second.Provider)
	if raw, code, err := request(http.MethodPatch, configEndpoint, managementSecret, patch); err != nil || code != http.StatusOK {
		t.Fatalf("budget config update: status=%d, err=%v, body=%s", code, err, raw)
	}
	waitFor("live shared budget reload", func() bool {
		for _, reader := range []*http.Client{client, observer} {
			raw, code, err := requestFrom(reader, http.MethodGet, reportPath+url.QueryEscape(first.Provider), managementSecret, "")
			var report providerReport
			if err != nil || code != http.StatusOK || json.Unmarshal(raw, &report) != nil || report.Budget.LimitUSD != 12.5 {
				return false
			}
		}
		return true
	})
	var saved map[string]any
	raw, code, err := requestFrom(observer, http.MethodGet, configEndpoint, managementSecret, "")
	if err != nil || code != http.StatusOK || json.Unmarshal(raw, &saved) != nil || saved["summary_precompute_enabled"] != false || saved["model_price_auto_update_enabled"] != false {
		t.Fatalf("budget update changed other settings: status=%d, err=%v, body=%s", code, err, raw)
	}
}

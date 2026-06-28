package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	_ "github.com/mattn/go-sqlite3"
)

const (
	abiVersion       uint32 = 1
	pluginID                = "codex-token-usage"
	codexQuotaAPIURL        = "https://chatgpt.com/backend-api/wham/usage"
)

var (
	pluginVersion    = "0.1.8"
	pluginAuthor     = "Codex Token Usage Contributors"
	pluginRepository = "https://github.com/zhumengling/codex-token-usage"
)

var globalStore = &store{}
var globalQuotaTrigger = &quotaTriggerManager{}
var globalModelPriceUpdater = &modelPriceUpdateManager{}
var codexQuotaURLOverrideForTest string

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pluginRegisterResponse struct {
	SchemaVersion int            `json:"schema_version"`
	Metadata      pluginMetadata `json:"metadata"`
	Capabilities  capabilities   `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type capabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
	Scheduler     bool `json:"scheduler"`
}

type lifecycleRequest struct {
	ConfigYAML json.RawMessage `json:"config_yaml"`
}

type pluginConfig struct {
	QuotaTriggerEnabled                   bool
	QuotaTriggerIntervalMinutes           int
	QuotaTriggerMode                      string
	QuotaTriggerMaxConcurrency            int
	QuotaTriggerTimeoutSeconds            int
	QuotaTriggerMinAccountCooldownMinutes int
	ModelPriceAutoUpdateEnabled           bool
	ModelPriceUpdateIntervalHours         int
	ModelPriceUpdateURL                   string
	ModelPriceUpdateTimeoutSeconds        int
}

type quotaTriggerState struct {
	Enabled                   bool   `json:"enabled"`
	Running                   bool   `json:"running"`
	Mode                      string `json:"mode"`
	IntervalMinutes           int    `json:"interval_minutes"`
	MaxConcurrency            int    `json:"max_concurrency"`
	TimeoutSeconds            int    `json:"timeout_seconds"`
	MinAccountCooldownMinutes int    `json:"min_account_cooldown_minutes"`
	LastRunAt                 string `json:"last_run_at,omitempty"`
	LastRunStartedAt          string `json:"last_run_started_at,omitempty"`
	LastSuccess               int    `json:"last_success"`
	LastFailed                int    `json:"last_failed"`
	LastSkipped               int    `json:"last_skipped"`
	LastCandidates            int    `json:"last_candidates"`
	LastError                 string `json:"last_error,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type schedulerPickRequest struct {
	Provider   string                   `json:"Provider"`
	Providers  []string                 `json:"Providers"`
	Model      string                   `json:"Model"`
	Stream     bool                     `json:"Stream"`
	Options    schedulerOptions         `json:"Options"`
	Candidates []schedulerAuthCandidate `json:"Candidates"`
}

type schedulerOptions struct {
	Headers  map[string][]string `json:"Headers"`
	Metadata map[string]any      `json:"Metadata"`
}

type schedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
	Metadata   map[string]any    `json:"Metadata"`
}

type schedulerPickResponse struct {
	AuthID          string `json:"AuthID"`
	DelegateBuiltin string `json:"DelegateBuiltin"`
	Handled         bool   `json:"Handled"`
}

type usageRecord struct {
	Provider        string              `json:"Provider"`
	ExecutorType    string              `json:"ExecutorType"`
	Model           string              `json:"Model"`
	Alias           string              `json:"Alias"`
	APIKey          string              `json:"APIKey"`
	AuthID          string              `json:"AuthID"`
	AuthIndex       string              `json:"AuthIndex"`
	AuthType        string              `json:"AuthType"`
	Source          string              `json:"Source"`
	ReasoningEffort string              `json:"ReasoningEffort"`
	ServiceTier     string              `json:"ServiceTier"`
	RequestedAt     time.Time           `json:"RequestedAt"`
	Latency         int64               `json:"Latency"`
	TTFT            int64               `json:"TTFT"`
	Failed          bool                `json:"Failed"`
	Failure         usageFailure        `json:"Failure"`
	Detail          usageDetail         `json:"Detail"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(nil)
	_ = host
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), req)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	globalQuotaTrigger.stop()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okJSON(pluginRegisterResponse{
			SchemaVersion: 1,
			Metadata: pluginMetadata{
				Name:             "CPA Token Usage",
				Version:          pluginVersion,
				Author:           pluginAuthor,
				GitHubRepository: pluginRepository,
				Logo:             "",
				ConfigFields:     pluginConfigFields(),
			},
			Capabilities: capabilities{UsagePlugin: true, ManagementAPI: true, Scheduler: true},
		})
	case "management.register":
		return okJSON(managementRegistrationResponse{
			Routes: []managementRoute{
				{Method: "GET", Path: "/plugins/codex-token-usage/summary", Description: "Token usage summary JSON."},
			},
			Resources: []resourceRoute{
				{Path: "/dashboard", Menu: "Token Usage", Description: "Account token usage dashboard."},
				{Path: "/summary", Description: "Token usage summary JSON for the dashboard."},
			},
		})
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return okJSON(jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": err.Error()}))
		}
		return okJSON(handleManagement(req))
	case "usage.handle":
		var rec usageRecord
		if err := json.Unmarshal(request, &rec); err != nil {
			return okJSON(map[string]any{"ignored": true, "error": err.Error()})
		}
		if err := globalStore.recordUsage(context.Background(), rec); err != nil {
			return okJSON(map[string]any{"stored": false, "error": err.Error()})
		}
		return okJSON(map[string]any{"stored": true})
	case "scheduler.pick":
		var req schedulerPickRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return okJSON(schedulerPickResponse{Handled: false})
		}
		resp, err := globalStore.pickAuth(context.Background(), req)
		if err != nil {
			return okJSON(schedulerPickResponse{Handled: false})
		}
		return okJSON(resp)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginConfigFields() []configField {
	return []configField{
		{Name: "开启定时额度触发", Type: "boolean", Description: "是否开启 Codex 账号定时额度触发。默认关闭。"},
		{Name: "触发间隔分钟", Type: "number", Description: "每轮触发间隔，单位分钟。默认 10。"},
		{Name: "触发模式", Type: "enum", Description: "quota=只查询额度；probe=探测请求。默认 quota。"},
		{Name: "最大并发账号数", Type: "number", Description: "每轮最大并发触发账号数。默认 1。"},
		{Name: "单账号超时秒数", Type: "number", Description: "单个账号触发请求超时时间，单位秒。默认 20。"},
		{Name: "单账号最小冷却分钟", Type: "number", Description: "同一账号两次触发的最小冷却时间，单位分钟。默认 10。"},
		{Name: "自动更新模型价格表", Type: "boolean", Description: "是否自动下载并更新 model_prices.json。默认开启。"},
		{Name: "模型价格更新间隔小时", Type: "number", Description: "model_prices.json 自动检查间隔，单位小时。默认 6。"},
		{Name: "模型价格表地址", Type: "string", Description: "模型价格 JSON 下载地址。默认使用 LiteLLM 官方价格表。"},
		{Name: "模型价格更新超时秒数", Type: "number", Description: "下载 model_prices.json 的超时时间，单位秒。默认 20。"},
	}
}

func handleManagement(req managementRequest) managementResponse {
	if strings.HasPrefix(req.Path, "/v0/resource/plugins/"+pluginID+"/dashboard") {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    map[string][]string{"content-type": {"text/html; charset=utf-8"}, "cache-control": {"no-store"}},
			Body:       []byte(dashboardHTML),
		}
	}
	if strings.HasPrefix(req.Path, "/v0/resource/plugins/"+pluginID+"/summary") {
		window := firstQuery(req.Query, "window", "24h")
		limit := parseInt(firstQuery(req.Query, "limit", "50"), 50, 1, 5000)
		data, err := globalStore.summary(context.Background(), window, limit)
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "summary_failed", "message": err.Error()})
		}
		return jsonResponse(http.StatusOK, data)
	}
	if strings.HasPrefix(req.Path, "/v0/management/plugins/"+pluginID+"/summary") {
		window := firstQuery(req.Query, "window", "24h")
		limit := parseInt(firstQuery(req.Query, "limit", "50"), 50, 1, 5000)
		data, err := globalStore.summary(context.Background(), window, limit)
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "summary_failed", "message": err.Error()})
		}
		return jsonResponse(http.StatusOK, data)
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
}

func firstQuery(query map[string][]string, key, fallback string) string {
	if query == nil {
		return fallback
	}
	values := query[key]
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return fallback
	}
	return strings.TrimSpace(values[0])
}

func parseInt(value string, fallback, min, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		QuotaTriggerEnabled:                   false,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            20,
		QuotaTriggerMinAccountCooldownMinutes: 10,
		ModelPriceAutoUpdateEnabled:           true,
		ModelPriceUpdateIntervalHours:         6,
		ModelPriceUpdateURL:                   defaultModelPriceURL,
		ModelPriceUpdateTimeoutSeconds:        20,
	}
}

func configurePlugin(request []byte) error {
	cfg := defaultPluginConfig()
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return err
		}
	}
	raw, err := lifecycleConfigYAML(req.ConfigYAML)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		cfg = parsePluginConfigYAML(raw, cfg)
	}
	cfg = normalizePluginConfig(cfg)
	globalQuotaTrigger.configure(cfg)
	globalModelPriceUpdater.configure(cfg)
	return nil
}

func lifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, err := base64.StdEncoding.DecodeString(text); err == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, errors.New("config_yaml must be a string or byte array")
}

func parsePluginConfigYAML(raw []byte, cfg pluginConfig) pluginConfig {
	values := yamlScalars(string(raw))
	if value, ok := configValue(values, "quota_trigger_enabled", "开启定时额度触发"); ok {
		cfg.QuotaTriggerEnabled = parseBoolString(value, cfg.QuotaTriggerEnabled)
	}
	if value, ok := configValue(values, "quota_trigger_interval_minutes", "触发间隔分钟"); ok {
		cfg.QuotaTriggerIntervalMinutes = parseInt(value, cfg.QuotaTriggerIntervalMinutes, 1, 1440)
	}
	if value, ok := configValue(values, "quota_trigger_mode", "触发模式"); ok {
		cfg.QuotaTriggerMode = value
	}
	if value, ok := configValue(values, "quota_trigger_max_concurrency", "最大并发账号数"); ok {
		cfg.QuotaTriggerMaxConcurrency = parseInt(value, cfg.QuotaTriggerMaxConcurrency, 1, 32)
	}
	if value, ok := configValue(values, "quota_trigger_timeout_seconds", "单账号超时秒数"); ok {
		cfg.QuotaTriggerTimeoutSeconds = parseInt(value, cfg.QuotaTriggerTimeoutSeconds, 3, 300)
	}
	if value, ok := configValue(values, "quota_trigger_min_account_cooldown_minutes", "单账号最小冷却分钟"); ok {
		cfg.QuotaTriggerMinAccountCooldownMinutes = parseInt(value, cfg.QuotaTriggerMinAccountCooldownMinutes, 1, 1440)
	}
	if value, ok := configValue(values, "model_price_auto_update_enabled", "自动更新模型价格表"); ok {
		cfg.ModelPriceAutoUpdateEnabled = parseBoolString(value, cfg.ModelPriceAutoUpdateEnabled)
	}
	if value, ok := configValue(values, "model_price_update_interval_hours", "模型价格更新间隔小时"); ok {
		cfg.ModelPriceUpdateIntervalHours = parseInt(value, cfg.ModelPriceUpdateIntervalHours, 1, 168)
	}
	if value, ok := configValue(values, "model_price_update_url", "模型价格表地址"); ok {
		cfg.ModelPriceUpdateURL = value
	}
	if value, ok := configValue(values, "model_price_update_timeout_seconds", "模型价格更新超时秒数"); ok {
		cfg.ModelPriceUpdateTimeoutSeconds = parseInt(value, cfg.ModelPriceUpdateTimeoutSeconds, 3, 300)
	}
	return cfg
}

func configValue(values map[string]string, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	return "", false
}

func normalizePluginConfig(cfg pluginConfig) pluginConfig {
	cfg.QuotaTriggerIntervalMinutes = clampInt(cfg.QuotaTriggerIntervalMinutes, 1, 1440)
	cfg.QuotaTriggerMaxConcurrency = clampInt(cfg.QuotaTriggerMaxConcurrency, 1, 32)
	cfg.QuotaTriggerTimeoutSeconds = clampInt(cfg.QuotaTriggerTimeoutSeconds, 3, 300)
	cfg.QuotaTriggerMinAccountCooldownMinutes = clampInt(cfg.QuotaTriggerMinAccountCooldownMinutes, 1, 1440)
	cfg.ModelPriceUpdateIntervalHours = clampInt(cfg.ModelPriceUpdateIntervalHours, 1, 168)
	cfg.ModelPriceUpdateTimeoutSeconds = clampInt(cfg.ModelPriceUpdateTimeoutSeconds, 3, 300)
	if strings.TrimSpace(cfg.ModelPriceUpdateURL) == "" {
		cfg.ModelPriceUpdateURL = defaultModelPriceURL
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.QuotaTriggerMode))
	switch mode {
	case "探测请求", "probe模式", "probe 模式":
		mode = "probe"
	case "只查询额度", "查询额度", "额度查询", "quota模式", "quota 模式":
		mode = "quota"
	}
	if mode != "probe" {
		mode = "quota"
	}
	cfg.QuotaTriggerMode = mode
	return cfg
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func parseBoolString(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func yamlScalars(raw string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		value = strings.Trim(value, `"'`)
		out[key] = value
	}
	return out
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func jsonResponse(status int, v any) managementResponse {
	body, _ := json.Marshal(v)
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"content-type": {"application/json; charset=utf-8"}, "cache-control": {"no-store"}},
		Body:       body,
	}
}

type store struct {
	mu     sync.Mutex
	db     *sql.DB
	dbPath string
}

func (s *store) open(ctx context.Context) (*sql.DB, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, s.dbPath, nil
	}
	dir := strings.TrimSpace(os.Getenv("CPA_TOKEN_USAGE_DIR"))
	if dir == "" {
		dir = "/root/.cli-proxy-api/plugins/codex-token-usage"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "usage.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, "", err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	if err := normalizeStoredResetColumns(ctx, db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	if err := ensureUsageEventColumns(ctx, db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	if err := normalizeStoredLatencyColumns(ctx, db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	if err := ensureQuotaTriggerRunColumns(ctx, db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	s.db = db
	s.dbPath = path
	return db, path, nil
}

func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func normalizeStoredResetColumns(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`UPDATE usage_events SET primary_reset_at = CAST(primary_reset_at / 1000 AS INTEGER) WHERE primary_reset_at > 1000000000000`,
		`UPDATE usage_events SET secondary_reset_at = CAST(secondary_reset_at / 1000 AS INTEGER) WHERE secondary_reset_at > 1000000000000`,
		`UPDATE autoban_bans SET reset_at = CAST(reset_at / 1000 AS INTEGER) WHERE reset_at > 1000000000000`,
		`UPDATE autoban_bans SET primary_reset_at = CAST(primary_reset_at / 1000 AS INTEGER) WHERE primary_reset_at > 1000000000000`,
		`UPDATE autoban_bans SET secondary_reset_at = CAST(secondary_reset_at / 1000 AS INTEGER) WHERE secondary_reset_at > 1000000000000`,
		`UPDATE quota_trigger_runs SET primary_reset_at = CAST(primary_reset_at / 1000 AS INTEGER) WHERE primary_reset_at > 1000000000000`,
		`UPDATE quota_trigger_runs SET secondary_reset_at = CAST(secondary_reset_at / 1000 AS INTEGER) WHERE secondary_reset_at > 1000000000000`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStoredLatencyColumns(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`UPDATE usage_events SET latency_ms = CAST((latency_ms + 999999) / 1000000 AS INTEGER) WHERE latency_ms > 10000`,
		`UPDATE usage_events SET ttft_ms = CAST((ttft_ms + 999999) / 1000000 AS INTEGER) WHERE ttft_ms > 10000`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func ensureQuotaTriggerRunColumns(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		def  string
	}{
		{"primary_used_tokens", "INTEGER"},
		{"primary_remaining_tokens", "INTEGER"},
		{"primary_limit_tokens", "INTEGER"},
		{"secondary_used_tokens", "INTEGER"},
		{"secondary_remaining_tokens", "INTEGER"},
		{"secondary_limit_tokens", "INTEGER"},
	}
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(quota_trigger_runs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE quota_trigger_runs ADD COLUMN `+column.name+` `+column.def); err != nil {
			return err
		}
	}
	return nil
}

func ensureUsageEventColumns(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		def  string
	}{
		{"latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"ttft_ms", "INTEGER NOT NULL DEFAULT 0"},
	}
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(usage_events)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE usage_events ADD COLUMN `+column.name+` `+column.def); err != nil {
			return err
		}
	}
	return nil
}

type quotaTriggerManager struct {
	mu     sync.Mutex
	cfg    pluginConfig
	cancel context.CancelFunc
	state  quotaTriggerState
}

func (m *quotaTriggerManager) configure(cfg pluginConfig) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.cfg = cfg
	m.state.Enabled = cfg.QuotaTriggerEnabled
	m.state.Running = false
	m.state.Mode = cfg.QuotaTriggerMode
	m.state.IntervalMinutes = cfg.QuotaTriggerIntervalMinutes
	m.state.MaxConcurrency = cfg.QuotaTriggerMaxConcurrency
	m.state.TimeoutSeconds = cfg.QuotaTriggerTimeoutSeconds
	m.state.MinAccountCooldownMinutes = cfg.QuotaTriggerMinAccountCooldownMinutes
	if !cfg.QuotaTriggerEnabled {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state.Running = true
	m.mu.Unlock()
	go m.loop(ctx, cfg)
}

func (m *quotaTriggerManager) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.state.Running = false
	m.mu.Unlock()
}

func (m *quotaTriggerManager) status() quotaTriggerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *quotaTriggerManager) loop(ctx context.Context, cfg pluginConfig) {
	m.runRound(ctx, cfg)
	ticker := time.NewTicker(time.Duration(cfg.QuotaTriggerIntervalMinutes) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.cancel != nil {
				m.state.Running = false
			}
			m.mu.Unlock()
			return
		case <-ticker.C:
			m.runRound(ctx, cfg)
		}
	}
}

func (m *quotaTriggerManager) runRound(ctx context.Context, cfg pluginConfig) {
	started := time.Now()
	m.mu.Lock()
	m.state.LastRunStartedAt = started.Format(time.RFC3339)
	m.state.LastError = ""
	m.mu.Unlock()

	success, failed, skipped, candidates, err := globalStore.runQuotaTriggerRound(ctx, cfg)

	m.mu.Lock()
	m.state.LastRunAt = time.Now().Format(time.RFC3339)
	m.state.LastSuccess = success
	m.state.LastFailed = failed
	m.state.LastSkipped = skipped
	m.state.LastCandidates = candidates
	if err != nil {
		m.state.LastError = sanitizeTriggerError(err)
	}
	m.mu.Unlock()
}

func (s *store) recordUsage(ctx context.Context, rec usageRecord) error {
	db, _, err := s.open(ctx)
	if err != nil {
		return err
	}
	if rec.RequestedAt.IsZero() {
		rec.RequestedAt = time.Now()
	}
	total := rec.Detail.TotalTokens
	if total == 0 {
		total = rec.Detail.InputTokens + rec.Detail.OutputTokens + rec.Detail.ReasoningTokens
	}
	if total == 0 {
		total = rec.Detail.InputTokens + rec.Detail.OutputTokens + rec.Detail.ReasoningTokens + rec.Detail.CachedTokens
	}
	primaryPct := headerFloat(rec.ResponseHeaders, "x-codex-primary-used-percent")
	secondaryPct := headerFloat(rec.ResponseHeaders, "x-codex-secondary-used-percent")
	primaryReset := headerInt(rec.ResponseHeaders, "x-codex-primary-reset-at")
	secondaryReset := headerInt(rec.ResponseHeaders, "x-codex-secondary-reset-at")
	normalizeInt64Ptr(primaryReset)
	normalizeInt64Ptr(secondaryReset)
	status := rec.Failure.StatusCode
	if rec.Failed && status == 0 {
		status = 599
	}
	_, err = db.ExecContext(ctx, insertSQL,
		rec.RequestedAt.Unix(),
		trim(rec.Provider), trim(rec.ExecutorType), trim(rec.Model), trim(rec.Alias),
		trim(rec.APIKey), trim(rec.AuthID), trim(rec.AuthIndex), trim(rec.AuthType), trim(rec.Source),
		trim(rec.ReasoningEffort), trim(rec.ServiceTier), durationToMilliseconds(rec.Latency), durationToMilliseconds(rec.TTFT), boolInt(rec.Failed), status,
		rec.Detail.InputTokens, rec.Detail.OutputTokens, rec.Detail.ReasoningTokens, rec.Detail.CachedTokens,
		rec.Detail.CacheReadTokens, rec.Detail.CacheCreationTokens, total,
		primaryPct, primaryReset, secondaryPct, secondaryReset,
	)
	if err != nil {
		return err
	}
	if err := recordInvalidAuthIfNeeded(ctx, db, rec, status); err != nil {
		return err
	}
	return recordAutobanIfNeeded(ctx, db, rec, status, primaryPct, primaryReset, secondaryPct, secondaryReset)
}

func recordInvalidAuthIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if status != http.StatusUnauthorized {
		return nil
	}
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	authID := firstNonEmptyString(rec.AuthID, rec.AuthIndex, rec.Source)
	if authID == "" {
		return nil
	}
	now := time.Now().Unix()
	authFile, authFileMTime := authFileStateForRecord(rec)
	_, err := db.ExecContext(ctx, `
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_file_mtime
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider=excluded.provider,
  reason=excluded.reason,
  invalidated_at=excluded.invalidated_at,
  active=1,
  last_status_code=excluded.last_status_code,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime`,
		trim(authID), trim(rec.AuthIndex), trim(rec.Source), trim(rec.Provider),
		"401 unauthorized: credential is invalid", now, status, authFile, authFileMTime,
	)
	return err
}

func recordAutobanIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int, primaryPct *float64, primaryReset *int64, secondaryPct *float64, secondaryReset *int64) error {
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	if !rec.Failed || status != http.StatusTooManyRequests {
		return nil
	}
	authID := firstNonEmptyString(rec.AuthID, rec.AuthIndex, rec.Source)
	if authID == "" {
		return nil
	}
	now := time.Now().Unix()
	normalizeInt64Ptr(primaryReset)
	normalizeInt64Ptr(secondaryReset)
	resetAt, window, reason := classifyCodexBan(rec.ResponseHeaders, primaryPct, primaryReset, secondaryPct, secondaryReset, now)
	_, err := db.ExecContext(ctx, `
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider=excluded.provider,
  window=excluded.window,
  reason=excluded.reason,
  banned_at=excluded.banned_at,
  reset_at=excluded.reset_at,
  active=1,
  last_status_code=excluded.last_status_code,
  primary_used_percent=excluded.primary_used_percent,
  primary_reset_at=excluded.primary_reset_at,
  secondary_used_percent=excluded.secondary_used_percent,
  secondary_reset_at=excluded.secondary_reset_at`,
		trim(authID), trim(rec.AuthIndex), trim(rec.Source), trim(rec.Provider), window, reason, now, resetAt, status,
		primaryPct, primaryReset, secondaryPct, secondaryReset,
	)
	return err
}

func (s *store) runQuotaTriggerRound(ctx context.Context, cfg pluginConfig) (int, int, int, int, error) {
	db, _, err := s.open(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if err := clearReplacedInvalidAuths(ctx, db); err != nil {
		return 0, 0, 0, 0, err
	}
	now := time.Now().Unix()
	if err := expireAutobans(ctx, db, now); err != nil {
		return 0, 0, 0, 0, err
	}
	candidates, skipped, err := selectQuotaTriggerCandidates(ctx, db, cfg)
	if err != nil {
		return 0, 0, skipped, 0, err
	}
	if len(candidates) == 0 {
		return 0, 0, skipped, 0, nil
	}
	sem := make(chan struct{}, cfg.QuotaTriggerMaxConcurrency)
	results := make(chan quotaTriggerRun, len(candidates))
	var wg sync.WaitGroup
	for _, account := range candidates {
		select {
		case <-ctx.Done():
			return 0, 0, skipped, len(candidates), ctx.Err()
		default:
		}
		wg.Add(1)
		go func(account triggerAuthAccount) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- quotaTriggerRunFromAccount(account, cfg.QuotaTriggerMode, "failed", 0, ctx.Err().Error())
				return
			}
			results <- executeQuotaTrigger(ctx, db, account, cfg)
		}(account)
	}
	wg.Wait()
	close(results)

	success := 0
	failed := 0
	for run := range results {
		if err := recordQuotaTriggerRun(ctx, db, run); err != nil {
			failed++
			continue
		}
		if run.Status == "success" {
			success++
		} else if run.Status == "skipped" {
			skipped++
		} else {
			failed++
		}
	}
	_ = reconcileAutobansWithQuotaSnapshots(ctx, db, time.Now().Unix())
	return success, failed, skipped, len(candidates), nil
}

func selectQuotaTriggerCandidates(ctx context.Context, db *sql.DB, cfg pluginConfig) ([]triggerAuthAccount, int, error) {
	accounts := readTriggerAuthAccounts()
	if len(accounts) == 0 {
		return nil, 0, nil
	}
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return nil, 0, err
	}
	bans, err := queryActiveAutobans(ctx, db, time.Now().Unix())
	if err != nil {
		return nil, 0, err
	}
	externalAlerts, err := queryExternalUseAlerts(ctx, db, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		return nil, 0, err
	}
	cooldown := time.Duration(cfg.QuotaTriggerMinAccountCooldownMinutes) * time.Minute
	skipped := 0
	out := make([]triggerAuthAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.Disabled || account.Expired || !strings.EqualFold(firstNonEmptyString(account.Provider, "codex"), "codex") {
			skipped++
			continue
		}
		if account.AccessToken == "" {
			skipped++
			continue
		}
		isBanned := configuredMatchesAutoban(account.configuredAccount, bans)
		if configuredMatchesInvalidAuth(account.configuredAccount, invalids) || configuredMatchesExternalAlert(account.configuredAccount, externalAlerts) {
			skipped++
			continue
		}
		if isBanned && cfg.QuotaTriggerMode != "quota" {
			skipped++
			continue
		}
		row := accountRow{AuthIndex: account.AuthIndex, AuthID: account.AuthID, Source: account.Source, AuthFile: account.AuthFile, Email: account.Email, Name: account.Name}
		pp, pr := queryLatestAccountWindowQuota(ctx, db, row, 0, "primary")
		sp, sr := queryLatestAccountWindowQuota(ctx, db, row, 0, "secondary")
		if !isBanned && (quotaWindowFull(pp, pr) || quotaWindowFull(sp, sr)) {
			skipped++
			continue
		}
		if last, ok := latestQuotaTriggerFinishedAt(ctx, db, account.configuredAccount); ok && time.Since(time.Unix(last, 0)) < cooldown {
			skipped++
			continue
		}
		out = append(out, account)
	}
	return out, skipped, nil
}

func executeQuotaTrigger(ctx context.Context, db *sql.DB, account triggerAuthAccount, cfg pluginConfig) quotaTriggerRun {
	if cfg.QuotaTriggerMode == "probe" {
		return quotaTriggerRunFromAccount(account, cfg.QuotaTriggerMode, "skipped", 0, "probe mode requires host-directed model request support; no token was consumed")
	}
	return executeQuotaUsageRequest(ctx, db, account, cfg)
}

func executeQuotaUsageRequest(ctx context.Context, db *sql.DB, account triggerAuthAccount, cfg pluginConfig) quotaTriggerRun {
	run := quotaTriggerRunFromAccount(account, cfg.QuotaTriggerMode, "failed", 0, "")
	started := time.Now()
	run.StartedAt = started.Unix()
	timeout := time.Duration(cfg.QuotaTriggerTimeoutSeconds) * time.Second
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, codexQuotaURL(), nil)
	if err != nil {
		run.Error = sanitizeTriggerError(err)
		run.FinishedAt = time.Now().Unix()
		return run
	}
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	if account.ChatGPTAccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", account.ChatGPTAccountID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		run.Error = sanitizeTriggerError(err)
		run.FinishedAt = time.Now().Unix()
		return run
	}
	defer resp.Body.Close()

	run.HTTPStatus = resp.StatusCode
	headers := cloneHeaders(resp.Header)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	mergeCodexQuotaPayload(headers, body)
	run.PrimaryUsedPercent = headerFloat(headers, "x-codex-primary-used-percent")
	run.PrimaryResetAt = headerInt(headers, "x-codex-primary-reset-at")
	run.SecondaryUsedPercent = headerFloat(headers, "x-codex-secondary-used-percent")
	run.SecondaryResetAt = headerInt(headers, "x-codex-secondary-reset-at")
	run.PrimaryUsedTokens = headerInt(headers, "x-codex-primary-used-tokens")
	run.PrimaryRemaining = headerInt(headers, "x-codex-primary-remaining-tokens")
	run.PrimaryLimit = headerInt(headers, "x-codex-primary-limit-tokens")
	run.SecondaryUsedTokens = headerInt(headers, "x-codex-secondary-used-tokens")
	run.SecondaryRemaining = headerInt(headers, "x-codex-secondary-remaining-tokens")
	run.SecondaryLimit = headerInt(headers, "x-codex-secondary-limit-tokens")
	run.FinishedAt = time.Now().Unix()

	rec := usageRecord{
		Provider:        "codex",
		ExecutorType:    "quota-trigger",
		Model:           "quota-trigger",
		Alias:           "quota-trigger",
		AuthID:          account.AuthID,
		AuthIndex:       account.AuthIndex,
		AuthType:        "codex",
		Source:          account.Source,
		RequestedAt:     time.Unix(run.StartedAt, 0),
		ResponseHeaders: headers,
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		run.Status = "success"
	case resp.StatusCode == http.StatusUnauthorized:
		run.Status = "failed"
		run.Error = "401 unauthorized: credential is invalid"
		_ = recordInvalidAuthIfNeeded(context.Background(), db, rec, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		run.Status = "failed"
		run.Error = "429 rate limited"
		rec.Failed = true
		rec.Failure = usageFailure{StatusCode: resp.StatusCode}
		_ = recordAutobanIfNeeded(context.Background(), db, rec, resp.StatusCode, run.PrimaryUsedPercent, run.PrimaryResetAt, run.SecondaryUsedPercent, run.SecondaryResetAt)
	default:
		run.Status = "failed"
		run.Error = "http " + strconv.Itoa(resp.StatusCode)
	}
	return run
}

func recordQuotaTriggerRun(ctx context.Context, db *sql.DB, run quotaTriggerRun) error {
	if run.StartedAt <= 0 {
		run.StartedAt = time.Now().Unix()
	}
	if run.FinishedAt <= 0 {
		run.FinishedAt = time.Now().Unix()
	}
	normalizeInt64Ptr(run.PrimaryResetAt)
	normalizeInt64Ptr(run.SecondaryResetAt)
	_, err := db.ExecContext(ctx, `
INSERT INTO quota_trigger_runs (
  auth_id, auth_index, source, provider, auth_file, mode, status, http_status, error,
  started_at, finished_at, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at,
  primary_used_tokens, primary_remaining_tokens, primary_limit_tokens,
  secondary_used_tokens, secondary_remaining_tokens, secondary_limit_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		trim(run.AuthID), trim(run.AuthIndex), trim(run.Source), trim(run.Provider), trim(run.AuthFile),
		trim(run.Mode), trim(run.Status), run.HTTPStatus, trim(run.Error), run.StartedAt, run.FinishedAt,
		run.PrimaryUsedPercent, run.PrimaryResetAt, run.SecondaryUsedPercent, run.SecondaryResetAt,
		run.PrimaryUsedTokens, run.PrimaryRemaining, run.PrimaryLimit, run.SecondaryUsedTokens, run.SecondaryRemaining, run.SecondaryLimit,
	)
	return err
}

func quotaTriggerRunFromAccount(account triggerAuthAccount, mode, status string, httpStatus int, message string) quotaTriggerRun {
	now := time.Now().Unix()
	return quotaTriggerRun{
		AuthID:     account.AuthID,
		AuthIndex:  account.AuthIndex,
		Source:     account.Source,
		Provider:   firstNonEmptyString(account.Provider, "codex"),
		AuthFile:   account.AuthFile,
		Mode:       mode,
		Status:     status,
		HTTPStatus: httpStatus,
		Error:      sanitizeTriggerError(message),
		StartedAt:  now,
		FinishedAt: now,
	}
}

func configuredMatchesInvalidAuth(cfg configuredAccount, invalids []invalidAuthRow) bool {
	for _, invalid := range invalids {
		for _, left := range configuredAliases(cfg) {
			for _, right := range normalizeAccountAliases(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile) {
				if left != "" && left == right {
					return true
				}
			}
		}
	}
	return false
}

func configuredMatchesAutoban(cfg configuredAccount, bans []autobanRow) bool {
	for _, ban := range bans {
		for _, left := range configuredAliases(cfg) {
			for _, right := range normalizeAccountAliases(ban.AuthID, ban.AuthIndex, ban.Source) {
				if left != "" && left == right {
					return true
				}
			}
		}
	}
	return false
}

func configuredMatchesExternalAlert(cfg configuredAccount, alerts []externalUseAlert) bool {
	for _, alert := range alerts {
		for _, left := range configuredAliases(cfg) {
			for _, right := range normalizeAccountAliases(alert.AuthID, alert.AuthIndex, alert.Source) {
				if left != "" && left == right {
					return true
				}
			}
		}
	}
	return false
}

func quotaWindowFull(percent sql.NullFloat64, reset sql.NullInt64) bool {
	if !percent.Valid || percent.Float64 < 100 {
		return false
	}
	if !reset.Valid {
		return true
	}
	return normalizeUnixSeconds(reset.Int64) > time.Now().Unix()
}

func quotaWindowObserved(percent sql.NullFloat64, reset sql.NullInt64) bool {
	if percent.Valid {
		return true
	}
	return reset.Valid && normalizeUnixSeconds(reset.Int64) > 0
}

func latestQuotaTriggerFinishedAt(ctx context.Context, db *sql.DB, cfg configuredAccount) (int64, bool) {
	aliases := configuredAliases(cfg)
	var latest int64
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		var value sql.NullInt64
		_ = db.QueryRowContext(ctx, `
SELECT MAX(finished_at)
FROM quota_trigger_runs
WHERE lower(auth_id)=? OR lower(auth_index)=? OR lower(source)=? OR lower(auth_file)=?`, alias, alias, alias, alias).Scan(&value)
		if value.Valid && value.Int64 > latest {
			latest = value.Int64
		}
	}
	return latest, latest > 0
}

func codexQuotaURL() string {
	if codexQuotaURLOverrideForTest != "" {
		return codexQuotaURLOverrideForTest
	}
	return codexQuotaAPIURL
}

func cloneHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		cp := append([]string(nil), values...)
		out[key] = cp
	}
	return out
}

func setHeaderIfMissing(headers map[string][]string, key, value string) {
	if value == "" || headerValue(headers, key) != "" {
		return
	}
	headers[key] = []string{value}
}

func mergeCodexQuotaPayload(headers map[string][]string, body []byte) {
	if len(body) == 0 {
		return
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	root := mapFromAny(payload)
	if nested := mapFromAny(root["body"]); len(nested) > 0 {
		root = nested
	}
	limits := []map[string]any{
		mapFromAny(root["rate_limit"]),
		mapFromAny(root["rateLimit"]),
		mapFromAny(root["code"]),
		mapFromAny(root["codex"]),
		root,
	}
	for _, limit := range limits {
		if len(limit) == 0 {
			continue
		}
		mergeCodexWindowPayload(headers, "primary", mapFromAny(firstAny(limit, "primary_window", "primaryWindow")))
		mergeCodexWindowPayload(headers, "secondary", mapFromAny(firstAny(limit, "secondary_window", "secondaryWindow")))
	}
}

func mergeCodexWindowPayload(headers map[string][]string, prefix string, window map[string]any) {
	if len(window) == 0 {
		return
	}
	percent := numberStringFromAny(firstAny(window, "used_percent", "usedPercent", "percent", "used"))
	resetAt := resetAtFromWindow(window)
	windowSeconds := int64FromAny(firstAny(window, "limit_window_seconds", "limitWindowSeconds", "window_seconds", "windowSeconds"))
	usedTokens := int64FromAny(firstAny(window, "used_tokens", "usedTokens", "used_token_count", "usedTokenCount", "used_count", "usedCount"))
	remainingTokens := int64FromAny(firstAny(window, "remaining_tokens", "remainingTokens", "remaining_token_count", "remainingTokenCount", "remaining", "remaining_count", "remainingCount", "available_tokens", "availableTokens"))
	limitTokens := int64FromAny(firstAny(window, "limit_tokens", "limitTokens", "quota_tokens", "quotaTokens", "total_tokens", "totalTokens", "limit", "quota", "total"))
	if limitTokens <= 0 && usedTokens > 0 && remainingTokens >= 0 {
		limitTokens = usedTokens + remainingTokens
	}
	if usedTokens <= 0 && limitTokens > 0 && remainingTokens >= 0 {
		usedTokens = limitTokens - remainingTokens
	}
	if remainingTokens < 0 {
		remainingTokens = 0
	}
	if percent != "" {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-used-percent", percent)
	}
	if resetAt > 0 {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-reset-at", strconv.FormatInt(resetAt, 10))
	}
	if windowSeconds > 0 {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-window-minutes", strconv.FormatInt(windowSeconds/60, 10))
	}
	if usedTokens > 0 {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-used-tokens", strconv.FormatInt(usedTokens, 10))
	}
	if remainingTokens >= 0 && (limitTokens > 0 || usedTokens > 0) {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-remaining-tokens", strconv.FormatInt(remainingTokens, 10))
	}
	if limitTokens > 0 {
		setHeaderIfMissing(headers, "x-codex-"+prefix+"-limit-tokens", strconv.FormatInt(limitTokens, 10))
	}
}

func resetAtFromWindow(window map[string]any) int64 {
	if value := int64FromAny(firstAny(window, "reset_at", "resetAt")); value > 0 {
		return normalizeUnixSeconds(value)
	}
	if value := stringFromAny(firstAny(window, "reset_time", "resetTime")); value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed.Unix()
		}
	}
	if value := int64FromAny(firstAny(window, "reset_after_seconds", "resetAfterSeconds", "reset_in", "resetIn")); value > 0 {
		return time.Now().Add(time.Duration(value) * time.Second).Unix()
	}
	return 0
}

func mapFromAny(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

func firstAny(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func numberStringFromAny(value any) string {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	case string:
		v = strings.TrimSpace(v)
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
	}
	return ""
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

func sanitizeTriggerError(value any) string {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return ""
	}
	for _, marker := range []string{"Bearer ", "access_token", "refresh_token", "id_token"} {
		if strings.Contains(text, marker) {
			return "trigger failed"
		}
	}
	if len(text) > 220 {
		text = text[:220]
	}
	return text
}

func classifyCodexBan(headers map[string][]string, primaryPct *float64, primaryReset *int64, secondaryPct *float64, secondaryReset *int64, now int64) (int64, string, string) {
	normalizeInt64Ptr(primaryReset)
	normalizeInt64Ptr(secondaryReset)
	const threshold = 100.0
	primaryFull := primaryPct != nil && *primaryPct >= threshold
	secondaryFull := secondaryPct != nil && *secondaryPct >= threshold
	if primaryFull && secondaryFull {
		if secondaryReset != nil && *secondaryReset > 0 {
			return *secondaryReset, "week", "primary and secondary windows are full"
		}
		if primaryReset != nil && *primaryReset > 0 {
			return *primaryReset, "5h", "both windows full, secondary reset missing"
		}
	}
	if secondaryFull && secondaryReset != nil && *secondaryReset > 0 {
		return *secondaryReset, "week", "secondary weekly window is full"
	}
	if primaryFull && primaryReset != nil && *primaryReset > 0 {
		return *primaryReset, "5h", "primary 5h window is full"
	}
	if primaryReset != nil && *primaryReset > 0 && headerIntValue(headers, "x-codex-primary-window-minutes") == 300 {
		return *primaryReset, "5h", "primary reset header present"
	}
	if secondaryReset != nil && *secondaryReset > 0 && headerIntValue(headers, "x-codex-secondary-window-minutes") == 10080 {
		return *secondaryReset, "week", "secondary reset header present"
	}
	return now + int64((5 * time.Hour).Seconds()), "5h", "fallback: missing x-codex quota headers"
}

func (s *store) pickAuth(ctx context.Context, req schedulerPickRequest) (schedulerPickResponse, error) {
	if !isCodexSchedulerRequest(req) {
		return schedulerPickResponse{Handled: false}, nil
	}
	if len(req.Candidates) == 0 {
		return schedulerPickResponse{Handled: false}, nil
	}
	db, _, err := s.open(ctx)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	now := time.Now().Unix()
	if err := backfillAutobansFromUsage(ctx, db, now); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := expireAutobans(ctx, db, now); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := reconcileAutobansWithQuotaSnapshots(ctx, db, now); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := clearReplacedInvalidAuths(ctx, db); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	bans, err := queryActiveAutobans(ctx, db, now)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if len(bans) == 0 && len(invalids) == 0 {
		return schedulerPickResponse{DelegateBuiltin: "round-robin", Handled: true}, nil
	}
	available := make([]schedulerAuthCandidate, 0, len(req.Candidates))
	filtered := false
	for _, candidate := range req.Candidates {
		if !strings.EqualFold(candidate.Provider, "codex") {
			available = append(available, candidate)
			continue
		}
		if candidateMatchesActiveBan(candidate, bans) {
			filtered = true
			continue
		}
		if candidateMatchesInvalidAuth(candidate, invalids) {
			filtered = true
			continue
		}
		available = append(available, candidate)
	}
	if !filtered {
		return schedulerPickResponse{DelegateBuiltin: "round-robin", Handled: true}, nil
	}
	if len(available) == 0 {
		return schedulerPickResponse{Handled: false}, nil
	}
	chosen := available[0]
	for _, candidate := range available[1:] {
		if candidate.Priority > chosen.Priority {
			chosen = candidate
		}
	}
	return schedulerPickResponse{AuthID: chosen.ID, Handled: true}, nil
}

func isCodexSchedulerRequest(req schedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "codex") {
		return true
	}
	if len(req.Providers) == 1 && strings.EqualFold(strings.TrimSpace(req.Providers[0]), "codex") {
		return true
	}
	return false
}

func expireAutobans(ctx context.Context, db *sql.DB, now int64) error {
	if err := normalizeStoredResetColumns(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE autoban_bans SET active=0 WHERE active=1 AND reset_at <= ?`, now)
	return err
}

func reconcileAutobansWithQuotaSnapshots(ctx context.Context, db *sql.DB, now int64) error {
	bans, err := queryActiveAutobans(ctx, db, now)
	if err != nil {
		return err
	}
	for _, ban := range bans {
		account := accountRow{
			AuthID:    ban.AuthID,
			AuthIndex: ban.AuthIndex,
			Source:    ban.Source,
			Provider:  ban.Provider,
		}
		pp, pr := queryLatestAccountWindowQuota(ctx, db, account, 0, "primary")
		sp, sr := queryLatestAccountWindowQuota(ctx, db, account, 0, "secondary")
		shouldRelease := false
		switch strings.ToLower(strings.TrimSpace(ban.Window)) {
		case "5h", "primary":
			shouldRelease = quotaWindowObserved(pp, pr) && !quotaWindowFull(pp, pr)
		case "week", "7d", "secondary":
			shouldRelease = quotaWindowObserved(sp, sr) && !quotaWindowFull(sp, sr)
		default:
			primaryObserved := quotaWindowObserved(pp, pr)
			secondaryObserved := quotaWindowObserved(sp, sr)
			shouldRelease = (primaryObserved || secondaryObserved) && !quotaWindowFull(pp, pr) && !quotaWindowFull(sp, sr)
		}
		if !shouldRelease {
			continue
		}
		_, err := db.ExecContext(ctx, `
UPDATE autoban_bans
SET active=0,
  primary_used_percent=?,
  primary_reset_at=?,
  secondary_used_percent=?,
  secondary_reset_at=?
WHERE active=1 AND auth_id=?`, nullFloatPtr(pp), nullIntPtr(pr), nullFloatPtr(sp), nullIntPtr(sr), ban.AuthID)
		if err != nil {
			return err
		}
	}
	return nil
}

func backfillAutobansFromUsage(ctx context.Context, db *sql.DB, now int64) error {
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, requested_at, status_code,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
FROM usage_events
WHERE provider='codex'
  AND failed=1
  AND status_code=429
  AND (
    (primary_used_percent >= 100 AND primary_reset_at > ?)
    OR (secondary_used_percent >= 100 AND secondary_reset_at > ?)
  )
ORDER BY requested_at DESC
LIMIT 1000`, now, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var authID, authIndex, source, provider string
		var requestedAt int64
		var status int
		var pp, sp sql.NullFloat64
		var pr, sr sql.NullInt64
		if err := rows.Scan(&authID, &authIndex, &source, &provider, &requestedAt, &status, &pp, &pr, &sp, &sr); err != nil {
			return err
		}
		key := firstNonEmptyString(authID, authIndex, source)
		if key == "" {
			continue
		}
		resetAt, window, reason := classifyStoredCodexBan(pp, pr, sp, sr, now)
		if resetAt <= now {
			continue
		}
		_, err := db.ExecContext(ctx, `
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider=excluded.provider,
  window=excluded.window,
  reason=excluded.reason,
  banned_at=excluded.banned_at,
  reset_at=excluded.reset_at,
  active=CASE WHEN excluded.reset_at > ? THEN 1 ELSE autoban_bans.active END,
  last_status_code=excluded.last_status_code,
  primary_used_percent=excluded.primary_used_percent,
  primary_reset_at=excluded.primary_reset_at,
  secondary_used_percent=excluded.secondary_used_percent,
  secondary_reset_at=excluded.secondary_reset_at
WHERE autoban_bans.active=0 OR excluded.reset_at >= autoban_bans.reset_at`,
			key, authIndex, source, provider, window, reason, requestedAt, resetAt, status,
			nullFloatPtr(pp), nullIntPtr(pr), nullFloatPtr(sp), nullIntPtr(sr), now,
		)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func classifyStoredCodexBan(pp sql.NullFloat64, pr sql.NullInt64, sp sql.NullFloat64, sr sql.NullInt64, now int64) (int64, string, string) {
	if pr.Valid {
		pr.Int64 = normalizeUnixSeconds(pr.Int64)
	}
	if sr.Valid {
		sr.Int64 = normalizeUnixSeconds(sr.Int64)
	}
	primaryFull := pp.Valid && pp.Float64 >= 100 && pr.Valid && pr.Int64 > now
	secondaryFull := sp.Valid && sp.Float64 >= 100 && sr.Valid && sr.Int64 > now
	if primaryFull && secondaryFull {
		if sr.Int64 >= pr.Int64 {
			return sr.Int64, "week", "backfilled: primary and secondary windows are full"
		}
		return pr.Int64, "5h", "backfilled: primary and secondary windows are full"
	}
	if secondaryFull {
		return sr.Int64, "week", "backfilled: secondary weekly window is full"
	}
	if primaryFull {
		return pr.Int64, "5h", "backfilled: primary 5h window is full"
	}
	return now, "", ""
}

func clearReplacedInvalidAuths(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredAuthAccounts()
	if len(configured) == 0 {
		return nil
	}
	for _, cfg := range configured {
		if cfg.AuthFileMTime <= 0 {
			continue
		}
		_, err := db.ExecContext(ctx, `
UPDATE invalid_auths
SET active=0
WHERE active=1
AND auth_file <> ''
AND auth_file = ?
AND ? > invalidated_at`, cfg.AuthFile, cfg.AuthFileMTime)
		if err != nil {
			return err
		}
		for _, alias := range configuredAliases(cfg) {
			if alias == "" {
				continue
			}
			_, err := db.ExecContext(ctx, `
UPDATE invalid_auths
SET active=0
WHERE active=1
AND ? > invalidated_at
AND (
  lower(auth_id)=?
  OR lower(auth_index)=?
  OR lower(source)=?
)`, cfg.AuthFileMTime, alias, alias, alias)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func queryActiveInvalidAuths(ctx context.Context, db *sql.DB) ([]invalidAuthRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, reason, invalidated_at, active, last_status_code, auth_file, auth_file_mtime
FROM invalid_auths
WHERE active=1
ORDER BY invalidated_at DESC
LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []invalidAuthRow
	for rows.Next() {
		var r invalidAuthRow
		var active int
		if err := rows.Scan(&r.AuthID, &r.AuthIndex, &r.Source, &r.Provider, &r.Reason, &r.InvalidatedAt, &active, &r.LastStatusCode, &r.AuthFile, &r.AuthFileMTime); err != nil {
			return nil, err
		}
		r.Active = active != 0
		r.InvalidatedAtText = unixTime(r.InvalidatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func candidateMatchesActiveBan(candidate schedulerAuthCandidate, bans []autobanRow) bool {
	candidateID := trim(candidate.ID)
	authIndex := firstNonEmptyString(candidate.Attributes["auth_index"], stringFromAny(candidate.Metadata["auth_index"]))
	source := firstNonEmptyString(candidate.Attributes["source"], stringFromAny(candidate.Metadata["source"]))
	for _, ban := range bans {
		if candidateID != "" && candidateID == ban.AuthID {
			return true
		}
		if authIndex != "" && authIndex == ban.AuthIndex {
			return true
		}
		if source != "" && source == ban.Source {
			return true
		}
	}
	return false
}

func candidateMatchesInvalidAuth(candidate schedulerAuthCandidate, invalids []invalidAuthRow) bool {
	aliases := normalizeAccountAliases(
		candidate.ID,
		candidate.Attributes["auth_index"],
		candidate.Attributes["source"],
		candidate.Attributes["auth_file"],
		candidate.Attributes["email"],
		stringFromAny(candidate.Metadata["auth_index"]),
		stringFromAny(candidate.Metadata["source"]),
		stringFromAny(candidate.Metadata["auth_file"]),
		stringFromAny(candidate.Metadata["email"]),
	)
	if len(aliases) == 0 {
		return false
	}
	for _, invalid := range invalids {
		for _, invalidAlias := range normalizeAccountAliases(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile) {
			for _, alias := range aliases {
				if alias != "" && alias == invalidAlias {
					return true
				}
			}
		}
	}
	return false
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

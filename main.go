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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	sqlite3 "github.com/mattn/go-sqlite3"
)

const (
	abiVersion            uint32 = 1
	pluginID                     = "codex-token-usage"
	codexQuotaAPIURL             = "https://chatgpt.com/backend-api/wham/usage"
	codexResponsesAPIURL         = "https://chatgpt.com/backend-api/codex/responses/compact"
	codexProbeModel              = "gpt-5.5"
	dbHealthCheckInterval        = 10 * time.Minute
)

var (
	pluginVersion    = "0.1.33"
	pluginAuthor     = "Codex Token Usage Contributors"
	pluginRepository = "https://github.com/zhumengling/codex-token-usage"
)

var globalStore = &store{}
var globalQuotaTrigger = &quotaTriggerManager{}
var globalModelPriceUpdater = &modelPriceUpdateManager{}
var globalRetentionCleaner = &retentionCleaner{}
var globalDBHealth = &dbHealthMonitor{}
var globalSummaryMaintenance = &summaryMaintenanceManager{}
var globalSchedulerDiagnostics = &schedulerDiagnosticsTracker{}
var globalSummaryPrecomputer = &summaryPrecomputeManager{}
var codexQuotaURLOverrideForTest string
var codexResponsesURLOverrideForTest string

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
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
	AccountProtectionEnabled                bool
	AccountProtectionFreeConcurrency        int
	AccountProtectionPlusConcurrency        int
	AccountProtectionK12Concurrency         int
	AccountProtectionTeamConcurrency        int
	AccountProtectionProConcurrency         int
	AccountProtectionFreeTokenLimit         int64
	AccountProtectionPlusTokenLimit         int64
	AccountProtectionK12TokenLimit          int64
	AccountProtectionTeamTokenLimit         int64
	AccountProtectionProTokenLimit          int64
	AccountProtectionTokenWindowSeconds     int
	AccountProtectionReservationTTLSeconds  int
	QuotaTriggerEnabled                     bool
	QuotaTriggerIntervalMinutes             int
	QuotaTriggerMode                        string
	QuotaTriggerMaxConcurrency              int
	QuotaTriggerTimeoutSeconds              int
	QuotaTriggerMinAccountCooldownMinutes   int
	ModelPriceAutoUpdateEnabled             bool
	ModelPriceUpdateIntervalHours           int
	ModelPriceUpdateURL                     string
	ModelPriceUpdateTimeoutSeconds          int
	UsageRetentionDays                      int
	QuotaTriggerRetentionDays               int
	RequestDetailRetentionDays              int
	SummaryPrecomputeEnabled                bool
	SummaryPrecomputeIntervalSeconds        int
	SummaryPrecomputeMode                   string
	SummaryCacheMaxAgeSeconds               int
	SummaryMaintenanceIntervalSeconds       int
	SummaryPrecomputeActiveWindowTTLSeconds int
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

type invalidAuthResolveRequest struct {
	Items []invalidAuthResolveRequestItem `json:"items"`
}

type invalidAuthResolveRequestItem struct {
	AuthID string `json:"auth_id"`
	Action string `json:"action"`
}

type invalidAuthResolveResponse struct {
	Items           []invalidAuthResolveResult `json:"items"`
	Resolved        int                        `json:"resolved"`
	AlreadyResolved int                        `json:"already_resolved"`
	ReplacementKept int                        `json:"replacement_kept"`
	Failed          int                        `json:"failed"`
}

type invalidAuthResolveResult struct {
	AuthID  string `json:"auth_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
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

type schedulerRejectError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *schedulerRejectError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
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
	AuthFile        string              `json:"AuthFile"`
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

const forbiddenInvalidAuthThreshold = 3

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
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
	globalModelPriceUpdater.stop()
	globalRetentionCleaner.stop()
	globalDBHealth.stop()
	globalSummaryMaintenance.stop()
	globalSummaryPrecomputer.stop()
	globalStore.close()
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		ptr := C.CBytes(rawPayload)
		if ptr == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(ptr)
		requestPtr = (*C.uint8_t)(ptr)
	}
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s unavailable, code=%d", method, int(code))
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if code != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(code))
	}
	return append(json.RawMessage(nil), env.Result...), nil
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
				{Method: "GET", Path: "/plugins/codex-token-usage/export", Description: "Token usage CSV/JSON export."},
				{Method: "POST", Path: "/plugins/codex-token-usage/autobans/release", Description: "Manually release active Codex 429 auto-bans."},
				{Method: "POST", Path: "/plugins/codex-token-usage/invalid-auths/resolve", Description: "Resolve deleted, replaced, or runtime-disabled Codex 401 records."},
				{Method: "POST", Path: "/plugins/codex-token-usage/auth-import/preview", Description: "Preview non-standard Codex auth JSON imports."},
				{Method: "POST", Path: "/plugins/codex-token-usage/auth-import/commit", Description: "Convert and save non-standard Codex auth JSON imports."},
			},
			Resources: []resourceRoute{
				{Path: "/dashboard", Menu: "Token Usage", Description: "Account token usage dashboard."},
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
		mustHandle := schedulerPickRequiresPlugin(req)
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()
		resp, err := globalStore.pickAuth(ctx, req)
		if err != nil {
			var reject *schedulerRejectError
			if errors.As(err, &reject) && reject != nil {
				return errorEnvelopeWithStatus(reject.Code, reject.Message, reject.HTTPStatus), nil
			}
			if mustHandle || schedulerPickRequiresPlugin(req) {
				return errorEnvelopeWithStatus(
					"scheduler_unavailable",
					"account protection scheduler is temporarily unavailable",
					http.StatusServiceUnavailable,
				), nil
			}
			return okJSON(schedulerPickResponse{Handled: false})
		}
		return okJSON(resp)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginConfigFields() []configField {
	return []configField{
		{Name: "开启定时额度触发（不建议账号多的情况下开启）", Type: "boolean", Description: "是否开启 Codex 账号定时额度触发。探测结果会参与 401、402、403、429 状态管理。默认关闭。"},
		{Name: "触发间隔分钟", Type: "number", Description: "每轮触发间隔，单位分钟。默认 10。"},
		{Name: "触发模式", Type: "enum", Description: "probe=真实极小模型请求，会消耗少量 token；旧 quota 配置会自动按 probe 执行。默认 probe。"},
		{Name: "最大并发账号数", Type: "number", Description: "每轮最大并发触发账号数。默认 1。"},
		{Name: "单账号超时秒数", Type: "number", Description: "单个账号触发请求超时时间，单位秒。默认 20。"},
		{Name: "单账号最小冷却分钟", Type: "number", Description: "同一账号两次触发的最小冷却时间，单位分钟。默认 10。"},
		{Name: "自动更新模型价格表", Type: "boolean", Description: "是否自动下载并更新 model_prices.json。默认开启。"},
		{Name: "模型价格更新间隔小时", Type: "number", Description: "model_prices.json 自动检查间隔，单位小时。默认 6。"},
		{Name: "模型价格表地址", Type: "string", Description: "模型价格 JSON 下载地址。默认使用 LiteLLM 官方价格表。"},
		{Name: "模型价格更新超时秒数", Type: "number", Description: "下载 model_prices.json 的超时时间，单位秒。默认 20。"},
		{Name: "用量保留天数", Type: "number", Description: "usage_events 保留天数，单位天。默认 90。"},
		{Name: "额度触发记录保留天数", Type: "number", Description: "quota_trigger_runs 保留天数，单位天。默认 30。"},
		{Name: "请求明细保留天数", Type: "number", Description: "请求明细保留天数，当前随 usage_events 保守保留。默认 30。"},
		{Name: "开启后台预计算", Type: "boolean", Description: "是否后台预热常用 summary，减少页面首次等待。默认开启。"},
		{Name: "预计算间隔秒数", Type: "number", Description: "后台 summary 预热间隔，单位秒。默认 30；低占用模式下只检查活跃脏窗口。"},
		{Name: "summary_precompute_mode", Type: "enum", Description: "active_dirty=只刷新活跃且变脏窗口；legacy=按旧逻辑刷新全部默认窗口。默认 active_dirty。"},
		{Name: "summary_cache_max_age_seconds", Type: "number", Description: "summary 缓存直接复用秒数；revision 变化时会先返回短期旧缓存并异步刷新。默认 30。"},
		{Name: "summary_maintenance_interval_seconds", Type: "number", Description: "后台状态维护间隔，单位秒；无数据变化会跳过。默认 180。"},
		{Name: "summary_precompute_active_window_ttl_seconds", Type: "number", Description: "窗口被访问后保留为活跃预计算窗口的时间。默认 120。"},
		{Name: "开启账号保护调度（可能会影响缓存）", Type: "boolean", Description: "按套餐并发保护和 Token 软降级；接管账号选择时可能降低长会话缓存命中率。默认关闭。"},
		{Name: "Free 并发上限", Type: "number", Description: "Free 账号最大同时在途请求数。默认 2。"},
		{Name: "Plus 并发上限", Type: "number", Description: "Plus 或未知套餐账号最大同时在途请求数。默认 5。"},
		{Name: "K12 并发上限", Type: "number", Description: "K12 账号最大同时在途请求数。默认 5。"},
		{Name: "Team 并发上限", Type: "number", Description: "Team 账号最大同时在途请求数。默认 5。"},
		{Name: "Pro 并发上限", Type: "number", Description: "Pro 账号最大同时在途请求数。默认 10。"},
		{Name: "Free 5 分钟 Token 上限", Type: "number", Description: "超过后仅降到候选末尾。默认 2000000。"},
		{Name: "Plus 5 分钟 Token 上限", Type: "number", Description: "超过后仅降到候选末尾。默认 8000000。"},
		{Name: "K12 5 分钟 Token 上限", Type: "number", Description: "超过后仅降到候选末尾。默认 8000000。"},
		{Name: "Team 5 分钟 Token 上限", Type: "number", Description: "超过后仅降到候选末尾。默认 8000000。"},
		{Name: "Pro 5 分钟 Token 上限", Type: "number", Description: "超过后仅降到候选末尾。默认 12000000。"},
		{Name: "账号保护 Token 窗口秒数", Type: "number", Description: "滑动 Token 统计窗口。默认 300 秒。"},
		{Name: "账号保护预约超时秒数", Type: "number", Description: "没有完成回调时自动释放并发名额。默认 900 秒。"},
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
	if strings.HasPrefix(req.Path, "/v0/management/plugins/"+pluginID+"/summary") {
		window := firstQuery(req.Query, "window", "24h")
		limit := parseInt(firstQuery(req.Query, "limit", "50"), 50, 1, 5000)
		forceRefresh := parseBoolString(firstQuery(req.Query, "refresh", "false"), false)
		syncRefresh := forceRefresh && parseBoolString(firstQuery(req.Query, "sync", "false"), false)
		var data map[string]any
		var err error
		if syncRefresh {
			if err := globalStore.runSummaryMaintenance(context.Background()); err != nil {
				return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "summary_failed", "message": err.Error()})
			}
			data, err = globalSummaryPrecomputer.summarySync(context.Background(), globalStore, window, limit)
		} else if forceRefresh {
			data, err = globalSummaryPrecomputer.summaryFresh(context.Background(), globalStore, window, limit)
		} else {
			data, err = globalSummaryPrecomputer.summary(context.Background(), globalStore, window, limit)
		}
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "summary_failed", "message": err.Error()})
		}
		return jsonResponse(http.StatusOK, data)
	}
	if strings.HasPrefix(req.Path, "/v0/management/plugins/"+pluginID+"/export") {
		window := firstQuery(req.Query, "window", "24h")
		limit := parseInt(firstQuery(req.Query, "limit", "5000"), 5000, 1, 20000)
		kind := firstQuery(req.Query, "type", "accounts")
		format := firstQuery(req.Query, "format", "csv")
		return handleExportWithFilters(context.Background(), window, kind, format, limit, req.Query)
	}
	if req.Path == "/v0/management/plugins/"+pluginID+"/autobans/release" {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		}
		var body autobanReleaseRequest
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &body); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": err.Error()})
			}
		}
		db, _, err := globalStore.open(context.Background())
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "release_failed", "message": err.Error()})
		}
		result, err := releaseAutobans(context.Background(), db, body)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "release_failed", "message": err.Error()})
		}
		return jsonResponse(http.StatusOK, result)
	}
	if req.Path == "/v0/management/plugins/"+pluginID+"/invalid-auths/resolve" {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		}
		var body invalidAuthResolveRequest
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": err.Error()})
		}
		if len(body.Items) == 0 || len(body.Items) > 2000 {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "items must contain between 1 and 2000 entries"})
		}
		result, err := globalStore.resolveInvalidAuths(context.Background(), body)
		if err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "resolve_failed", "message": err.Error()})
		}
		return jsonResponse(http.StatusOK, result)
	}
	if req.Path == "/v0/management/plugins/"+pluginID+"/auth-import/preview" {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		}
		return handleAuthImportPreview(req.Body)
	}
	if req.Path == "/v0/management/plugins/"+pluginID+"/auth-import/commit" {
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		}
		return handleAuthImportCommit(req.Body)
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
}

func (s *store) resolveInvalidAuths(ctx context.Context, req invalidAuthResolveRequest) (invalidAuthResolveResponse, error) {
	db, _, err := s.open(ctx)
	if err != nil {
		return invalidAuthResolveResponse{}, err
	}
	// The native auth-file mutation happens before this callback. Drop the
	// plugin-side three-second cache so validation always observes the newest
	// host inventory instead of resurrecting a deleted row.
	globalCodexAuthSource.invalidate()
	inventory, inventoryErr := readCodexHostAuthInventory()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return invalidAuthResolveResponse{}, err
	}
	deactivated := 0
	response := invalidAuthResolveResponse{Items: make([]invalidAuthResolveResult, 0, len(req.Items))}
	for _, item := range req.Items {
		result, changed, err := resolveInvalidAuthItem(ctx, tx, item, inventory, inventoryErr)
		if err != nil {
			_ = tx.Rollback()
			return invalidAuthResolveResponse{}, err
		}
		if changed {
			deactivated++
		}
		response.add(result)
	}
	if err := tx.Commit(); err != nil {
		return invalidAuthResolveResponse{}, err
	}
	if deactivated > 0 {
		// Force concurrent picks to consult SQLite until the refreshed snapshot is
		// available. A refresh error is safe: leaving the cache invalidated keeps
		// the database on the scheduling path.
		globalSchedulerState.invalidate()
		if err := globalSchedulerState.refresh(ctx, db); err != nil {
			globalSchedulerState.invalidate()
		}
	}
	return response, nil
}

func (r *invalidAuthResolveResponse) add(result invalidAuthResolveResult) {
	r.Items = append(r.Items, result)
	switch result.Status {
	case "resolved":
		r.Resolved++
	case "already_resolved":
		r.AlreadyResolved++
	case "replacement_kept":
		r.ReplacementKept++
	default:
		r.Failed++
	}
}

func resolveInvalidAuthItem(ctx context.Context, tx *sql.Tx, item invalidAuthResolveRequestItem, inventory []configuredAccount, inventoryErr error) (invalidAuthResolveResult, bool, error) {
	authID := strings.TrimSpace(item.AuthID)
	result := invalidAuthResolveResult{AuthID: authID}
	if authID == "" {
		result.Status = "failed"
		result.Message = "auth_id is required"
		return result, false, nil
	}
	invalid, err := queryActiveInvalidAuthByID(ctx, tx, authID)
	if errors.Is(err, sql.ErrNoRows) {
		result.Status = "already_resolved"
		return result, false, nil
	}
	if err != nil {
		return invalidAuthResolveResult{}, false, err
	}
	if invalid.LastStatusCode != http.StatusUnauthorized {
		result.Status = "failed"
		result.Message = "only active 401 records can be resolved by this endpoint"
		return result, false, nil
	}
	action := strings.ToLower(strings.TrimSpace(item.Action))
	if action != "file_deleted" && action != "file_absent" && action != "runtime_disabled" {
		result.Status = "failed"
		result.Message = "unsupported action"
		return result, false, nil
	}
	switch action {
	case "file_deleted", "file_absent":
		kind := normalizeAuthSourceKind(invalid.AuthSourceKind)
		if kind == authSourceKindLegacy && fileBackedAuthState(invalid.AuthFile, invalid.AuthID, invalid.AuthIndex) {
			kind = authSourceKindFile
		}
		if kind != authSourceKindFile {
			result.Status = "failed"
			result.Message = "record is not backed by a physical JSON auth file"
			return result, false, nil
		}
		present, currentMTime, stateErr := invalidAuthFileOnDisk(invalid)
		if stateErr != nil {
			result.Status = "failed"
			result.Message = stateErr.Error()
			return result, false, nil
		}
		if present {
			baseline := normalizeAuthFileMTimeMillis(invalid.AuthFileMTime)
			if baseline <= 0 {
				baseline = invalid.InvalidatedAt * 1000
			}
			if currentMTime <= 0 || currentMTime <= baseline {
				result.Status = "still_present"
				result.Message = "the original auth file is still present"
				return result, false, nil
			}
			changed, err := deactivateInvalidAuth401(ctx, tx, invalid.AuthID)
			if err != nil {
				return invalidAuthResolveResult{}, false, err
			}
			if !changed {
				result.Status = "already_resolved"
				return result, false, nil
			}
			result.Status = "replacement_kept"
			result.Message = "a newer auth file was preserved and the old 401 state was cleared"
			return result, true, nil
		}
		changed, err := deactivateInvalidAuth401(ctx, tx, invalid.AuthID)
		if err != nil {
			return invalidAuthResolveResult{}, false, err
		}
		if !changed {
			result.Status = "already_resolved"
			return result, false, nil
		}
		result.Status = "resolved"
		return result, true, nil
	case "runtime_disabled":
		if inventoryErr != nil {
			result.Status = "failed"
			result.Message = "fresh host auth inventory is unavailable: " + sanitizeTriggerError(inventoryErr)
			return result, false, nil
		}
		current, present, ambiguous := currentInvalidAuthInventoryEntry(invalid, inventory)
		if ambiguous {
			changed, err := deactivateInvalidAuth401(ctx, tx, invalid.AuthID)
			if err != nil {
				return invalidAuthResolveResult{}, false, err
			}
			result.Status = "already_resolved"
			result.Message = "stale runtime 401 state was cleared; the current conflicting credential was preserved"
			return result, changed, nil
		}
		kind := effectiveInvalidAuthSourceKind(invalid, current, present)
		if kind != authSourceKindRuntimeOnly {
			result.Status = "failed"
			result.Message = "record is not a runtime-only auth entry"
			return result, false, nil
		}
		if present && !runtimeAuthEntryDisabled(current) {
			result.Status = "still_present"
			result.Message = "runtime auth is still enabled"
			return result, false, nil
		}
		changed, err := deactivateInvalidAuth401(ctx, tx, invalid.AuthID)
		if err != nil {
			return invalidAuthResolveResult{}, false, err
		}
		if !changed {
			result.Status = "already_resolved"
			return result, false, nil
		}
		result.Status = "resolved"
		return result, true, nil
	default:
		result.Status = "failed"
		result.Message = "unsupported action"
		return result, false, nil
	}
}

func invalidAuthFileOnDisk(row invalidAuthRow) (bool, int64, error) {
	authFile := fileNameIfJSON(row.AuthFile)
	if authFile == "" {
		return false, 0, fmt.Errorf("record has no safe physical JSON file name")
	}
	authDir := configuredAuthDir()
	if authDir == "" {
		return false, 0, fmt.Errorf("CPA auth directory is unavailable")
	}
	info, err := os.Stat(filepath.Join(authDir, authFile))
	if err == nil {
		return true, info.ModTime().UnixMilli(), nil
	}
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	return false, 0, fmt.Errorf("inspect auth file: %w", err)
}

func normalizeAuthFileMTimeMillis(value int64) int64 {
	if value <= 0 {
		return 0
	}
	switch {
	case value >= 100000000000000000:
		return value / 1000000
	case value >= 100000000000000:
		return value / 1000
	case value >= 100000000000:
		return value
	default:
		return value * 1000
	}
}

func bestInvalidAuthFileMTimeMillis(authFile string, fallback int64) int64 {
	fallback = normalizeAuthFileMTimeMillis(fallback)
	if fileNameIfJSON(authFile) == "" {
		return fallback
	}
	present, current, err := invalidAuthFileOnDisk(invalidAuthRow{AuthFile: authFile})
	if err == nil && present && current > 0 {
		return current
	}
	return fallback
}

func queryActiveInvalidAuthByID(ctx context.Context, tx *sql.Tx, authID string) (invalidAuthRow, error) {
	var row invalidAuthRow
	var active int
	err := tx.QueryRowContext(ctx, `
SELECT auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_file_mtime, auth_source_kind
FROM invalid_auths
WHERE active=1 AND auth_id=?`, authID).Scan(
		&row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.Reason,
		&row.InvalidatedAt, &active, &row.LastStatusCode, &row.AuthFile,
		&row.AuthFileMTime, &row.AuthSourceKind,
	)
	row.Active = active != 0
	return row, err
}

func deactivateInvalidAuth401(ctx context.Context, tx *sql.Tx, authID string) (bool, error) {
	result, err := tx.ExecContext(ctx, `
UPDATE invalid_auths
SET active=0
WHERE active=1 AND auth_id=? AND last_status_code=?`, authID, http.StatusUnauthorized)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func currentInvalidAuthInventoryEntry(row invalidAuthRow, inventory []configuredAccount) (configuredAccount, bool, bool) {
	if current, ok := matchCodexHostAuthInventoryExact(row, inventory); ok {
		return current, true, false
	}
	matches := make([]configuredAccount, 0, 2)
	for _, candidate := range inventory {
		if invalidAuthInventoryIdentityOverlaps(row, candidate) {
			matches = append(matches, candidate)
		}
	}
	return configuredAccount{}, false, len(matches) > 0
}

func invalidAuthInventoryIdentityOverlaps(row invalidAuthRow, candidate configuredAccount) bool {
	for _, pair := range [][2]string{
		{row.AuthID, candidate.AuthID},
		{row.AuthIndex, candidate.AuthIndex},
	} {
		if left, right := normalizeAccountAlias(pair[0]), normalizeAccountAlias(pair[1]); left != "" && left == right {
			return true
		}
	}
	candidateFile := normalizeAccountAlias(fileNameIfJSON(candidate.AuthFile))
	if candidateFile == "" {
		return false
	}
	for _, value := range []string{row.AuthFile, row.AuthID, row.AuthIndex, row.Source} {
		if normalizeAccountAlias(fileNameIfJSON(value)) == candidateFile {
			return true
		}
	}
	return false
}

func effectiveInvalidAuthSourceKind(row invalidAuthRow, current configuredAccount, present bool) string {
	if present {
		if kind := normalizeAuthSourceKind(current.AuthSourceKind); kind != authSourceKindLegacy {
			return kind
		}
	}
	if kind := normalizeAuthSourceKind(row.AuthSourceKind); kind != authSourceKindLegacy {
		return kind
	}
	if fileBackedAuthState(row.AuthFile, row.AuthID, row.AuthIndex, row.Source) {
		return authSourceKindFile
	}
	return authSourceKindLegacy
}

func runtimeAuthEntryDisabled(account configuredAccount) bool {
	if account.Disabled {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(account.RuntimeStatus)) {
	case "disabled", "inactive":
		return true
	default:
		return false
	}
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
		AccountProtectionEnabled:                false,
		AccountProtectionFreeConcurrency:        2,
		AccountProtectionPlusConcurrency:        5,
		AccountProtectionK12Concurrency:         5,
		AccountProtectionTeamConcurrency:        5,
		AccountProtectionProConcurrency:         10,
		AccountProtectionFreeTokenLimit:         2_000_000,
		AccountProtectionPlusTokenLimit:         8_000_000,
		AccountProtectionK12TokenLimit:          8_000_000,
		AccountProtectionTeamTokenLimit:         8_000_000,
		AccountProtectionProTokenLimit:          12_000_000,
		AccountProtectionTokenWindowSeconds:     300,
		AccountProtectionReservationTTLSeconds:  900,
		QuotaTriggerEnabled:                     false,
		QuotaTriggerIntervalMinutes:             10,
		QuotaTriggerMode:                        "probe",
		QuotaTriggerMaxConcurrency:              1,
		QuotaTriggerTimeoutSeconds:              20,
		QuotaTriggerMinAccountCooldownMinutes:   10,
		ModelPriceAutoUpdateEnabled:             true,
		ModelPriceUpdateIntervalHours:           6,
		ModelPriceUpdateURL:                     defaultModelPriceURL,
		ModelPriceUpdateTimeoutSeconds:          20,
		UsageRetentionDays:                      90,
		QuotaTriggerRetentionDays:               30,
		RequestDetailRetentionDays:              30,
		SummaryPrecomputeEnabled:                true,
		SummaryPrecomputeIntervalSeconds:        30,
		SummaryPrecomputeMode:                   "active_dirty",
		SummaryCacheMaxAgeSeconds:               30,
		SummaryMaintenanceIntervalSeconds:       180,
		SummaryPrecomputeActiveWindowTTLSeconds: 120,
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
	globalAccountProtection.configure(cfg)
	globalQuotaTrigger.configure(cfg)
	globalModelPriceUpdater.configure(cfg)
	globalRetentionCleaner.configure(cfg)
	globalDBHealth.configure(cfg)
	globalSummaryMaintenance.configure(cfg)
	globalSummaryPrecomputer.configure(cfg)
	if err := globalStore.refreshSchedulerState(context.Background()); err != nil {
		globalSchedulerState.invalidate()
	}
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
	if value, ok := configValue(values, "account_protection_enabled", "开启账号保护调度（可能会影响缓存）", "开启账号保护调度"); ok {
		cfg.AccountProtectionEnabled = parseBoolString(value, cfg.AccountProtectionEnabled)
	}
	if value, ok := configValue(values, "account_protection_free_concurrency", "Free 并发上限"); ok {
		cfg.AccountProtectionFreeConcurrency = parseInt(value, cfg.AccountProtectionFreeConcurrency, 1, 100)
	}
	if value, ok := configValue(values, "account_protection_plus_concurrency", "Plus 并发上限"); ok {
		cfg.AccountProtectionPlusConcurrency = parseInt(value, cfg.AccountProtectionPlusConcurrency, 1, 100)
	}
	if value, ok := configValue(values, "account_protection_k12_concurrency", "K12 并发上限"); ok {
		cfg.AccountProtectionK12Concurrency = parseInt(value, cfg.AccountProtectionK12Concurrency, 1, 100)
	}
	if value, ok := configValue(values, "account_protection_team_concurrency", "Team 并发上限"); ok {
		cfg.AccountProtectionTeamConcurrency = parseInt(value, cfg.AccountProtectionTeamConcurrency, 1, 100)
	}
	if value, ok := configValue(values, "account_protection_pro_concurrency", "Pro 并发上限"); ok {
		cfg.AccountProtectionProConcurrency = parseInt(value, cfg.AccountProtectionProConcurrency, 1, 100)
	}
	if value, ok := configValue(values, "account_protection_free_token_limit", "Free 5 分钟 Token 上限"); ok {
		cfg.AccountProtectionFreeTokenLimit = int64(parseInt(value, int(cfg.AccountProtectionFreeTokenLimit), 1, 100_000_000))
	}
	if value, ok := configValue(values, "account_protection_plus_token_limit", "Plus 5 分钟 Token 上限"); ok {
		cfg.AccountProtectionPlusTokenLimit = int64(parseInt(value, int(cfg.AccountProtectionPlusTokenLimit), 1, 100_000_000))
	}
	if value, ok := configValue(values, "account_protection_k12_token_limit", "K12 5 分钟 Token 上限"); ok {
		cfg.AccountProtectionK12TokenLimit = int64(parseInt(value, int(cfg.AccountProtectionK12TokenLimit), 1, 100_000_000))
	}
	if value, ok := configValue(values, "account_protection_team_token_limit", "Team 5 分钟 Token 上限"); ok {
		cfg.AccountProtectionTeamTokenLimit = int64(parseInt(value, int(cfg.AccountProtectionTeamTokenLimit), 1, 100_000_000))
	}
	if value, ok := configValue(values, "account_protection_pro_token_limit", "Pro 5 分钟 Token 上限"); ok {
		cfg.AccountProtectionProTokenLimit = int64(parseInt(value, int(cfg.AccountProtectionProTokenLimit), 1, 100_000_000))
	}
	if value, ok := configValue(values, "account_protection_token_window_seconds", "账号保护 Token 窗口秒数"); ok {
		cfg.AccountProtectionTokenWindowSeconds = parseInt(value, cfg.AccountProtectionTokenWindowSeconds, 30, 3600)
	}
	if value, ok := configValue(values, "account_protection_reservation_ttl_seconds", "账号保护预约超时秒数"); ok {
		cfg.AccountProtectionReservationTTLSeconds = parseInt(value, cfg.AccountProtectionReservationTTLSeconds, 30, 7200)
	}
	if value, ok := configValue(values, "quota_trigger_enabled", "开启定时额度触发（不建议账号多的情况下开启）", "开启定时额度触发"); ok {
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
	if value, ok := configValue(values, "usage_retention_days", "用量保留天数"); ok {
		cfg.UsageRetentionDays = parseInt(value, cfg.UsageRetentionDays, 7, 3650)
	}
	if value, ok := configValue(values, "quota_trigger_retention_days", "额度触发记录保留天数"); ok {
		cfg.QuotaTriggerRetentionDays = parseInt(value, cfg.QuotaTriggerRetentionDays, 1, 3650)
	}
	if value, ok := configValue(values, "request_detail_retention_days", "请求明细保留天数"); ok {
		cfg.RequestDetailRetentionDays = parseInt(value, cfg.RequestDetailRetentionDays, 1, 3650)
	}
	if value, ok := configValue(values, "summary_precompute_enabled", "开启后台预计算"); ok {
		cfg.SummaryPrecomputeEnabled = parseBoolString(value, cfg.SummaryPrecomputeEnabled)
	}
	if value, ok := configValue(values, "summary_precompute_interval_seconds", "预计算间隔秒数"); ok {
		cfg.SummaryPrecomputeIntervalSeconds = parseInt(value, cfg.SummaryPrecomputeIntervalSeconds, 5, 3600)
	}
	if value, ok := configValue(values, "summary_precompute_mode"); ok {
		cfg.SummaryPrecomputeMode = value
	}
	if value, ok := configValue(values, "summary_cache_max_age_seconds"); ok {
		cfg.SummaryCacheMaxAgeSeconds = parseInt(value, cfg.SummaryCacheMaxAgeSeconds, 1, 3600)
	}
	if value, ok := configValue(values, "summary_maintenance_interval_seconds"); ok {
		cfg.SummaryMaintenanceIntervalSeconds = parseInt(value, cfg.SummaryMaintenanceIntervalSeconds, 10, 3600)
	}
	if value, ok := configValue(values, "summary_precompute_active_window_ttl_seconds"); ok {
		cfg.SummaryPrecomputeActiveWindowTTLSeconds = parseInt(value, cfg.SummaryPrecomputeActiveWindowTTLSeconds, 30, 86400)
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
	cfg.AccountProtectionFreeConcurrency = clampInt(cfg.AccountProtectionFreeConcurrency, 1, 100)
	cfg.AccountProtectionPlusConcurrency = clampInt(cfg.AccountProtectionPlusConcurrency, 1, 100)
	cfg.AccountProtectionK12Concurrency = clampInt(cfg.AccountProtectionK12Concurrency, 1, 100)
	cfg.AccountProtectionTeamConcurrency = clampInt(cfg.AccountProtectionTeamConcurrency, 1, 100)
	cfg.AccountProtectionProConcurrency = clampInt(cfg.AccountProtectionProConcurrency, 1, 100)
	cfg.AccountProtectionFreeTokenLimit = int64(clampInt(int(cfg.AccountProtectionFreeTokenLimit), 1, 100_000_000))
	cfg.AccountProtectionPlusTokenLimit = int64(clampInt(int(cfg.AccountProtectionPlusTokenLimit), 1, 100_000_000))
	cfg.AccountProtectionK12TokenLimit = int64(clampInt(int(cfg.AccountProtectionK12TokenLimit), 1, 100_000_000))
	cfg.AccountProtectionTeamTokenLimit = int64(clampInt(int(cfg.AccountProtectionTeamTokenLimit), 1, 100_000_000))
	cfg.AccountProtectionProTokenLimit = int64(clampInt(int(cfg.AccountProtectionProTokenLimit), 1, 100_000_000))
	cfg.AccountProtectionTokenWindowSeconds = clampInt(cfg.AccountProtectionTokenWindowSeconds, 30, 3600)
	cfg.AccountProtectionReservationTTLSeconds = clampInt(cfg.AccountProtectionReservationTTLSeconds, 30, 7200)
	cfg.QuotaTriggerIntervalMinutes = clampInt(cfg.QuotaTriggerIntervalMinutes, 1, 1440)
	cfg.QuotaTriggerMaxConcurrency = clampInt(cfg.QuotaTriggerMaxConcurrency, 1, 32)
	cfg.QuotaTriggerTimeoutSeconds = clampInt(cfg.QuotaTriggerTimeoutSeconds, 3, 300)
	cfg.QuotaTriggerMinAccountCooldownMinutes = clampInt(cfg.QuotaTriggerMinAccountCooldownMinutes, 1, 1440)
	cfg.ModelPriceUpdateIntervalHours = clampInt(cfg.ModelPriceUpdateIntervalHours, 1, 168)
	cfg.ModelPriceUpdateTimeoutSeconds = clampInt(cfg.ModelPriceUpdateTimeoutSeconds, 3, 300)
	cfg.UsageRetentionDays = clampInt(cfg.UsageRetentionDays, 7, 3650)
	cfg.QuotaTriggerRetentionDays = clampInt(cfg.QuotaTriggerRetentionDays, 1, 3650)
	cfg.RequestDetailRetentionDays = clampInt(cfg.RequestDetailRetentionDays, 1, 3650)
	cfg.SummaryPrecomputeIntervalSeconds = clampInt(cfg.SummaryPrecomputeIntervalSeconds, 5, 3600)
	cfg.SummaryCacheMaxAgeSeconds = clampInt(cfg.SummaryCacheMaxAgeSeconds, 1, 3600)
	cfg.SummaryMaintenanceIntervalSeconds = clampInt(cfg.SummaryMaintenanceIntervalSeconds, 10, 3600)
	cfg.SummaryPrecomputeActiveWindowTTLSeconds = clampInt(cfg.SummaryPrecomputeActiveWindowTTLSeconds, 30, 86400)
	precomputeMode := strings.ToLower(strings.TrimSpace(cfg.SummaryPrecomputeMode))
	switch precomputeMode {
	case "", "active", "dirty", "active-dirty", "active_dirty":
		precomputeMode = "active_dirty"
	case "legacy", "all", "full":
		precomputeMode = "legacy"
	default:
		precomputeMode = "active_dirty"
	}
	cfg.SummaryPrecomputeMode = precomputeMode
	if strings.TrimSpace(cfg.ModelPriceUpdateURL) == "" {
		cfg.ModelPriceUpdateURL = defaultModelPriceURL
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.QuotaTriggerMode))
	switch mode {
	case "探测请求", "真实请求", "真实探测", "probe模式", "probe 模式":
		mode = "probe"
	case "quota", "quota mode", "只查询额度", "查询额度", "额度查询", "quota模式", "quota 模式":
		mode = "probe"
	}
	if mode != "probe" {
		mode = "probe"
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
	mu       sync.Mutex
	repairMu sync.Mutex
	db       *sql.DB
	dbPath   string
}

func (s *store) open(ctx context.Context) (*sql.DB, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, s.dbPath, nil
	}
	path, err := usageDBPath()
	if err != nil {
		return nil, "", err
	}
	db, err := openSQLiteDB(path)
	if err != nil {
		return nil, "", err
	}
	if err := initializeSQLiteStore(ctx, db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	s.db = db
	s.dbPath = path
	return db, path, nil
}

func openSQLiteDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
}

func initializeSQLiteStore(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	if err := ensureSummaryCacheColumns(ctx, db); err != nil {
		return err
	}
	if err := normalizeStoredResetColumns(ctx, db); err != nil {
		return err
	}
	if err := ensureUsageEventColumns(ctx, db); err != nil {
		return err
	}
	if err := normalizeStoredLatencyColumns(ctx, db); err != nil {
		return err
	}
	if err := ensureQuotaTriggerRunColumns(ctx, db); err != nil {
		return err
	}
	if err := ensureAutobanBanColumns(ctx, db); err != nil {
		return err
	}
	if err := ensureInvalidAuthColumns(ctx, db); err != nil {
		return err
	}
	return nil
}

func ensureSummaryCacheColumns(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		def  string
	}{
		{"revision", "TEXT NOT NULL DEFAULT ''"},
	}
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(summary_cache)`)
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
		if _, err := db.ExecContext(ctx, `ALTER TABLE summary_cache ADD COLUMN `+column.name+` `+column.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func withSQLiteAutoRepair[T any](ctx context.Context, s *store, operation string, fn func() (T, error)) (T, error) {
	value, err := fn()
	if !isSQLiteCorruptionError(err) {
		return value, err
	}
	if repairErr := s.repairSQLiteDatabase(ctx); repairErr != nil {
		var zero T
		return zero, fmt.Errorf("%s failed with sqlite corruption (%w); automatic repair failed: %v", operation, err, repairErr)
	}
	value, retryErr := fn()
	if retryErr != nil {
		return value, fmt.Errorf("%s failed after automatic sqlite repair: %w", operation, retryErr)
	}
	return value, nil
}

func isSQLiteCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code == sqlite3.ErrCorrupt || sqliteErr.Code == sqlite3.ErrNotADB {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database disk image is malformed") ||
		strings.Contains(text, "database schema is corrupt") ||
		strings.Contains(text, "file is not a database") ||
		(strings.Contains(text, "database") && strings.Contains(text, "malformed")) ||
		(strings.Contains(text, "database") && strings.Contains(text, "corrupt"))
}

func (s *store) repairSQLiteDatabase(ctx context.Context) error {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	db, closeAfter, err := s.repairDBHandle()
	if err != nil {
		return err
	}
	if closeAfter {
		defer db.Close()
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=30000`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `REINDEX`); err != nil {
		return err
	}
	problems, err := sqliteIntegrityProblems(ctx, db, 5)
	if err != nil {
		return err
	}
	if sqliteIntegrityOK(problems) {
		return nil
	}
	if len(problems) == 0 {
		return errors.New("integrity_check returned no rows after reindex")
	}
	return fmt.Errorf("integrity_check still reports: %s", strings.Join(problems, "; "))
}

func sqliteIntegrityProblems(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	return sqliteCheckProblems(ctx, db, `PRAGMA integrity_check`, limit)
}

func sqliteQuickCheckProblems(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	return sqliteCheckProblems(ctx, db, `PRAGMA quick_check`, limit)
}

func sqliteCheckProblems(ctx context.Context, db *sql.DB, query string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var problem string
		if err := rows.Scan(&problem); err != nil {
			return nil, err
		}
		problems = append(problems, problem)
		if len(problems) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return problems, nil
}

func sqliteIntegrityOK(problems []string) bool {
	return len(problems) == 1 && strings.EqualFold(strings.TrimSpace(problems[0]), "ok")
}

func singleFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *store) repairDBHandle() (*sql.DB, bool, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db, false, nil
	}
	path, err := usageDBPath()
	if err != nil {
		return nil, false, err
	}
	db, err = openSQLiteDB(path)
	if err != nil {
		return nil, false, err
	}
	return db, true, nil
}

type dbHealthState struct {
	Status               string `json:"status"`
	LastCheckAt          string `json:"last_check_at,omitempty"`
	LastRepairAt         string `json:"last_repair_at,omitempty"`
	LastCheckpointAt     string `json:"last_checkpoint_at,omitempty"`
	LastDurationMs       int64  `json:"last_duration_ms"`
	LastError            string `json:"last_error,omitempty"`
	LastQuickCheckResult string `json:"last_quick_check_result,omitempty"`
	WALBytes             int64  `json:"wal_bytes,omitempty"`
	RepairCount          int64  `json:"repair_count"`
	CheckpointCount      int64  `json:"checkpoint_count"`
}

type dbHealthMonitor struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	state  dbHealthState
}

func (m *dbHealthMonitor) configure(cfg pluginConfig) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.state.Status == "" {
		m.state.Status = "unknown"
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()
	go m.loop(ctx)
}

func (m *dbHealthMonitor) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
}

func (m *dbHealthMonitor) status() dbHealthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *dbHealthMonitor) loop(ctx context.Context) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.run(ctx)
			timer.Reset(dbHealthCheckInterval)
		}
	}
}

func (m *dbHealthMonitor) run(ctx context.Context) {
	started := time.Now()
	status := "ok"
	var errText string
	var quickResult string
	var walBytes int64
	repaired := false
	checkpointed := false
	db, closeAfter, err := globalStore.repairDBHandle()
	if err != nil {
		status = "error"
		errText = sanitizeTriggerError(err)
	} else {
		if closeAfter {
			defer db.Close()
		}
		problems, checkErr := sqliteQuickCheckProblems(ctx, db, 5)
		quickResult = strings.Join(problems, "; ")
		if checkErr != nil {
			if isSQLiteCorruptionError(checkErr) {
				repaired = true
				if repairErr := globalStore.repairSQLiteDatabase(ctx); repairErr != nil {
					status = "repair_failed"
					errText = sanitizeTriggerError(repairErr)
				}
			} else {
				status = "error"
				errText = sanitizeTriggerError(checkErr)
			}
		} else if !sqliteIntegrityOK(problems) {
			integrityProblems, integrityErr := sqliteIntegrityProblems(ctx, db, 5)
			if integrityErr != nil {
				status = "error"
				errText = sanitizeTriggerError(integrityErr)
			} else if !sqliteIntegrityOK(integrityProblems) {
				repaired = true
				if repairErr := globalStore.repairSQLiteDatabase(ctx); repairErr != nil {
					status = "repair_failed"
					errText = sanitizeTriggerError(repairErr)
				}
			}
		}
		if path, pathErr := usageDBPath(); pathErr == nil {
			walBytes = singleFileSize(path + "-wal")
			if walBytes > envInt64("CPA_DB_WAL_CHECKPOINT_BYTES", 256*1024*1024) {
				if _, checkpointErr := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); checkpointErr != nil {
					if errText == "" {
						errText = sanitizeTriggerError(checkpointErr)
					}
				} else {
					checkpointed = true
					walBytes = singleFileSize(path + "-wal")
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.LastCheckAt = time.Now().Format(time.RFC3339)
	m.state.LastDurationMs = time.Since(started).Milliseconds()
	m.state.LastQuickCheckResult = quickResult
	m.state.LastError = errText
	m.state.Status = status
	m.state.WALBytes = walBytes
	if repaired && errText == "" {
		m.state.Status = "repaired"
		m.state.LastRepairAt = time.Now().Format(time.RFC3339)
		m.state.RepairCount++
	}
	if checkpointed {
		m.state.LastCheckpointAt = time.Now().Format(time.RFC3339)
		m.state.CheckpointCount++
	}
}

type summaryMaintenanceState struct {
	Running                        bool   `json:"running"`
	LastRunStarted                 string `json:"last_run_started_at,omitempty"`
	LastRunAt                      string `json:"last_run_at,omitempty"`
	LastDurationMs                 int64  `json:"last_duration_ms"`
	LastError                      string `json:"last_error,omitempty"`
	SkippedReason                  string `json:"skipped_reason,omitempty"`
	LastMode                       string `json:"last_mode,omitempty"`
	LastRevision                   string `json:"last_revision,omitempty"`
	LastProcessedUsageEventID      int64  `json:"last_processed_usage_event_id"`
	LastProcessedQuotaTriggerID    int64  `json:"last_processed_quota_trigger_id"`
	LastProcessedAuthFilesRevision string `json:"last_processed_auth_files_revision,omitempty"`
}

type summaryMaintenanceManager struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	cfg    pluginConfig
	state  summaryMaintenanceState
}

func (m *summaryMaintenanceManager) configure(cfg pluginConfig) {
	cfg = normalizePluginConfig(cfg)
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.cfg = cfg
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()
	go m.loop(ctx, cfg)
}

func (m *summaryMaintenanceManager) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
}

func (m *summaryMaintenanceManager) status() summaryMaintenanceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *summaryMaintenanceManager) loop(ctx context.Context, cfg pluginConfig) {
	m.run(ctx)
	interval := time.Duration(maxInt(cfg.SummaryMaintenanceIntervalSeconds, 10)) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.run(ctx)
		}
	}
}

func (m *summaryMaintenanceManager) run(ctx context.Context) {
	started := time.Now()
	m.mu.Lock()
	if m.state.Running {
		m.mu.Unlock()
		return
	}
	m.state.Running = true
	m.state.LastRunStarted = started.Format(time.RFC3339)
	m.mu.Unlock()

	revision, revErr := globalStore.currentRevision(ctx)
	if revErr == nil {
		m.mu.Lock()
		unchanged := m.state.LastRevision != "" && m.state.LastRevision == revision.Revision
		resetDue := storeRevisionResetDue(revision, time.Now().Unix())
		if unchanged {
			if resetDue {
				m.mu.Unlock()
			} else {
				m.state.Running = false
				m.state.LastRunAt = time.Now().Format(time.RFC3339)
				m.state.LastDurationMs = time.Since(started).Milliseconds()
				m.state.LastError = ""
				m.state.SkippedReason = "unchanged"
				m.state.LastMode = "skip"
				m.state.LastProcessedUsageEventID = revision.UsageMaxID
				m.state.LastProcessedQuotaTriggerID = revision.QuotaMaxID
				m.state.LastProcessedAuthFilesRevision = revision.AuthFilesRevision
				m.mu.Unlock()
				return
			}
		} else {
			m.mu.Unlock()
		}
	}

	mode := "full"
	if revErr == nil && m.lightMaintenanceEnough(revision) {
		mode = "light"
	}
	err := globalStore.runSummaryMaintenanceMode(ctx, mode)
	if err == nil {
		if after, afterErr := globalStore.currentRevision(ctx); afterErr == nil {
			revision = after
			revErr = nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Running = false
	m.state.LastRunAt = time.Now().Format(time.RFC3339)
	m.state.LastDurationMs = time.Since(started).Milliseconds()
	m.state.SkippedReason = ""
	m.state.LastMode = mode
	if err != nil && !errors.Is(err, context.Canceled) {
		m.state.LastError = sanitizeTriggerError(err)
	} else {
		m.state.LastError = ""
	}
	if err == nil && revErr == nil {
		m.state.LastRevision = revision.Revision
		m.state.LastProcessedUsageEventID = revision.UsageMaxID
		m.state.LastProcessedQuotaTriggerID = revision.QuotaMaxID
		m.state.LastProcessedAuthFilesRevision = revision.AuthFilesRevision
	}
}

func (m *summaryMaintenanceManager) lightMaintenanceEnough(revision storeRevision) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.LastRevision == "" {
		return false
	}
	if m.state.LastProcessedAuthFilesRevision != "" && m.state.LastProcessedAuthFilesRevision != revision.AuthFilesRevision {
		return false
	}
	return true
}

func storeRevisionResetDue(revision storeRevision, now int64) bool {
	return (revision.NextBanResetAt > 0 && revision.NextBanResetAt <= now) ||
		(revision.NextXAIResetAt > 0 && revision.NextXAIResetAt <= now)
}

func (s *store) runSummaryMaintenance(ctx context.Context) error {
	return s.runSummaryMaintenanceMode(ctx, "full")
}

func (s *store) runSummaryMaintenanceMode(ctx context.Context, mode string) error {
	_, err := withSQLiteAutoRepair(ctx, s, "summary maintenance", func() (struct{}, error) {
		db, _, err := s.open(ctx)
		if err != nil {
			return struct{}{}, err
		}
		now := time.Now().Unix()
		if err := expireAutobans(ctx, db, now); err != nil {
			return struct{}{}, err
		}
		if err := expireXAIStates(ctx, db, now); err != nil {
			return struct{}{}, err
		}
		if err := backfillAutobansFromUsage(ctx, db, now); err != nil {
			return struct{}{}, err
		}
		if err := backfillWorkspaceDeactivatedAuthsFromUsage(ctx, db); err != nil {
			return struct{}{}, err
		}
		if err := backfillWorkspaceDeactivatedAuthsFromQuotaTriggerRuns(ctx, db); err != nil {
			return struct{}{}, err
		}
		if err := reconcileAutobansWithQuotaSnapshots(ctx, db, now); err != nil {
			return struct{}{}, err
		}
		if err := clearRecoveredAuthStatesFromUsage(ctx, db); err != nil {
			return struct{}{}, err
		}
		if strings.EqualFold(mode, "light") {
			if err := globalSchedulerState.refresh(ctx, db); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		}
		if err := reconcileInvalidAuthSourceKinds(ctx, db); err != nil {
			return struct{}{}, err
		}
		if err := clearReplacedInvalidAuths(ctx, db); err != nil {
			return struct{}{}, err
		}
		if err := clearReplacedAutobans(ctx, db); err != nil {
			return struct{}{}, err
		}
		if err := clearReplacedOrMissingXAIStates(ctx, db); err != nil {
			return struct{}{}, err
		}
		configuredAccounts := readConfiguredAuthAccounts()
		if err := clearMissingConfiguredAuthState(ctx, db, configuredAccounts, globalCodexAuthSource.authoritative()); err != nil {
			return struct{}{}, err
		}
		if err := globalSchedulerState.refresh(ctx, db); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
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
		`UPDATE usage_events SET latency_ms = ttft_ms WHERE ttft_ms > 0 AND (latency_ms <= 0 OR latency_ms < ttft_ms)`,
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

func ensureAutobanBanColumns(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		def  string
	}{
		{"auth_file", "TEXT NOT NULL DEFAULT ''"},
		{"auth_file_mtime", "INTEGER NOT NULL DEFAULT 0"},
		{"released_at", "INTEGER NOT NULL DEFAULT 0"},
		{"release_reason", "TEXT NOT NULL DEFAULT ''"},
	}
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(autoban_bans)`)
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
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE autoban_bans ADD COLUMN `+column.name+` `+column.def); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_autoban_bans_auth_file ON autoban_bans(auth_file, active)`)
	return err
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
	interval := time.Duration(cfg.QuotaTriggerIntervalMinutes) * time.Minute
	for {
		m.runRound(ctx, cfg)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			m.mu.Lock()
			if m.cancel != nil {
				m.state.Running = false
			}
			m.mu.Unlock()
			return
		case <-timer.C:
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
	if err != nil && !errors.Is(err, context.Canceled) {
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
	latencyMs := durationToMilliseconds(rec.Latency)
	ttftMs := durationToMilliseconds(rec.TTFT)
	if ttftMs > 0 && (latencyMs <= 0 || latencyMs < ttftMs) {
		latencyMs = ttftMs
	}
	_, err = db.ExecContext(ctx, insertSQL,
		rec.RequestedAt.Unix(),
		trim(rec.Provider), trim(rec.ExecutorType), trim(rec.Model), trim(rec.Alias),
		trim(rec.APIKey), trim(rec.AuthID), trim(rec.AuthIndex), trim(rec.AuthType), trim(rec.Source),
		trim(rec.ReasoningEffort), trim(rec.ServiceTier), latencyMs, ttftMs, boolInt(rec.Failed), status,
		rec.Detail.InputTokens, rec.Detail.OutputTokens, rec.Detail.ReasoningTokens, rec.Detail.CachedTokens,
		rec.Detail.CacheReadTokens, rec.Detail.CacheCreationTokens, total,
		primaryPct, primaryReset, secondaryPct, secondaryReset,
	)
	if err != nil {
		return err
	}
	if err := releaseProtectionReservation(ctx, db, rec); err != nil {
		return err
	}
	if err := recordXAIStateIfNeeded(ctx, db, rec, status); err != nil {
		return err
	}
	if err := recordInvalidAuthIfNeeded(ctx, db, rec, status); err != nil {
		return err
	}
	if err := recordRepeatedForbiddenIfNeeded(ctx, db, rec, status); err != nil {
		return err
	}
	if err := clearRecoveredAuthStateIfNeeded(ctx, db, rec, status); err != nil {
		return err
	}
	if err := recordAutobanIfNeeded(ctx, db, rec, status, primaryPct, primaryReset, secondaryPct, secondaryReset); err != nil {
		return err
	}
	return nil
}

func recordInvalidAuthIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	if status != http.StatusUnauthorized && status != http.StatusPaymentRequired {
		return nil
	}
	account, ok := exactInvalidAuthAccountForRecord(rec)
	if !ok {
		// A 401 must never turn an email/display name into a guessed file
		// deletion target. If the current CPA inventory cannot resolve one unique
		// stable identity, keep the usage event but do not create an invalid row.
		return nil
	}
	kind := normalizeAuthSourceKind(account.AuthSourceKind)
	reason := invalidAuthReasonForRecord(rec, status)
	if status == http.StatusPaymentRequired && reason == "" && kind == authSourceKindFile {
		reason = "402 deactivated_workspace: team workspace is deactivated"
	}
	if reason == "" {
		return nil
	}
	authID := firstNonEmptyString(account.AuthID, account.AuthIndex, account.AuthFile)
	if authID == "" || kind == authSourceKindLegacy {
		return nil
	}
	rec.AuthID = authID
	rec.AuthIndex = firstNonEmptyString(account.AuthIndex, rec.AuthIndex)
	rec.Source = firstNonEmptyString(account.Source, rec.Source)
	rec.Provider = "codex"
	invalidatedAt := rec.RequestedAt.Unix()
	if invalidatedAt <= 0 {
		invalidatedAt = time.Now().Unix()
	}
	return upsertInvalidAuth(ctx, db, rec, status, reason, authID, account.AuthFile, account.AuthFileMTime, kind, invalidatedAt)
}

func exactInvalidAuthAccountForRecord(rec usageRecord) (configuredAccount, bool) {
	inventory, err := readCodexHostAuthInventory()
	if err != nil {
		// The filesystem fallback remains exact because only explicit JSON names
		// and auth indexes participate; email and Source are never selectors.
		inventory = configuredCodexFileInventory(readConfiguredAuthFiles())
	}
	probe := invalidAuthRow{
		AuthID:    trim(rec.AuthID),
		AuthIndex: trim(rec.AuthIndex),
		AuthFile: firstNonEmptyString(
			fileNameIfJSON(rec.AuthFile),
			fileNameIfJSON(rec.AuthID),
			fileNameIfJSON(rec.AuthIndex),
		),
	}
	account, ok := matchCodexHostAuthInventoryExact(probe, inventory)
	if ok && isCodexAuthProvider(account.Provider) {
		return account, true
	}
	if runtime, runtimeErr := readCodexRuntimeAuth(rec.AuthIndex); runtimeErr == nil {
		if matched, matchedOK := matchCodexHostAuthInventoryExact(probe, []configuredAccount{runtime}); matchedOK {
			return matched, true
		}
	}
	return configuredAccount{}, false
}

func upsertInvalidAuth(ctx context.Context, db *sql.DB, rec usageRecord, status int, reason, authID, authFile string, authFileMTime int64, sourceKind string, invalidatedAt int64) error {
	authFileMTime = bestInvalidAuthFileMTimeMillis(authFile, authFileMTime)
	globalSchedulerState.beginRestrictionWrite("codex")
	_, err := db.ExecContext(ctx, `
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_file_mtime, auth_source_kind
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider=excluded.provider,
  reason=excluded.reason,
  invalidated_at=excluded.invalidated_at,
  active=1,
	last_status_code=excluded.last_status_code,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime,
  auth_source_kind=excluded.auth_source_kind`,
		trim(authID), trim(rec.AuthIndex), trim(rec.Source), trim(rec.Provider),
		reason, invalidatedAt, status, authFile, authFileMTime, normalizeAuthSourceKind(sourceKind),
	)
	globalSchedulerState.finishRestrictionWrite("codex")
	if err != nil {
		return err
	}
	return nil
}

func invalidAuthIDForRecord(rec usageRecord, authFile string) string {
	if authFile != "" && shouldUseStrictAuthFileIdentity(rec, authFile) {
		return authFile
	}
	return firstNonEmptyString(rec.AuthID, rec.AuthIndex, rec.Source)
}

func shouldUseStrictAuthFileIdentity(rec usageRecord, authFile string) bool {
	if authFile == "" {
		return false
	}
	if fileNameIfJSON(rec.AuthID) != "" || fileNameIfJSON(rec.Source) != "" {
		return true
	}
	email := normalizeAccountAlias(firstNonEmptyString(rec.Source, rec.AuthID))
	if email == "" {
		return false
	}
	return configuredEmailCounts(readConfiguredAuthAccounts())[email] > 1
}

func recordRepeatedForbiddenIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	if !rec.Failed || status != http.StatusForbidden {
		return nil
	}
	account, ok := exactInvalidAuthAccountForRecord(rec)
	if !ok {
		return nil
	}
	kind := normalizeAuthSourceKind(account.AuthSourceKind)
	if kind == authSourceKindLegacy {
		return nil
	}
	aliases := normalizeAccountAliases(account.AuthFile, account.AuthID, account.AuthIndex)
	if len(aliases) == 0 || !recentForbiddenThresholdReached(ctx, db, aliases, forbiddenInvalidAuthThreshold) {
		return nil
	}
	authID := firstNonEmptyString(account.AuthID, account.AuthIndex, account.AuthFile)
	if authID == "" {
		return nil
	}
	rec.AuthID = authID
	rec.AuthIndex = firstNonEmptyString(account.AuthIndex, rec.AuthIndex)
	rec.Source = firstNonEmptyString(account.Source, rec.Source)
	rec.Provider = "codex"
	reason := "403 forbidden: repeated failures for this credential/workspace"
	return upsertInvalidAuth(ctx, db, rec, status, reason, authID, account.AuthFile, account.AuthFileMTime, kind, time.Now().Unix())
}

func invalidAuthReasonForRecord(rec usageRecord, status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "401 unauthorized: credential is invalid"
	case http.StatusPaymentRequired:
		if codexWorkspaceDeactivatedBody(rec.Failure.Body) {
			return "402 deactivated_workspace: team workspace is deactivated"
		}
		return "402 payment required: account or workspace is unavailable"
	}
	return ""
}

func codexWorkspaceDeactivatedBody(body string) bool {
	return strings.Contains(strings.ToLower(body), "deactivated_workspace")
}

func clearRecoveredAuthStateIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	if rec.Failed || !successfulStatusCode(status) {
		return nil
	}
	if err := clearRecoveredInvalidAuthForRecord(ctx, db, rec); err != nil {
		return err
	}
	aliases, strict := recoveryMatchAliasesForRecord(rec)
	for _, alias := range aliases {
		if strict {
			if _, err := db.ExecContext(ctx, `
UPDATE autoban_bans
SET active=0
WHERE active=1
AND (
  lower(auth_id)=?
  OR lower(auth_index)=?
  OR lower(auth_file)=?
)`, alias, alias, alias); err != nil {
				return err
			}
			continue
		}
		if _, err := db.ExecContext(ctx, `
UPDATE autoban_bans
SET active=0
WHERE active=1
AND (
  lower(auth_id)=?
  OR lower(auth_index)=?
  OR lower(source)=?
)`, alias, alias, alias); err != nil {
			return err
		}
	}
	return nil
}

func clearRecoveredInvalidAuthForRecord(ctx context.Context, db *sql.DB, rec usageRecord) error {
	authIndex := normalizeAccountAlias(rec.AuthIndex)
	authID := normalizeAccountAlias(rec.AuthID)
	fileAliases := normalizeAccountAliases(
		fileNameIfJSON(rec.AuthFile),
		fileNameIfJSON(rec.AuthID),
		fileNameIfJSON(rec.AuthIndex),
	)
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 8)
	if authIndex != "" {
		conditions = append(conditions, "lower(auth_index)=?")
		args = append(args, authIndex)
	}
	if authID != "" && !strings.Contains(authID, "@") {
		conditions = append(conditions, "lower(auth_id)=?")
		args = append(args, authID)
	}
	if len(fileAliases) > 0 {
		condition, conditionArgs := sqlLowerInCondition([]string{"auth_file", "auth_id", "auth_index"}, fileAliases)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}
	if len(conditions) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `
UPDATE invalid_auths
SET active=0
WHERE active=1 AND (`+strings.Join(conditions, " OR ")+`)`, args...)
	return err
}

func recoveryMatchAliasesForRecord(rec usageRecord) ([]string, bool) {
	if fileNameIfJSON(rec.AuthFile) != "" {
		aliases := fileBackedCleanupAliases(rec.AuthID, rec.AuthIndex, rec.Source, rec.AuthFile)
		return aliases, len(aliases) > 0
	}
	authFile, _ := authFileStateForRecord(rec)
	email := normalizeAccountAlias(firstNonEmptyString(rec.Source, rec.AuthID))
	strict := authFile != "" && email != "" && configuredEmailCounts(readConfiguredAuthAccounts())[email] > 1
	if strict {
		aliases := strictAuthStateAliasesForValues(rec.AuthID, rec.AuthIndex, rec.Source, authFile)
		return aliases, len(aliases) > 0
	}
	return normalizeAccountAliases(rec.AuthID, rec.AuthIndex, rec.Source), false
}

func successfulStatusCode(status int) bool {
	return status == 0 || (status >= 200 && status < 300)
}

func codexAuthRecordLooksFileBacked(rec usageRecord) bool {
	if authFile, _ := authFileStateForRecord(rec); authFile != "" {
		return true
	}
	return fileBackedAuthState(rec.AuthID, rec.AuthIndex, rec.Source)
}

func recordAutobanIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int, primaryPct *float64, primaryReset *int64, secondaryPct *float64, secondaryReset *int64) error {
	if !strings.EqualFold(trim(rec.Provider), "codex") {
		return nil
	}
	if !rec.Failed || status != http.StatusTooManyRequests {
		return nil
	}
	authFile, authFileMTime := authFileStateForRecord(rec)
	authID := invalidAuthIDForRecord(rec, authFile)
	if authID == "" {
		return nil
	}
	now := time.Now().Unix()
	normalizeInt64Ptr(primaryReset)
	normalizeInt64Ptr(secondaryReset)
	resetAt, window, reason := classifyCodexBan(rec.ResponseHeaders, primaryPct, primaryReset, secondaryPct, secondaryReset, now)
	globalSchedulerState.beginRestrictionWrite("codex")
	_, err := db.ExecContext(ctx, `
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at,
  auth_file, auth_file_mtime, released_at, release_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 0, '')
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
  secondary_reset_at=excluded.secondary_reset_at,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime,
  released_at=0,
  release_reason=''`,
		trim(authID), trim(rec.AuthIndex), trim(rec.Source), trim(rec.Provider), window, reason, now, resetAt, status,
		primaryPct, primaryReset, secondaryPct, secondaryReset, authFile, authFileMTime,
	)
	globalSchedulerState.finishRestrictionWrite("codex")
	if err != nil {
		return err
	}
	return nil
}

type autobanReleaseRequest struct {
	Scope string               `json:"scope"`
	Items []autobanReleaseItem `json:"items"`
}

type autobanReleaseItem struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Source    string `json:"source"`
	AuthFile  string `json:"auth_file"`
}

type autobanReleaseResult struct {
	Released int                    `json:"released"`
	Skipped  int                    `json:"skipped"`
	NotFound int                    `json:"not_found"`
	Items    []autobanReleaseDetail `json:"items,omitempty"`
}

type autobanReleaseDetail struct {
	AuthID    string `json:"auth_id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	Source    string `json:"source,omitempty"`
	AuthFile  string `json:"auth_file,omitempty"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

func releaseAutobans(ctx context.Context, db *sql.DB, req autobanReleaseRequest) (autobanReleaseResult, error) {
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope == "" {
		scope = "selected"
	}
	now := time.Now().Unix()
	switch scope {
	case "all429":
		return releaseAll429Autobans(ctx, db, now)
	case "selected":
		return releaseSelectedAutobans(ctx, db, req.Items, now)
	default:
		return autobanReleaseResult{}, fmt.Errorf("unsupported release scope %q", req.Scope)
	}
}

func releaseAll429Autobans(ctx context.Context, db *sql.DB, now int64) (autobanReleaseResult, error) {
	rows, err := queryActiveAutobanReleaseRows(ctx, db)
	if err != nil {
		return autobanReleaseResult{}, err
	}
	var result autobanReleaseResult
	for _, row := range rows {
		detail := autobanReleaseDetail{
			AuthID:    row.AuthID,
			AuthIndex: row.AuthIndex,
			Source:    row.Source,
			AuthFile:  row.AuthFile,
		}
		if !isReleasable429Autoban(row) {
			result.Skipped++
			detail.Status = "skipped"
			detail.Reason = "not_429"
			result.Items = append(result.Items, detail)
			continue
		}
		ok, err := markAutobanReleased(ctx, db, row.AuthID, now)
		if err != nil {
			return result, err
		}
		if ok {
			result.Released++
			detail.Status = "released"
		} else {
			result.NotFound++
			detail.Status = "not_found"
		}
		result.Items = append(result.Items, detail)
	}
	return result, nil
}

func releaseSelectedAutobans(ctx context.Context, db *sql.DB, items []autobanReleaseItem, now int64) (autobanReleaseResult, error) {
	rows, err := queryActiveAutobanReleaseRows(ctx, db)
	if err != nil {
		return autobanReleaseResult{}, err
	}
	var result autobanReleaseResult
	released := make(map[string]bool, len(items))
	for _, item := range items {
		detail := autobanReleaseDetail{
			AuthID:    strings.TrimSpace(item.AuthID),
			AuthIndex: strings.TrimSpace(item.AuthIndex),
			Source:    strings.TrimSpace(item.Source),
			AuthFile:  strings.TrimSpace(item.AuthFile),
		}
		match := -1
		for i, row := range rows {
			if released[row.AuthID] {
				continue
			}
			if autobanReleaseItemMatchesRow(item, row) {
				match = i
				break
			}
		}
		if match < 0 {
			result.NotFound++
			detail.Status = "not_found"
			result.Items = append(result.Items, detail)
			continue
		}
		row := rows[match]
		detail.AuthID = row.AuthID
		detail.AuthIndex = row.AuthIndex
		detail.Source = row.Source
		detail.AuthFile = row.AuthFile
		if !isReleasable429Autoban(row) {
			result.Skipped++
			detail.Status = "skipped"
			detail.Reason = "not_429"
			result.Items = append(result.Items, detail)
			continue
		}
		ok, err := markAutobanReleased(ctx, db, row.AuthID, now)
		if err != nil {
			return result, err
		}
		if ok {
			released[row.AuthID] = true
			result.Released++
			detail.Status = "released"
		} else {
			result.NotFound++
			detail.Status = "not_found"
		}
		result.Items = append(result.Items, detail)
	}
	return result, nil
}

func queryActiveAutobanReleaseRows(ctx context.Context, db *sql.DB) ([]autobanRow, error) {
	if err := normalizeStoredResetColumns(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active, last_status_code,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at, auth_file, auth_file_mtime
FROM autoban_bans
WHERE active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []autobanRow
	for rows.Next() {
		var row autobanRow
		var active int
		var pp, sp sql.NullFloat64
		var pr, sr sql.NullInt64
		if err := rows.Scan(
			&row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.Window, &row.Reason,
			&row.BannedAt, &row.ResetAt, &active, &row.LastStatusCode, &pp, &pr, &sp, &sr, &row.AuthFile, &row.AuthFileMTime,
		); err != nil {
			return nil, err
		}
		row.Active = active != 0
		for _, value := range []string{row.AuthID, row.AuthIndex, row.Source} {
			if file := fileNameIfJSON(value); file != "" {
				row.AuthFile = file
				break
			}
		}
		if pp.Valid {
			row.PrimaryUsedPercent = &pp.Float64
		}
		if pr.Valid {
			row.PrimaryResetAt = &pr.Int64
		}
		if sp.Valid {
			row.SecondaryUsedPercent = &sp.Float64
		}
		if sr.Valid {
			row.SecondaryResetAt = &sr.Int64
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func isReleasable429Autoban(row autobanRow) bool {
	window := strings.ToLower(strings.TrimSpace(row.Window))
	status := row.LastStatusCode
	if status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden {
		return false
	}
	switch window {
	case "401", "402", "403":
		return false
	}
	if status == http.StatusTooManyRequests {
		return true
	}
	switch window {
	case "429", "5h", "primary", "7d", "week", "secondary":
		return true
	}
	return false
}

func autobanReleaseItemMatchesRow(item autobanReleaseItem, row autobanRow) bool {
	itemAliases := authStateMatchAliases(item.AuthID, item.AuthIndex, item.Source, item.AuthFile)
	rowAliases := authStateMatchAliases(row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
	if strict := strictAuthStateAliasesForValues(item.AuthID, item.AuthIndex, item.Source, item.AuthFile); len(strict) > 0 {
		itemAliases = strict
	}
	if strict := strictAuthStateAliasesForValues(row.AuthID, row.AuthIndex, row.Source, row.AuthFile); len(strict) > 0 {
		rowAliases = strict
	}
	if len(itemAliases) == 0 || len(rowAliases) == 0 {
		return false
	}
	for _, left := range itemAliases {
		for _, right := range rowAliases {
			if left != "" && left == right {
				return true
			}
		}
	}
	return false
}

func markAutobanReleased(ctx context.Context, db *sql.DB, authID string, now int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
UPDATE autoban_bans
SET active=0, released_at=?, release_reason='manual'
WHERE active=1 AND auth_id=?`, now, strings.TrimSpace(authID))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *store) runQuotaTriggerRound(ctx context.Context, cfg pluginConfig) (int, int, int, int, error) {
	db, _, err := s.open(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if err := clearReplacedInvalidAuths(ctx, db); err != nil {
		return 0, 0, 0, 0, err
	}
	if err := clearReplacedAutobans(ctx, db); err != nil {
		return 0, 0, 0, 0, err
	}
	configuredAccounts := readConfiguredAuthAccounts()
	if err := clearMissingConfiguredAuthState(ctx, db, configuredAccounts, globalCodexAuthSource.authoritative()); err != nil {
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
	go func() {
		wg.Wait()
		close(results)
	}()

	success := 0
	failed := 0
	for run := range results {
		if err := recordQuotaTriggerRun(ctx, db, run); err != nil {
			failed++
			continue
		}
		if err := applyQuotaTriggerAccountState(ctx, db, run); err != nil {
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
	effectiveBans := mergeEffectiveAutobans(bans, invalids)
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
		isBanned := configuredMatchesAutoban(account.configuredAccount, effectiveBans)
		isInvalid := configuredMatchesInvalidAuth(account.configuredAccount, invalids)
		if configuredMatchesExternalAlert(account.configuredAccount, externalAlerts) {
			skipped++
			continue
		}
		// Restricted accounts stay in the probe rotation so later healthy results
		// can clear state even when an older quota snapshot is still full.
		row := accountRow{AuthIndex: account.AuthIndex, AuthID: account.AuthID, Source: account.Source, AuthFile: account.AuthFile, Email: account.Email, Name: account.Name}
		primary := queryLatestAccountWindowQuota(ctx, db, row, 0, "primary")
		secondary := queryLatestAccountWindowQuota(ctx, db, row, 0, "secondary")
		if !isBanned && !isInvalid && (quotaWindowFull(primary.Percent, primary.ResetAt) || quotaWindowFull(secondary.Percent, secondary.ResetAt)) {
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
	if cfg.QuotaTriggerMode == "quota" {
		run := executeQuotaProbeRequest(ctx, db, account, cfg)
		run.Mode = "quota"
		return run
	}
	return executeQuotaProbeRequest(ctx, db, account, cfg)
}

func executeQuotaProbeRequest(ctx context.Context, _ *sql.DB, account triggerAuthAccount, cfg pluginConfig) quotaTriggerRun {
	run := quotaTriggerRunFromAccount(account, cfg.QuotaTriggerMode, "failed", 0, "")
	started := time.Now()
	run.StartedAt = started.Unix()
	timeout := time.Duration(cfg.QuotaTriggerTimeoutSeconds) * time.Second
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := codexProbeRequestBody(codexProbeModel)
	if err != nil {
		run.Error = sanitizeTriggerError(err)
		run.FinishedAt = time.Now().Unix()
		return run
	}
	headers := map[string][]string{
		"Authorization": {"Bearer " + account.AccessToken},
		"Content-Type":  {"application/json"},
		"Accept":        {"text/event-stream"},
		"Connection":    {"Keep-Alive"},
		"Originator":    {"codex-tui"},
		"User-Agent":    {"codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"},
	}
	if account.ChatGPTAccountID != "" {
		headers["Chatgpt-Account-Id"] = []string{account.ChatGPTAccountID}
	}
	resp, respBody, err := doQuotaTriggerHTTPRequest(reqCtx, http.MethodPost, codexResponsesURL(), headers, body)
	if err != nil {
		run.Error = sanitizeTriggerError(err)
		run.FinishedAt = time.Now().Unix()
		return run
	}
	if shouldRetryMinimalCodexProbe(resp.StatusCode, respBody) {
		body, err = codexProbeMinimalRequestBody(codexProbeModel)
		if err != nil {
			run.Error = sanitizeTriggerError(err)
			run.FinishedAt = time.Now().Unix()
			return run
		}
		resp, respBody, err = doQuotaTriggerHTTPRequest(reqCtx, http.MethodPost, codexResponsesURL(), headers, body)
		if err != nil {
			run.Error = sanitizeTriggerError(err)
			run.FinishedAt = time.Now().Unix()
			return run
		}
	}

	run.HTTPStatus = resp.StatusCode
	responseHeaders := cloneHeaders(resp.Header)
	mergeCodexQuotaPayload(responseHeaders, respBody)
	run.PrimaryUsedPercent = headerFloat(responseHeaders, "x-codex-primary-used-percent")
	run.PrimaryResetAt = headerInt(responseHeaders, "x-codex-primary-reset-at")
	run.SecondaryUsedPercent = headerFloat(responseHeaders, "x-codex-secondary-used-percent")
	run.SecondaryResetAt = headerInt(responseHeaders, "x-codex-secondary-reset-at")
	run.PrimaryUsedTokens = headerInt(responseHeaders, "x-codex-primary-used-tokens")
	run.PrimaryRemaining = headerInt(responseHeaders, "x-codex-primary-remaining-tokens")
	run.PrimaryLimit = headerInt(responseHeaders, "x-codex-primary-limit-tokens")
	run.SecondaryUsedTokens = headerInt(responseHeaders, "x-codex-secondary-used-tokens")
	run.SecondaryRemaining = headerInt(responseHeaders, "x-codex-secondary-remaining-tokens")
	run.SecondaryLimit = headerInt(responseHeaders, "x-codex-secondary-limit-tokens")
	run.ResponseHeaders = responseHeaders
	run.FinishedAt = time.Now().Unix()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		run.Status = "success"
	case resp.StatusCode == http.StatusUnauthorized:
		run.Status = "failed"
		run.Error = "401 unauthorized: credential is invalid"
	case resp.StatusCode == http.StatusPaymentRequired:
		run.Status = "failed"
		if codexWorkspaceDeactivatedBody(string(respBody)) {
			run.Error = "402 deactivated_workspace: team workspace is deactivated"
		} else {
			run.Error = "402 payment required: account or workspace is unavailable"
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		run.Status = "failed"
		run.Error = "429 rate limited"
	default:
		run.Status = "failed"
		run.Error = "http " + strconv.Itoa(resp.StatusCode)
	}
	return run
}

type quotaTriggerHTTPResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
}

func codexProbeRequestBody(model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = codexProbeModel
	}
	return json.Marshal(map[string]any{
		"model":        model,
		"instructions": "Reply with OK.",
		"input":        codexProbeInput(),
		"stream":       true,
		"store":        false,
	})
}

func codexProbeMinimalRequestBody(model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = codexProbeModel
	}
	return json.Marshal(map[string]any{
		"model":        model,
		"instructions": "Reply with OK.",
		"input":        codexProbeInput(),
	})
}

func codexProbeInput() []map[string]any {
	return []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "ping"},
			},
		},
	}
}

func shouldRetryMinimalCodexProbe(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "unknown_parameter") && !strings.Contains(text, "unknown parameter") {
		return false
	}
	return strings.Contains(text, "stream") || strings.Contains(text, "store")
}

func doQuotaTriggerHTTPRequest(ctx context.Context, method, targetURL string, headers map[string][]string, body []byte) (quotaTriggerHTTPResponse, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return quotaTriggerHTTPResponse{}, nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return quotaTriggerHTTPResponse{}, nil, err
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2<<20))
	return quotaTriggerHTTPResponse{
		StatusCode: httpResp.StatusCode,
		Header:     cloneHeaders(httpResp.Header),
		Headers:    cloneHeaders(httpResp.Header),
		Body:       respBody,
	}, respBody, nil
}

func executeQuotaUsageRequest(ctx context.Context, _ *sql.DB, account triggerAuthAccount, cfg pluginConfig) quotaTriggerRun {
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
	run.ResponseHeaders = headers
	run.FinishedAt = time.Now().Unix()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		run.Status = "success"
	case resp.StatusCode == http.StatusUnauthorized:
		run.Status = "failed"
		run.Error = "401 unauthorized: credential is invalid"
	case resp.StatusCode == http.StatusPaymentRequired:
		run.Status = "failed"
		if codexWorkspaceDeactivatedBody(string(body)) {
			run.Error = "402 deactivated_workspace: team workspace is deactivated"
		} else {
			run.Error = "402 payment required: account or workspace is unavailable"
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		run.Status = "failed"
		run.Error = "429 rate limited"
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

func applyQuotaTriggerAccountState(ctx context.Context, db *sql.DB, run quotaTriggerRun) error {
	requestedAt := run.StartedAt
	if requestedAt <= 0 {
		requestedAt = run.FinishedAt
	}
	if requestedAt <= 0 {
		requestedAt = time.Now().Unix()
	}
	rec := usageRecord{
		Provider:        firstNonEmptyString(run.Provider, "codex"),
		ExecutorType:    "quota-trigger",
		Model:           "quota-trigger",
		Alias:           "quota-trigger",
		AuthID:          run.AuthID,
		AuthIndex:       run.AuthIndex,
		AuthType:        "codex",
		AuthFile:        run.AuthFile,
		Source:          run.Source,
		RequestedAt:     time.Unix(requestedAt, 0),
		ResponseHeaders: cloneHeaders(run.ResponseHeaders),
	}
	if successfulStatusCode(run.HTTPStatus) && strings.EqualFold(run.Status, "success") {
		return clearRecoveredAuthStateIfNeeded(ctx, db, rec, run.HTTPStatus)
	}
	rec.Failed = true
	rec.Failure = usageFailure{StatusCode: run.HTTPStatus, Body: run.Error}
	switch run.HTTPStatus {
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return recordInvalidAuthIfNeeded(ctx, db, rec, run.HTTPStatus)
	case http.StatusForbidden:
		return recordRepeatedForbiddenIfNeeded(ctx, db, rec, run.HTTPStatus)
	case http.StatusTooManyRequests:
		return recordAutobanIfNeeded(ctx, db, rec, run.HTTPStatus, run.PrimaryUsedPercent, run.PrimaryResetAt, run.SecondaryUsedPercent, run.SecondaryResetAt)
	default:
		return nil
	}
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
	identityIndex := newCodexAuthIdentityIndex([]configuredAccount{cfg})
	for _, invalid := range invalids {
		if _, ok := identityIndex.match(invalid); ok {
			return true
		}
	}
	return false
}

func configuredMatchesAutoban(cfg configuredAccount, bans []autobanRow) bool {
	for _, ban := range bans {
		leftAliases := configuredAliases(cfg)
		rightAliases := normalizeAccountAliases(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
		if strict := strictAuthStateAliasesForValues(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile); len(strict) > 0 {
			leftAliases = normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex, cfg.ChatGPTAccountID)
			rightAliases = strict
		}
		for _, left := range leftAliases {
			for _, right := range rightAliases {
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

func codexResponsesURL() string {
	if codexResponsesURLOverrideForTest != "" {
		return codexResponsesURLOverrideForTest
	}
	return codexResponsesAPIURL
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
	text := ""
	switch v := value.(type) {
	case error:
		text = v.Error()
	case fmt.Stringer:
		text = v.String()
	default:
		text = stringFromAny(value)
	}
	text = strings.TrimSpace(text)
	if text == "" || text == "{}" || text == "[]" {
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
	return withSQLiteAutoRepair(ctx, s, "pick auth", func() (schedulerPickResponse, error) {
		return s.pickAuthOnce(ctx, req)
	})
}

func (s *store) pickAuthOnce(ctx context.Context, req schedulerPickRequest) (schedulerPickResponse, error) {
	if isXAISchedulerRequest(req) {
		if s == globalStore && !globalSchedulerState.needsDatabase("xai", false) {
			return schedulerPickResponse{Handled: false}, nil
		}
		return s.pickXAIAuthOnce(ctx, req)
	}
	if !isCodexSchedulerRequest(req) {
		return schedulerPickResponse{Handled: false}, nil
	}
	if len(req.Candidates) == 0 {
		return schedulerPickResponse{Handled: false}, nil
	}
	protectionCfg := globalAccountProtection.config()
	if s == globalStore && !globalSchedulerState.needsDatabase("codex", protectionCfg.AccountProtectionEnabled) {
		return schedulerPickResponse{Handled: false}, nil
	}
	var stateGeneration uint64
	if s == globalStore {
		stateGeneration = globalSchedulerState.generation("codex")
	}
	db, _, err := s.open(ctx)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	now := time.Now().Unix()
	configuredAccounts := readConfiguredAuthAccounts()
	if err := clearReplacedInvalidAuthsForConfigured(ctx, db, configuredAccounts); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := clearReplacedAutobansForConfigured(ctx, db, configuredAccounts); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := clearMissingConfiguredAuthState(ctx, db, configuredAccounts, globalCodexAuthSource.authoritative()); err != nil {
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
	if len(bans) == 0 && len(invalids) == 0 && !protectionCfg.AccountProtectionEnabled {
		if s == globalStore {
			globalSchedulerState.clearRestrictedIfGeneration("codex", stateGeneration)
		}
		return schedulerPickResponse{Handled: false}, nil
	}
	effectiveBans := mergeEffectiveAutobans(bans, invalids)
	available, filtered, restrictionFilteredCandidates, matchedBanIndexes, matchedInvalidIndexes := filterCodexSchedulerCandidates(req.Candidates, bans, invalids)
	recordSchedulerFilteringDiagnostics(bans, invalids, restrictionFilteredCandidates, matchedBanIndexes, matchedInvalidIndexes)
	if filtered && len(available) == 0 {
		globalCodexAuthSource.invalidate()
		configuredAccounts = readConfiguredAuthAccounts()
		if err := clearReplacedInvalidAuthsForConfigured(ctx, db, configuredAccounts); err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		if err := clearReplacedAutobansForConfigured(ctx, db, configuredAccounts); err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		if err := clearMissingConfiguredAuthState(ctx, db, configuredAccounts, globalCodexAuthSource.authoritative()); err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		bans, err = queryActiveAutobans(ctx, db, now)
		if err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		invalids, err = queryActiveInvalidAuths(ctx, db)
		if err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		effectiveBans = mergeEffectiveAutobans(bans, invalids)
		available, filtered, restrictionFilteredCandidates, matchedBanIndexes, matchedInvalidIndexes = filterCodexSchedulerCandidates(req.Candidates, bans, invalids)
		recordSchedulerFilteringDiagnostics(bans, invalids, restrictionFilteredCandidates, matchedBanIndexes, matchedInvalidIndexes)
	}
	if !filtered && !protectionCfg.AccountProtectionEnabled {
		if len(bans) == 0 && len(invalids) == 0 && s == globalStore {
			globalSchedulerState.clearRestrictedIfGeneration("codex", stateGeneration)
		}
		return schedulerPickResponse{Handled: false}, nil
	}
	if len(available) == 0 {
		return schedulerPickResponse{}, newNoAvailableCodexAuthError(effectiveBans, now)
	}
	rotationKey := schedulerRotationKey(req, "codex")
	affinityKey := schedulerAffinityKey(req, "codex")
	if protectionCfg.AccountProtectionEnabled {
		chosen, err := s.pickProtectedAuth(ctx, db, available, protectionCfg, rotationKey, affinityKey)
		if err != nil {
			return schedulerPickResponse{Handled: false}, err
		}
		return schedulerPickResponse{AuthID: chosen.ID, Handled: true}, nil
	}
	chosen := pickSchedulerCandidate(rotationKey, affinityKey, available)
	return schedulerPickResponse{AuthID: chosen.ID, Handled: true}, nil
}

func filterCodexSchedulerCandidates(candidates []schedulerAuthCandidate, bans []autobanRow, invalids []invalidAuthRow) ([]schedulerAuthCandidate, bool, int, map[int]bool, map[int]bool) {
	available := make([]schedulerAuthCandidate, 0, len(candidates))
	filtered := false
	restrictionFilteredCandidates := 0
	matchedBanIndexes := map[int]bool{}
	matchedInvalidIndexes := map[int]bool{}
	invalidCandidateIndexes := map[int]bool{}
	if len(invalids) > 0 {
		configured := make([]configuredAccount, 0, len(candidates))
		originalIndexes := make([]int, 0, len(candidates))
		for i, candidate := range candidates {
			if !strings.EqualFold(candidate.Provider, "codex") {
				continue
			}
			account := configuredAccountForSchedulerCandidate(candidate)
			if normalizeAccountAlias(account.AuthID) == "" && normalizeAccountAlias(account.AuthIndex) == "" && normalizeAccountAlias(account.AuthFile) == "" {
				continue
			}
			configured = append(configured, account)
			originalIndexes = append(originalIndexes, i)
		}
		identityIndex := newCodexAuthIdentityIndex(configured)
		for invalidIndex, invalid := range invalids {
			for _, configuredIndex := range identityIndex.matchIndexes(invalid) {
				if configuredIndex < 0 || configuredIndex >= len(originalIndexes) {
					continue
				}
				invalidCandidateIndexes[originalIndexes[configuredIndex]] = true
				matchedInvalidIndexes[invalidIndex] = true
			}
		}
	}
	for candidateIndex, candidate := range candidates {
		if !strings.EqualFold(candidate.Provider, "codex") {
			available = append(available, candidate)
			continue
		}
		banMatched, banIndexes := candidateMatchesActiveBanIndexes(candidate, bans)
		for _, index := range banIndexes {
			matchedBanIndexes[index] = true
		}
		if banMatched || invalidCandidateIndexes[candidateIndex] {
			filtered = true
			restrictionFilteredCandidates++
			continue
		}
		available = append(available, candidate)
	}
	return available, filtered, restrictionFilteredCandidates, matchedBanIndexes, matchedInvalidIndexes
}

func recordSchedulerFilteringDiagnostics(bans []autobanRow, invalids []invalidAuthRow, filteredCandidates int, matchedBanIndexes map[int]bool, matchedInvalidIndexes map[int]bool) {
	if filteredCandidates <= 0 {
		return
	}
	activeRestrictions := len(bans) + len(invalids)
	unmatchedRestrictions := activeRestrictions - len(matchedBanIndexes) - len(matchedInvalidIndexes)
	globalSchedulerDiagnostics.record(activeRestrictions, filteredCandidates, maxInt(0, unmatchedRestrictions))
}

func (s *store) pickXAIAuthOnce(ctx context.Context, req schedulerPickRequest) (schedulerPickResponse, error) {
	if len(req.Candidates) == 0 {
		return schedulerPickResponse{Handled: false}, nil
	}
	var stateGeneration uint64
	if s == globalStore {
		stateGeneration = globalSchedulerState.generation("xai")
	}
	db, _, err := s.open(ctx)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if err := clearReplacedOrMissingXAIStates(ctx, db); err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	now := time.Now().Unix()
	states, err := queryActiveXAIStates(ctx, db, now)
	if err != nil {
		return schedulerPickResponse{Handled: false}, err
	}
	if len(states) == 0 {
		if s == globalStore {
			globalSchedulerState.clearRestrictedIfGeneration("xai", stateGeneration)
		}
		return schedulerPickResponse{Handled: false}, nil
	}
	available := make([]schedulerAuthCandidate, 0, len(req.Candidates))
	filtered := false
	for _, candidate := range req.Candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "xai") {
			available = append(available, candidate)
			continue
		}
		if candidateMatchesXAIState(candidate, states) {
			filtered = true
			continue
		}
		available = append(available, candidate)
	}
	if !filtered {
		return schedulerPickResponse{Handled: false}, nil
	}
	if len(available) == 0 {
		message := "no available xAI auth candidates: all candidates are unavailable by 401/403/429"
		if resetAt := earliestXAIStateReset(states, now); resetAt > 0 {
			message += "; earliest retry at " + unixTime(resetAt)
		}
		return schedulerPickResponse{}, &schedulerRejectError{Code: "auth_unavailable", Message: message, HTTPStatus: http.StatusServiceUnavailable}
	}
	chosen := globalSchedulerRotation.pick(schedulerRotationKey(req, "xai"), available)
	return schedulerPickResponse{AuthID: chosen.ID, Handled: true}, nil
}

func newNoAvailableCodexAuthError(bans []autobanRow, now int64) error {
	message := "no available Codex auth candidates: all candidates are auto-banned by 429/401/402/403"
	if resetAt := earliestActiveBanReset(bans, now); resetAt > 0 {
		message += "; earliest autoban reset at " + unixTime(resetAt)
	}
	return &schedulerRejectError{
		Code:       "auth_unavailable",
		Message:    message,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

func earliestActiveBanReset(bans []autobanRow, now int64) int64 {
	var earliest int64
	for _, ban := range bans {
		if !ban.Active || ban.ResetAt <= now {
			continue
		}
		if earliest == 0 || ban.ResetAt < earliest {
			earliest = ban.ResetAt
		}
	}
	return earliest
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

func schedulerPickRequiresPlugin(req schedulerPickRequest) bool {
	if isXAISchedulerRequest(req) {
		return globalSchedulerState.needsDatabase("xai", false)
	}
	if isCodexSchedulerRequest(req) {
		return globalSchedulerState.needsDatabase("codex", globalAccountProtection.config().AccountProtectionEnabled)
	}
	return false
}

func expireAutobans(ctx context.Context, db *sql.DB, now int64) error {
	_, err := db.ExecContext(ctx, `UPDATE autoban_bans SET active=0 WHERE active=1 AND reset_at <= ?`, now)
	return err
}

func reconcileAutobansWithQuotaSnapshots(ctx context.Context, db *sql.DB, now int64) error {
	bans, err := queryActiveAutobans(ctx, db, now)
	if err != nil {
		return err
	}
	accounts := make([]accountRow, len(bans))
	for i, ban := range bans {
		accounts[i] = accountRow{
			AuthID:    ban.AuthID,
			AuthIndex: ban.AuthIndex,
			Source:    ban.Source,
			Provider:  ban.Provider,
			AuthFile:  ban.AuthFile,
		}
	}
	primarySnapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, accounts, 0, "primary")
	secondarySnapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, accounts, 0, "secondary")
	for i, ban := range bans {
		primary := primarySnapshots[i]
		secondary := secondarySnapshots[i]
		shouldRelease := false
		switch strings.ToLower(strings.TrimSpace(ban.Window)) {
		case "5h", "primary":
			shouldRelease = quotaWindowObserved(primary.Percent, primary.ResetAt) && !quotaWindowFull(primary.Percent, primary.ResetAt)
		case "week", "7d", "secondary":
			shouldRelease = quotaWindowObserved(secondary.Percent, secondary.ResetAt) && !quotaWindowFull(secondary.Percent, secondary.ResetAt)
		default:
			primaryObserved := quotaWindowObserved(primary.Percent, primary.ResetAt)
			secondaryObserved := quotaWindowObserved(secondary.Percent, secondary.ResetAt)
			shouldRelease = (primaryObserved || secondaryObserved) && !quotaWindowFull(primary.Percent, primary.ResetAt) && !quotaWindowFull(secondary.Percent, secondary.ResetAt)
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
WHERE active=1 AND auth_id=?`, nullFloatPtr(primary.Percent), nullIntPtr(primary.ResetAt), nullFloatPtr(secondary.Percent), nullIntPtr(secondary.ResetAt), ban.AuthID)
		if err != nil {
			return err
		}
	}
	return nil
}

func backfillAutobansFromUsage(ctx context.Context, db *sql.DB, now int64) error {
	configured := readConfiguredAuthAccounts()
	authSourceAuthoritative := globalCodexAuthSource.authoritative()
	rows, err := db.QueryContext(ctx, `
WITH latest AS (
  SELECT
    auth_id, auth_index, source, provider, requested_at, status_code, failed,
    primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at,
    ROW_NUMBER() OVER (
      PARTITION BY lower(COALESCE(NULLIF(auth_id,''), NULLIF(source,''), NULLIF(auth_index,'')))
      ORDER BY requested_at DESC, id DESC
    ) AS rn
  FROM usage_events
  WHERE provider='codex'
    AND COALESCE(NULLIF(auth_id,''), NULLIF(source,''), NULLIF(auth_index,'')) <> ''
)
SELECT auth_id, auth_index, source, provider, requested_at, status_code,
primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
FROM latest
WHERE rn=1
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
		rec := usageRecord{
			Provider:  provider,
			AuthID:    authID,
			AuthIndex: authIndex,
			Source:    source,
		}
		if recordReferencesMissingCurrentAuthFile(rec, configured, authSourceAuthoritative) {
			continue
		}
		authFile, authFileMTime := authFileStateForRecord(rec)
		if authFile != "" && authFileMTime > requestedAt && configuredRecordIdentityChanged(rec, authFile, configured) {
			continue
		}
		key := invalidAuthIDForRecord(rec, authFile)
		if key == "" {
			continue
		}
		resetAt, window, reason := classifyStoredCodexBan(pp, pr, sp, sr, now)
		if resetAt <= now {
			continue
		}
		if hasLaterSuccessfulUsage(ctx, db, rec, requestedAt) {
			continue
		}
		globalSchedulerState.beginRestrictionWrite("codex")
		_, err := db.ExecContext(ctx, `
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at,
  auth_file, auth_file_mtime, released_at, release_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 0, '')
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider=excluded.provider,
  window=excluded.window,
  reason=excluded.reason,
  banned_at=excluded.banned_at,
  reset_at=excluded.reset_at,
  active=CASE
    WHEN excluded.reset_at > ? AND (COALESCE(autoban_bans.released_at,0)=0 OR excluded.banned_at > COALESCE(autoban_bans.released_at,0)) THEN 1
    ELSE autoban_bans.active
  END,
  last_status_code=excluded.last_status_code,
  primary_used_percent=excluded.primary_used_percent,
  primary_reset_at=excluded.primary_reset_at,
  secondary_used_percent=excluded.secondary_used_percent,
  secondary_reset_at=excluded.secondary_reset_at,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime,
  released_at=CASE WHEN excluded.banned_at > COALESCE(autoban_bans.released_at,0) THEN 0 ELSE autoban_bans.released_at END,
  release_reason=CASE WHEN excluded.banned_at > COALESCE(autoban_bans.released_at,0) THEN '' ELSE autoban_bans.release_reason END
WHERE (autoban_bans.active=0 OR excluded.reset_at >= autoban_bans.reset_at)
  AND (COALESCE(autoban_bans.released_at,0)=0 OR excluded.banned_at > COALESCE(autoban_bans.released_at,0) OR autoban_bans.active=1)`,
			key, authIndex, source, provider, window, reason, requestedAt, resetAt, status,
			nullFloatPtr(pp), nullIntPtr(pr), nullFloatPtr(sp), nullIntPtr(sr), authFile, authFileMTime, now,
		)
		globalSchedulerState.finishRestrictionWrite("codex")
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func backfillWorkspaceDeactivatedAuthsFromUsage(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredAuthAccounts()
	authDirReadable := globalCodexAuthSource.authoritative()
	rows, err := db.QueryContext(ctx, `
WITH latest AS (
  SELECT
    auth_id, auth_index, source, provider, requested_at, status_code, failed,
    ROW_NUMBER() OVER (
      PARTITION BY lower(COALESCE(NULLIF(auth_id,''), NULLIF(source,''), NULLIF(auth_index,'')))
      ORDER BY requested_at DESC, id DESC
    ) AS rn
  FROM usage_events
  WHERE provider='codex'
    AND COALESCE(NULLIF(auth_id,''), NULLIF(source,''), NULLIF(auth_index,'')) <> ''
)
SELECT auth_id, auth_index, source, provider, requested_at, status_code
FROM latest
WHERE rn=1 AND failed=1 AND status_code=402
ORDER BY requested_at DESC
LIMIT 1000`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rec usageRecord
		var requestedAt int64
		var status int
		if err := rows.Scan(&rec.AuthID, &rec.AuthIndex, &rec.Source, &rec.Provider, &requestedAt, &status); err != nil {
			return err
		}
		rec.RequestedAt = time.Unix(requestedAt, 0)
		rec.Failed = true
		rec.Failure = usageFailure{StatusCode: status}
		if recordReferencesMissingCurrentAuthFile(rec, configured, authDirReadable) {
			continue
		}
		if !codexAuthRecordLooksFileBacked(rec) {
			continue
		}
		if authFile, authFileMTime := authFileStateForRecord(rec); authFileMTime > requestedAt && authFile != "" {
			continue
		}
		if hasLaterSuccessfulUsage(ctx, db, rec, requestedAt) {
			continue
		}
		if err := recordInvalidAuthIfNeeded(ctx, db, rec, status); err != nil {
			return err
		}
	}
	return rows.Err()
}

func backfillWorkspaceDeactivatedAuthsFromQuotaTriggerRuns(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredAuthAccounts()
	authDirReadable := globalCodexAuthSource.authoritative()
	rows, err := db.QueryContext(ctx, `
WITH latest AS (
  SELECT
    auth_id, auth_index, source, provider, auth_file, finished_at, http_status, error,
    ROW_NUMBER() OVER (
      PARTITION BY lower(COALESCE(NULLIF(auth_file,''), NULLIF(auth_id,''), NULLIF(auth_index,''), NULLIF(source,'')))
      ORDER BY finished_at DESC, id DESC
    ) AS rn
  FROM quota_trigger_runs
  WHERE provider='codex'
    AND COALESCE(NULLIF(auth_file,''), NULLIF(auth_id,''), NULLIF(auth_index,''), NULLIF(source,'')) <> ''
)
SELECT auth_id, auth_index, source, provider, auth_file, finished_at, http_status, error
FROM latest
WHERE rn=1 AND http_status=402
ORDER BY finished_at DESC
LIMIT 1000`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var authID, authIndex, source, provider, authFile, message string
		var finishedAt int64
		var status int
		if err := rows.Scan(&authID, &authIndex, &source, &provider, &authFile, &finishedAt, &status, &message); err != nil {
			return err
		}
		rec := usageRecord{
			Provider:    firstNonEmptyString(provider, "codex"),
			AuthID:      authID,
			AuthIndex:   authIndex,
			AuthFile:    authFile,
			Source:      source,
			RequestedAt: time.Unix(finishedAt, 0),
			Failed:      true,
			Failure:     usageFailure{StatusCode: status, Body: message},
		}
		if recordReferencesMissingCurrentAuthFile(rec, configured, authDirReadable) {
			continue
		}
		if !codexAuthRecordLooksFileBacked(rec) {
			continue
		}
		if authFile, authFileMTime := authFileStateForRecord(rec); authFileMTime > finishedAt && authFile != "" {
			continue
		}
		if hasLaterSuccessfulUsage(ctx, db, rec, finishedAt) {
			continue
		}
		if err := recordInvalidAuthIfNeeded(ctx, db, rec, status); err != nil {
			return err
		}
	}
	return rows.Err()
}

func recordReferencesMissingCurrentAuthFile(rec usageRecord, configured []configuredAccount, authDirReadable bool) bool {
	if !authDirReadable {
		return false
	}
	var explicit []string
	for _, value := range []string{rec.AuthFile, rec.AuthID, rec.AuthIndex, rec.Source} {
		if file := fileNameIfJSON(value); file != "" {
			explicit = append(explicit, file)
		}
	}
	if len(explicit) == 0 {
		return false
	}
	strictAliases := configuredStrictAliasSet(configured)
	for _, file := range explicit {
		if !aliasesContainAny(strictAliases, file) {
			return true
		}
	}
	return false
}

func hasLaterSuccessfulUsage(ctx context.Context, db *sql.DB, rec usageRecord, after int64) bool {
	aliases, strict := recoveryMatchAliasesForRecord(rec)
	if len(aliases) == 0 {
		return false
	}
	usageColumns := []string{"auth_id", "auth_index", "source"}
	triggerColumns := []string{"auth_id", "auth_index", "source", "auth_file"}
	if strict {
		usageColumns = []string{"auth_id", "auth_index"}
		triggerColumns = []string{"auth_id", "auth_index", "auth_file"}
	}
	usageCond, usageArgs := sqlLowerInCondition(usageColumns, aliases)
	triggerCond, triggerArgs := sqlLowerInCondition(triggerColumns, aliases)
	if usageCond == "" || triggerCond == "" {
		return false
	}
	args := []any{after}
	args = append(args, usageArgs...)
	args = append(args, after)
	args = append(args, triggerArgs...)
	var count int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM (
  SELECT requested_at AS ts
  FROM usage_events
  WHERE provider='codex'
    AND requested_at > ?
    AND failed=0
    AND (status_code=0 OR (status_code >= 200 AND status_code < 300))
    AND `+usageCond+`
  UNION ALL
  SELECT finished_at AS ts
  FROM quota_trigger_runs
  WHERE provider='codex'
    AND finished_at > ?
    AND status='success'
    AND `+triggerCond+`
)`, args...).Scan(&count)
	return err == nil && count > 0
}

func legacyHasLaterSuccessfulUsage(ctx context.Context, db *sql.DB, rec usageRecord, after int64) bool {
	for _, alias := range normalizeAccountAliases(rec.AuthID, rec.AuthIndex, rec.Source) {
		if alias == "" {
			continue
		}
		var count int
		err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM usage_events
WHERE provider='codex'
  AND requested_at > ?
  AND failed=0
  AND (status_code=0 OR (status_code >= 200 AND status_code < 300))
  AND (
    lower(auth_id)=?
    OR lower(auth_index)=?
    OR lower(source)=?
  )`, after, alias, alias, alias).Scan(&count)
		if err == nil && count > 0 {
			return true
		}
	}
	return false
}

func clearRecoveredAuthStatesFromUsage(ctx context.Context, db *sql.DB) error {
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return err
	}
	for _, invalid := range invalids {
		if !hasLaterSuccessfulInvalidAuthUsage(ctx, db, invalid) {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE invalid_auths SET active=0 WHERE active=1 AND auth_id=?`, invalid.AuthID); err != nil {
			return err
		}
	}
	bans, err := queryActiveAutobans(ctx, db, time.Now().Unix())
	if err != nil {
		return err
	}
	for _, ban := range bans {
		rec := usageRecord{
			Provider:  firstNonEmptyString(ban.Provider, "codex"),
			AuthID:    ban.AuthID,
			AuthIndex: ban.AuthIndex,
			AuthFile:  ban.AuthFile,
			Source:    ban.Source,
		}
		if !hasLaterSuccessfulUsage(ctx, db, rec, ban.BannedAt) {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE autoban_bans SET active=0 WHERE active=1 AND auth_id=?`, ban.AuthID); err != nil {
			return err
		}
	}
	return nil
}

func hasLaterSuccessfulInvalidAuthUsage(ctx context.Context, db *sql.DB, invalid invalidAuthRow) bool {
	aliases := normalizeAccountAliases(invalid.AuthID, invalid.AuthIndex, invalid.AuthFile)
	if len(aliases) == 0 {
		return false
	}
	usageCond, usageArgs := sqlLowerInCondition([]string{"auth_id", "auth_index"}, aliases)
	triggerCond, triggerArgs := sqlLowerInCondition([]string{"auth_id", "auth_index", "auth_file"}, aliases)
	if usageCond == "" || triggerCond == "" {
		return false
	}
	args := []any{invalid.InvalidatedAt}
	args = append(args, usageArgs...)
	args = append(args, invalid.InvalidatedAt)
	args = append(args, triggerArgs...)
	var count int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM (
  SELECT requested_at AS ts
  FROM usage_events
  WHERE provider='codex'
    AND requested_at > ?
    AND failed=0
    AND (status_code=0 OR (status_code >= 200 AND status_code < 300))
    AND `+usageCond+`
  UNION ALL
  SELECT finished_at AS ts
  FROM quota_trigger_runs
  WHERE provider='codex'
    AND finished_at > ?
    AND status='success'
    AND `+triggerCond+`
)`, args...).Scan(&count)
	return err == nil && count > 0
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

func reconcileInvalidAuthSourceKinds(ctx context.Context, db *sql.DB) error {
	globalCodexAuthSource.invalidate()
	inventory, err := readCodexHostAuthInventory()
	hostInventoryAuthoritative := err == nil
	if err != nil {
		if !configuredAuthDirectoryReadable() {
			// Never deactivate records from a non-authoritative empty snapshot.
			return nil
		}
		inventory = configuredCodexFileInventory(readConfiguredAuthFiles())
	}
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return err
	}
	if len(invalids) == 0 {
		return nil
	}
	type reconcileAction struct {
		authID       string
		deactivate   bool
		sourceKind   string
		authFile     string
		authFileTime int64
	}
	identityIndex := newCodexAuthIdentityIndex(inventory)
	actions := make([]reconcileAction, 0, len(invalids))
	for _, invalid := range invalids {
		rowKind := normalizeAuthSourceKind(invalid.AuthSourceKind)
		var current configuredAccount
		var ok bool
		if hostInventoryAuthoritative || rowKind == authSourceKindFile {
			current, ok = identityIndex.match(invalid)
		}
		identityConflict := false
		if !ok && strings.TrimSpace(invalid.AuthIndex) != "" {
			if runtime, runtimeErr := readCodexRuntimeAuth(invalid.AuthIndex); runtimeErr == nil {
				if matched, matchedOK := matchCodexHostAuthInventoryExact(invalid, []configuredAccount{runtime}); matchedOK {
					current, ok = matched, true
				} else if invalidAuthInventoryIdentityOverlaps(invalid, runtime) {
					identityConflict = true
				}
			}
		}
		if !ok {
			if !identityConflict && hostInventoryAuthoritative {
				for _, candidate := range inventory {
					if invalidAuthInventoryIdentityOverlaps(invalid, candidate) {
						identityConflict = true
						break
					}
				}
			}
			if identityConflict {
				actions = append(actions, reconcileAction{authID: invalid.AuthID, deactivate: true})
				continue
			}
			if rowKind == authSourceKindRuntimeOnly {
				// Disabled runtime-only credentials are deliberately omitted from
				// host.auth.list, so an authoritative list cannot distinguish them
				// from disconnected runtime sources. Keep the explicit runtime row
				// until resolve or a successful request clears it.
				continue
			}
			if !hostInventoryAuthoritative && rowKind != authSourceKindFile {
				// A readable file directory cannot classify legacy or runtime
				// identities, even when their stable ID happens to end in .json.
				// Preserve every non-file row until host.auth.list is authoritative.
				continue
			}
			// Missing and ambiguous legacy identities are both unsafe deletion
			// targets. Retire the plugin state without touching any credential.
			actions = append(actions, reconcileAction{authID: invalid.AuthID, deactivate: true})
			continue
		}
		kind := normalizeAuthSourceKind(current.AuthSourceKind)
		if kind == authSourceKindLegacy {
			actions = append(actions, reconcileAction{authID: invalid.AuthID, deactivate: true})
			continue
		}
		actions = append(actions, reconcileAction{
			authID: invalid.AuthID, sourceKind: kind, authFile: current.AuthFile, authFileTime: bestInvalidAuthFileMTimeMillis(current.AuthFile, current.AuthFileMTime),
		})
	}
	if len(actions) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, action := range actions {
		if action.deactivate {
			if _, err := tx.ExecContext(ctx, `UPDATE invalid_auths SET active=0 WHERE active=1 AND auth_id=?`, action.authID); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE invalid_auths
SET auth_source_kind=?,
  auth_file=CASE WHEN auth_file='' THEN ? ELSE auth_file END,
  auth_file_mtime=CASE WHEN auth_file_mtime=0 THEN ? ELSE auth_file_mtime END
WHERE active=1 AND auth_id=?`, action.sourceKind, action.authFile, action.authFileTime, action.authID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func configuredCodexFileInventory(accounts []configuredAccount) []configuredAccount {
	out := make([]configuredAccount, 0, len(accounts))
	for _, account := range accounts {
		if !isCodexAuthProvider(account.Provider) || fileNameIfJSON(account.AuthFile) == "" {
			continue
		}
		account.AuthID = ""
		account.AuthIndex = ""
		account.Source = ""
		account.AuthSourceKind = authSourceKindFile
		out = append(out, account)
	}
	return out
}

func clearReplacedInvalidAuths(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredAuthAccounts()
	return clearReplacedInvalidAuthsForConfigured(ctx, db, configured)
}

func clearReplacedInvalidAuthsForConfigured(ctx context.Context, db *sql.DB, configured []configuredAccount) error {
	if len(configured) == 0 {
		return nil
	}
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return err
	}
	fileIndex := newConfiguredAuthFileIndex(configured)
	for _, invalid := range invalids {
		if normalizeAuthSourceKind(invalid.AuthSourceKind) != authSourceKindFile {
			continue
		}
		baseline := invalid.InvalidatedAt
		state := autobanRow{
			AuthID:    invalid.AuthID,
			AuthIndex: invalid.AuthIndex,
			Source:    invalid.Source,
			AuthFile:  invalid.AuthFile,
		}
		replaced := false
		for _, cfg := range fileIndex.matchingAccounts(state) {
			if cfg.AuthFileMTime <= baseline {
				continue
			}
			replaced = true
			break
		}
		if !replaced {
			continue
		}
		_, err := db.ExecContext(ctx, `
UPDATE invalid_auths
SET active=0
WHERE active=1 AND auth_id=?`, invalid.AuthID)
		if err != nil {
			return err
		}
	}
	return nil
}

func clearReplacedAutobans(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredAuthAccounts()
	return clearReplacedAutobansForConfigured(ctx, db, configured)
}

func clearReplacedAutobansForConfigured(ctx context.Context, db *sql.DB, configured []configuredAccount) error {
	if len(configured) == 0 {
		return nil
	}
	bans, err := queryActiveAutobans(ctx, db, time.Now().Unix())
	if err != nil {
		return err
	}
	fileIndex := newConfiguredAuthFileIndex(configured)
	now := time.Now().Unix()
	for _, ban := range bans {
		baseline := ban.AuthFileMTime
		if baseline <= 0 {
			baseline = ban.BannedAt
		}
		for _, cfg := range fileIndex.matchingAccounts(ban) {
			if cfg.AuthFileMTime <= baseline || !autobanAccountIdentityChanged(cfg, ban) {
				continue
			}
			_, err := db.ExecContext(ctx, `
UPDATE autoban_bans
SET active=0,
  released_at=?,
  release_reason='auth file replaced'
WHERE active=1 AND auth_id=?`, now, ban.AuthID)
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

type configuredAuthFileIndex struct {
	accounts []configuredAccount
	byAlias  map[string][]int
}

func newConfiguredAuthFileIndex(accounts []configuredAccount) *configuredAuthFileIndex {
	index := &configuredAuthFileIndex{
		accounts: accounts,
		byAlias:  make(map[string][]int, len(accounts)*3),
	}
	for i, account := range accounts {
		for _, alias := range normalizeAccountAliases(account.AuthFile, account.AuthIndex, account.AuthID) {
			index.byAlias[alias] = append(index.byAlias[alias], i)
		}
	}
	return index
}

func (index *configuredAuthFileIndex) matchingAccounts(state autobanRow) []configuredAccount {
	if index == nil || len(index.accounts) == 0 {
		return nil
	}
	aliases := fileBackedCleanupAliases(state.AuthID, state.AuthIndex, state.Source, state.AuthFile)
	if len(aliases) == 0 {
		aliases = strictAuthStateAliasesForValues(state.AuthID, state.AuthIndex, state.Source, state.AuthFile)
	}
	seen := make(map[int]struct{}, len(aliases))
	matches := make([]configuredAccount, 0, len(aliases))
	for _, alias := range aliases {
		for _, accountIndex := range index.byAlias[alias] {
			if _, ok := seen[accountIndex]; ok {
				continue
			}
			seen[accountIndex] = struct{}{}
			matches = append(matches, index.accounts[accountIndex])
		}
	}
	return matches
}

func configuredAccountMatchesAutobanFile(cfg configuredAccount, ban autobanRow) bool {
	left := normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex, cfg.AuthID)
	right := fileBackedCleanupAliases(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
	if len(right) == 0 {
		right = strictAuthStateAliasesForValues(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
	}
	for _, l := range left {
		for _, r := range right {
			if l != "" && l == r {
				return true
			}
		}
	}
	return false
}

func autobanAccountIdentityChanged(cfg configuredAccount, ban autobanRow) bool {
	current := nonFileAccountIdentityAliases(cfg.Email, cfg.ChatGPTAccountID)
	previous := nonFileAccountIdentityAliases(ban.Source, ban.AuthID)
	return knownAccountIdentityChanged(current, previous)
}

func configuredRecordIdentityChanged(rec usageRecord, authFile string, configured []configuredAccount) bool {
	state := autobanRow{AuthID: rec.AuthID, AuthIndex: rec.AuthIndex, Source: rec.Source, AuthFile: authFile}
	previous := nonFileAccountIdentityAliases(rec.Source, rec.AuthID)
	for _, cfg := range configured {
		if !configuredAccountMatchesAutobanFile(cfg, state) {
			continue
		}
		current := nonFileAccountIdentityAliases(cfg.Email, cfg.ChatGPTAccountID)
		return knownAccountIdentityChanged(current, previous)
	}
	return false
}

func knownAccountIdentityChanged(current, previous []string) bool {
	if len(current) == 0 || len(previous) == 0 {
		return false
	}
	for _, left := range current {
		for _, right := range previous {
			if left == right {
				return false
			}
		}
	}
	return true
}

func nonFileAccountIdentityAliases(values ...string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || fileNameIfJSON(value) != "" || looksOpaqueAccountKey(value) {
			continue
		}
		switch strings.ToLower(value) {
		case "file", "memory", "oauth", "codex":
			continue
		}
		out = append(out, normalizeAccountAlias(value))
	}
	return uniqueNonEmptyAliases(out)
}

func clearMissingConfiguredAuthState(ctx context.Context, db *sql.DB, configured []configuredAccount, authDirReadable bool) error {
	if !authDirReadable {
		return nil
	}
	aliases := configuredAliasSet(configured)
	strictAliases := configuredStrictAliasSet(configured)
	if err := clearMissingInvalidAuths(ctx, db, aliases, strictAliases); err != nil {
		return err
	}
	return clearMissingAutobans(ctx, db, aliases, strictAliases)
}

func configuredAliasSet(configured []configuredAccount) map[string]struct{} {
	aliases := make(map[string]struct{}, len(configured)*6)
	for _, cfg := range configured {
		for _, alias := range configuredAliases(cfg) {
			if alias != "" {
				aliases[alias] = struct{}{}
			}
		}
	}
	return aliases
}

func configuredStrictAliasSet(configured []configuredAccount) map[string]struct{} {
	aliases := make(map[string]struct{}, len(configured)*4)
	for _, cfg := range configured {
		for _, alias := range normalizeAccountAliases(cfg.AuthFile, cfg.AuthIndex, cfg.ChatGPTAccountID) {
			if alias != "" {
				aliases[alias] = struct{}{}
			}
		}
	}
	return aliases
}

func aliasesContainAny(aliases map[string]struct{}, values ...string) bool {
	for _, alias := range normalizeAccountAliases(values...) {
		if _, ok := aliases[alias]; ok {
			return true
		}
	}
	return false
}

func aliasesAllContained(aliases map[string]struct{}, values ...string) bool {
	normalized := normalizeAccountAliases(values...)
	if len(normalized) == 0 {
		return false
	}
	for _, alias := range normalized {
		if _, ok := aliases[alias]; !ok {
			return false
		}
	}
	return true
}

func clearMissingInvalidAuths(ctx context.Context, db *sql.DB, configuredAliases map[string]struct{}, configuredStrictAliases map[string]struct{}) error {
	invalids, err := queryActiveInvalidAuths(ctx, db)
	if err != nil {
		return err
	}
	for _, invalid := range invalids {
		if normalizeAuthSourceKind(invalid.AuthSourceKind) == authSourceKindRuntimeOnly {
			continue
		}
		cleanupAliases := fileBackedCleanupAliases(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile)
		if len(cleanupAliases) > 0 {
			if aliasesContainAny(configuredStrictAliases, cleanupAliases...) {
				continue
			}
			if _, err := db.ExecContext(ctx, `UPDATE invalid_auths SET active=0 WHERE active=1 AND auth_id=?`, invalid.AuthID); err != nil {
				return err
			}
			continue
		}
		strictAliases := strictAuthStateAliasesForValues(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile)
		if len(strictAliases) == 0 && !fileBackedAuthState(invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile) {
			continue
		}
		if len(strictAliases) > 0 {
			if aliasesContainAny(configuredStrictAliases, strictAliases...) {
				continue
			}
		} else if aliasesContainAny(configuredAliases, invalid.AuthID, invalid.AuthIndex, invalid.Source, invalid.AuthFile) {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE invalid_auths SET active=0 WHERE active=1 AND auth_id=?`, invalid.AuthID); err != nil {
			return err
		}
	}
	return nil
}

func clearMissingAutobans(ctx context.Context, db *sql.DB, configuredAliases map[string]struct{}, configuredStrictAliases map[string]struct{}) error {
	bans, err := queryActiveAutobans(ctx, db, time.Now().Unix())
	if err != nil {
		return err
	}
	for _, ban := range bans {
		cleanupAliases := fileBackedCleanupAliases(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
		if len(cleanupAliases) > 0 {
			if aliasesContainAny(configuredStrictAliases, cleanupAliases...) {
				continue
			}
			if _, err := db.ExecContext(ctx, `UPDATE autoban_bans SET active=0 WHERE active=1 AND auth_id=?`, ban.AuthID); err != nil {
				return err
			}
			continue
		}
		strictAliases := strictAuthStateAliasesForValues(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
		if len(strictAliases) == 0 && !fileBackedAuthState(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile) {
			continue
		}
		if len(strictAliases) > 0 {
			if aliasesContainAny(configuredStrictAliases, strictAliases...) {
				continue
			}
		} else if aliasesContainAny(configuredAliases, ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile) {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE autoban_bans SET active=0 WHERE active=1 AND auth_id=?`, ban.AuthID); err != nil {
			return err
		}
	}
	return nil
}

func fileBackedAuthState(values ...string) bool {
	for _, value := range values {
		if fileNameIfJSON(value) != "" {
			return true
		}
	}
	return false
}

func fileBackedCleanupAliases(authID, authIndex, source, authFile string) []string {
	var values []string
	for _, value := range []string{authFile, authID, authIndex, source} {
		if file := fileNameIfJSON(value); file != "" {
			values = append(values, file)
		}
	}
	if fileNameIfJSON(authFile) != "" && strings.TrimSpace(authIndex) != "" {
		values = append(values, authIndex)
	}
	return normalizeAccountAliases(values...)
}

func strictAuthStateAliasesForValues(authID, authIndex, source, authFile string) []string {
	authFile = fileNameIfJSON(authFile)
	authIDFile := fileNameIfJSON(authID)
	sourceFile := fileNameIfJSON(source)
	if authIDFile == "" && sourceFile == "" && (authFile == "" || normalizeAccountAlias(authID) != normalizeAccountAlias(authFile)) {
		return nil
	}
	var values []string
	if authFile != "" {
		values = append(values, authFile)
	}
	for _, file := range []string{authIDFile, sourceFile} {
		if file != "" {
			values = append(values, file)
		}
	}
	if authFile != "" && strings.TrimSpace(authIndex) != "" {
		values = append(values, authIndex)
	}
	return normalizeAccountAliases(values...)
}

func authStateMatchAliases(authID, authIndex, source, authFile string) []string {
	if aliases := strictAuthStateAliasesForValues(authID, authIndex, source, authFile); len(aliases) > 0 {
		return aliases
	}
	return normalizeAccountAliases(authID, authIndex, source, authFile)
}

func recentForbiddenThresholdReached(ctx context.Context, db *sql.DB, aliases []string, threshold int) bool {
	if threshold <= 0 || len(aliases) == 0 {
		return false
	}
	usageCond, usageArgs := sqlLowerInCondition([]string{"auth_id", "auth_index"}, aliases)
	triggerCond, triggerArgs := sqlLowerInCondition([]string{"auth_id", "auth_index", "auth_file"}, aliases)
	if usageCond == "" && triggerCond == "" {
		return false
	}
	query := `
SELECT status_code, failed
FROM (
  SELECT requested_at AS ts, id AS seq, status_code, failed
  FROM usage_events
  WHERE provider='codex' AND ` + usageCond + `
  UNION ALL
  SELECT finished_at AS ts, id + 1000000000 AS seq, http_status AS status_code,
    CASE WHEN status='success' THEN 0 ELSE 1 END AS failed
  FROM quota_trigger_runs
  WHERE provider='codex' AND ` + triggerCond + `
)
ORDER BY ts DESC, seq DESC
LIMIT ?`
	args := append([]any{}, usageArgs...)
	args = append(args, triggerArgs...)
	args = append(args, threshold)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return false
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var status int
		var failed int
		if err := rows.Scan(&status, &failed); err != nil {
			return false
		}
		if failed == 0 || status != http.StatusForbidden {
			return false
		}
		count++
	}
	return count >= threshold
}

func sqlLowerInCondition(columns []string, aliases []string) (string, []any) {
	seen := map[string]bool{}
	var values []string
	for _, alias := range aliases {
		alias = normalizeAccountAlias(alias)
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		values = append(values, alias)
	}
	if len(values) == 0 || len(columns) == 0 {
		return "", nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(values)), ",")
	parts := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)*len(values))
	for _, column := range columns {
		parts = append(parts, "lower("+column+") IN ("+placeholders+")")
		for _, value := range values {
			args = append(args, value)
		}
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func queryActiveInvalidAuths(ctx context.Context, db *sql.DB) ([]invalidAuthRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_file_mtime, auth_source_kind
FROM invalid_auths
WHERE active=1
ORDER BY invalidated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []invalidAuthRow
	for rows.Next() {
		var r invalidAuthRow
		var active int
		if err := rows.Scan(
			&r.AuthID, &r.AuthIndex, &r.Source, &r.Provider, &r.Reason,
			&r.InvalidatedAt, &active, &r.LastStatusCode, &r.AuthFile,
			&r.AuthFileMTime, &r.AuthSourceKind,
		); err != nil {
			return nil, err
		}
		r.Active = active != 0
		r.InvalidatedAtText = unixTime(r.InvalidatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func candidateMatchesActiveBan(candidate schedulerAuthCandidate, bans []autobanRow) bool {
	matched, _ := candidateMatchesActiveBanIndexes(candidate, bans)
	return matched
}

func candidateMatchesActiveBanIndexes(candidate schedulerAuthCandidate, bans []autobanRow) (bool, []int) {
	aliases := schedulerCandidateAliases(candidate)
	strictAliases := schedulerCandidateStrictAliases(candidate)
	if len(aliases) == 0 {
		return false, nil
	}
	var indexes []int
	for i, ban := range bans {
		banAliases := normalizeAccountAliases(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile)
		candidateAliases := aliases
		if strict := strictAuthStateAliasesForValues(ban.AuthID, ban.AuthIndex, ban.Source, ban.AuthFile); len(strict) > 0 {
			banAliases = strict
			candidateAliases = strictAliases
		}
		for _, banAlias := range banAliases {
			for _, alias := range candidateAliases {
				if alias != "" && alias == banAlias {
					indexes = append(indexes, i)
					goto nextBan
				}
			}
		}
	nextBan:
	}
	return len(indexes) > 0, indexes
}

func candidateMatchesInvalidAuth(candidate schedulerAuthCandidate, invalids []invalidAuthRow) bool {
	return len(candidateMatchesInvalidAuthIndexes(candidate, invalids)) > 0
}

func candidateMatchesInvalidAuthIndexes(candidate schedulerAuthCandidate, invalids []invalidAuthRow) []int {
	configured := configuredAccountForSchedulerCandidate(candidate)
	if normalizeAccountAlias(configured.AuthID) == "" && normalizeAccountAlias(configured.AuthIndex) == "" && normalizeAccountAlias(configured.AuthFile) == "" {
		return nil
	}
	identityIndex := newCodexAuthIdentityIndex([]configuredAccount{configured})
	var matched []int
	for i, invalid := range invalids {
		if len(identityIndex.matchIndexes(invalid)) > 0 {
			matched = append(matched, i)
		}
	}
	return matched
}

func configuredAccountForSchedulerCandidate(candidate schedulerAuthCandidate) configuredAccount {
	authIndex := firstNonEmptyString(candidate.Attributes["auth_index"], stringFromAny(candidate.Metadata["auth_index"]))
	authFile := firstNonEmptyString(
		candidate.Attributes["auth_file"],
		stringFromAny(candidate.Metadata["auth_file"]),
		candidate.Attributes["path"],
		candidate.Attributes["file"],
		stringFromAny(candidate.Metadata["path"]),
		stringFromAny(candidate.Metadata["file"]),
	)
	return configuredAccount{
		AuthID:    candidate.ID,
		AuthIndex: authIndex,
		Source:    firstNonEmptyString(candidate.Attributes["source"], stringFromAny(candidate.Metadata["source"])),
		Email:     firstNonEmptyString(candidate.Attributes["email"], stringFromAny(candidate.Metadata["email"])),
		AuthFile:  fileNameIfJSON(authFile),
		Provider:  candidate.Provider,
	}
}

func schedulerCandidateAliases(candidate schedulerAuthCandidate) []string {
	return accountIdentityAliases(accountIdentity{
		AuthID:    candidate.ID,
		AuthIndex: firstNonEmptyString(candidate.Attributes["auth_index"], stringFromAny(candidate.Metadata["auth_index"])),
		Source:    firstNonEmptyString(candidate.Attributes["source"], stringFromAny(candidate.Metadata["source"])),
		AuthFile:  firstNonEmptyString(candidate.Attributes["auth_file"], stringFromAny(candidate.Metadata["auth_file"])),
		Email:     firstNonEmptyString(candidate.Attributes["email"], stringFromAny(candidate.Metadata["email"])),
		Path: firstNonEmptyString(
			candidate.Attributes["path"],
			candidate.Attributes["file"],
			stringFromAny(candidate.Metadata["path"]),
			stringFromAny(candidate.Metadata["file"]),
		),
	})
}

func schedulerCandidateStrictAliases(candidate schedulerAuthCandidate) []string {
	authID := candidate.ID
	authIndex := firstNonEmptyString(candidate.Attributes["auth_index"], stringFromAny(candidate.Metadata["auth_index"]))
	source := firstNonEmptyString(candidate.Attributes["source"], stringFromAny(candidate.Metadata["source"]))
	authFile := firstNonEmptyString(
		candidate.Attributes["auth_file"],
		stringFromAny(candidate.Metadata["auth_file"]),
		candidate.Attributes["path"],
		candidate.Attributes["file"],
		stringFromAny(candidate.Metadata["path"]),
		stringFromAny(candidate.Metadata["file"]),
	)
	if file := fileNameIfJSON(authFile); file != "" {
		return normalizeAccountAliases(file, authIndex)
	}
	return strictAuthStateAliasesForValues(authID, authIndex, source, authFile)
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

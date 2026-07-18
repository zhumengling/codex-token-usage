package main

import (
	"strings"
	"testing"
)

func TestDashboardCacheTokensDoesNotDoubleCountOverlappingFields(t *testing.T) {
	markers := []string{
		`const cached=Number(r.cached_tokens||0),read=Number(r.cache_read_tokens||0),creation=Number(r.cache_creation_tokens||0);`,
		`return Math.max(cached-read-creation,0)+read;`,
		`function cacheWriteTokens(r){return Math.max(0,Number(r.cache_creation_tokens||0))}`,
		`cacheInputIncludesDetails(r)?Math.max(input,cache):input+cache`,
		`function cacheRate(r){return ratio(cacheTokens(r),cacheInputTotal(r))}`,
		`provider.includes('claude')||provider.includes('anthropic')`,
	}
	for _, marker := range markers {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard cache normalization missing %q", marker)
		}
	}
	if strings.Contains(dashboardScripts, `(r.cached_tokens||0)+(r.cache_read_tokens||0)+(r.cache_creation_tokens||0)`) {
		t.Fatal("dashboard still double-counts overlapping cache fields")
	}
}

func TestPoolTabSwitchReappliesLocale(t *testing.T) {
	start := strings.Index(dashboardScripts, "function switchPage(page){")
	if start < 0 {
		t.Fatal("switchPage function not found")
	}
	end := strings.Index(dashboardScripts[start:], "\nfunction providerStorageKey()")
	if end < 0 {
		t.Fatal("switchPage function end not found")
	}
	switchPage := dashboardScripts[start : start+end]
	renderAt := strings.Index(switchPage, "renderPoolPage(lastData);")
	localeAt := strings.Index(switchPage, "applyLocale();")
	if renderAt < 0 || localeAt < 0 || localeAt < renderAt {
		t.Fatalf("pool tab switch must reapply locale after rendering: %q", switchPage)
	}
}

func TestXAITabRequiresConfiguredAccount(t *testing.T) {
	if !strings.Contains(dashboardBody, `data-target="xai" role="tab" aria-selected="false" hidden`) {
		t.Fatal("xAI tab must start hidden until configured credentials are loaded")
	}
	if !strings.Contains(dashboardScripts, `const xaiVisible=(data.xai_accounts||[]).some(r=>r.configured);`) {
		t.Fatal("xAI tab visibility must depend on configured xAI auth accounts")
	}
	if !strings.Contains(dashboardScripts, `if(!xaiVisible&&activePage==='xai')activePage='codex';`) {
		t.Fatal("removed xAI auth must return the dashboard to Codex")
	}
}

func TestXAITierDisplayUsesMetadataFields(t *testing.T) {
	for _, marker := range []string{"r.xai_tier", "tier-free", "tier-super", "tier-heavy", "套餐分布"} {
		if !strings.Contains(dashboardScripts+dashboardStyles, marker) {
			t.Fatalf("xAI tier display marker %q not found", marker)
		}
	}
}

func TestCodexPoolDataCarriesForbiddenAuths(t *testing.T) {
	if !strings.Contains(dashboardScripts, "forbidden_auths:data.forbidden_auths||[]") {
		t.Fatal("Codex pool data must carry standalone 403 auth records into insights")
	}
}

func TestDashboardExplainsWaitingRuntimeAccountsAndStaleCandidates(t *testing.T) {
	for _, marker := range []string{"waiting_runtime_load", "等待 CPA 加载", "candidate_pool_stale", "CPA 候选缺少"} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard missing Issue #12 diagnostic marker %q", marker)
		}
	}
}

func TestInvalidAuthManagementUsesUnfilteredCountsAndPartialDeleteResults(t *testing.T) {
	for _, marker := range []string{
		"const allInvalidRows=",
		"const allWorkspaceRows=",
		"parseAuthFileDeleteResult(res,body,names)",
		"HTTP 207 部分删除失败",
		"/\\.json$/i.test(name)?name:''",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("401 management marker %q not found", marker)
		}
	}
}

func TestNonStandardAuthImportUIUsesPluginHostSaveFlow(t *testing.T) {
	for _, marker := range []string{
		"账号 JSON 导入",
		"auth-import/preview",
		"auth-import/commit",
		"host.auth.save",
		"无 RT",
	} {
		if !strings.Contains(dashboardBody+dashboardScripts, marker) && !strings.Contains(dashboardBody+dashboardScripts+dashboardStyles, marker) {
			t.Fatalf("auth import UI marker %q not found", marker)
		}
	}
}

func TestInvalidAuthManagementSeparatesSourcesAndResolvesStableIDs(t *testing.T) {
	for _, marker := range []string{
		"invalid-auths/resolve",
		"/v0/management/auth-files/status",
		"auth_source_kind",
		"runtime_only",
		"sameStableAuthIdentity",
		"Object.freeze(selected.map",
		"data-invalid-runtime-disable",
		"file_deleted",
		"file_absent",
		"runtime_disabled",
		"replacement_kept",
		"invalidAuthFileIdentityChanged",
		"invalid_auth_status_code",
		"forbidden_auths",
		"isCredentialStateBan",
		"403 拒绝",
		"renderOpenManagementModals",
		"原本不存在",
		"替换文件已保留",
		"临时禁用",
		"已经解除",
		"不可处理",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("401 stable cleanup marker %q not found", marker)
		}
	}
	for _, marker := range []string{"处理所有 401 账号", "处理选中"} {
		if !strings.Contains(dashboardBody, marker) {
			t.Fatalf("401 management UI marker %q not found", marker)
		}
	}
}

func TestQuotaActivationDashboardRequiresPreviewConfirmationAndBilingualCopy(t *testing.T) {
	for _, marker := range []string{
		"一次性启动额度窗口",
		"managementQuotaActivationPreviewApi",
		"managementQuotaActivationRunApi",
		"quotaActivationPreview.confirmation_token",
		"!!preview.force===document.getElementById('quota-activation-force').checked",
		"quota-activation-ack",
		"quotaActivationWindowTransitionHTML(row.before&&row.before.primary,row.after&&row.after.primary)",
		"quota-activation-pagination",
		"quotaActivationPageSize=50",
		"强制恢复模式必须先明确勾选账号。",
		"不保证恰好消耗一个 Token",
		"Primary 上报窗口（前 → 后）",
		"window.limit_window_seconds",
		"window.reset_after_seconds",
		"window.presence==='absent'",
		"'所有上报窗口均已验证':'All reported windows verified'",
		"'已发送但验证未知':'Sent; verification unknown'",
	} {
		if !strings.Contains(dashboardBody+dashboardScripts+dashboardStyles, marker) {
			t.Fatalf("quota activation UI marker %q not found", marker)
		}
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "Authorization: Bearer"} {
		if strings.Contains(dashboardBody, forbidden) {
			t.Fatalf("quota activation HTML contains credential marker %q", forbidden)
		}
	}
}

func TestEnglishLocaleTranslatesDynamicPhrasesBeforeUnits(t *testing.T) {
	for _, marker := range []string{
		"'账号 JSON 导入':'Import account JSON'",
		"'窗口：':'Window: '",
		"Object.entries(i18nEn).sort((left,right)=>right[0].length-left[0].length).forEach(pair=>exact(pair[0],pair[1]))",
		"'部分模型缺价格':'Some model prices missing'",
		"'管理接口':'Management API'",
		"'显示接入点':'Show endpoints'",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard script missing English dynamic-phrase translation marker %q", marker)
		}
	}
}

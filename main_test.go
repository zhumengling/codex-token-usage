package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResourceRoutesExposeOnlyStaticDashboard(t *testing.T) {
	raw, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatalf("management.register returned error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal registration envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("registration envelope ok = false: %#v", env.Error)
	}
	var registration managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &registration); err != nil {
		t.Fatalf("unmarshal registration: %v", err)
	}
	for _, resource := range registration.Resources {
		switch resource.Path {
		case "/dashboard":
		case "/summary", "/export":
			t.Fatalf("resource route %q exposes dynamic data without management auth", resource.Path)
		default:
			t.Fatalf("unexpected resource route %q", resource.Path)
		}
	}
	for _, path := range []string{
		"/v0/resource/plugins/codex-token-usage/summary",
		"/v0/resource/plugins/codex-token-usage/export",
	} {
		resp := handleManagement(managementRequest{Path: path})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("handleManagement(%q) status = %d, want 404", path, resp.StatusCode)
		}
	}
	resp := handleManagement(managementRequest{Path: "/v0/resource/plugins/codex-token-usage/dashboard"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard resource status = %d, want 200", resp.StatusCode)
	}
}

func TestDashboardHasAutobanPaginationAndBottomInset(t *testing.T) {
	for _, id := range []string{
		`id="autoban-scope"`,
		`id="autoban-page-size"`,
		`id="autoban-prev"`,
		`id="autoban-page-label"`,
		`id="autoban-next"`,
		`class="scroll autoban-table-wrap"`,
	} {
		if !strings.Contains(dashboardHTML, id) {
			t.Fatalf("dashboardHTML missing %s", id)
		}
	}
	if !strings.Contains(dashboardHTML, "padding:10px 10px 34px") {
		t.Fatalf("dashboardHTML missing desktop bottom inset")
	}
	if !strings.Contains(dashboardHTML, "main{padding:7px 7px 28px") {
		t.Fatalf("dashboardHTML missing mobile bottom inset")
	}
	for _, snippet := range []string{
		`let autobanPage=1;`,
		`let autobanPageSize=10;`,
		`cpa_token_usage_autoban_page_size`,
		`renderAutobans((lastData&&lastData.autobans)||[])`,
		`sortAutobansByRemaining(rows)`,
		`const pageRows=rows.slice(start,start+autobanPageSize);`,
		`document.getElementById('autoban-next').disabled=autobanPage>=pages;`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing autoban pagination script %q", snippet)
		}
	}
}

func TestDashboardLanguageSelectionPersistsAndIsNotTranslated(t *testing.T) {
	for _, snippet := range []string{
		`id="language" data-no-i18n`,
		`function safeStorageGet(storage,key)`,
		`function safeStorageSet(storage,key,value)`,
		`function safeStorageRemove(storage,key)`,
		`languageEl&&languageEl.value`,
		`safeStorageSet(safeLocalStorage(),languageStorageKey(),value);safeStorageSet(safeSessionStorage(),languageStorageKey(),value)`,
		`n.parentElement.closest('[data-no-i18n]')`,
		`el.closest('[data-no-i18n]')`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing language persistence/isolation snippet %q", snippet)
		}
	}
}

func TestDashboardShowsAndSearchesChatGPTAccountID(t *testing.T) {
	for _, snippet := range []string{
		`r.chatgpt_account_id`,
		`const accountId=firstText(r.chatgpt_account_id,'');`,
		`const id=accountId?('id '+accountId+' · '+fileId):fileId;`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing chatgpt account id snippet %q", snippet)
		}
	}
}

func TestDashboardHasClickableInvalidAuthCardAndManagementModal(t *testing.T) {
	for _, snippet := range []string{
		`id="invalid-auth-card"`,
		`account-summary-action invalid-auth-action`,
		`id="invalid-auth-modal"`,
		`id="invalid-auth-list"`,
		`id="invalid-auth-delete-all"`,
		`id="invalid-auth-select-page"`,
		`id="invalid-auth-delete-selected"`,
		`id="invalid-auth-oauth-url"`,
		`const managementCodexAuthUrlApi='/v0/management/codex-auth-url';`,
		`const managementAuthStatusApi='/v0/management/get-auth-status';`,
		`let invalidAuthPage=1;`,
		`const invalidAuthPageSize=10;`,
		`function openInvalidAuthModal()`,
		`function startInvalidAuthOAuth(key)`,
		`managementCodexAuthUrlApi+'?is_webui=true'`,
		`managementAuthStatusApi+'?state='`,
		`function selectCurrentInvalidAuthPage()`,
		`function deleteAllInvalidAuths()`,
		`function deleteSelectedInvalidAuths()`,
		`function handleInvalidAuthOAuthLinkClick(e)`,
		`data-oauth-copy`,
		`复制授权链接`,
		`body:JSON.stringify({names:names})`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing invalid auth management snippet %q", snippet)
		}
	}
	for _, snippet := range []string{
		`.account-summary-action`,
		`.invalid-auth-action.has-invalid`,
		`backdrop-filter:blur(10px) saturate(1.16)`,
		`.invalid-auth-panel`,
		`.invalid-auth-list`,
		`.invalid-auth-row`,
		`.oauth-link-row`,
		`.oauth-copy-link`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing invalid auth management style %q", snippet)
		}
	}
}

func TestDashboardHasWorkspaceDeactivatedCardAndDeleteOnlyModal(t *testing.T) {
	for _, snippet := range []string{
		`id="workspace-deactivated-card"`,
		`account-summary-action workspace-deactivated-action`,
		`id="workspace-deactivated-modal"`,
		`id="workspace-deactivated-list"`,
		`id="workspace-deactivated-delete-all"`,
		`id="workspace-deactivated-select-page"`,
		`id="workspace-deactivated-delete-selected"`,
		`let workspaceDeactivatedPage=1;`,
		`const workspaceDeactivatedPageSize=10;`,
		`function openWorkspaceDeactivatedModal()`,
		`function workspaceDeactivatedRows()`,
		`function selectCurrentWorkspaceDeactivatedPage()`,
		`function deleteAllWorkspaceDeactivatedAuths()`,
		`function deleteSelectedWorkspaceDeactivatedAuths()`,
		`body:JSON.stringify({names:names})`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing workspace deactivated management snippet %q", snippet)
		}
	}
	workspaceStart := strings.Index(dashboardHTML, `id="workspace-deactivated-modal"`)
	workspaceEnd := strings.Index(dashboardHTML[workspaceStart:], `<script>`)
	if workspaceStart < 0 || workspaceEnd < 0 {
		t.Fatalf("dashboardHTML missing workspace modal boundary")
	}
	workspaceModal := dashboardHTML[workspaceStart : workspaceStart+workspaceEnd]
	if strings.Contains(workspaceModal, "OAuth 登录") || strings.Contains(workspaceModal, "data-invalid-login") {
		t.Fatalf("workspace deactivated modal contains OAuth login action")
	}
	for _, snippet := range []string{
		`.account-summary-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(104px,1fr))`,
		`.workspace-deactivated-action.has-invalid`,
		`.workspace-deactivated-panel`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing workspace deactivated style %q", snippet)
		}
	}
}

func TestDashboardAppliesLocaleAfterTranslationsAreInitialized(t *testing.T) {
	translations := strings.Index(dashboardHTML, "const i18nEn={")
	apply := strings.Index(dashboardHTML, "applyLocale();")
	if translations < 0 || apply < 0 {
		t.Fatalf("dashboardHTML missing translations/applyLocale")
	}
	if apply < translations {
		t.Fatalf("applyLocale runs before i18nEn is initialized")
	}
}

func TestDashboardRejectedManagementKeyExpires(t *testing.T) {
	for _, snippet := range []string{
		`function recentRejectedManagementKey(){`,
		`cpa_token_usage_rejected_at`,
		`Date.now()-ts>5*60*1000`,
		`safeStorageRemove(storage,'cpa_token_usage_rejected_key')`,
		`safeStorageSet(safeSessionStorage(),'cpa_token_usage_rejected_at',String(Date.now()))`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing rejected key expiry snippet %q", snippet)
		}
	}
}

func TestDashboardStorageAccessIsGuarded(t *testing.T) {
	if strings.Contains(dashboardHTML, "localStorage.") || strings.Contains(dashboardHTML, "sessionStorage.") {
		t.Fatalf("dashboardHTML contains direct storage access; use safeStorage helpers so refresh cannot abort initialization")
	}
	for _, snippet := range []string{
		"safeStorageGet(localStorage,",
		"safeStorageSet(localStorage,",
		"safeStorageGet(sessionStorage,",
		"safeStorageSet(sessionStorage,",
		"safeStorageRemove(sessionStorage,",
	} {
		if strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML contains unsafe storage object access %q", snippet)
		}
	}
	for _, snippet := range []string{
		`function safeLocalStorage(){try{return window.localStorage}catch(e){return null}}`,
		`function safeSessionStorage(){try{return window.sessionStorage}catch(e){return null}}`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Fatalf("dashboardHTML missing storage accessor %q", snippet)
		}
	}
}

func TestSanitizeTriggerErrorKeepsErrorText(t *testing.T) {
	if got := sanitizeTriggerError(errors.New("context canceled")); got != "context canceled" {
		t.Fatalf("sanitizeTriggerError(error) = %q, want context canceled", got)
	}
	if got := sanitizeTriggerError(map[string]any{}); got == "{}" {
		t.Fatalf("sanitizeTriggerError(empty map) = %q, want no raw JSON object noise", got)
	}
}

func TestCodex429AutobanFiltersSchedulerCandidate(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(time.Hour).Unix()
	err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "auth-banned",
		AuthIndex:   "idx-banned",
		Source:      "banned@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-primary-window-minutes": {"300"},
		},
	})
	if err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-banned", Provider: "codex", Priority: 100},
			{ID: "auth-ok", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "auth-ok" {
		t.Fatalf("pickAuth response = %+v, want handled auth-ok", resp)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans, ok := data["autobans"].([]autobanRow)
	if !ok || len(bans) != 1 {
		t.Fatalf("summary autobans = %#v, want one active ban", data["autobans"])
	}
	if bans[0].AuthID != "auth-banned" || bans[0].Window != "5h" {
		t.Fatalf("ban = %+v, want auth-banned 5h", bans[0])
	}
}

func TestCodex429AutobanFiltersSchedulerCandidateByNormalizedAlias(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(time.Hour).Unix()
	err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "banned@example.cpa.json",
		AuthIndex:   "legacy-index",
		Source:      "banned@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {intToString(resetAt)},
		},
	})
	if err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{
				ID:       "other-id",
				Provider: "codex",
				Priority: 100,
				Attributes: map[string]string{
					"auth_file": "banned@example.cpa.json",
					"email":     "banned@example.com",
				},
			},
			{ID: "auth-ok", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "auth-ok" {
		t.Fatalf("pickAuth response = %+v, want handled auth-ok", resp)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	diagnostics := data["diagnostics"].(diagnosticsSummary)
	if diagnostics.Scheduler.ActiveBanCount != 0 || diagnostics.Scheduler.FilteredCandidates != 1 || diagnostics.Scheduler.UnmatchedActiveBans != 0 || diagnostics.Scheduler.LastFilteredAt == "" {
		t.Fatalf("scheduler diagnostics = %+v, want missing-file ban cleaned after one candidate was filtered", diagnostics.Scheduler)
	}
}

func TestAccountIdentityAliasesNormalizePathsAndCPAJSON(t *testing.T) {
	aliases := accountIdentityAliases(accountIdentity{
		AuthID:   `C:\auth\Nested\User@Example.COM.cpa.json`,
		AuthFile: `/var/lib/cpa/auth/User@Example.COM.cpa.json`,
		Email:    "user@example.com",
		Name:     "User Example",
	})
	want := []string{
		`c:\auth\nested\user@example.com.cpa.json`,
		"user@example.com.cpa.json",
		"user@example.com.cpa",
		"user@example.com",
		"user example",
	}
	for _, value := range want {
		found := false
		for _, alias := range aliases {
			if alias == value {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("aliases = %#v, want %q", aliases, value)
		}
	}
}

func TestCodex429AutobanRejectsWhenAllCandidatesFiltered(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "auth-banned",
		AuthIndex:   "idx-banned",
		Source:      "banned@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-primary-window-minutes": {"300"},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-banned", Provider: "codex", Priority: 100},
		},
	})
	if err == nil {
		t.Fatalf("pickAuth returned nil error with response %+v, want scheduler rejection", resp)
	}
	if !strings.Contains(err.Error(), "no available Codex auth") {
		t.Fatalf("pickAuth error = %q, want no available Codex auth", err.Error())
	}
}

func TestSchedulerPickRejectsAllBannedWithHTTPStatus(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	globalStore.close()
	t.Cleanup(globalStore.close)

	resetAt := time.Now().Add(time.Hour).Unix()
	if err := globalStore.recordUsage(context.Background(), usageRecord{
		Provider:    "codex",
		AuthID:      "auth-banned",
		AuthIndex:   "idx-banned",
		Source:      "banned@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-primary-window-minutes": {"300"},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	rawReq, err := json.Marshal(schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-banned", Provider: "codex", Priority: 100},
		},
	})
	if err != nil {
		t.Fatalf("marshal scheduler request: %v", err)
	}
	raw, err := handleMethod("scheduler.pick", rawReq)
	if err != nil {
		t.Fatalf("scheduler.pick returned error: %v", err)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal scheduler envelope: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("scheduler envelope = %s, want error envelope", string(raw))
	}
	if env.Error.Code != "auth_unavailable" || env.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("scheduler error = %+v, want auth_unavailable status 503", env.Error)
	}
}

func TestExpiredAutobanWithMillisecondResetIsClearedFromSummary(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	ctx := context.Background()
	store := &store{}
	defer store.close()

	expiredResetMS := time.Now().Add(-time.Minute).UnixMilli()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "expired-ban@example.com",
		AuthIndex:   "expired-ban",
		Source:      "expired-ban@example.com",
		RequestedAt: time.Now().Add(-2 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {strconv.FormatInt(expiredResetMS, 10)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 0 {
		t.Fatalf("autobans = %#v, want expired millisecond reset ban cleared", bans)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].PrimaryUsedPercent != nil || accounts[0].PrimaryResetAt != nil {
		t.Fatalf("accounts = %#v, want expired quota snapshot cleared", accounts)
	}
}

func TestAutobanSummaryUsesLatestQuotaSnapshotForDisplay(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	banResetAt := time.Now().Add(7 * 24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "quota-display@example.com",
		AuthIndex:   "quota-display",
		Source:      "quota-display@example.com",
		RequestedAt: time.Now().Add(-30 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"100"},
			"x-codex-primary-reset-at":       {intToString(time.Now().Add(2 * time.Hour).Unix())},
			"x-codex-secondary-used-percent": {"100"},
			"x-codex-secondary-reset-at":     {intToString(banResetAt)},
		},
	}); err != nil {
		t.Fatalf("record ban usage returned error: %v", err)
	}

	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	latestPrimary := 30.0
	latestSecondary := 100.0
	primaryResetAt := time.Now().Add(4 * time.Hour).Unix()
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "quota-display@example.com",
		AuthIndex:            "quota-display",
		Source:               "quota-display@example.com",
		Provider:             "codex",
		Mode:                 "quota",
		Status:               "success",
		StartedAt:            time.Now().Add(-5 * time.Minute).Unix(),
		FinishedAt:           time.Now().Add(-4 * time.Minute).Unix(),
		PrimaryUsedPercent:   &latestPrimary,
		PrimaryResetAt:       &primaryResetAt,
		SecondaryUsedPercent: &latestSecondary,
		SecondaryResetAt:     &banResetAt,
	}); err != nil {
		t.Fatalf("record quota trigger run: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 {
		t.Fatalf("autobans = %#v, want one active weekly ban", bans)
	}
	if bans[0].PrimaryUsedPercent == nil || math.Abs(*bans[0].PrimaryUsedPercent-30.0) > 0.000001 {
		t.Fatalf("autoban primary percent = %v, want latest 30", bans[0].PrimaryUsedPercent)
	}
	if bans[0].SecondaryUsedPercent == nil || math.Abs(*bans[0].SecondaryUsedPercent-100.0) > 0.000001 {
		t.Fatalf("autoban secondary percent = %v, want latest 100 to keep weekly ban active", bans[0].SecondaryUsedPercent)
	}
}

func TestCodex401InvalidAuthFiltersUntilAuthFileReplaced(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "broken.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":         "broken@example.com",
		"name":          "Broken",
		"type":          "codex",
		"access_token":  "old-secret",
		"refresh_token": "old-refresh",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	oldMod := time.Now().Add(-time.Hour)
	if err := os.Chtimes(authFile, oldMod, oldMod); err != nil {
		t.Fatalf("chtimes old auth file: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "broken@example.com",
		AuthIndex:   "broken.cpa.json",
		Source:      "broken@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "broken@example.com", Provider: "codex", Priority: 100},
			{ID: "healthy@example.com", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "healthy@example.com" {
		t.Fatalf("pickAuth response = %+v, want healthy account", resp)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	invalids, ok := data["invalid_auths"].([]invalidAuthRow)
	if !ok || len(invalids) != 1 {
		t.Fatalf("invalid_auths = %#v, want one invalid auth", data["invalid_auths"])
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 || bans[0].AuthID != "broken@example.com" || bans[0].Window != "401" || bans[0].LastStatusCode != http.StatusUnauthorized {
		t.Fatalf("autobans = %#v, want 401 invalid auth to use autoban flow", bans)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || !accounts[0].InvalidAuth {
		t.Fatalf("accounts = %#v, want invalid auth marked", accounts)
	}

	newMod := time.Now().Add(time.Hour)
	if err := os.WriteFile(authFile, []byte(`{"email":"broken@example.com","type":"codex","access_token":"new-secret"}`), 0600); err != nil {
		t.Fatalf("replace auth file: %v", err)
	}
	if err := os.Chtimes(authFile, newMod, newMod); err != nil {
		t.Fatalf("chtimes new auth file: %v", err)
	}

	resp, err = store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "broken@example.com", Provider: "codex", Priority: 100},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth after replace returned error: %v", err)
	}
	if resp.Handled {
		t.Fatalf("pickAuth after replace = %+v, want unhandled so CPA keeps configured scheduler", resp)
	}
	data, err = store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary after replace returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans after replace = %#v, want 401 ban cleared", got)
	}
}

func TestCodex402DeactivatedWorkspaceFiltersUntilAuthFileDeletedOrReplaced(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "team-deactivated.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "team-deactivated@example.com",
		"name":         "Team Deactivated",
		"type":         "codex",
		"access_token": "old-secret",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	oldMod := time.Now().Add(-time.Hour)
	if err := os.Chtimes(authFile, oldMod, oldMod); err != nil {
		t.Fatalf("chtimes old auth file: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "team-deactivated@example.com",
		AuthIndex:   "team-deactivated.cpa.json",
		Source:      "team-deactivated@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure: usageFailure{
			StatusCode: http.StatusPaymentRequired,
			Body:       `{"detail":{"code":"deactivated_workspace"}}`,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	resp, err := store.pickAuth(ctx, schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "team-deactivated@example.com", Provider: "codex", Priority: 100},
			{ID: "healthy@example.com", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if !resp.Handled || resp.AuthID != "healthy@example.com" {
		t.Fatalf("pickAuth response = %+v, want healthy account", resp)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	workspaceRows, ok := data["workspace_deactivated_auths"].([]invalidAuthRow)
	if !ok || len(workspaceRows) != 1 {
		t.Fatalf("workspace_deactivated_auths = %#v, want one row", data["workspace_deactivated_auths"])
	}
	if workspaceRows[0].LastStatusCode != http.StatusPaymentRequired || !strings.Contains(workspaceRows[0].Reason, "deactivated_workspace") {
		t.Fatalf("workspace row = %+v, want 402 deactivated_workspace", workspaceRows[0])
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 || bans[0].AuthID != "team-deactivated@example.com" || bans[0].Window != "402" || bans[0].LastStatusCode != http.StatusPaymentRequired {
		t.Fatalf("autobans = %#v, want 402 workspace auth to use autoban flow", bans)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || !accounts[0].WorkspaceDeactivated {
		t.Fatalf("accounts = %#v, want workspace deactivated marked", accounts)
	}

	newMod := time.Now().Add(time.Hour)
	if err := os.WriteFile(authFile, []byte(`{"email":"team-deactivated@example.com","type":"codex","access_token":"new-secret"}`), 0600); err != nil {
		t.Fatalf("replace auth file: %v", err)
	}
	if err := os.Chtimes(authFile, newMod, newMod); err != nil {
		t.Fatalf("chtimes new auth file: %v", err)
	}
	data, err = store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary after replace returned error: %v", err)
	}
	if got := data["workspace_deactivated_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("workspace_deactivated_auths after replace = %#v, want cleared", got)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans after replace = %#v, want 402 ban cleared", got)
	}
}

func TestCodex402SameEmailOnlyMarksMatchingAuthFile(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	const email = "same-workspace@example.com"
	files := []string{"same-a.cpa.json", "same-b.cpa.json", "same-c.cpa.json"}
	for _, file := range files {
		raw, err := json.Marshal(map[string]any{
			"email":              email,
			"type":               "codex",
			"access_token":       "secret",
			"chatgpt_account_id": strings.TrimSuffix(file, ".cpa.json"),
		})
		if err != nil {
			t.Fatalf("marshal %s: %v", file, err)
		}
		if err := os.WriteFile(filepath.Join(authDir, file), raw, 0600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      email,
		AuthIndex:   "same-b.cpa.json",
		Source:      email,
		RequestedAt: time.Now(),
		Failed:      true,
		Failure: usageFailure{
			StatusCode: http.StatusPaymentRequired,
			Body:       `{"detail":{"code":"deactivated_workspace"}}`,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	workspaces := data["workspace_deactivated_auths"].([]invalidAuthRow)
	if len(workspaces) != 1 || workspaces[0].AuthFile != "same-b.cpa.json" {
		t.Fatalf("workspace_deactivated_auths = %#v, want only same-b.cpa.json", workspaces)
	}
	accounts := data["accounts"].([]accountRow)
	marked := []string{}
	for _, account := range accounts {
		if account.WorkspaceDeactivated {
			marked = append(marked, account.AuthFile)
		}
	}
	if len(marked) != 1 || marked[0] != "same-b.cpa.json" {
		t.Fatalf("workspace-deactivated accounts = %#v, want only same-b.cpa.json", marked)
	}
}

func TestCodex402WithoutBodyRecordsWorkspaceDeactivatedForAuthFileAccount(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "ezraferreira6412@outlook.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"ezraferreira6412@outlook.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "ezraferreira6412@outlook.com.json",
		AuthIndex:   "2da74e4c2a9372b4",
		Source:      "ezraferreira6412@outlook.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusPaymentRequired},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	workspaces := data["workspace_deactivated_auths"].([]invalidAuthRow)
	if len(workspaces) != 1 || workspaces[0].AuthID != "ezraferreira6412@outlook.com.json" || workspaces[0].LastStatusCode != http.StatusPaymentRequired {
		t.Fatalf("workspace_deactivated_auths = %#v, want bodyless 402 auth file account", workspaces)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 || bans[0].Window != "402" || bans[0].AuthID != "ezraferreira6412@outlook.com.json" {
		t.Fatalf("autobans = %#v, want 402 effective ban", bans)
	}
}

func TestSummaryBackfillsWorkspaceDeactivatedFromHistoricalBodylessUsage402(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "ezradavidson6460@outlook.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"ezradavidson6460@outlook.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := db.ExecContext(ctx, insertSQL,
		time.Now().Unix(),
		"codex", "", "gpt-5.5", "",
		"", "ezradavidson6460@outlook.com.json", "f52489c585545c97", "", "ezradavidson6460@outlook.com",
		"", "", 0, 0, 1, http.StatusPaymentRequired,
		0, 0, 0, 0, 0, 0, 0,
		nil, nil, nil, nil,
	); err != nil {
		t.Fatalf("insert historical usage event: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	workspaces := data["workspace_deactivated_auths"].([]invalidAuthRow)
	if len(workspaces) != 1 || workspaces[0].AuthID != "ezradavidson6460@outlook.com.json" || workspaces[0].AuthFile != "ezradavidson6460@outlook.com.json" {
		t.Fatalf("workspace_deactivated_auths = %#v, want historical bodyless 402 backfilled", workspaces)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 1 || bans[0].Window != "402" || bans[0].AuthID != "ezradavidson6460@outlook.com.json" {
		t.Fatalf("autobans = %#v, want backfilled 402 effective ban", bans)
	}
}

func TestSummaryDoesNotBackfillHistorical402AfterLaterSuccessfulUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	for _, row := range []struct {
		at     int64
		failed int
		status int
	}{
		{at: base, failed: 1, status: http.StatusPaymentRequired},
		{at: base + 30, failed: 0, status: http.StatusOK},
	} {
		if _, err := db.ExecContext(ctx, insertSQL,
			row.at,
			"codex", "", "gpt-5.5", "",
			"", "recovered@example.com.json", "abc123recovered", "", "recovered@example.com",
			"", "", 0, 0, row.failed, row.status,
			0, 0, 0, 0, 0, 0, 0,
			nil, nil, nil, nil,
		); err != nil {
			t.Fatalf("insert usage event status %d: %v", row.status, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["workspace_deactivated_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("workspace_deactivated_auths = %#v, want no backfill after later success", got)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want no effective 402 ban after later success", got)
	}
}

func TestSummaryDoesNotBackfillHistorical402AfterLaterSuccessfulUsageWithDifferentAliasFields(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-alias@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-alias@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	for _, row := range []struct {
		at        int64
		authID    string
		authIndex string
		source    string
		failed    int
		status    int
	}{
		{at: base, authID: "recovered-alias@example.com.json", authIndex: "abc123alias", source: "recovered-alias@example.com", failed: 1, status: http.StatusPaymentRequired},
		{at: base + 30, source: "recovered-alias@example.com", failed: 0, status: http.StatusOK},
	} {
		if _, err := db.ExecContext(ctx, insertSQL,
			row.at,
			"codex", "", "gpt-5.5", "",
			"", row.authID, row.authIndex, "", row.source,
			"", "", 0, 0, row.failed, row.status,
			0, 0, 0, 0, 0, 0, 0,
			nil, nil, nil, nil,
		); err != nil {
			t.Fatalf("insert usage event status %d: %v", row.status, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["workspace_deactivated_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("workspace_deactivated_auths = %#v, want no backfill after later success matched by source", got)
	}
}

func TestSuccessfulUsageClearsActiveWorkspaceDeactivatedAuth(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-active@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-active@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	rec := usageRecord{
		Provider:    "codex",
		AuthID:      "recovered-active@example.com.json",
		AuthIndex:   "abc123active",
		Source:      "recovered-active@example.com",
		RequestedAt: time.Now().Add(-time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusPaymentRequired},
	}
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record 402 usage: %v", err)
	}
	rec.RequestedAt = time.Now()
	rec.Failed = false
	rec.Failure = usageFailure{}
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record successful usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["workspace_deactivated_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("workspace_deactivated_auths = %#v, want cleared after success", got)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want cleared after success", got)
	}
}

func TestSuccessfulUsageClearsActiveUnauthorizedAuth(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-401@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-401@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	rec := usageRecord{
		Provider:    "codex",
		AuthID:      "recovered-401@example.com.json",
		AuthIndex:   "abc123401",
		Source:      "recovered-401@example.com",
		RequestedAt: time.Now().Add(-time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
	}
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record 401 usage: %v", err)
	}
	rec.RequestedAt = time.Now()
	rec.Failed = false
	rec.Failure = usageFailure{}
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record successful usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["invalid_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("invalid_auths = %#v, want cleared after success", got)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want cleared after success", got)
	}
}

func TestSummaryClearsHistoricalActiveInvalidAuthAfterLaterSuccessfulUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	if _, err := db.ExecContext(ctx, `
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active, last_status_code
) VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		"historical-401@example.com.json", "abc401historical", "historical-401@example.com", "codex",
		"401 unauthorized: credential is invalid", base, http.StatusUnauthorized,
	); err != nil {
		t.Fatalf("insert invalid auth: %v", err)
	}
	if _, err := db.ExecContext(ctx, insertSQL,
		base+30,
		"codex", "", "gpt-5.5", "",
		"", "", "", "", "historical-401@example.com",
		"", "", 0, 0, 0, http.StatusOK,
		0, 0, 0, 0, 0, 0, 0,
		nil, nil, nil, nil,
	); err != nil {
		t.Fatalf("insert successful usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["invalid_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("invalid_auths = %#v, want summary to clear recovered invalid auth", got)
	}
}

func TestSummaryDoesNotBackfillHistorical429AfterLaterSuccessfulUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-429@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-429@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	resetAt := time.Now().Add(time.Hour).Unix()
	for _, row := range []struct {
		at     int64
		failed int
		status int
		pp     any
		pr     any
	}{
		{at: base, failed: 1, status: http.StatusTooManyRequests, pp: 100.0, pr: resetAt},
		{at: base + 30, failed: 0, status: http.StatusOK},
	} {
		if _, err := db.ExecContext(ctx, insertSQL,
			row.at,
			"codex", "", "gpt-5.5", "",
			"", "recovered-429@example.com.json", "abc123429", "", "recovered-429@example.com",
			"", "", 0, 0, row.failed, row.status,
			0, 0, 0, 0, 0, 0, 0,
			row.pp, row.pr, nil, nil,
		); err != nil {
			t.Fatalf("insert usage event status %d: %v", row.status, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want no 429 backfill after later success", got)
	}
}

func TestSummaryDoesNotBackfillHistorical429AfterLaterSuccessfulUsageWithDifferentAliasFields(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-alias-429@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-alias-429@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	resetAt := time.Now().Add(time.Hour).Unix()
	for _, row := range []struct {
		at        int64
		authID    string
		authIndex string
		source    string
		failed    int
		status    int
		pp        any
		pr        any
	}{
		{at: base, authID: "recovered-alias-429@example.com.json", authIndex: "abc123alias429", source: "recovered-alias-429@example.com", failed: 1, status: http.StatusTooManyRequests, pp: 100.0, pr: resetAt},
		{at: base + 30, source: "recovered-alias-429@example.com", failed: 0, status: http.StatusOK},
	} {
		if _, err := db.ExecContext(ctx, insertSQL,
			row.at,
			"codex", "", "gpt-5.5", "",
			"", row.authID, row.authIndex, "", row.source,
			"", "", 0, 0, row.failed, row.status,
			0, 0, 0, 0, 0, 0, 0,
			row.pp, row.pr, nil, nil,
		); err != nil {
			t.Fatalf("insert usage event status %d: %v", row.status, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want no 429 backfill after later success matched by source", got)
	}
}

func TestSummaryClearsHistoricalActive429AfterLaterSuccessfulUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().Add(-time.Minute).Unix()
	resetAt := time.Now().Add(time.Hour).Unix()
	if _, err := db.ExecContext(ctx, `
INSERT INTO autoban_bans (
  auth_id, auth_index, source, provider, window, reason, banned_at, reset_at, active,
  last_status_code, primary_used_percent, primary_reset_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		"historical-429@example.com.json", "abc429historical", "historical-429@example.com", "codex",
		"5h", "primary 5h window is full", base, resetAt, http.StatusTooManyRequests, 100.0, resetAt,
	); err != nil {
		t.Fatalf("insert autoban: %v", err)
	}
	if _, err := db.ExecContext(ctx, insertSQL,
		base+30,
		"codex", "", "gpt-5.5", "",
		"", "", "", "", "historical-429@example.com",
		"", "", 0, 0, 0, http.StatusOK,
		0, 0, 0, 0, 0, 0, 0,
		nil, nil, nil, nil,
	); err != nil {
		t.Fatalf("insert successful usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want summary to clear recovered 429 ban", got)
	}
}

func TestSuccessfulUsageClearsActive429Autoban(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "recovered-active-429@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"recovered-active-429@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	resetAt := time.Now().Add(time.Hour).Unix()
	rec := usageRecord{
		Provider:    "codex",
		AuthID:      "recovered-active-429@example.com.json",
		AuthIndex:   "abc123active429",
		Source:      "recovered-active-429@example.com",
		RequestedAt: time.Now().Add(-time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {strconv.FormatInt(resetAt, 10)},
		},
	}
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record 429 usage: %v", err)
	}
	rec.RequestedAt = time.Now()
	rec.Failed = false
	rec.Failure = usageFailure{}
	rec.ResponseHeaders = nil
	if err := store.recordUsage(ctx, rec); err != nil {
		t.Fatalf("record successful usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want cleared after success", got)
	}
}

func TestSummaryHidesDeletedConfiguredCodexAccounts(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	activeAuth := filepath.Join(authDir, "active.cpa.json")
	if err := os.WriteFile(activeAuth, []byte(`{"email":"active@example.com","type":"codex","access_token":"active-token"}`), 0600); err != nil {
		t.Fatalf("write active auth: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "active@example.com",
		AuthIndex:   "active@example.com",
		Source:      "active@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}); err != nil {
		t.Fatalf("recordUsage active returned error: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "deleted@example.cpa.json",
		AuthIndex:   "deleted@example.cpa.json",
		Source:      "deleted@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
		Detail: usageDetail{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}); err != nil {
		t.Fatalf("recordUsage deleted returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want only current auth account: %#v", len(accounts), accounts)
	}
	if accounts[0].Email != "active@example.com" || !accounts[0].Configured {
		t.Fatalf("account = %#v, want configured active@example.com", accounts[0])
	}
	invalids := data["invalid_auths"].([]invalidAuthRow)
	if len(invalids) != 0 {
		t.Fatalf("invalid_auths = %#v, want deleted auth cleared from active diagnostics", invalids)
	}
}

func TestSummaryClearsDeletedConfiguredAuthState(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	activeAuth := filepath.Join(authDir, "active.cpa.json")
	if err := os.WriteFile(activeAuth, []byte(`{"email":"active@example.com","type":"codex","access_token":"active-token"}`), 0600); err != nil {
		t.Fatalf("write active auth: %v", err)
	}
	resetAt := time.Now().Add(time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "deleted@example.cpa.json",
		AuthIndex:   "deleted-index",
		Source:      "deleted@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage deleted 429 returned error: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "deleted401@example.cpa.json",
		AuthIndex:   "deleted401-index",
		Source:      "deleted401@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
	}); err != nil {
		t.Fatalf("recordUsage deleted 401 returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	if got := data["autobans"].([]autobanRow); len(got) != 0 {
		t.Fatalf("autobans = %#v, want deleted auth autoban cleared", got)
	}
	if got := data["invalid_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("invalid_auths = %#v, want deleted auth invalid cleared", got)
	}
}

func TestSchedulerKeepsHostFillFirstWhenNoFilteringNeeded(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	resp, err := store.pickAuth(context.Background(), schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "auth-a", Provider: "codex", Priority: 100},
			{ID: "auth-b", Provider: "codex", Priority: 10},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if resp.Handled || resp.DelegateBuiltin != "" {
		t.Fatalf("pickAuth = %+v, want unhandled to preserve CPA fill-first/round-robin setting", resp)
	}
}

func TestSchedulerDoesNotHandleNonCodexRoute(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	resp, err := store.pickAuth(context.Background(), schedulerPickRequest{
		Provider: "claude",
		Candidates: []schedulerAuthCandidate{
			{ID: "claude-a", Provider: "claude", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	if resp.Handled {
		t.Fatalf("non-codex pickAuth response = %+v, want unhandled", resp)
	}
}

func TestSummaryMergesConfiguredCodexAccountsWithoutLeakingTokens(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)

	ctx := context.Background()
	store := &store{}
	defer store.close()

	for i := 1; i <= 12; i++ {
		email := fmt.Sprintf("account%02d@example.com", i)
		authFile := filepath.Join(authDir, email+".cpa.json")
		raw, err := json.Marshal(map[string]any{
			"email":         email,
			"name":          fmt.Sprintf("Account %02d", i),
			"type":          "codex",
			"plan_type":     "plus",
			"disabled":      i == 12,
			"expired":       false,
			"access_token":  "secret-access-token",
			"refresh_token": "secret-refresh-token",
			"id_token":      "secret-id-token",
		})
		if err != nil {
			t.Fatalf("marshal auth file: %v", err)
		}
		if err := os.WriteFile(authFile, raw, 0600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}
		if i <= 9 {
			if err := store.recordUsage(ctx, usageRecord{
				Provider:    "codex",
				AuthID:      email,
				AuthIndex:   fmt.Sprintf("%016x", i),
				Source:      email,
				RequestedAt: time.Now(),
				Detail: usageDetail{
					InputTokens:  100,
					OutputTokens: 50,
				},
			}); err != nil {
				t.Fatalf("recordUsage %d returned error: %v", i, err)
			}
		}
	}

	data, err := store.summary(ctx, "24h", 2000)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts, ok := data["accounts"].([]accountRow)
	if !ok {
		t.Fatalf("summary accounts = %#v, want []accountRow", data["accounts"])
	}
	if len(accounts) != 12 {
		t.Fatalf("summary accounts len = %d, want 12", len(accounts))
	}
	configured := 0
	zeroUsage := 0
	disabled := 0
	for _, account := range accounts {
		if account.Configured {
			configured++
		}
		if account.Requests == 0 {
			zeroUsage++
		}
		if account.Disabled {
			disabled++
		}
	}
	if configured != 12 || zeroUsage != 3 || disabled != 1 {
		t.Fatalf("configured=%d zeroUsage=%d disabled=%d, want 12/3/1", configured, zeroUsage, disabled)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if text := string(raw); strings.Contains(text, "secret-access-token") || strings.Contains(text, "secret-refresh-token") || strings.Contains(text, "secret-id-token") {
		t.Fatalf("summary leaked token material: %s", text)
	}
}

func TestSummarySplitsConfiguredCodexAccountsByChatGPTAccountID(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)

	ctx := context.Background()
	store := &store{}
	defer store.close()

	const email = "same@example.com"
	authFiles := []struct {
		file      string
		email     string
		accountID string
	}{
		{file: "same-workspace-a.cpa.json", email: email, accountID: "account-id-a"},
		{file: "same-workspace-b.cpa.json", email: email, accountID: "account-id-b"},
		{file: "other-email-same-workspace.cpa.json", email: "other@example.com", accountID: "account-id-a"},
	}
	for _, authFile := range authFiles {
		raw, err := json.Marshal(map[string]any{
			"email":              authFile.email,
			"type":               "codex",
			"access_token":       "secret-access-token",
			"chatgpt_account_id": authFile.accountID,
		})
		if err != nil {
			t.Fatalf("marshal auth file %s: %v", authFile.file, err)
		}
		if err := os.WriteFile(filepath.Join(authDir, authFile.file), raw, 0600); err != nil {
			t.Fatalf("write auth file %s: %v", authFile.file, err)
		}
	}

	data, err := store.summary(ctx, "24h", 2000)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != len(authFiles) {
		t.Fatalf("accounts len = %d, want one row per auth file: %#v", len(accounts), accounts)
	}
	seenFiles := map[string]string{}
	for _, account := range accounts {
		if !account.Configured {
			t.Fatalf("account = %#v, want configured account", account)
		}
		seenFiles[account.AuthFile] = account.ChatGPTAccountID
	}
	for _, authFile := range authFiles {
		if seenFiles[authFile.file] != authFile.accountID {
			t.Fatalf("auth file %s account id = %q, want %q; seen=%#v", authFile.file, seenFiles[authFile.file], authFile.accountID, seenFiles)
		}
	}
}

func TestSummaryKeepsQuotaWindowsSeparateForSameEmailAuthFiles(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)

	ctx := context.Background()
	store := &store{}
	defer store.close()

	const email = "same@example.com"
	resetAt := time.Now().Add(4 * time.Hour).Unix()
	secondaryResetAt := time.Now().Add(7*24*time.Hour - time.Hour).Unix()
	authFiles := []struct {
		file      string
		index     string
		accountID string
		tokens    int64
		primary   string
		secondary string
	}{
		{file: "same-a.cpa.json", index: "idx-a", accountID: "workspace-a", tokens: 100, primary: "10", secondary: "2"},
		{file: "same-b.cpa.json", index: "idx-b", accountID: "workspace-b", tokens: 200, primary: "30", secondary: "5"},
	}
	for _, authFile := range authFiles {
		raw, err := json.Marshal(map[string]any{
			"email":              email,
			"type":               "codex",
			"access_token":       "secret-access-token",
			"chatgpt_account_id": authFile.accountID,
		})
		if err != nil {
			t.Fatalf("marshal auth file %s: %v", authFile.file, err)
		}
		if err := os.WriteFile(filepath.Join(authDir, authFile.file), raw, 0600); err != nil {
			t.Fatalf("write auth file %s: %v", authFile.file, err)
		}
		if err := store.recordUsage(ctx, usageRecord{
			Provider:    "codex",
			AuthID:      authFile.file,
			AuthIndex:   authFile.index,
			Source:      email,
			RequestedAt: time.Now().Add(-10 * time.Minute),
			Detail: usageDetail{
				TotalTokens: authFile.tokens,
			},
			ResponseHeaders: map[string][]string{
				"x-codex-primary-used-percent":   {authFile.primary},
				"x-codex-primary-reset-at":       {intToString(resetAt)},
				"x-codex-secondary-used-percent": {authFile.secondary},
				"x-codex-secondary-reset-at":     {intToString(secondaryResetAt)},
			},
		}); err != nil {
			t.Fatalf("recordUsage %s returned error: %v", authFile.file, err)
		}
	}

	data, err := store.summary(ctx, "24h", 2000)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	byFile := map[string]accountRow{}
	for _, account := range accounts {
		byFile[account.AuthFile] = account
	}
	for _, authFile := range authFiles {
		account, ok := byFile[authFile.file]
		if !ok {
			t.Fatalf("missing account row for %s in %#v", authFile.file, accounts)
		}
		if account.PrimaryWindowTokens != authFile.tokens || account.SecondaryWindowTokens != authFile.tokens {
			t.Fatalf("%s window tokens = primary %d secondary %d, want %d/%d", authFile.file, account.PrimaryWindowTokens, account.SecondaryWindowTokens, authFile.tokens, authFile.tokens)
		}
		wantPrimary, _ := strconv.ParseFloat(authFile.primary, 64)
		wantSecondary, _ := strconv.ParseFloat(authFile.secondary, 64)
		if account.PrimaryUsedPercent == nil || *account.PrimaryUsedPercent != wantPrimary || account.SecondaryUsedPercent == nil || *account.SecondaryUsedPercent != wantSecondary {
			t.Fatalf("%s quota percent = primary %v secondary %v, want %v/%v", authFile.file, account.PrimaryUsedPercent, account.SecondaryUsedPercent, wantPrimary, wantSecondary)
		}
	}
}

func TestSummaryFlagsQuotaDropWithoutLocalUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(4 * time.Hour).Unix()
	account := "shared@example.com"
	first := time.Now().Add(-20 * time.Minute)
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      account,
			AuthIndex:   "shared-account",
			Source:      account,
			RequestedAt: first,
			ResponseHeaders: map[string][]string{
				"x-codex-primary-used-percent": {"12"},
				"x-codex-primary-reset-at":     {intToString(resetAt)},
			},
		},
		{
			Provider:    "codex",
			AuthID:      account,
			AuthIndex:   "shared-account",
			Source:      account,
			RequestedAt: first.Add(15 * time.Minute),
			ResponseHeaders: map[string][]string{
				"x-codex-primary-used-percent": {"18.5"},
				"x-codex-primary-reset-at":     {intToString(resetAt)},
			},
		},
	}
	for i, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage %d returned error: %v", i, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	alerts, ok := data["external_use_alerts"].([]externalUseAlert)
	if !ok || len(alerts) != 1 {
		t.Fatalf("external_use_alerts = %#v, want one alert", data["external_use_alerts"])
	}
	if alerts[0].Window != "5h" || alerts[0].DeltaPercent != 6.5 || alerts[0].LocalTokens != 0 {
		t.Fatalf("alert = %+v, want 5h delta 6.5 local 0", alerts[0])
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || !accounts[0].ExternalUseSuspected || accounts[0].ExternalUseWindow != "5h" {
		t.Fatalf("accounts = %#v, want external use suspected on 5h", accounts)
	}
}

func TestSummaryEstimatesSecondaryQuotaCapacity(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "quota@example.com",
		AuthIndex:   "quota-account",
		Source:      "quota@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  200,
			OutputTokens: 50,
			TotalTokens:  250,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"25"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 750 {
		t.Fatalf("account quota estimates = total %d remaining %d, want 1000/750", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
	totals := data["totals"].(totalsRow)
	if totals.SecondaryQuotaTotalEstimate != 1000 || totals.SecondaryQuotaRemainingEstimate != 750 || totals.SecondaryQuotaEstimatedAccounts != 1 {
		t.Fatalf("total quota estimates = total %d remaining %d accounts %d, want 1000/750/1", totals.SecondaryQuotaTotalEstimate, totals.SecondaryQuotaRemainingEstimate, totals.SecondaryQuotaEstimatedAccounts)
	}
	if accounts[0].SecondaryQuotaSource != "usage" || accounts[0].SecondaryQuotaEstimateSource != "estimated" || accounts[0].QuotaSource != "estimated" {
		t.Fatalf("quota sources = source %q estimate %q overall %q, want usage/estimated/estimated", accounts[0].SecondaryQuotaSource, accounts[0].SecondaryQuotaEstimateSource, accounts[0].QuotaSource)
	}
	if accounts[0].SecondaryQuotaObservedFrom != "response_header" || accounts[0].SecondaryQuotaEstimateMethod != "local_tokens_percent_estimate" || accounts[0].QuotaWindowSource != "default_7d" {
		t.Fatalf("quota details = observed %q method %q window source %q, want response_header/local_tokens_percent_estimate/default_7d", accounts[0].SecondaryQuotaObservedFrom, accounts[0].SecondaryQuotaEstimateMethod, accounts[0].QuotaWindowSource)
	}
}

func TestSchemaCreatesPerformanceIndexes(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	indexes := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{
		"idx_usage_events_requested_auth_id",
		"idx_usage_events_requested_source",
		"idx_usage_events_quota_scan",
		"idx_quota_trigger_runs_status_finished",
		"idx_quota_trigger_runs_auth_file_finished",
	} {
		if !indexes[name] {
			t.Fatalf("missing performance index %q; indexes=%#v", name, indexes)
		}
	}
}

func TestSummaryPrecomputeCacheReturnsCachedData(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "cached@example.com",
		AuthIndex:   "cached-account",
		Source:      "cached@example.com",
		RequestedAt: time.Now(),
		Detail:      usageDetail{TotalTokens: 123},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	manager := &summaryPrecomputeManager{}
	cfg := normalizePluginConfig(defaultPluginConfig())
	cfg.SummaryPrecomputeEnabled = true
	cfg.SummaryPrecomputeIntervalSeconds = 60
	if err := manager.refresh(ctx, store, cfg, []summaryCacheKey{{Window: "24h", Limit: 10}}); err != nil {
		t.Fatalf("precompute refresh returned error: %v", err)
	}
	data, ok := manager.cached("24h", 10, cfg)
	if !ok {
		t.Fatalf("precompute cache miss after refresh")
	}
	cacheInfo, ok := data["precompute"].(summaryPrecomputeInfo)
	if !ok || !cacheInfo.Hit || cacheInfo.Window != "24h" || cacheInfo.Limit != 10 {
		t.Fatalf("precompute info = %#v, want cache hit 24h/10", data["precompute"])
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].TotalTokens != 123 {
		t.Fatalf("cached accounts = %#v, want cached usage account", accounts)
	}
}

func TestSummaryPrecomputeForceRefreshBypassesStaleAuthFileCache(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "stale-delete@example.com.json")
	if err := os.WriteFile(authFile, []byte(`{"email":"stale-delete@example.com","type":"codex","access_token":"secret"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "stale-delete@example.com.json",
		AuthIndex:   "stale-delete@example.com.json",
		Source:      "stale-delete@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	manager := &summaryPrecomputeManager{}
	cfg := normalizePluginConfig(defaultPluginConfig())
	cfg.SummaryPrecomputeEnabled = true
	cfg.SummaryPrecomputeIntervalSeconds = 60
	if err := manager.refresh(ctx, store, cfg, []summaryCacheKey{{Window: "all", Limit: 10}}); err != nil {
		t.Fatalf("precompute refresh returned error: %v", err)
	}
	if err := os.Remove(authFile); err != nil {
		t.Fatalf("remove auth file: %v", err)
	}
	cached, err := manager.summary(ctx, store, "all", 10)
	if err != nil {
		t.Fatalf("cached summary returned error: %v", err)
	}
	if got := cached["invalid_auths"].([]invalidAuthRow); len(got) != 1 {
		t.Fatalf("cached invalid_auths = %#v, want stale cached row before force refresh", got)
	}
	fresh, err := manager.summaryFresh(ctx, store, "all", 10)
	if err != nil {
		t.Fatalf("fresh summary returned error: %v", err)
	}
	if got := fresh["invalid_auths"].([]invalidAuthRow); len(got) != 0 {
		t.Fatalf("fresh invalid_auths = %#v, want deleted auth file removed", got)
	}
	info, ok := fresh["precompute"].(summaryPrecomputeInfo)
	if !ok || info.Hit || !info.Synchronous {
		t.Fatalf("fresh precompute info = %#v, want synchronous cache bypass", fresh["precompute"])
	}
}

func TestBulkQuotaSnapshotMatchesAuthFileAlias(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	resetAt := time.Now().Add(24 * time.Hour).Unix()
	percent := 42.0
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "other",
		AuthIndex:            "legacy",
		Source:               "alias@example.com",
		AuthFile:             "alias@example.cpa.json",
		Provider:             "codex",
		Mode:                 "quota",
		Status:               "success",
		StartedAt:            time.Now().Unix() - 1,
		FinishedAt:           time.Now().Unix(),
		SecondaryUsedPercent: &percent,
		SecondaryResetAt:     &resetAt,
	}); err != nil {
		t.Fatalf("record quota trigger run: %v", err)
	}
	snapshots := queryLatestAccountWindowQuotaSnapshots(ctx, db, []accountRow{{
		AuthIndex: "alias@example.cpa.json",
		AuthFile:  "alias@example.cpa.json",
		Email:     "alias@example.com",
	}}, 0, "secondary")
	got, ok := snapshots[0]
	if !ok || !got.Percent.Valid || got.Percent.Float64 != 42 || got.Source != "trigger" {
		t.Fatalf("bulk snapshot = %+v ok=%v, want trigger percent 42", got, ok)
	}
}

func TestSecondaryQuotaEstimateAdjustsTriggerRemainingWithLaterUsage(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	resetAt := time.Now().Add(48 * time.Hour).Unix()
	finishedAt := time.Now().Add(-2 * time.Minute).Unix()
	limit := int64(1000)
	remaining := int64(775)
	secondaryPct := 22.5
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "paid@example.com",
		AuthIndex:            "paid-account",
		Source:               "paid@example.com",
		Provider:             "codex",
		Mode:                 "quota",
		Status:               "success",
		StartedAt:            finishedAt - 1,
		FinishedAt:           finishedAt,
		SecondaryUsedPercent: &secondaryPct,
		SecondaryResetAt:     &resetAt,
		SecondaryLimit:       &limit,
		SecondaryRemaining:   &remaining,
	}); err != nil {
		t.Fatalf("record quota trigger run: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "paid@example.com",
		AuthIndex:   "paid-account",
		Source:      "paid@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  100,
			OutputTokens: 25,
			TotalTokens:  125,
		},
	}); err != nil {
		t.Fatalf("record later usage: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 650 {
		t.Fatalf("secondary quota estimate = total %d remaining %d, want trigger total 1000 and adjusted remaining 650", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
	if accounts[0].SecondaryQuotaSource != "trigger" || accounts[0].SecondaryQuotaEstimateSource != "trigger" || accounts[0].QuotaSource != "trigger" {
		t.Fatalf("quota sources = source %q estimate %q overall %q, want trigger", accounts[0].SecondaryQuotaSource, accounts[0].SecondaryQuotaEstimateSource, accounts[0].QuotaSource)
	}
	if accounts[0].SecondaryQuotaObservedFrom != "quota_trigger" || accounts[0].SecondaryQuotaEstimateMethod != "quota_trigger_capacity" {
		t.Fatalf("quota details = observed %q method %q, want quota_trigger/quota_trigger_capacity", accounts[0].SecondaryQuotaObservedFrom, accounts[0].SecondaryQuotaEstimateMethod)
	}
}

func TestFreeAccountUsesMonthlyQuotaWindowIndependentOfSummaryWindow(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "free.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "free@example.com",
		"type":         "codex",
		"plan_type":    "free",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	resetAt := time.Now().Add(20 * 24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "free@example.com",
		AuthIndex:   "free.cpa.json",
		Source:      "free@example.com",
		RequestedAt: time.Now().Add(-10 * 24 * time.Hour),
		Detail: usageDetail{
			InputTokens:  240,
			OutputTokens: 60,
			TotalTokens:  300,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"30"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want configured free account", len(accounts))
	}
	if accounts[0].SecondaryQuotaWindow != "month" {
		t.Fatalf("secondary quota window = %q, want month", accounts[0].SecondaryQuotaWindow)
	}
	if accounts[0].SecondaryWindowTokens != 300 || accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 700 {
		t.Fatalf("monthly quota = window tokens %d total %d remaining %d, want 300/1000/700", accounts[0].SecondaryWindowTokens, accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
	if accounts[0].PrimaryQuotaWindow != "" || accounts[0].SecondaryQuotaWindow != "month" || accounts[0].SecondaryQuotaSource != "usage" {
		t.Fatalf("quota windows/sources = primary %q secondary %q source %q, want no primary/month/usage", accounts[0].PrimaryQuotaWindow, accounts[0].SecondaryQuotaWindow, accounts[0].SecondaryQuotaSource)
	}
}

func TestTeamAccountUsesMonthlyQuotaFromPrimaryWindow(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "team.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "team@example.com",
		"type":         "codex",
		"plan_type":    "team",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	resetAt := time.Now().Add(30*24*time.Hour + 3*time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "team@example.com",
		AuthIndex:   "team.cpa.json",
		Source:      "team@example.com",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  240,
			OutputTokens: 60,
			TotalTokens:  300,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"30"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-secondary-used-percent": {"0"},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want configured team account", len(accounts))
	}
	if accounts[0].PrimaryUsedPercent != nil || accounts[0].PrimaryResetAt != nil {
		t.Fatalf("primary quota = pct %v reset %v, want monthly primary moved out of 5h", accounts[0].PrimaryUsedPercent, accounts[0].PrimaryResetAt)
	}
	if accounts[0].SecondaryQuotaWindow != "month" {
		t.Fatalf("secondary quota window = %q, want month", accounts[0].SecondaryQuotaWindow)
	}
	if accounts[0].SecondaryUsedPercent == nil || *accounts[0].SecondaryUsedPercent != 30 {
		t.Fatalf("secondary used percent = %v, want 30", accounts[0].SecondaryUsedPercent)
	}
	if accounts[0].SecondaryWindowTokens != 300 || accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 700 {
		t.Fatalf("monthly quota = window tokens %d total %d remaining %d, want 300/1000/700", accounts[0].SecondaryWindowTokens, accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}
	if accounts[0].PrimaryQuotaWindow != "" || accounts[0].SecondaryQuotaWindow != "month" || accounts[0].SecondaryQuotaSource != "usage" || accounts[0].SecondaryQuotaEstimateSource != "estimated" {
		t.Fatalf("quota windows/sources = primary %q secondary %q source %q estimate %q, want monthly usage estimate", accounts[0].PrimaryQuotaWindow, accounts[0].SecondaryQuotaWindow, accounts[0].SecondaryQuotaSource, accounts[0].SecondaryQuotaEstimateSource)
	}
	if accounts[0].QuotaWindowSource != "reset_duration" {
		t.Fatalf("quota window source = %q, want reset_duration for team monthly reset", accounts[0].QuotaWindowSource)
	}
}

func TestSummaryClearsExpiredQuotaSnapshots(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(-time.Minute).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "expired-quota@example.com",
		AuthIndex:   "expired-quota",
		Source:      "expired-quota@example.com",
		RequestedAt: time.Now().Add(-10 * time.Minute),
		Detail: usageDetail{
			InputTokens:  800,
			OutputTokens: 200,
			TotalTokens:  1000,
		},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent":   {"85"},
			"x-codex-primary-reset-at":       {intToString(resetAt)},
			"x-codex-secondary-used-percent": {"90"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	if accounts[0].PrimaryUsedPercent != nil || accounts[0].SecondaryUsedPercent != nil {
		t.Fatalf("quota percent = primary %v secondary %v, want cleared", accounts[0].PrimaryUsedPercent, accounts[0].SecondaryUsedPercent)
	}
	if accounts[0].PrimaryWindowTokens != 0 || accounts[0].SecondaryWindowTokens != 0 {
		t.Fatalf("window tokens = primary %d secondary %d, want 0/0", accounts[0].PrimaryWindowTokens, accounts[0].SecondaryWindowTokens)
	}
	totals := data["totals"].(totalsRow)
	if totals.SecondaryQuotaTotalEstimate != 0 || totals.SecondaryQuotaRemainingEstimate != 0 || totals.SecondaryQuotaEstimatedAccounts != 0 {
		t.Fatalf("total quota estimates = total %d remaining %d accounts %d, want 0/0/0", totals.SecondaryQuotaTotalEstimate, totals.SecondaryQuotaRemainingEstimate, totals.SecondaryQuotaEstimatedAccounts)
	}
}

func TestSummaryUsesLatestQuotaSnapshotInsteadOfMaxPercent(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      "latest-quota@example.com",
			AuthIndex:   "latest-quota",
			Source:      "latest-quota@example.com",
			RequestedAt: time.Now().Add(-30 * time.Minute),
			Detail:      usageDetail{InputTokens: 100, TotalTokens: 100},
			ResponseHeaders: map[string][]string{
				"x-codex-secondary-used-percent": {"80"},
				"x-codex-secondary-reset-at":     {intToString(resetAt)},
			},
		},
		{
			Provider:    "codex",
			AuthID:      "latest-quota@example.com",
			AuthIndex:   "latest-quota",
			Source:      "latest-quota@example.com",
			RequestedAt: time.Now().Add(-5 * time.Minute),
			Detail:      usageDetail{InputTokens: 50, TotalTokens: 50},
			ResponseHeaders: map[string][]string{
				"x-codex-secondary-used-percent": {"10"},
				"x-codex-secondary-reset-at":     {intToString(resetAt)},
			},
		},
	}
	for i, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage %d returned error: %v", i, err)
		}
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].SecondaryUsedPercent == nil || math.Abs(*accounts[0].SecondaryUsedPercent-10) > 0.000001 {
		t.Fatalf("accounts = %#v, want latest secondary percent 10", accounts)
	}
}

func TestSummaryIgnoresFailedUsageQuotaSnapshotUnlessRateLimited(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(5 * 24 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "failed-quota@example.com",
		AuthIndex:   "failed-quota",
		Source:      "failed-quota@example.com",
		RequestedAt: time.Now().Add(-30 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"99"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("record 429 snapshot returned error: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "failed-quota@example.com",
		AuthIndex:   "failed-quota",
		Source:      "failed-quota@example.com",
		RequestedAt: time.Now().Add(-5 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: 599},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"46"},
			"x-codex-secondary-reset-at":     {intToString(time.Now().Add(6 * 24 * time.Hour).Unix())},
		},
	}); err != nil {
		t.Fatalf("record failed snapshot returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].SecondaryUsedPercent == nil || math.Abs(*accounts[0].SecondaryUsedPercent-99) > 0.000001 {
		t.Fatalf("accounts = %#v, want 429 secondary percent 99 to survive later 599 snapshot", accounts)
	}
}

func TestQuotaTriggerDefaultConfigIsDisabled(t *testing.T) {
	cfg := normalizePluginConfig(defaultPluginConfig())
	if cfg.QuotaTriggerEnabled {
		t.Fatalf("default quota trigger enabled = true, want false")
	}
	if cfg.QuotaTriggerMode != "probe" || cfg.QuotaTriggerIntervalMinutes != 10 || cfg.QuotaTriggerMinAccountCooldownMinutes != 10 {
		t.Fatalf("default config = %+v, want probe/10m/10m", cfg)
	}
	decoded := parsePluginConfigYAML([]byte("quota_trigger_enabled: true\nquota_trigger_mode: probe\nquota_trigger_interval_minutes: 5\n"), defaultPluginConfig())
	decoded = normalizePluginConfig(decoded)
	if !decoded.QuotaTriggerEnabled || decoded.QuotaTriggerMode != "probe" || decoded.QuotaTriggerIntervalMinutes != 5 {
		t.Fatalf("decoded config = %+v, want enabled probe 5m", decoded)
	}
	chinese := parsePluginConfigYAML([]byte("开启定时额度触发: true\n触发模式: 探测请求\n触发间隔分钟: 6\n最大并发账号数: 2\n单账号超时秒数: 12\n单账号最小冷却分钟: 7\n"), defaultPluginConfig())
	chinese = normalizePluginConfig(chinese)
	if !chinese.QuotaTriggerEnabled ||
		chinese.QuotaTriggerMode != "probe" ||
		chinese.QuotaTriggerIntervalMinutes != 6 ||
		chinese.QuotaTriggerMaxConcurrency != 2 ||
		chinese.QuotaTriggerTimeoutSeconds != 12 ||
		chinese.QuotaTriggerMinAccountCooldownMinutes != 7 {
		t.Fatalf("chinese config = %+v, want enabled probe 6m/2/12s/7m", chinese)
	}
}

func TestQuotaTriggerQuotaModeUpdatesSnapshotAndCooldown(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "quota-trigger.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "quota-trigger@example.com",
		"type":         "codex",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-access-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept header = %q, want text/event-stream", got)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode probe body: %v", err)
		}
		if req["model"] != codexProbeModel || req["stream"] != true {
			t.Fatalf("probe body = %#v, want model %s and stream", req, codexProbeModel)
		}
		if _, ok := req["max_output_tokens"]; ok {
			t.Fatalf("probe body = %#v, want no max_output_tokens", req)
		}
		input, ok := req["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("probe input = %#v, want one Codex responses input item", req["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":12.5,"reset_at":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":22.5,"reset_at":%d,"limit_window_seconds":604800,"remaining_tokens":775,"limit_tokens":1000}}}`, resetAt, resetAt)))
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 1 || failed != 0 || skipped != 0 || candidates != 1 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want 1/0/0/1", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || accounts[0].PrimaryUsedPercent == nil || math.Abs(*accounts[0].PrimaryUsedPercent-12.5) > 0.000001 {
		t.Fatalf("accounts = %#v, want primary quota 12.5 from trigger", accounts)
	}
	if accounts[0].QuotaTriggerStatus != "success" || accounts[0].QuotaTriggerLastAt == "" {
		t.Fatalf("quota trigger account status = %+v, want success with time", accounts[0])
	}
	if accounts[0].SecondaryQuotaTotalEstimate != 1000 || accounts[0].SecondaryQuotaRemainingEstimate != 775 {
		t.Fatalf("secondary quota estimate = total %d remaining %d, want trigger absolute 1000/775", accounts[0].SecondaryQuotaTotalEstimate, accounts[0].SecondaryQuotaRemainingEstimate)
	}

	success, failed, skipped, candidates, err = store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("second runQuotaTriggerRound returned error: %v", err)
	}
	if success != 0 || failed != 0 || skipped != 1 || candidates != 0 {
		t.Fatalf("second round = success %d failed %d skipped %d candidates %d, want cooldown skip 0/0/1/0", success, failed, skipped, candidates)
	}
}

func TestCodexProbeRequestBodyUsesResponsesInputFormat(t *testing.T) {
	body, err := codexProbeRequestBody("")
	if err != nil {
		t.Fatalf("codexProbeRequestBody returned error: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	if req["model"] != codexProbeModel {
		t.Fatalf("model = %#v, want %s", req["model"], codexProbeModel)
	}
	if req["stream"] != true {
		t.Fatalf("stream = %#v, want true", req["stream"])
	}
	if _, ok := req["max_output_tokens"]; ok {
		t.Fatalf("body = %#v, want no max_output_tokens", req)
	}
	input, ok := req["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want one item array", req["input"])
	}
	message, ok := input[0].(map[string]any)
	if !ok || message["role"] != "user" {
		t.Fatalf("input[0] = %#v, want user message", input[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one content item", message["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != "ping" {
		t.Fatalf("content[0] = %#v, want input_text ping", content[0])
	}
}

func TestQuotaTriggerRetriesMinimalProbeWhenStreamUnsupported(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode probe body: %v", err)
		}
		switch calls {
		case 1:
			if req["stream"] != true || req["store"] != false {
				t.Fatalf("first probe body = %#v, want streaming body", req)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unknown parameter: 'stream'.","type":"invalid_request_error","param":"stream","code":"unknown_parameter"}}`))
		case 2:
			if _, ok := req["stream"]; ok {
				t.Fatalf("fallback body = %#v, want no stream", req)
			}
			if _, ok := req["store"]; ok {
				t.Fatalf("fallback body = %#v, want no store", req)
			}
			if _, ok := req["max_output_tokens"]; ok {
				t.Fatalf("fallback body = %#v, want no max_output_tokens", req)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":45,"reset_at":%d},"secondary_window":{"used_percent":29,"reset_at":%d}}}`, resetAt, resetAt)))
		default:
			t.Fatalf("unexpected probe call %d", calls)
		}
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	run := executeQuotaProbeRequest(ctx, db, triggerAuthAccount{
		configuredAccount: configuredAccount{
			AuthID:    "fallback@example.com",
			AuthIndex: "fallback.cpa.json",
			Source:    "fallback@example.com",
			AuthFile:  "fallback.cpa.json",
			Provider:  "codex",
		},
		AccessToken: "secret-access-token",
	}, normalizePluginConfig(defaultPluginConfig()))
	if calls != 2 {
		t.Fatalf("probe calls = %d, want retry", calls)
	}
	if run.Status != "success" || run.HTTPStatus != http.StatusOK || run.PrimaryUsedPercent == nil || *run.PrimaryUsedPercent != 45 {
		t.Fatalf("run = %+v, want successful fallback quota snapshot", run)
	}
}

func TestQuotaTriggerRefreshesAndReleasesActiveAutoban(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	authFile := filepath.Join(authDir, "autoban-refresh.cpa.json")
	raw, err := json.Marshal(map[string]any{
		"email":        "autoban-refresh@example.com",
		"type":         "codex",
		"access_token": "secret-access-token",
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	banResetAt := time.Now().Add(2 * time.Hour).Unix()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "autoban-refresh@example.com",
		AuthIndex:   "autoban-refresh.cpa.json",
		Source:      "autoban-refresh@example.com",
		RequestedAt: time.Now().Add(-10 * time.Minute),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: map[string][]string{
			"x-codex-primary-used-percent": {"100"},
			"x-codex-primary-reset-at":     {intToString(banResetAt)},
		},
	}); err != nil {
		t.Fatalf("record ban usage returned error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-access-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":45,"reset_at":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":12,"reset_at":%d,"limit_window_seconds":604800}}}`, banResetAt, time.Now().Add(6*24*time.Hour).Unix())))
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 1 || failed != 0 || skipped != 0 || candidates != 1 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want active ban refreshed 1/0/0/1", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	bans := data["autobans"].([]autobanRow)
	if len(bans) != 0 {
		t.Fatalf("autobans = %#v, want 5h ban released after quota refresh", bans)
	}
}

func TestExternalUseUsesQuotaTriggerSnapshotsWithFivePercentThreshold(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	resetAt := time.Now().Add(6 * 24 * time.Hour).Unix()
	first := time.Now().Add(-30 * time.Minute).Unix()
	second := time.Now().Add(-10 * time.Minute).Unix()
	p20, p26 := 20.0, 26.0
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "shared-trigger@example.com",
		AuthIndex:            "shared-trigger.cpa.json",
		Source:               "shared-trigger@example.com",
		Provider:             "codex",
		AuthFile:             "shared-trigger.cpa.json",
		Mode:                 "quota",
		Status:               "success",
		HTTPStatus:           200,
		StartedAt:            first,
		FinishedAt:           first,
		SecondaryUsedPercent: &p20,
		SecondaryResetAt:     &resetAt,
	}); err != nil {
		t.Fatalf("record first trigger: %v", err)
	}
	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "quota-trigger",
		Model:        "quota-trigger",
		Alias:        "quota-trigger",
		AuthID:       "shared-trigger@example.com",
		AuthIndex:    "shared-trigger.cpa.json",
		Source:       "shared-trigger@example.com",
		RequestedAt:  time.Unix(first+600, 0),
		Detail:       usageDetail{TotalTokens: 999999},
		ResponseHeaders: map[string][]string{
			"x-codex-secondary-used-percent": {"25"},
			"x-codex-secondary-reset-at":     {intToString(resetAt)},
		},
	}); err != nil {
		t.Fatalf("record quota-trigger usage: %v", err)
	}
	if err := recordQuotaTriggerRun(ctx, db, quotaTriggerRun{
		AuthID:               "shared-trigger@example.com",
		AuthIndex:            "shared-trigger.cpa.json",
		Source:               "shared-trigger@example.com",
		Provider:             "codex",
		AuthFile:             "shared-trigger.cpa.json",
		Mode:                 "quota",
		Status:               "success",
		HTTPStatus:           200,
		StartedAt:            second,
		FinishedAt:           second,
		SecondaryUsedPercent: &p26,
		SecondaryResetAt:     &resetAt,
	}); err != nil {
		t.Fatalf("record second trigger: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	alerts := data["external_use_alerts"].([]externalUseAlert)
	if len(alerts) != 1 {
		t.Fatalf("external_use_alerts = %#v, want one alert", alerts)
	}
	if alerts[0].Window != "7d" || alerts[0].DeltaPercent != 6 || alerts[0].LocalTokens != 0 {
		t.Fatalf("alert = %+v, want 7d delta 6 local 0", alerts[0])
	}
}

func TestQuotaTriggerFiltersBadAccountsAndRecords401429(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	ctx := context.Background()
	store := &store{}
	defer store.close()

	fixtures := []struct {
		name     string
		email    string
		token    string
		disabled bool
		expired  bool
	}{
		{name: "invalid.cpa.json", email: "invalid@example.com", token: "invalid-token"},
		{name: "workspace.cpa.json", email: "workspace@example.com", token: "workspace-token"},
		{name: "limited.cpa.json", email: "limited@example.com", token: "limited-token"},
		{name: "disabled.cpa.json", email: "disabled@example.com", token: "disabled-token", disabled: true},
		{name: "expired.cpa.json", email: "expired@example.com", token: "expired-token", expired: true},
	}
	for _, fixture := range fixtures {
		raw, err := json.Marshal(map[string]any{
			"email":        fixture.email,
			"type":         "codex",
			"access_token": fixture.token,
			"disabled":     fixture.disabled,
			"expired":      fixture.expired,
		})
		if err != nil {
			t.Fatalf("marshal auth: %v", err)
		}
		if err := os.WriteFile(filepath.Join(authDir, fixture.name), raw, 0600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
	}

	resetAt := time.Now().Add(time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("quota trigger method = %s, want POST model probe", r.Method)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer invalid-token":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer workspace-token":
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"detail":{"code":"deactivated_workspace"}}`))
		case "Bearer limited-token":
			w.Header().Set("x-codex-primary-used-percent", "100")
			w.Header().Set("x-codex-primary-reset-at", strconv.FormatInt(resetAt, 10))
			w.Header().Set("x-codex-primary-window-minutes", "300")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Fatalf("unexpected trigger token: %s", r.Header.Get("Authorization"))
		}
	}))
	defer server.Close()
	withCodexQuotaURLForTest(t, server.URL)

	cfg := normalizePluginConfig(pluginConfig{
		QuotaTriggerEnabled:                   true,
		QuotaTriggerIntervalMinutes:           10,
		QuotaTriggerMode:                      "quota",
		QuotaTriggerMaxConcurrency:            1,
		QuotaTriggerTimeoutSeconds:            5,
		QuotaTriggerMinAccountCooldownMinutes: 10,
	})
	success, failed, skipped, candidates, err := store.runQuotaTriggerRound(ctx, cfg)
	if err != nil {
		t.Fatalf("runQuotaTriggerRound returned error: %v", err)
	}
	if success != 0 || failed != 3 || skipped != 2 || candidates != 3 {
		t.Fatalf("round = success %d failed %d skipped %d candidates %d, want 0/3/2/3", success, failed, skipped, candidates)
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	invalids := data["invalid_auths"].([]invalidAuthRow)
	if len(invalids) != 1 || invalids[0].AuthID != "invalid@example.com" {
		t.Fatalf("invalid_auths = %#v, want invalid@example.com", invalids)
	}
	workspaces := data["workspace_deactivated_auths"].([]invalidAuthRow)
	if len(workspaces) != 1 || workspaces[0].AuthID != "workspace@example.com" || workspaces[0].LastStatusCode != http.StatusPaymentRequired {
		t.Fatalf("workspace_deactivated_auths = %#v, want workspace@example.com 402", workspaces)
	}
	bans := data["autobans"].([]autobanRow)
	seenLimited := false
	seenInvalid := false
	seenWorkspace := false
	for _, ban := range bans {
		if ban.AuthID == "limited@example.com" && ban.Window == "5h" {
			seenLimited = true
		}
		if ban.AuthID == "invalid@example.com" && ban.Window == "401" {
			seenInvalid = true
		}
		if ban.AuthID == "workspace@example.com" && ban.Window == "402" {
			seenWorkspace = true
		}
	}
	if len(bans) != 3 || !seenLimited || !seenInvalid || !seenWorkspace {
		t.Fatalf("autobans = %#v, want limited 429, invalid 401, and workspace 402 bans", bans)
	}
	rawSummary, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(rawSummary), "invalid-token") || strings.Contains(string(rawSummary), "limited-token") || strings.Contains(string(rawSummary), "workspace-token") {
		t.Fatalf("summary leaked trigger token material: %s", rawSummary)
	}
}

func TestSummaryCalculatesModelCosts(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "priced@example.com",
		AuthIndex:   "priced-account",
		Source:      "priced@example.com",
		Model:       "gpt-5.5",
		Alias:       "gpt-5.5",
		ServiceTier: "default",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:  1_000_000,
			OutputTokens: 500_000,
			CachedTokens: 200_000,
			TotalTokens:  1_500_000,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	totals := data["totals"].(totalsRow)
	const wantCost = 19.1
	if math.Abs(totals.CostUSD-wantCost) > 0.000001 || !totals.CostAvailable {
		t.Fatalf("totals cost = %.8f available=%v, want %.2f true", totals.CostUSD, totals.CostAvailable, wantCost)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 || math.Abs(accounts[0].CostUSD-wantCost) > 0.000001 || !accounts[0].CostAvailable {
		t.Fatalf("accounts = %#v, want one priced account cost %.2f", accounts, wantCost)
	}
	models := data["models"].([]modelRow)
	if len(models) != 1 || math.Abs(models[0].CostUSD-wantCost) > 0.000001 || !models[0].CostAvailable {
		t.Fatalf("models = %#v, want one priced model cost %.2f", models, wantCost)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	if math.Abs(recent[0].CostUSD-wantCost) > 0.000001 || !recent[0].CostAvailable || recent[0].PriceDetail == "" {
		t.Fatalf("recent cost = %.8f available=%v price=%q, want %.2f true with price detail", recent[0].CostUSD, recent[0].CostAvailable, recent[0].PriceDetail, wantCost)
	}
}

func TestRecentRequestsExposeLatencyAndCacheBreakdown(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "latency@example.com",
		AuthIndex:   "latency-account",
		Source:      "latency@example.com",
		Model:       "gpt-5.5",
		Alias:       "gpt-5.5",
		ServiceTier: "standard",
		Latency:     20_000_000_000,
		TTFT:        4_800_000_000,
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:         85_168,
			OutputTokens:        796,
			CachedTokens:        84_352,
			CacheReadTokens:     84_352,
			CacheCreationTokens: 0,
			TotalTokens:         85_964,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	row := recent[0]
	if row.LatencyMs != 20_000 || row.TTFTMs != 4_800 {
		t.Fatalf("latency fields = %d/%d, want 20000/4800", row.LatencyMs, row.TTFTMs)
	}
	if row.InputTokens != 85_168 || row.OutputTokens != 796 || row.CachedTokens != 84_352 || row.CacheReadTokens != 84_352 {
		t.Fatalf("token breakdown = %+v, want input/output/cache/read populated", row)
	}
	if !row.CostAvailable || row.CostUSD <= 0 || !strings.Contains(row.PriceDetail, "$5 / $30/M") {
		t.Fatalf("recent pricing = cost %.8f available=%v detail=%q, want gpt-5.5 pricing", row.CostUSD, row.CostAvailable, row.PriceDetail)
	}
}

func TestLatencyIsClampedToTTFTForThroughput(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "latency-clamp@example.com",
		AuthIndex:   "latency-clamp",
		Source:      "latency-clamp@example.com",
		Model:       "gpt-5.5",
		Latency:     1,
		TTFT:        1_800_000_000,
		RequestedAt: time.Now(),
		Detail: usageDetail{
			OutputTokens: 900,
			TotalTokens:  900,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["recent"].([]recentRow)
	if len(recent) != 1 {
		t.Fatalf("recent = %#v, want one recent row", recent)
	}
	if recent[0].LatencyMs != 1800 || recent[0].TTFTMs != 1800 {
		t.Fatalf("latency fields = %d/%d, want clamped to TTFT 1800/1800", recent[0].LatencyMs, recent[0].TTFTMs)
	}
	totals := data["totals"].(totalsRow)
	if totals.OutputTokensPerSecond > 600 {
		t.Fatalf("throughput = %.2f, want reasonable value after latency clamp", totals.OutputTokensPerSecond)
	}
}

func TestThroughputUsesWeightedReliableSamples(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	store := &store{}
	defer store.close()

	ctx := context.Background()
	now := time.Now()
	records := []usageRecord{
		{
			Provider:    "codex",
			AuthID:      "throughput@example.com",
			AuthIndex:   "throughput",
			Source:      "throughput@example.com",
			Model:       "gpt-5.5",
			Latency:     int64(1300 * time.Millisecond),
			TTFT:        int64(1300 * time.Millisecond),
			RequestedAt: now,
			Detail: usageDetail{
				OutputTokens: 4000,
				TotalTokens:  4000,
			},
		},
		{
			Provider:    "codex",
			AuthID:      "throughput@example.com",
			AuthIndex:   "throughput",
			Source:      "throughput@example.com",
			Model:       "gpt-5.5",
			Latency:     int64(20 * time.Second),
			TTFT:        int64(1500 * time.Millisecond),
			RequestedAt: now.Add(time.Second),
			Detail: usageDetail{
				OutputTokens: 1000,
				TotalTokens:  1000,
			},
		},
	}
	for _, rec := range records {
		if err := store.recordUsage(ctx, rec); err != nil {
			t.Fatalf("recordUsage returned error: %v", err)
		}
	}
	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	totals := data["totals"].(totalsRow)
	if totals.OutputTokensPerSecond < 49 || totals.OutputTokensPerSecond > 51 {
		t.Fatalf("throughput = %.2f, want weighted reliable throughput near 50", totals.OutputTokensPerSecond)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one account", accounts)
	}
	if accounts[0].OutputTokensPerSecond < 49 || accounts[0].OutputTokensPerSecond > 51 {
		t.Fatalf("account throughput = %.2f, want weighted reliable throughput near 50", accounts[0].OutputTokensPerSecond)
	}
}

func TestSummaryCalculatesOpenAICompatibleProviderCostsFromPriceFile(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	priceFile := filepath.Join(t.TempDir(), "model_prices.json")
	t.Setenv("CPA_MODEL_PRICE_FILE", priceFile)
	raw := []byte(`{
		"openrouter/anthropic/claude-sonnet-4.5": {
			"litellm_provider": "openrouter",
			"input_cost_per_token": 0.000003,
			"output_cost_per_token": 0.000015,
			"cache_read_input_token_cost": 0.0000003,
			"cache_creation_input_token_cost": 0.00000375
		}
	}`)
	if err := os.WriteFile(priceFile, raw, 0600); err != nil {
		t.Fatalf("write price file: %v", err)
	}
	store := &store{}
	defer store.close()

	ctx := context.Background()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "openai-compatible",
		AuthID:      "openai-compatibility:openrouter:upstream-key",
		AuthIndex:   "upstream-account",
		Source:      "openrouter",
		Model:       "anthropic/claude-sonnet-4.5",
		Alias:       "claude-sonnet",
		RequestedAt: time.Now(),
		Detail: usageDetail{
			InputTokens:         1_000_000,
			OutputTokens:        500_000,
			CachedTokens:        200_000,
			CacheReadTokens:     200_000,
			CacheCreationTokens: 100_000,
			TotalTokens:         1_800_000,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 10)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	const wantCost = 10.335
	totals := data["totals"].(totalsRow)
	if totals.TotalTokens != 0 || totals.CostUSD != 0 {
		t.Fatalf("codex totals = %#v, want other provider usage excluded", totals)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if math.Abs(providerTotals.CostUSD-wantCost) > 0.000001 || !providerTotals.CostAvailable {
		t.Fatalf("provider totals cost = %.8f available=%v, want %.3f true", providerTotals.CostUSD, providerTotals.CostAvailable, wantCost)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 1 || providers[0].Provider != "openrouter" || math.Abs(providers[0].CostUSD-wantCost) > 0.000001 {
		t.Fatalf("providers = %#v, want openrouter cost %.3f", providers, wantCost)
	}
	models := data["models"].([]modelRow)
	if len(models) != 0 {
		t.Fatalf("codex models = %#v, want other provider models excluded", models)
	}
	providerModels := data["provider_models"].([]modelRow)
	if len(providerModels) != 1 || providerModels[0].Provider != "openrouter" || math.Abs(providerModels[0].CostUSD-wantCost) > 0.000001 {
		t.Fatalf("provider_models = %#v, want openrouter cost %.3f", providerModels, wantCost)
	}
}

func TestUnconfiguredCodexAPIKeyProviderDoesNotPolluteStats(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	t.Setenv("CPA_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	ctx := context.Background()
	store := &store{}
	defer store.close()

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "apikey",
		AuthID:       "codex:apikey:b575a2ab1607",
		AuthIndex:    "e88eaa4c2018a1fa",
		Source:       "sk-provider-secret",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  120,
			OutputTokens: 30,
			TotalTokens:  150,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "oauth",
		AuthID:       "real-account@example.com.cpa.json",
		AuthIndex:    "real-account",
		Source:       "real-account@example.com",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  200,
			OutputTokens: 50,
			TotalTokens:  250,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	accounts := data["accounts"].([]accountRow)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want only oauth Codex account", accounts)
	}
	if strings.Contains(accounts[0].Source, "sk-") || accounts[0].AuthID == "codex:apikey:b575a2ab1607" {
		t.Fatalf("Codex account pool leaked API key provider row: %+v", accounts[0])
	}
	totals := data["totals"].(totalsRow)
	if totals.TotalTokens != 250 {
		t.Fatalf("codex totals = %d, want only oauth tokens 250", totals.TotalTokens)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if providerTotals.TotalTokens != 0 {
		t.Fatalf("provider totals = %d, want unconfigured API-key Codex provider excluded", providerTotals.TotalTokens)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 0 {
		t.Fatalf("providers = %#v, want unconfigured API-key Codex provider hidden", providers)
	}
	providerRecent := data["provider_recent"].([]recentRow)
	if len(providerRecent) != 0 {
		t.Fatalf("provider_recent = %#v, want unconfigured API-key Codex requests hidden", providerRecent)
	}
}

func TestCodexAPIKeyProviderUsesConfiguredEndpointName(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", configPath)
	if err := os.WriteFile(configPath, []byte(`
codex-api-key:
  - api-key: sk-provider-secret
    base-url: https://api.kmoon.site/v1
    models:
      - name: gpt-5.5
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		AuthType:     "apikey",
		AuthID:       "codex:apikey:b575a2ab1607",
		AuthIndex:    "e88eaa4c2018a1fa",
		Source:       "sk-provider-secret",
		Model:        "gpt-5.5",
		Alias:        "gpt-5.5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:  120,
			OutputTokens: 30,
			TotalTokens:  150,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	providerTotals := data["provider_totals"].(totalsRow)
	if providerTotals.TotalTokens != 150 {
		t.Fatalf("provider totals = %d, want configured Codex endpoint tokens 150", providerTotals.TotalTokens)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 1 || providers[0].Provider != "Codex · api.kmoon.site" || providers[0].TotalTokens != 150 {
		t.Fatalf("providers = %#v, want one configured Codex endpoint row", providers)
	}
	providerModels := data["provider_models"].([]modelRow)
	if len(providerModels) != 1 || providerModels[0].Provider != "Codex · api.kmoon.site" {
		t.Fatalf("provider_models = %#v, want configured Codex endpoint name", providerModels)
	}
}

func TestClaudeAPIKeyProviderUsesConfiguredEndpointName(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", configPath)
	if err := os.WriteFile(configPath, []byte(`
claude-api-key:
  - api-key: sk-claude-secret
    base-url: https://api.kmoon.site
    models:
      - name: claude-sonnet-5
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()

	if err := store.recordUsage(ctx, usageRecord{
		Provider:     "claude",
		ExecutorType: "ClaudeExecutor",
		AuthType:     "apikey",
		AuthID:       "claude:apikey:b575a2ab1607",
		AuthIndex:    "0ae6b99ac9b81719",
		Source:       "sk-claude-secret",
		Model:        "claude-sonnet-5",
		Alias:        "claude-sonnet-5",
		RequestedAt:  time.Now(),
		Detail: usageDetail{
			InputTokens:         42,
			OutputTokens:        37,
			CacheCreationTokens: 1447,
			CachedTokens:        1447,
			TotalTokens:         1526,
		},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}

	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	providers := data["providers"].([]providerRow)
	if len(providers) != 1 || providers[0].Provider != "Claude · api.kmoon.site" || providers[0].TotalTokens != 1526 {
		t.Fatalf("providers = %#v, want one configured Claude endpoint row", providers)
	}
	providerRecent := data["provider_recent"].([]recentRow)
	if len(providerRecent) != 1 || providerRecent[0].Provider != "Claude · api.kmoon.site" {
		t.Fatalf("provider_recent = %#v, want configured Claude endpoint name", providerRecent)
	}
}

func TestProviderRecentKeepsOlderEndpointRows(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Setenv("CPA_AUTH_DIR", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CPA_CONFIG_PATH", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
openai-compatibility:
  - name: 字节
    api-key: sk-byte-secret
codex-api-key:
  - name: Codex · api.kmoon.site
    api-key: sk-codex-secret
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()
	base := time.Now().Add(-time.Hour)
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "openai-compatible-字节",
		AuthID:      "openai-compatibility:字节:test",
		AuthType:    "api_key",
		Source:      "ark-byte-key",
		Model:       "deepseek-v4-pro",
		RequestedAt: base,
		Detail:      usageDetail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
	}); err != nil {
		t.Fatalf("record byte usage: %v", err)
	}
	for i := 0; i < 60; i++ {
		if err := store.recordUsage(ctx, usageRecord{
			Provider:    "codex",
			AuthID:      "codex:apikey:test",
			AuthType:    "apikey",
			Source:      "sk-codex-secret",
			Model:       "gpt-5.5",
			RequestedAt: base.Add(time.Duration(i+1) * time.Minute),
			Failed:      true,
			Failure:     usageFailure{StatusCode: http.StatusTooManyRequests},
		}); err != nil {
			t.Fatalf("record codex api key usage %d: %v", i, err)
		}
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	recent := data["provider_recent"].([]recentRow)
	var foundByte bool
	var leakedKey bool
	for _, row := range recent {
		if row.Provider == "字节" {
			foundByte = true
		}
		if strings.Contains(row.Source, "sk-codex-secret") {
			leakedKey = true
		}
	}
	if !foundByte {
		t.Fatalf("provider_recent len=%d missing older 字节 row after newer Codex endpoint rows", len(recent))
	}
	if leakedKey {
		t.Fatalf("provider_recent leaked raw API key: %#v", recent)
	}
}

func TestConfiguredProviderNamesFromYAMLReadsOpenAICompatibilityNames(t *testing.T) {
	raw := `
openai-compatibility:
  - api-key-entries:
      - api-key: sk-redacted
    name: deepseek
  - base-url: http://example.invalid
    name: maas
  - name: '字节'
claude-api-key: []
codex-api-key:
  - api-key: sk-codex-redacted
    base-url: https://api.kmoon.site/v1
    models:
      - name: gpt-5.5
gemini-api-key: []
`
	got := configuredProviderNamesFromYAML(raw)
	want := []string{"deepseek", "maas", "字节", "Codex · api.kmoon.site"}
	if len(got) != len(want) {
		t.Fatalf("names = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %#v, want %#v", got, want)
		}
	}
}

func TestConfiguredProviderEntriesFromYAMLReadsAllCommonProviderEndpoints(t *testing.T) {
	raw := `
openai-compatibility:
  - api-key-entries:
      - api-key: sk-random-openai-compat
    base-url: https://compat-random.example/v1
    name: random-compat-a
openai-compatible:
  - api-key-entries:
      - api-key: sk-random-openai-compatible
    base-url: https://compatible-random.example/v1
    name: random-compat-b
codex-api-key:
  - api-key: sk-random-codex
    base-url: https://codex-random.example/v1
claude-api-key:
  - api-key: sk-random-claude
    base-url: https://claude-random.example/v1
anthropic-api-key:
  - api-key: sk-random-anthropic
    base-url: https://anthropic-random.example/v1
gemini-api-key:
  - api-key: sk-random-gemini
    base-url: https://gemini-random.example/v1
antigravity-api-key:
  - api-key: sk-random-antigravity
    base-url: https://antigravity-random.example/v1
anthropic-oauth:
  - name: random-anthropic-oauth
antigravity-oauth:
  - name: random-antigravity-oauth
`
	entries := configuredProviderEntriesFromYAML(raw)
	got := map[string]providerConfigEntry{}
	for _, entry := range entries {
		got[entry.Name] = entry
	}
	want := map[string]string{
		"random-compat-a":                          "OpenAI",
		"random-compat-b":                          "OpenAI",
		"Codex · codex-random.example":             "Codex",
		"Claude · claude-random.example":           "Claude",
		"Claude · anthropic-random.example":        "Claude",
		"Gemini · gemini-random.example":           "Gemini",
		"Antigravity · antigravity-random.example": "Antigravity",
		"random-anthropic-oauth":                   "Claude",
		"random-antigravity-oauth":                 "Antigravity",
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %#v, want %d common provider endpoints", entries, len(want))
	}
	for name, provider := range want {
		entry, ok := got[name]
		if !ok {
			t.Fatalf("missing provider endpoint %q in %#v", name, entries)
		}
		if entry.Provider != provider {
			t.Fatalf("provider for %q = %q, want %q", name, entry.Provider, provider)
		}
	}
	if got["random-compat-a"].APIKey != "sk-random-openai-compat" {
		t.Fatalf("nested openai-compatible api key was not read: %+v", got["random-compat-a"])
	}
	if got["Codex · codex-random.example"].APIKey != "sk-random-codex" {
		t.Fatalf("codex api key was not read: %+v", got["Codex · codex-random.example"])
	}
}

func TestSchemaCreatesQuotaTriggerCapacityColumns(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	ctx := context.Background()
	store := &store{}
	defer store.close()
	db, _, err := store.open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(quota_trigger_runs)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{"primary_used_tokens", "primary_remaining_tokens", "primary_limit_tokens", "secondary_used_tokens", "secondary_remaining_tokens", "secondary_limit_tokens"} {
		if !columns[name] {
			t.Fatalf("quota_trigger_runs missing capacity column %q; columns=%#v", name, columns)
		}
	}
}

func TestConfiguredAuthFilesReadCodexAnthropicAndAntigravityOAuth(t *testing.T) {
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	writeAuth := func(name, raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(raw), 0600); err != nil {
			t.Fatalf("write auth %s: %v", name, err)
		}
	}
	writeAuth("codex-random.cpa.json", `{"provider":"codex","email":"codex-random@example.com","access_token":"redacted"}`)
	writeAuth("anthropic-random.json", `{"provider":"anthropic","email":"anthropic-random@example.com","refresh_token":"redacted"}`)
	writeAuth("antigravity-random.json", `{"platform":"antigravity","email":"antigravity-random@example.com","refresh_token":"redacted"}`)

	files := readConfiguredAuthFiles()
	if len(files) != 3 {
		t.Fatalf("auth files = %#v, want codex/anthropic/antigravity", files)
	}
	providers := map[string]string{}
	for _, file := range files {
		providers[file.Email] = file.Provider
	}
	if providers["codex-random@example.com"] != "codex" || providers["anthropic-random@example.com"] != "anthropic" || providers["antigravity-random@example.com"] != "antigravity" {
		t.Fatalf("auth file providers = %#v", providers)
	}
	codexAccounts := readConfiguredAuthAccounts()
	if len(codexAccounts) != 1 || codexAccounts[0].Email != "codex-random@example.com" {
		t.Fatalf("codex account pool auth files = %#v, want only codex OAuth file", codexAccounts)
	}
	triggerAccounts := readTriggerAuthAccounts()
	if len(triggerAccounts) != 1 || triggerAccounts[0].Email != "codex-random@example.com" || triggerAccounts[0].AccessToken != "redacted" {
		t.Fatalf("trigger auth accounts = %#v, want only Codex with access token", triggerAccounts)
	}
}

func TestRetentionConfigParsesChineseAndEnglishFields(t *testing.T) {
	cfg := parsePluginConfigYAML([]byte(`
usage_retention_days: 120
额度触发记录保留天数: 45
请求明细保留天数: 20
`), defaultPluginConfig())
	cfg = normalizePluginConfig(cfg)
	if cfg.UsageRetentionDays != 120 || cfg.QuotaTriggerRetentionDays != 45 || cfg.RequestDetailRetentionDays != 20 {
		t.Fatalf("retention config = %+v", cfg)
	}
}

func TestSummaryIncludesDiagnosticsAndAlerts(t *testing.T) {
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	authDir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", authDir)
	if err := os.WriteFile(filepath.Join(authDir, "broken.json"), []byte(`{"provider":"codex","email":"broken@example.com","access_token":"redacted"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	ctx := context.Background()
	store := &store{}
	defer store.close()
	if err := store.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		AuthID:      "broken@example.com",
		AuthIndex:   "broken.json",
		Source:      "broken@example.com",
		RequestedAt: time.Now(),
		Failed:      true,
		Failure:     usageFailure{StatusCode: http.StatusUnauthorized, Body: "unauthorized sk-secret-should-not-leak"},
	}); err != nil {
		t.Fatalf("recordUsage returned error: %v", err)
	}
	data, err := store.summary(ctx, "24h", 20)
	if err != nil {
		t.Fatalf("summary returned error: %v", err)
	}
	diagnostics, ok := data["diagnostics"].(diagnosticsSummary)
	if !ok {
		t.Fatalf("diagnostics = %#v, want diagnosticsSummary", data["diagnostics"])
	}
	if diagnostics.Database.UsageEvents != 1 || diagnostics.AuthFiles.Invalid401 != 1 {
		t.Fatalf("diagnostics = %+v, want one usage event and one invalid auth", diagnostics)
	}
	alerts, ok := data["alerts"].([]dashboardAlert)
	if !ok || len(alerts) == 0 {
		t.Fatalf("alerts = %#v, want at least one alert", data["alerts"])
	}
}

func TestExportRecordsDoNotLeakSecrets(t *testing.T) {
	data := map[string]any{
		"accounts": []accountRow{{
			AuthIndex:   "auth.json",
			AuthID:      "acct@example.com",
			Source:      "sk-secret-should-not-export",
			Provider:    "codex",
			Requests:    1,
			TotalTokens: 42,
			CostUSD:     0.01,
		}},
		"provider_recent": []recentRow{{
			Time:        time.Now().Format(time.RFC3339),
			Provider:    "Codex · api.example.com",
			Source:      "sk-secret-should-not-export",
			Model:       "test-model",
			TotalTokens: 42,
			CostUSD:     0.01,
		}},
	}
	records, headers := exportRecords(data, "accounts")
	body, err := recordsToCSV(headers, records)
	if err != nil {
		t.Fatalf("recordsToCSV returned error: %v", err)
	}
	if strings.Contains(string(body), "sk-secret-should-not-export") {
		t.Fatalf("account export leaked secret: %s", body)
	}
	records, headers = exportRecords(data, "recent")
	body, err = recordsToCSV(headers, records)
	if err != nil {
		t.Fatalf("recordsToCSV recent returned error: %v", err)
	}
	if strings.Contains(string(body), "sk-secret-should-not-export") {
		t.Fatalf("recent export leaked secret: %s", body)
	}
}

func TestMergeConfiguredProvidersReflectsCurrentConfig(t *testing.T) {
	rows := []providerRow{{Provider: "deepseek", TotalTokens: 100}}
	withCodex := mergeConfiguredProviders(rows, []string{"deepseek", "Codex · api.kmoon.site"})
	if len(withCodex) != 2 || withCodex[1].Provider != "Codex · api.kmoon.site" {
		t.Fatalf("providers with codex config = %#v", withCodex)
	}
	withoutCodex := mergeConfiguredProviders(rows, []string{"deepseek"})
	if len(withoutCodex) != 1 || withoutCodex[0].Provider != "deepseek" {
		t.Fatalf("providers after config removal = %#v, want only deepseek", withoutCodex)
	}
}

func TestPriceNameCandidatesMatchAliasesAndDateSuffixes(t *testing.T) {
	candidates := priceNameCandidates("deepseek-v4-pro-260425 OpenAI")
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate] = true
	}
	for _, want := range []string{"deepseek-v4-pro-260425 openai", "deepseek-v4-pro-260425", "deepseek-v4-pro"} {
		if !seen[want] {
			t.Fatalf("priceNameCandidates missing %q in %#v", want, candidates)
		}
	}
}

func TestProviderSpecificPricesDoNotOverrideGenericFallback(t *testing.T) {
	prices := map[string]modelPrice{}
	generic := modelPrice{Prompt: 0.435, Completion: 0.87}
	azure := modelPrice{Prompt: 1.74, Completion: 3.48}
	registerPriceCandidate(prices, "deepseek-v4-pro", generic)
	registerPriceCandidate(prices, "azure_ai/deepseek-v4-pro", azure)

	price, ok := resolveModelPrice(costTokenRow{Provider: "字节", Model: "deepseek-v4-pro-260425"}, prices)
	if !ok {
		t.Fatalf("resolve generic fallback returned no price")
	}
	if price.Prompt != generic.Prompt || price.Completion != generic.Completion {
		t.Fatalf("unknown provider price = %+v, want generic %+v", price, generic)
	}

	price, ok = resolveModelPrice(costTokenRow{Provider: "azure_ai", Model: "deepseek-v4-pro"}, prices)
	if !ok {
		t.Fatalf("resolve azure provider returned no price")
	}
	if price.Prompt != azure.Prompt || price.Completion != azure.Completion {
		t.Fatalf("azure provider price = %+v, want provider-specific %+v", price, azure)
	}
}

func TestModelPriceUpdateConfigParsesChineseFields(t *testing.T) {
	cfg := parsePluginConfigYAML([]byte(`
自动更新模型价格表: false
模型价格更新间隔小时: 12
模型价格表地址: https://example.test/model_prices.json
模型价格更新超时秒数: 9
`), defaultPluginConfig())
	cfg = normalizePluginConfig(cfg)
	if cfg.ModelPriceAutoUpdateEnabled {
		t.Fatalf("ModelPriceAutoUpdateEnabled = true, want false")
	}
	if cfg.ModelPriceUpdateIntervalHours != 12 {
		t.Fatalf("ModelPriceUpdateIntervalHours = %d, want 12", cfg.ModelPriceUpdateIntervalHours)
	}
	if cfg.ModelPriceUpdateURL != "https://example.test/model_prices.json" {
		t.Fatalf("ModelPriceUpdateURL = %q", cfg.ModelPriceUpdateURL)
	}
	if cfg.ModelPriceUpdateTimeoutSeconds != 9 {
		t.Fatalf("ModelPriceUpdateTimeoutSeconds = %d, want 9", cfg.ModelPriceUpdateTimeoutSeconds)
	}
}

func TestDownloadModelPricesValidatesAndWritesFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"openai/test-model": {
				"input_cost_per_token": 0.000001,
				"output_cost_per_token": 0.000002,
				"litellm_provider": "openai"
			}
		}`))
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "model_prices.json")
	entries, loaded, size, err := downloadModelPrices(context.Background(), server.URL, target)
	if err != nil {
		t.Fatalf("downloadModelPrices returned error: %v", err)
	}
	if entries != 1 || loaded != 1 || size <= 0 {
		t.Fatalf("entries=%d loaded=%d size=%d, want one loaded price", entries, loaded, size)
	}
	prices := readPricesFromPathForTest(t, target)
	price, ok := prices["openai/test-model"]
	if !ok {
		t.Fatalf("downloaded prices = %#v, want openai/test-model", prices)
	}
	if price.Prompt != 1 || price.Completion != 2 {
		t.Fatalf("price = %+v, want per-token values converted to per-million", price)
	}
}

func readPricesFromPathForTest(t *testing.T, path string) map[string]modelPrice {
	t.Helper()
	t.Setenv("CPA_MODEL_PRICE_FILE", path)
	old := globalModelPriceUpdater
	globalModelPriceUpdater = &modelPriceUpdateManager{}
	t.Cleanup(func() { globalModelPriceUpdater = old })
	return readModelPricesFromFile()
}

func withCodexQuotaURLForTest(t *testing.T, url string) {
	t.Helper()
	old := codexQuotaURLOverrideForTest
	oldResponses := codexResponsesURLOverrideForTest
	codexQuotaURLOverrideForTest = url
	codexResponsesURLOverrideForTest = url
	t.Cleanup(func() {
		codexQuotaURLOverrideForTest = old
		codexResponsesURLOverrideForTest = oldResponses
	})
}

func intToString(v int64) string {
	return stringFromAny(v)
}

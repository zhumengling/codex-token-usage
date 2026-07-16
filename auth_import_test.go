package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func importTestJWT(payload map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
}

func TestParseAuthImportCardContent(t *testing.T) {
	token := importTestJWT(map[string]any{
		"iat": float64(1_752_300_000),
		"exp": float64(1_752_900_000),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-k12",
			"chatgpt_plan_type":  "k12",
			"chatgpt_user_id":    "user-k12",
		},
	})
	record := map[string]any{
		"name":     "student@example.com--order-id",
		"platform": "openai",
		"type":     "oauth",
		"credentials": map[string]any{
			"access_token":       token,
			"chatgpt_account_id": "account-k12",
			"chatgpt_user_id":    "user-k12",
			"email":              "student@example.com",
			"plan_type":          "k12",
		},
		"extra": map[string]any{"no_rt": true},
	}
	raw, _ := json.Marshal(record)
	items, errors := parseAuthImportText("=== 卡密内容 ===\n" + string(raw))
	if len(errors) != 0 || len(items) != 1 {
		t.Fatalf("items=%+v errors=%+v", items, errors)
	}
	item := items[0]
	if item.SourceFormat != "sub2api/account-product" || item.PlanType != "k12" || item.HasRefreshToken {
		t.Fatalf("unexpected item: %+v", item)
	}
	if !strings.HasPrefix(item.FileName, "codex-") || !strings.HasSuffix(item.FileName, "-student@example.com-k12.json") {
		t.Fatalf("file name=%q", item.FileName)
	}
	if item.AuthJSON["account_id"] != "account-k12" || item.AuthJSON["type"] != "codex" {
		t.Fatalf("auth json=%+v", item.AuthJSON)
	}
	if item.AuthJSON["id_token_synthetic"] != true {
		t.Fatalf("synthetic id token marker missing: %+v", item.AuthJSON)
	}
	if len(item.Warnings) == 0 || !strings.Contains(item.Warnings[0], "Refresh Token") {
		t.Fatalf("warnings=%+v", item.Warnings)
	}
}

func TestParseAuthImportNestedFormatsAndRefreshToken(t *testing.T) {
	token := importTestJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	document := map[string]any{
		"accounts": []any{
			map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"access_token":  token,
					"refresh_token": "refresh-example",
					"id_token":      "header.payload.signature",
					"account_id":    "account-one",
				},
				"email": "refresh@example.com",
			},
		},
	}
	raw, _ := json.Marshal(document)
	items, errors := parseAuthImportText(string(raw))
	if len(errors) != 0 || len(items) != 1 {
		t.Fatalf("items=%+v errors=%+v", items, errors)
	}
	if !items[0].HasRefreshToken || items[0].SourceFormat != "codex/axonhub" {
		t.Fatalf("item=%+v", items[0])
	}
	if _, exists := items[0].AuthJSON["expired"]; exists {
		t.Fatalf("refreshable auth should not be marked expired: %+v", items[0].AuthJSON)
	}
}

func TestParseAuthImportPrettyJSONFilesWithHeaders(t *testing.T) {
	token := importTestJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	document := map[string]any{"email": "pretty@example.com", "access_token": token, "account_id": "account-pretty"}
	raw, _ := json.MarshalIndent(document, "", "  ")
	text := "=== first.json ===\n" + string(raw) + "\n=== ignored heading ===\n"
	items, errors := parseAuthImportText(text)
	if len(errors) != 0 || len(items) != 1 || items[0].Email != "pretty@example.com" {
		t.Fatalf("items=%+v errors=%+v", items, errors)
	}
}

func TestCommitAuthImportSkipsExistingUnlessOverwrite(t *testing.T) {
	oldCaller := hostAuthCaller
	t.Cleanup(func() { hostAuthCaller = oldCaller })
	token := importTestJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	record := map[string]any{"email": "existing@example.com", "access_token": token, "account_id": "account-existing", "plan_type": "plus"}
	raw, _ := json.Marshal(record)
	items, errors := parseAuthImportText(string(raw))
	if len(errors) != 0 || len(items) != 1 {
		t.Fatalf("items=%+v errors=%+v", items, errors)
	}
	fileName := items[0].FileName
	saves := 0
	hostAuthCaller = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{Name: fileName, Provider: "codex"}}})
		case "host.auth.save":
			saves++
			return json.Marshal(map[string]any{"name": fileName})
		default:
			return nil, os.ErrNotExist
		}
	}
	result, err := commitAuthImport(string(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.Skipped != 1 || saves != 0 {
		t.Fatalf("result=%+v saves=%d", result, saves)
	}
	result, err = commitAuthImport(string(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || saves != 1 {
		t.Fatalf("result=%+v saves=%d", result, saves)
	}
}

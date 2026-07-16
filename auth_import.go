package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxAuthImportTextBytes = 8 << 20

type authImportRequest struct {
	Text      string `json:"text"`
	Overwrite bool   `json:"overwrite"`
}

type authImportItem struct {
	Index           int            `json:"index"`
	SourceFormat    string         `json:"source_format"`
	FileName        string         `json:"file_name"`
	Email           string         `json:"email,omitempty"`
	PlanType        string         `json:"plan_type,omitempty"`
	ExpiresAt       string         `json:"expires_at,omitempty"`
	HasRefreshToken bool           `json:"has_refresh_token"`
	Existing        bool           `json:"existing,omitempty"`
	Warnings        []string       `json:"warnings,omitempty"`
	AuthJSON        map[string]any `json:"-"`
}

type authImportResult struct {
	Detected int              `json:"detected"`
	Imported int              `json:"imported,omitempty"`
	Skipped  int              `json:"skipped"`
	Failed   int              `json:"failed,omitempty"`
	Items    []authImportItem `json:"items"`
	Errors   []string         `json:"errors,omitempty"`
}

func handleAuthImportPreview(raw []byte) managementResponse {
	request, response := decodeAuthImportRequest(raw)
	if response != nil {
		return *response
	}
	result, err := previewAuthImport(request.Text)
	if err != nil {
		return jsonResponse(httpStatusForAuthImportError(err), map[string]any{"error": "auth_import_preview_failed", "message": err.Error()})
	}
	return jsonResponse(200, result)
}

func handleAuthImportCommit(raw []byte) managementResponse {
	request, response := decodeAuthImportRequest(raw)
	if response != nil {
		return *response
	}
	result, err := commitAuthImport(request.Text, request.Overwrite)
	if err != nil {
		return jsonResponse(httpStatusForAuthImportError(err), map[string]any{"error": "auth_import_failed", "message": err.Error()})
	}
	return jsonResponse(200, result)
}

func decodeAuthImportRequest(raw []byte) (authImportRequest, *managementResponse) {
	var request authImportRequest
	if len(raw) == 0 {
		response := jsonResponse(400, map[string]any{"error": "bad_request", "message": "text is required"})
		return request, &response
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		response := jsonResponse(400, map[string]any{"error": "bad_request", "message": err.Error()})
		return request, &response
	}
	request.Text = strings.TrimSpace(request.Text)
	if request.Text == "" {
		response := jsonResponse(400, map[string]any{"error": "bad_request", "message": "text is required"})
		return request, &response
	}
	if len(request.Text) > maxAuthImportTextBytes {
		response := jsonResponse(413, map[string]any{"error": "payload_too_large", "message": "import text exceeds 8 MiB"})
		return request, &response
	}
	return request, nil
}

func httpStatusForAuthImportError(err error) int {
	if err == nil {
		return 200
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "host callback") || strings.Contains(text, "host.auth") {
		return 503
	}
	return 400
}

func previewAuthImport(text string) (authImportResult, error) {
	items, parseErrors := parseAuthImportText(text)
	existing, err := existingHostAuthFileNames()
	if err != nil {
		return authImportResult{}, err
	}
	for i := range items {
		_, items[i].Existing = existing[strings.ToLower(items[i].FileName)]
	}
	return authImportResult{Detected: len(items), Skipped: len(parseErrors), Items: items, Errors: parseErrors}, nil
}

func commitAuthImport(text string, overwrite bool) (authImportResult, error) {
	items, parseErrors := parseAuthImportText(text)
	existing, err := existingHostAuthFileNames()
	if err != nil {
		return authImportResult{}, err
	}
	result := authImportResult{Detected: len(items), Items: items, Errors: append([]string(nil), parseErrors...), Skipped: len(parseErrors)}
	for i := range result.Items {
		item := &result.Items[i]
		_, item.Existing = existing[strings.ToLower(item.FileName)]
		if item.Existing && !overwrite {
			item.Warnings = appendUniqueString(item.Warnings, "文件已存在，已跳过")
			result.Skipped++
			continue
		}
		rawJSON, err := json.Marshal(item.AuthJSON)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: encode failed", item.FileName))
			continue
		}
		_, err = hostAuthCaller("host.auth.save", map[string]any{"name": item.FileName, "json": json.RawMessage(rawJSON)})
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", item.FileName, sanitizeTriggerError(err)))
			continue
		}
		result.Imported++
		existing[strings.ToLower(item.FileName)] = struct{}{}
	}
	return result, nil
}

func existingHostAuthFileNames() (map[string]struct{}, error) {
	raw, err := hostAuthCaller("host.auth.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var response hostAuthListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	out := make(map[string]struct{}, len(response.Files))
	for _, entry := range response.Files {
		name := filepath.Base(firstNonEmptyString(entry.Name, entry.Path))
		if strings.HasSuffix(strings.ToLower(name), ".json") {
			out[strings.ToLower(name)] = struct{}{}
		}
	}
	return out, nil
}

func parseAuthImportText(text string) ([]authImportItem, []string) {
	documents, decodeErrors := decodeAuthImportDocuments(text)
	var records []map[string]any
	for _, document := range documents {
		collectImportRecords(document, &records)
	}
	items := make([]authImportItem, 0, len(records))
	errors := append([]string(nil), decodeErrors...)
	seen := make(map[string]struct{})
	for index, record := range records {
		item, err := convertImportRecord(record, index+1)
		if err != nil {
			errors = append(errors, fmt.Sprintf("记录 %d: %s", index+1, err.Error()))
			continue
		}
		key := strings.ToLower(item.FileName)
		if _, exists := seen[key]; exists {
			item.Warnings = appendUniqueString(item.Warnings, "输入中存在重复账号，已忽略")
			errors = append(errors, fmt.Sprintf("记录 %d: duplicate output file %s", index+1, item.FileName))
			continue
		}
		seen[key] = struct{}{}
		items = append(items, item)
	}
	return items, errors
}

func decodeAuthImportDocuments(text string) ([]any, []string) {
	var whole any
	if err := json.Unmarshal([]byte(text), &whole); err == nil {
		return []any{whole}, nil
	}
	fragments := extractAuthImportJSONFragments(text)
	var documents []any
	var errors []string
	for index, fragment := range fragments {
		var document any
		if err := json.Unmarshal([]byte(fragment), &document); err != nil {
			errors = append(errors, fmt.Sprintf("JSON 片段 %d 解析失败", index+1))
			continue
		}
		documents = append(documents, document)
	}
	if len(documents) > 0 {
		return documents, errors
	}
	for lineNumber, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		startObject := strings.Index(line, "{")
		startArray := strings.Index(line, "[")
		start := startObject
		if start < 0 || (startArray >= 0 && startArray < start) {
			start = startArray
		}
		if start < 0 {
			continue
		}
		var document any
		if err := json.Unmarshal([]byte(line[start:]), &document); err != nil {
			errors = append(errors, fmt.Sprintf("第 %d 行 JSON 解析失败", lineNumber+1))
			continue
		}
		documents = append(documents, document)
	}
	if len(documents) == 0 {
		errors = append(errors, "没有找到可解析的 JSON 文档")
	}
	return documents, errors
}

func extractAuthImportJSONFragments(text string) []string {
	var fragments []string
	start := -1
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		if start < 0 {
			if char == '{' || char == '[' {
				start = index
				stack = append(stack[:0], char)
				inString = false
				escaped = false
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			continue
		}
		switch char {
		case '{', '[':
			stack = append(stack, char)
		case '}', ']':
			if len(stack) == 0 {
				start = -1
				continue
			}
			open := stack[len(stack)-1]
			if (open == '{' && char != '}') || (open == '[' && char != ']') {
				start = -1
				stack = stack[:0]
				continue
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				fragments = append(fragments, text[start:index+1])
				start = -1
			}
		}
	}
	return fragments
}

func collectImportRecords(value any, out *[]map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		if importString(typed, "accessToken", "access_token", "tokens.accessToken", "tokens.access_token", "token.accessToken", "token.access_token", "credentials.accessToken", "credentials.access_token") != "" {
			*out = append(*out, typed)
			return
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectImportRecords(typed[key], out)
		}
	case []any:
		for _, item := range typed {
			collectImportRecords(item, out)
		}
	}
}

func convertImportRecord(record map[string]any, index int) (authImportItem, error) {
	accessToken := importString(record, "accessToken", "access_token", "tokens.accessToken", "tokens.access_token", "token.accessToken", "token.access_token", "credentials.accessToken", "credentials.access_token")
	if accessToken == "" {
		return authImportItem{}, fmt.Errorf("缺少 access_token")
	}
	accessClaims := parseImportJWTPayload(accessToken)
	idToken := importString(record, "idToken", "id_token", "tokens.idToken", "tokens.id_token", "token.idToken", "token.id_token", "credentials.id_token")
	idClaims := parseImportJWTPayload(idToken)
	refreshToken := importString(record, "refreshToken", "refresh_token", "tokens.refreshToken", "tokens.refresh_token", "token.refreshToken", "token.refresh_token", "credentials.refresh_token")
	sessionToken := importString(record, "sessionToken", "session_token", "tokens.sessionToken", "tokens.session_token", "token.sessionToken", "token.session_token", "credentials.session_token")
	email := firstNonEmptyString(
		importString(record, "user.email", "email", "credentials.email", "providerSpecificData.email", "meta.label", "label"),
		importString(accessClaims, "https://api.openai.com/profile.email", "email"),
		importString(idClaims, "email"),
	)
	accountID := firstNonEmptyString(
		importString(record, "account.id", "account_id", "tokens.accountId", "tokens.account_id", "chatgptAccountId", "chatgpt_account_id", "meta.chatgptAccountId", "meta.chatgpt_account_id", "tokens.chatgptAccountId", "tokens.chatgpt_account_id", "providerSpecificData.chatgptAccountId", "providerSpecificData.chatgpt_account_id", "credentials.chatgpt_account_id"),
		importString(accessClaims, "https://api.openai.com/auth.chatgpt_account_id"),
		importString(idClaims, "https://api.openai.com/auth.chatgpt_account_id"),
	)
	userID := firstNonEmptyString(
		importString(record, "user.id", "user_id", "chatgptUserId", "credentials.chatgpt_user_id", "providerSpecificData.chatgptUserId", "providerSpecificData.chatgpt_user_id"),
		importString(accessClaims, "https://api.openai.com/auth.chatgpt_user_id", "https://api.openai.com/auth.user_id", "sub"),
		importString(idClaims, "https://api.openai.com/auth.chatgpt_user_id", "https://api.openai.com/auth.user_id", "sub"),
	)
	planType := strings.ToLower(firstNonEmptyString(
		importString(record, "account.planType", "account.plan_type", "planType", "plan_type", "credentials.plan_type", "providerSpecificData.chatgptPlanType", "providerSpecificData.chatgpt_plan_type"),
		importString(accessClaims, "https://api.openai.com/auth.chatgpt_plan_type"),
		importString(idClaims, "https://api.openai.com/auth.chatgpt_plan_type"),
	))
	expiresAt := importTimestamp(firstNonEmptyString(
		importString(record, "expires", "expiresAt", "expired", "expires_at", "credentials.expires_at"),
		importNumberString(accessClaims, "exp"),
	))
	lastRefresh := importTimestamp(firstNonEmptyString(importString(record, "last_refresh", "lastRefresh", "extra.last_refresh", "updatedAt"), importNumberString(accessClaims, "iat")))
	if lastRefresh == "" {
		lastRefresh = time.Now().UTC().Format(time.RFC3339)
	}
	if email == "" && accountID == "" {
		return authImportItem{}, fmt.Errorf("缺少邮箱和 account_id")
	}
	if idToken == "" && accountID != "" {
		idToken = buildSyntheticImportIDToken(email, accountID, planType, userID, expiresAt)
	}
	authJSON := map[string]any{
		"type":         "codex",
		"access_token": accessToken,
		"last_refresh": lastRefresh,
	}
	setImportString(authJSON, "email", email)
	setImportString(authJSON, "account_id", accountID)
	setImportString(authJSON, "plan_type", planType)
	setImportString(authJSON, "refresh_token", refreshToken)
	setImportString(authJSON, "session_token", sessionToken)
	setImportString(authJSON, "id_token", idToken)
	if idToken != "" && importString(record, "idToken", "id_token", "tokens.idToken", "tokens.id_token", "token.idToken", "token.id_token", "credentials.id_token") == "" {
		authJSON["id_token_synthetic"] = true
	}
	if expiresAt != "" && refreshToken == "" {
		authJSON["expired"] = expiresAt
	}
	if disabled := importBool(record, "disabled"); disabled {
		authJSON["disabled"] = true
	}
	fileName := importCredentialFileName(email, planType, accountID, index)
	warnings := make([]string, 0, 3)
	if refreshToken == "" {
		warnings = append(warnings, "无 Refresh Token，Access Token 过期后无法自动续期")
	}
	if accountID == "" {
		warnings = append(warnings, "缺少 account_id，部分 Codex 请求可能不可用")
	}
	if expiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil && !parsed.After(time.Now()) {
			warnings = append(warnings, "Access Token 已过期")
		}
	}
	return authImportItem{
		Index: index, SourceFormat: detectImportFormat(record), FileName: fileName, Email: email,
		PlanType: planType, ExpiresAt: expiresAt, HasRefreshToken: refreshToken != "", Warnings: warnings, AuthJSON: authJSON,
	}, nil
}

func detectImportFormat(record map[string]any) string {
	if importString(record, "platform") == "openai" && importString(record, "credentials.access_token") != "" {
		return "sub2api/account-product"
	}
	if importString(record, "provider") == "codex" && importString(record, "authType") == "oauth" {
		return "9router"
	}
	if importString(record, "auth_mode") == "chatgpt" && importString(record, "tokens.access_token") != "" {
		return "codex/axonhub"
	}
	if importString(record, "meta.label") != "" && importString(record, "tokens.access_token") != "" {
		return "codex-manager"
	}
	if importString(record, "accessToken") != "" && importString(record, "user.email", "account.id") != "" {
		return "chatgpt-session"
	}
	return "generic-codex"
}

func importCredentialFileName(email, planType, accountID string, index int) string {
	email = sanitizeImportFilePart(firstNonEmptyString(email, fmt.Sprintf("account-%d", index)))
	plan := sanitizeImportFilePart(planType)
	if (plan == "team" || plan == "k12") && accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		return fmt.Sprintf("codex-%s-%s-%s.json", hex.EncodeToString(digest[:])[:8], email, plan)
	}
	if plan != "" {
		return fmt.Sprintf("codex-%s-%s.json", email, plan)
	}
	return fmt.Sprintf("codex-%s.json", email)
}

func sanitizeImportFilePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '@' || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastDash = r == '-'
		} else if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-.")
}

func parseImportJWTPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

func buildSyntheticImportIDToken(email, accountID, planType, userID, expiresAt string) string {
	now := time.Now().Unix()
	expires := now + int64((90 * 24 * time.Hour).Seconds())
	if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil {
		expires = parsed.Unix()
	}
	auth := map[string]any{"chatgpt_account_id": accountID}
	setImportString(auth, "chatgpt_plan_type", planType)
	setImportString(auth, "chatgpt_user_id", userID)
	payload := map[string]any{"iat": now, "exp": expires, "https://api.openai.com/auth": auth}
	setImportString(payload, "email", email)
	headerRaw, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT", "cpa_synthetic": true})
	payloadRaw, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(headerRaw) + "." + base64.RawURLEncoding.EncodeToString(payloadRaw) + ".synthetic"
}

func importString(root map[string]any, paths ...string) string {
	for _, path := range paths {
		value, ok := importPath(root, path)
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func importNumberString(root map[string]any, paths ...string) string {
	for _, path := range paths {
		value, ok := importPath(root, path)
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			return typed.String()
		case float64:
			return fmt.Sprintf("%.0f", typed)
		case int64:
			return fmt.Sprintf("%d", typed)
		}
	}
	return ""
}

func importBool(root map[string]any, path string) bool {
	value, ok := importPath(root, path)
	if !ok {
		return false
	}
	result, _ := value.(bool)
	return result
}

func importPath(root map[string]any, path string) (any, bool) {
	var current any = root
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func importTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if unix, err := json.Number(value).Int64(); err == nil {
		unix = normalizeUnixSeconds(unix)
		if unix > 0 {
			return time.Unix(unix, 0).UTC().Format(time.RFC3339)
		}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func setImportString(target map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		target[key] = strings.TrimSpace(value)
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xaiTierFree  = "free"
	xaiTierSuper = "super"
	xaiTierHeavy = "heavy"
)

type hostCallFunc func(method string, payload any) (json.RawMessage, error)

var hostAuthCaller hostCallFunc = callHost

type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthFileEntry struct {
	Account       string `json:"account"`
	AccountType   string `json:"account_type"`
	AuthIndex     string `json:"auth_index"`
	Disabled      bool   `json:"disabled"`
	Email         string `json:"email"`
	Expired       bool   `json:"expired"`
	ID            string `json:"id"`
	Label         string `json:"label"`
	Name          string `json:"name"`
	Note          string `json:"note"`
	Path          string `json:"path"`
	Plan          string `json:"plan"`
	PlanType      string `json:"plan_type"`
	Prefix        string `json:"prefix"`
	Priority      int    `json:"priority"`
	Provider      string `json:"provider"`
	Source        string `json:"source"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	Subscription  string `json:"subscription"`
	Tag           string `json:"tag"`
	Type          string `json:"type"`
	Unavailable   bool   `json:"unavailable"`
	RuntimeOnly   bool   `json:"runtime_only"`
	ModTime       string `json:"modtime"`
	UpdatedAt     string `json:"updated_at"`
}

type hostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

type hostAuthRuntimeResponse struct {
	Auth hostAuthFileEntry `json:"auth"`
}

type xaiTierClassification struct {
	Tier   string
	Source string
	Detail string
}

type cachedXAITier struct {
	Version   string
	FetchedAt time.Time
	Value     xaiTierClassification
}

type xaiAuthSourceDiagnostics struct {
	Source             string `json:"source"`
	Authoritative      bool   `json:"authoritative"`
	Accounts           int    `json:"accounts"`
	MetadataReadErrors int    `json:"metadata_read_errors"`
	LastSuccessAt      string `json:"last_success_at,omitempty"`
	LastError          string `json:"last_error,omitempty"`
}

type xaiAuthSourceManager struct {
	mu          sync.Mutex
	fetchedAt   time.Time
	accounts    []configuredAccount
	tierCache   map[string]cachedXAITier
	diagnostics xaiAuthSourceDiagnostics
}

var globalXAIAuthSource = &xaiAuthSourceManager{}

func (m *xaiAuthSourceManager) hostAccounts() ([]configuredAccount, error) {
	m.mu.Lock()
	if time.Since(m.fetchedAt) < 3*time.Second && m.diagnostics.Source == "host_callback" {
		accounts := cloneConfiguredAccounts(m.accounts)
		m.mu.Unlock()
		return accounts, nil
	}
	m.mu.Unlock()

	raw, err := hostAuthCaller("host.auth.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var response hostAuthListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	accounts := make([]configuredAccount, 0, len(response.Files))
	metadataErrors := 0
	for _, entry := range response.Files {
		provider := normalizeAuthProvider(firstNonEmptyString(entry.Provider, entry.Type), firstNonEmptyString(entry.Name, entry.Path, entry.AuthIndex))
		if !strings.EqualFold(provider, "xai") {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) != "" && strings.TrimSpace(entry.Status) == "" {
			if runtimeErr := m.mergeHostRuntime(&entry); runtimeErr != nil {
				metadataErrors++
			}
		}
		classification, metadataErr := m.classifyHostEntry(entry)
		if metadataErr != nil {
			metadataErrors++
		}
		email := firstNonEmptyString(entry.Email, entry.Account)
		authFile := firstNonEmptyString(fileNameIfJSON(entry.Name), fileNameIfJSON(entry.Path), fileNameIfJSON(entry.AuthIndex))
		authIndex := firstNonEmptyString(entry.AuthIndex, authFile, entry.ID)
		name := firstNonEmptyString(entry.Name, filepath.Base(entry.Path))
		source := firstNonEmptyString(entry.Source, email, name, authIndex)
		planType := firstNonEmptyString(entry.PlanType, entry.Plan, entry.Subscription)
		accounts = append(accounts, configuredAccount{
			AuthIndex:          authIndex,
			AuthID:             firstNonEmptyString(entry.ID, email),
			Source:             source,
			Provider:           "xai",
			Email:              email,
			Name:               name,
			AuthFile:           authFile,
			AuthFileMTime:      parseHostAuthUpdatedAt(entry.UpdatedAt),
			Disabled:           entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled"),
			Expired:            entry.Expired || strings.EqualFold(strings.TrimSpace(entry.Status), "expired"),
			PlanType:           planType,
			XAITier:            classification.Tier,
			XAITierSource:      classification.Source,
			XAITierDetail:      classification.Detail,
			RuntimeStatus:      strings.TrimSpace(entry.Status),
			RuntimeMessage:     sanitizeTriggerError(entry.StatusMessage),
			RuntimeUnavailable: entry.Unavailable,
		})
	}
	now := time.Now()
	m.mu.Lock()
	m.fetchedAt = now
	m.accounts = cloneConfiguredAccounts(accounts)
	m.diagnostics = xaiAuthSourceDiagnostics{
		Source:             "host_callback",
		Authoritative:      true,
		Accounts:           len(accounts),
		MetadataReadErrors: metadataErrors,
		LastSuccessAt:      now.Format(time.RFC3339),
	}
	m.mu.Unlock()
	return accounts, nil
}

func (m *xaiAuthSourceManager) mergeHostRuntime(entry *hostAuthFileEntry) error {
	raw, err := hostAuthCaller("host.auth.get_runtime", hostAuthGetRequest{AuthIndex: entry.AuthIndex})
	if err != nil {
		return err
	}
	var response hostAuthRuntimeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode host.auth.get_runtime result: %w", err)
	}
	runtime := response.Auth
	if runtime.Status != "" {
		entry.Status = runtime.Status
	}
	if runtime.StatusMessage != "" {
		entry.StatusMessage = runtime.StatusMessage
	}
	entry.Disabled = entry.Disabled || runtime.Disabled
	entry.Unavailable = entry.Unavailable || runtime.Unavailable
	if runtime.UpdatedAt != "" {
		entry.UpdatedAt = runtime.UpdatedAt
	}
	if runtime.Priority != 0 {
		entry.Priority = runtime.Priority
	}
	return nil
}

func (m *xaiAuthSourceManager) classifyHostEntry(entry hostAuthFileEntry) (xaiTierClassification, error) {
	base := classifyXAITierSignals([]xaiTierSignal{
		{Path: "host.account_type", Value: entry.AccountType},
		{Path: "host.plan_type", Value: entry.PlanType},
		{Path: "host.plan", Value: entry.Plan},
		{Path: "host.subscription", Value: entry.Subscription},
		{Path: "host.note", Value: entry.Note},
		{Path: "host.prefix", Value: entry.Prefix},
		{Path: "host.label", Value: entry.Label},
		{Path: "host.tag", Value: entry.Tag},
		{Path: "host.name", Value: entry.Name},
	})
	if strings.TrimSpace(entry.AuthIndex) == "" || base.Tier == xaiTierHeavy {
		return base, nil
	}
	cacheKey := normalizeAccountAlias(entry.AuthIndex)
	version := strings.TrimSpace(entry.UpdatedAt)
	m.mu.Lock()
	if cached, ok := m.tierCache[cacheKey]; ok && ((version != "" && cached.Version == version) || (version == "" && time.Since(cached.FetchedAt) < 5*time.Minute)) {
		m.mu.Unlock()
		return cached.Value, nil
	}
	m.mu.Unlock()
	raw, err := hostAuthCaller("host.auth.get", hostAuthGetRequest{AuthIndex: entry.AuthIndex})
	if err != nil {
		return base, err
	}
	var response hostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return base, fmt.Errorf("decode host.auth.get result: %w", err)
	}
	classification := classifyXAITierJSON(response.JSON, base)
	m.mu.Lock()
	if m.tierCache == nil {
		m.tierCache = make(map[string]cachedXAITier)
	}
	m.tierCache[cacheKey] = cachedXAITier{Version: version, FetchedAt: time.Now(), Value: classification}
	m.mu.Unlock()
	return classification, nil
}

func (m *xaiAuthSourceManager) markFilesystemFallback(accounts []configuredAccount, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics.Source = "filesystem_fallback"
	m.diagnostics.Authoritative = configuredAuthDirectoryReadable()
	m.diagnostics.Accounts = len(accounts)
	m.diagnostics.MetadataReadErrors = 0
	m.diagnostics.LastError = sanitizeTriggerError(err)
}

func (m *xaiAuthSourceManager) authoritative() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diagnostics.Authoritative
}

func (m *xaiAuthSourceManager) status() xaiAuthSourceDiagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diagnostics
}

func cloneConfiguredAccounts(accounts []configuredAccount) []configuredAccount {
	return append([]configuredAccount(nil), accounts...)
}

func parseHostAuthUpdatedAt(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		return normalizeUnixSeconds(unix)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

type xaiTierSignal struct {
	Path  string
	Value string
}

func classifyXAITierJSON(raw json.RawMessage, base xaiTierClassification) xaiTierClassification {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return base
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return base
	}
	signals := collectXAITierSignals(value, "auth")
	classified := classifyXAITierSignals(signals)
	if xaiTierRank(classified.Tier) > xaiTierRank(base.Tier) || (base.Source == "default" && classified.Source != "default") {
		return classified
	}
	return base
}

func classifyXAITierDocument(doc map[string]any) xaiTierClassification {
	raw, _ := json.Marshal(doc)
	return classifyXAITierJSON(raw, xaiTierClassification{Tier: xaiTierFree, Source: "default", Detail: "No paid xAI tier metadata"})
}

func collectXAITierSignals(value any, path string) []xaiTierSignal {
	var out []xaiTierSignal
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := path + "." + key
			if xaiTierMetadataKey(key) {
				switch scalar := child.(type) {
				case string:
					out = append(out, xaiTierSignal{Path: childPath, Value: scalar})
				case json.Number:
					out = append(out, xaiTierSignal{Path: childPath, Value: scalar.String()})
				}
			}
			out = append(out, collectXAITierSignals(child, childPath)...)
		}
	case []any:
		for index, child := range typed {
			out = append(out, collectXAITierSignals(child, fmt.Sprintf("%s[%d]", path, index))...)
		}
	}
	return out
}

func xaiTierMetadataKey(key string) bool {
	normalized := normalizeXAITierText(key)
	switch normalized {
	case "tier", "plantype", "plan", "accounttype", "accounttier", "subscription", "subscriptiontype", "subscriptiontier", "subscriptionplan", "membership", "membershiptier", "product", "producttier", "sku", "license", "entitlement", "entitlements", "xaitier", "xaiplan", "groktier", "grokplan", "groksubscription", "servicetier", "note", "prefix", "label", "tag", "grouptag", "grouplabel":
		return true
	default:
		return false
	}
}

func classifyXAITierSignals(signals []xaiTierSignal) xaiTierClassification {
	best := xaiTierClassification{Tier: xaiTierFree, Source: "default", Detail: "No paid xAI tier metadata"}
	for _, signal := range signals {
		tier := xaiTierFromText(signal.Value)
		if tier == "" {
			continue
		}
		candidate := xaiTierClassification{Tier: tier, Source: signal.Path, Detail: strings.TrimSpace(signal.Value)}
		if xaiTierRank(candidate.Tier) > xaiTierRank(best.Tier) || best.Source == "default" {
			best = candidate
		}
	}
	return best
}

func xaiTierFromText(value string) string {
	normalized := normalizeXAITierText(value)
	switch {
	case normalized == "":
		return ""
	case strings.Contains(normalized, "supergrokheavy"), strings.Contains(normalized, "grokheavy"), strings.Contains(normalized, "heavy"), strings.Contains(normalized, "supergrokpro"):
		return xaiTierHeavy
	case strings.Contains(normalized, "supergrok"), strings.Contains(normalized, "grokpro"), strings.Contains(normalized, "premiumplus"), strings.Contains(normalized, "premium"), normalized == "super", normalized == "pro", normalized == "plus", normalized == "paid":
		return xaiTierSuper
	case normalized == "free", strings.Contains(normalized, "freetier"), strings.Contains(normalized, "subscriptiontierfree"):
		return xaiTierFree
	default:
		return ""
	}
}

func normalizeXAITierText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func xaiTierRank(tier string) int {
	switch tier {
	case xaiTierHeavy:
		return 3
	case xaiTierSuper:
		return 2
	case xaiTierFree:
		return 1
	default:
		return 0
	}
}

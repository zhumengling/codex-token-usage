package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const codexAuthSourceCacheTTL = 3 * time.Second

type codexAuthSourceManager struct {
	mu          sync.Mutex
	fetchedAt   time.Time
	accounts    []configuredAccount
	revision    string
	lastError   error
	diagnostics xaiAuthSourceDiagnostics
}

var globalCodexAuthSource = &codexAuthSourceManager{}

func (m *codexAuthSourceManager) hostAccounts() ([]configuredAccount, error) {
	m.mu.Lock()
	if time.Since(m.fetchedAt) < codexAuthSourceCacheTTL && m.diagnostics.Source == "host_callback" {
		accounts := cloneConfiguredAccounts(m.accounts)
		m.mu.Unlock()
		return accounts, nil
	}
	if time.Since(m.fetchedAt) < codexAuthSourceCacheTTL && m.diagnostics.Source == "filesystem_fallback" && m.lastError != nil {
		err := m.lastError
		m.mu.Unlock()
		return nil, err
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
	for _, entry := range response.Files {
		provider := normalizeAuthProvider(firstNonEmptyString(entry.Provider, entry.Type), firstNonEmptyString(entry.Name, entry.Path, entry.AuthIndex))
		if !isCodexAuthProvider(provider) {
			continue
		}
		email := firstNonEmptyString(entry.Email, entry.Account)
		authFile := firstNonEmptyString(fileNameIfJSON(entry.Name), fileNameIfJSON(entry.Path), fileNameIfJSON(entry.ID))
		authIndex := firstNonEmptyString(entry.AuthIndex, authFile, entry.ID)
		name := firstNonEmptyString(entry.Name, filepath.Base(entry.Path), entry.Label)
		source := firstNonEmptyString(email, name, authIndex)
		accounts = append(accounts, configuredAccount{
			AuthIndex:          authIndex,
			AuthID:             firstNonEmptyString(entry.ID, email, authIndex),
			Source:             source,
			Provider:           "codex",
			Email:              email,
			Name:               name,
			AuthFile:           authFile,
			AuthFileMTime:      parseHostAuthUpdatedAt(firstNonEmptyString(entry.ModTime, entry.UpdatedAt)),
			Disabled:           entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled"),
			Expired:            entry.Expired || strings.EqualFold(strings.TrimSpace(entry.Status), "expired"),
			PlanType:           firstNonEmptyString(entry.PlanType, entry.Plan, entry.Subscription),
			RuntimeStatus:      strings.TrimSpace(entry.Status),
			RuntimeMessage:     sanitizeTriggerError(entry.StatusMessage),
			RuntimeUnavailable: entry.Unavailable,
		})
	}
	revision := configuredAccountListRevision(accounts)
	now := time.Now()
	m.mu.Lock()
	m.fetchedAt = now
	m.accounts = cloneConfiguredAccounts(accounts)
	m.revision = revision
	m.lastError = nil
	m.diagnostics = xaiAuthSourceDiagnostics{
		Source:        "host_callback",
		Authoritative: true,
		Accounts:      len(accounts),
		LastSuccessAt: now.Format(time.RFC3339),
	}
	m.mu.Unlock()
	return accounts, nil
}

func (m *codexAuthSourceManager) markFilesystemFallback(accounts []configuredAccount, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts = cloneConfiguredAccounts(accounts)
	m.revision = configuredAccountListRevision(accounts)
	m.fetchedAt = time.Now()
	m.lastError = err
	m.diagnostics.Source = "filesystem_fallback"
	m.diagnostics.Authoritative = configuredAuthDirectoryReadable()
	m.diagnostics.Accounts = len(accounts)
	m.diagnostics.MetadataReadErrors = 0
	m.diagnostics.LastError = sanitizeTriggerError(err)
}

func (m *codexAuthSourceManager) authoritative() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diagnostics.Authoritative
}

func (m *codexAuthSourceManager) status() xaiAuthSourceDiagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diagnostics
}

func (m *codexAuthSourceManager) currentRevision() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revision
}

func (m *codexAuthSourceManager) invalidate() {
	m.mu.Lock()
	m.fetchedAt = time.Time{}
	m.mu.Unlock()
}

func configuredAccountListRevision(accounts []configuredAccount) string {
	rows := make([]string, 0, len(accounts))
	for _, account := range accounts {
		rows = append(rows, strings.Join([]string{
			account.Provider,
			account.AuthIndex,
			account.AuthID,
			account.AuthFile,
			account.Email,
			strconv.FormatInt(account.AuthFileMTime, 10),
			strconv.FormatBool(account.Disabled),
			strconv.FormatBool(account.Expired),
			account.RuntimeStatus,
			strconv.FormatBool(account.RuntimeUnavailable),
		}, "\x00"))
	}
	sort.Strings(rows)
	hash := fnv.New64a()
	for _, row := range rows {
		_, _ = hash.Write([]byte(row))
		_, _ = hash.Write([]byte{0})
	}
	return strconv.Itoa(len(rows)) + ":" + strconv.FormatUint(hash.Sum64(), 16)
}

func mergeConfiguredAccountMetadata(accounts, metadata []configuredAccount) []configuredAccount {
	if len(accounts) == 0 || len(metadata) == 0 {
		return accounts
	}
	index := make(map[string]configuredAccount, len(metadata)*4)
	for _, item := range metadata {
		for _, alias := range configuredAliases(item) {
			if alias != "" {
				index[alias] = item
			}
		}
	}
	for i := range accounts {
		var detail configuredAccount
		found := false
		for _, alias := range configuredAliases(accounts[i]) {
			if item, ok := index[alias]; ok {
				detail = item
				found = true
				break
			}
		}
		if !found {
			continue
		}
		accounts[i].Email = firstNonEmptyString(accounts[i].Email, detail.Email)
		accounts[i].Name = firstNonEmptyString(accounts[i].Name, detail.Name)
		accounts[i].PlanType = firstNonEmptyString(accounts[i].PlanType, detail.PlanType)
		accounts[i].ChatGPTAccountID = firstNonEmptyString(accounts[i].ChatGPTAccountID, detail.ChatGPTAccountID)
		accounts[i].AccessToken = firstNonEmptyString(accounts[i].AccessToken, detail.AccessToken)
		if accounts[i].AuthFileMTime == 0 {
			accounts[i].AuthFileMTime = detail.AuthFileMTime
		}
	}
	return accounts
}

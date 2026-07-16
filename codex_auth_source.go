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

const (
	authSourceKindFile        = "file"
	authSourceKindRuntimeOnly = "runtime_only"
	authSourceKindLegacy      = "legacy"
)

type codexAuthSourceManager struct {
	mu          sync.Mutex
	fetchedAt   time.Time
	accounts    []configuredAccount
	inventory   []configuredAccount
	revision    string
	lastError   error
	diagnostics xaiAuthSourceDiagnostics
	epoch       uint64
}

var globalCodexAuthSource = &codexAuthSourceManager{}

func (m *codexAuthSourceManager) hostAccounts() ([]configuredAccount, error) {
	if err := m.refreshHostInventory(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneConfiguredAccounts(m.accounts), nil
}

func (m *codexAuthSourceManager) hostInventory() ([]configuredAccount, error) {
	if err := m.refreshHostInventory(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneConfiguredAccounts(m.inventory), nil
}

func (m *codexAuthSourceManager) refreshHostInventory() error {
	for {
		m.mu.Lock()
		if time.Since(m.fetchedAt) < codexAuthSourceCacheTTL && m.diagnostics.Source == "host_callback" {
			m.mu.Unlock()
			return nil
		}
		if time.Since(m.fetchedAt) < codexAuthSourceCacheTTL && m.diagnostics.Source == "filesystem_fallback" && m.lastError != nil {
			err := m.lastError
			m.mu.Unlock()
			return err
		}
		epoch := m.epoch
		m.mu.Unlock()

		raw, err := hostAuthCaller("host.auth.list", map[string]any{})
		if err != nil {
			return err
		}
		var response hostAuthListResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return fmt.Errorf("decode host.auth.list result: %w", err)
		}
		inventory := make([]configuredAccount, 0, len(response.Files))
		accounts := make([]configuredAccount, 0, len(response.Files))
		for _, entry := range response.Files {
			account, ok := configuredCodexHostAuthEntry(entry)
			if !ok {
				continue
			}
			inventory = append(inventory, account)
			if account.AuthSourceKind == authSourceKindFile {
				accounts = append(accounts, account)
			}
		}
		revision := configuredAccountListRevision(inventory)
		now := time.Now()
		m.mu.Lock()
		if m.epoch != epoch {
			m.mu.Unlock()
			continue
		}
		m.fetchedAt = now
		m.accounts = cloneConfiguredAccounts(accounts)
		m.inventory = cloneConfiguredAccounts(inventory)
		m.revision = revision
		m.lastError = nil
		m.diagnostics = xaiAuthSourceDiagnostics{
			Source:        "host_callback",
			Authoritative: true,
			Accounts:      len(accounts),
			LastSuccessAt: now.Format(time.RFC3339),
		}
		m.mu.Unlock()
		return nil
	}
}

func configuredCodexHostAuthEntry(entry hostAuthFileEntry) (configuredAccount, bool) {
	if pluginOwnedHostAuthEntry(entry) || !explicitCodexHostProvider(entry.Provider, entry.Type) {
		return configuredAccount{}, false
	}
	email := firstNonEmptyString(entry.Email, entry.Account)
	authFile := firstNonEmptyString(fileNameIfJSON(entry.Name), fileNameIfJSON(entry.Path), fileNameIfJSON(entry.ID))
	sourceKind := codexHostAuthSourceKind(entry, authFile)
	if sourceKind != authSourceKindFile {
		authFile = ""
	}
	authIndex := firstNonEmptyString(entry.AuthIndex, authFile, entry.ID)
	pathName := ""
	if strings.TrimSpace(entry.Path) != "" {
		pathName = filepath.Base(entry.Path)
	}
	name := firstNonEmptyString(entry.Name, pathName, entry.Label)
	source := firstNonEmptyString(email, name, authIndex)
	return configuredAccount{
		AuthIndex:          authIndex,
		AuthID:             firstNonEmptyString(entry.ID, authIndex),
		Source:             source,
		Provider:           "codex",
		Email:              email,
		Name:               name,
		AuthFile:           authFile,
		AuthFileMTime:      parseHostAuthUpdatedAt(firstNonEmptyString(entry.ModTime, entry.UpdatedAt)),
		AuthSourceKind:     sourceKind,
		Disabled:           entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled"),
		Expired:            entry.Expired || strings.EqualFold(strings.TrimSpace(entry.Status), "expired"),
		PlanType:           firstNonEmptyString(entry.PlanType, entry.Plan, entry.Subscription),
		RuntimeStatus:      strings.TrimSpace(entry.Status),
		RuntimeMessage:     sanitizeTriggerError(entry.StatusMessage),
		RuntimeUnavailable: entry.Unavailable,
	}, true
}

func explicitCodexHostProvider(values ...string) bool {
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "codex", "openai", "chatgpt":
			return true
		}
	}
	return false
}

func pluginOwnedHostAuthEntry(entry hostAuthFileEntry) bool {
	for _, value := range []string{entry.Path, entry.Name, entry.ID, entry.AuthIndex} {
		if pluginOwnedDataPath(value) {
			return true
		}
	}
	return false
}

func pluginOwnedDataPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	normalized := strings.ToLower(filepath.ToSlash(filepath.Clean(value)))
	marker := "/plugins/" + strings.ToLower(pluginID) + "/"
	if strings.Contains(normalized, marker) || strings.HasPrefix(normalized, "plugins/"+strings.ToLower(pluginID)+"/") {
		return true
	}
	if strings.ToLower(filepath.Base(value)) != "model_prices.json" {
		return false
	}
	dir, err := pluginDataDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absValue, err := filepath.Abs(value)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absValue)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func codexHostAuthSourceKind(entry hostAuthFileEntry, authFile string) string {
	source := strings.ToLower(strings.TrimSpace(entry.Source))
	if entry.RuntimeOnly || source == "memory" || source == "runtime" || source == authSourceKindRuntimeOnly {
		return authSourceKindRuntimeOnly
	}
	if authFile != "" {
		return authSourceKindFile
	}
	return authSourceKindLegacy
}

func normalizeAuthSourceKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case authSourceKindFile:
		return authSourceKindFile
	case authSourceKindRuntimeOnly:
		return authSourceKindRuntimeOnly
	default:
		return authSourceKindLegacy
	}
}

func readCodexHostAuthInventory() ([]configuredAccount, error) {
	return globalCodexAuthSource.hostInventory()
}

func readCodexRuntimeAuth(authIndex string) (configuredAccount, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return configuredAccount{}, fmt.Errorf("auth_index is required")
	}
	raw, err := hostAuthCaller("host.auth.get_runtime", hostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return configuredAccount{}, err
	}
	var response hostAuthRuntimeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return configuredAccount{}, fmt.Errorf("decode host.auth.get_runtime result: %w", err)
	}
	account, ok := configuredCodexHostAuthEntry(response.Auth)
	if !ok {
		return configuredAccount{}, fmt.Errorf("runtime auth is not a Codex credential")
	}
	return account, nil
}

// matchCodexHostAuthInventoryExact resolves only stable host identities. It
// deliberately ignores email, display name and Source so duplicate accounts
// can never cause a credential file to be guessed.
func matchCodexHostAuthInventoryExact(row invalidAuthRow, inventory []configuredAccount) (configuredAccount, bool) {
	return newCodexAuthIdentityIndex(inventory).match(row)
}

type codexAuthIdentityIndex struct {
	inventory      []configuredAccount
	byAuthIndex    map[string][]int
	byStableAuthID map[string][]int
	byAuthFile     map[string][]int
}

func newCodexAuthIdentityIndex(inventory []configuredAccount) *codexAuthIdentityIndex {
	index := &codexAuthIdentityIndex{
		inventory:      inventory,
		byAuthIndex:    make(map[string][]int),
		byStableAuthID: make(map[string][]int),
		byAuthFile:     make(map[string][]int),
	}
	for i, candidate := range inventory {
		authIndex := codexIndexedAuthIndex(candidate)
		authFile := normalizeAccountAlias(candidate.AuthFile)
		if authIndex != "" {
			index.byAuthIndex[authIndex] = append(index.byAuthIndex[authIndex], i)
		}
		if value := codexHostStableAuthID(candidate); value != "" {
			index.byStableAuthID[value] = append(index.byStableAuthID[value], i)
		}
		if authFile != "" {
			index.byAuthFile[authFile] = append(index.byAuthFile[authFile], i)
		}
	}
	return index
}

func (index *codexAuthIdentityIndex) match(row invalidAuthRow) (configuredAccount, bool) {
	matched, ok := index.matchIndex(row)
	if !ok {
		return configuredAccount{}, false
	}
	return index.inventory[matched], true
}

func (index *codexAuthIdentityIndex) matchIndex(row invalidAuthRow) (int, bool) {
	matches := index.matchIndexes(row)
	if len(matches) != 1 {
		return 0, false
	}
	return matches[0], true
}

func (index *codexAuthIdentityIndex) matchIndexes(row invalidAuthRow) []int {
	if index == nil || len(index.inventory) == 0 {
		return nil
	}
	rowKind := normalizeAuthSourceKind(row.AuthSourceKind)
	authIDValue := normalizeAccountAlias(row.AuthID)
	authIDFile := normalizeAccountAlias(fileNameIfJSON(row.AuthID))
	authIndexFile := normalizeAccountAlias(fileNameIfJSON(row.AuthIndex))
	var fileIdentities []string
	if rowKind != authSourceKindRuntimeOnly {
		fileValues := []string{
			normalizeAccountAlias(fileNameIfJSON(row.AuthFile)),
			normalizeAccountAlias(fileNameIfJSON(row.Source)),
			authIndexFile,
		}
		if rowKind == authSourceKindFile {
			fileValues = append(fileValues, authIDFile)
		}
		fileIdentities = uniqueNonEmptyAliases(fileValues)
	}
	authIndexValue := normalizeAccountAlias(row.AuthIndex)
	for _, fileIdentity := range fileIdentities {
		if authIndexValue == fileIdentity {
			// Filesystem probes know the exact physical JSON name but not CPA's
			// runtime AuthIndex hash. Treat a filename-shaped AuthIndex as the same
			// file identity so it cannot conflict with the authoritative host index.
			authIndexValue = ""
			break
		}
	}
	candidateSeen := make(map[int]struct{})
	candidateIndexes := make([]int, 0, 4)
	addCandidates := func(values []int) {
		for _, value := range values {
			if _, ok := candidateSeen[value]; ok {
				continue
			}
			candidateSeen[value] = struct{}{}
			candidateIndexes = append(candidateIndexes, value)
		}
	}
	if authIndexValue != "" {
		addCandidates(index.byAuthIndex[authIndexValue])
	}
	if authIDValue != "" && !strings.Contains(authIDValue, "@") {
		switch {
		case rowKind == authSourceKindRuntimeOnly || authIDFile == "":
			addCandidates(index.byStableAuthID[authIDValue])
		case rowKind == authSourceKindLegacy:
			addCandidates(index.byStableAuthID[authIDValue])
			addCandidates(index.byAuthFile[authIDFile])
		}
	}
	for _, fileIdentity := range fileIdentities {
		addCandidates(index.byAuthFile[fileIdentity])
	}
	if len(candidateIndexes) == 0 {
		return nil
	}
	matches := make([]int, 0, 1)
	for _, candidateIndex := range candidateIndexes {
		candidate := index.inventory[candidateIndex]
		candidateKind := normalizeAuthSourceKind(candidate.AuthSourceKind)
		candidateAuthIndex := codexIndexedAuthIndex(candidate)
		candidateAuthID := codexHostStableAuthID(candidate)
		candidateAuthFile := normalizeAccountAlias(candidate.AuthFile)
		if rowKind == authSourceKindFile && candidateKind == authSourceKindRuntimeOnly {
			continue
		}
		if rowKind == authSourceKindRuntimeOnly && candidateAuthFile != "" {
			continue
		}
		matchedIdentity := false
		conflict := false
		if authIndexValue != "" && candidateAuthIndex != "" {
			if authIndexValue != candidateAuthIndex {
				conflict = true
			} else {
				matchedIdentity = true
			}
		}
		if !conflict && authIDValue != "" && !strings.Contains(authIDValue, "@") {
			switch {
			case rowKind == authSourceKindRuntimeOnly || authIDFile == "":
				if candidateAuthID != "" {
					if authIDValue != candidateAuthID {
						conflict = true
					} else {
						matchedIdentity = true
					}
				}
			case rowKind == authSourceKindLegacy:
				hasComparableIdentity := candidateAuthID != "" || candidateAuthFile != ""
				identityMatches := candidateAuthID == authIDValue || candidateAuthFile == authIDFile
				if hasComparableIdentity && !identityMatches {
					conflict = true
				} else if identityMatches {
					matchedIdentity = true
				}
			}
		}
		if conflict {
			continue
		}
		for _, fileIdentity := range fileIdentities {
			if candidateAuthFile == "" {
				if rowKind == authSourceKindFile {
					conflict = true
					break
				}
				continue
			}
			if candidateAuthFile != fileIdentity {
				conflict = true
				break
			}
			matchedIdentity = true
		}
		if !conflict && matchedIdentity {
			matches = append(matches, candidateIndex)
		}
	}
	return matches
}

func codexIndexedAuthIndex(candidate configuredAccount) string {
	authIndex := normalizeAccountAlias(candidate.AuthIndex)
	authFile := normalizeAccountAlias(candidate.AuthFile)
	if authFile != "" && authIndex == authFile {
		return ""
	}
	return authIndex
}

func codexHostStableAuthID(candidate configuredAccount) string {
	authID := normalizeAccountAlias(candidate.AuthID)
	if authID == "" || authID == normalizeAccountAlias(candidate.Email) {
		return ""
	}
	return authID
}

func (m *codexAuthSourceManager) markFilesystemFallback(accounts []configuredAccount, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range accounts {
		if fileNameIfJSON(accounts[i].AuthFile) != "" {
			accounts[i].AuthSourceKind = authSourceKindFile
		}
	}
	m.accounts = cloneConfiguredAccounts(accounts)
	m.inventory = cloneConfiguredAccounts(accounts)
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
	m.epoch++
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
			account.AuthSourceKind,
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

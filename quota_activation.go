package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	quotaActivationPreviewTTL = 10 * time.Minute
	quotaActivationMaxItems   = 2000
)

var (
	globalQuotaActivation           = &quotaActivationManager{}
	quotaActivationPropagationGrace = 3 * time.Second
	quotaProbeGate                  = make(chan struct{}, 1)
)

func init() {
	quotaProbeGate <- struct{}{}
}

type quotaActivationReason string

const (
	activationEligible           quotaActivationReason = "eligible"
	activationNotSelected        quotaActivationReason = "not_selected"
	activationWrongProvider      quotaActivationReason = "wrong_provider"
	activationDisabled           quotaActivationReason = "disabled"
	activationExpired            quotaActivationReason = "expired"
	activationUnavailable        quotaActivationReason = "unavailable"
	activationUnstableIdentity   quotaActivationReason = "unstable_identity"
	activationMissingCredential  quotaActivationReason = "missing_credential"
	activationQuotaReadFailed    quotaActivationReason = "quota_read_failed"
	activationUnknownQuota       quotaActivationReason = "unknown_quota"
	activationPrimaryNotFresh    quotaActivationReason = "primary_not_fresh"
	activationSecondaryNotFresh  quotaActivationReason = "secondary_not_fresh"
	activationDuplicateCycle     quotaActivationReason = "duplicate_cycle"
	activationInventoryChanged   quotaActivationReason = "inventory_changed"
	activationNoLongerEligible   quotaActivationReason = "no_longer_eligible"
	activationPreviewNotEligible quotaActivationReason = "preview_not_eligible"
)

type quotaWindowPresence string

const (
	quotaWindowUnknown quotaWindowPresence = "unknown"
	quotaWindowPresent quotaWindowPresence = "present"
	quotaWindowAbsent  quotaWindowPresence = "absent"
)

func parseQuotaWindowPresence(value string) quotaWindowPresence {
	switch quotaWindowPresence(strings.ToLower(strings.TrimSpace(value))) {
	case quotaWindowPresent:
		return quotaWindowPresent
	case quotaWindowAbsent:
		return quotaWindowAbsent
	default:
		return quotaWindowUnknown
	}
}

type quotaActivationWindow struct {
	Presence           quotaWindowPresence `json:"presence"`
	UsedPercent        *float64            `json:"used_percent,omitempty"`
	ResetAt            *int64              `json:"reset_at,omitempty"`
	UsedTokens         *int64              `json:"used_tokens,omitempty"`
	RemainingTokens    *int64              `json:"remaining_tokens,omitempty"`
	LimitTokens        *int64              `json:"limit_tokens,omitempty"`
	LimitWindowSeconds *int64              `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  *int64              `json:"reset_after_seconds,omitempty"`
}

type quotaActivationQuota struct {
	ObservedAt int64                 `json:"observed_at"`
	Primary    quotaActivationWindow `json:"primary"`
	Secondary  quotaActivationWindow `json:"secondary"`
}

type quotaActivationDecision struct {
	Eligible bool                  `json:"eligible"`
	Reason   quotaActivationReason `json:"reason"`
}

type quotaActivationJob struct {
	ID                     string                         `json:"id"`
	Type                   string                         `json:"type"`
	State                  string                         `json:"state"`
	Force                  bool                           `json:"force"`
	SourcePreviewID        string                         `json:"source_preview_id,omitempty"`
	InventoryRevision      string                         `json:"inventory_revision,omitempty"`
	CreatedAt              string                         `json:"created_at"`
	UpdatedAt              string                         `json:"updated_at"`
	ExpiresAt              string                         `json:"expires_at,omitempty"`
	TotalAccounts          int                            `json:"total_accounts"`
	CompletedAccounts      int                            `json:"completed_accounts"`
	EligibleAccounts       int                            `json:"eligible_accounts"`
	SkippedAccounts        int                            `json:"skipped_accounts"`
	UnknownAccounts        int                            `json:"unknown_accounts"`
	FailedAccounts         int                            `json:"failed_accounts"`
	ConfirmationToken      string                         `json:"confirmation_token,omitempty"`
	ConfirmationConsumedAt string                         `json:"confirmation_consumed_at,omitempty"`
	Error                  string                         `json:"error,omitempty"`
	Accounts               []quotaActivationAccountResult `json:"accounts"`
}

type quotaActivationAccountResult struct {
	AccountKey string                `json:"account_key"`
	AuthID     string                `json:"-"`
	AuthIndex  string                `json:"auth_index"`
	Source     string                `json:"source,omitempty"`
	Provider   string                `json:"provider"`
	Email      string                `json:"email,omitempty"`
	Name       string                `json:"name,omitempty"`
	AuthFile   string                `json:"-"`
	PlanType   string                `json:"plan_type,omitempty"`
	Eligible   bool                  `json:"eligible"`
	Reason     quotaActivationReason `json:"reason"`
	Status     string                `json:"status"`
	HTTPStatus int                   `json:"http_status,omitempty"`
	CycleKey   string                `json:"cycle_key,omitempty"`
	Before     *quotaActivationQuota `json:"before,omitempty"`
	After      *quotaActivationQuota `json:"after,omitempty"`
	Error      string                `json:"error,omitempty"`
	StartedAt  string                `json:"started_at,omitempty"`
	FinishedAt string                `json:"finished_at,omitempty"`

	AuthFileMTime        int64  `json:"-"`
	InventoryFingerprint string `json:"-"`
}

type quotaActivationPreviewRequest struct {
	Force       bool     `json:"force"`
	AuthIndexes []string `json:"auth_indexes"`
}

type quotaActivationRunRequest struct {
	PreviewID         string   `json:"preview_id"`
	ConfirmationToken string   `json:"confirmation_token"`
	AuthIndexes       []string `json:"auth_indexes"`
}

type quotaActivationStartResponse struct {
	PreviewID string `json:"preview_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	State     string `json:"state"`
	PollURL   string `json:"poll_url"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type quotaActivationConfirmation struct {
	Token     string
	ExpiresAt int64
}

type quotaActivationManager struct {
	startMu       sync.Mutex
	mu            sync.Mutex
	cfg           pluginConfig
	cancel        context.CancelFunc
	confirmations map[string]quotaActivationConfirmation
	activeJobID   string
	recovered     bool
}

func (m *quotaActivationManager) configure(cfg pluginConfig) {
	m.mu.Lock()
	m.cfg = normalizePluginConfig(cfg)
	if m.confirmations == nil {
		m.confirmations = make(map[string]quotaActivationConfirmation)
	}
	needsRecovery := !m.recovered
	m.recovered = true
	m.mu.Unlock()
	if needsRecovery {
		db, _, err := globalStore.open(context.Background())
		if err == nil {
			err = recoverInterruptedQuotaActivationJobs(context.Background(), db)
		}
		if err != nil {
			m.mu.Lock()
			m.recovered = false
			m.mu.Unlock()
		}
	}
}

func (m *quotaActivationManager) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.confirmations = nil
	m.mu.Unlock()
}

func (m *quotaActivationManager) config() pluginConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	if cfg.QuotaTriggerMaxConcurrency == 0 {
		cfg = defaultPluginConfig()
	}
	return normalizePluginConfig(cfg)
}

func (m *quotaActivationManager) rememberConfirmation(previewID, token string, expiresAt int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.confirmations == nil {
		m.confirmations = make(map[string]quotaActivationConfirmation)
	}
	now := time.Now().Unix()
	for id, confirmation := range m.confirmations {
		if confirmation.ExpiresAt <= now {
			delete(m.confirmations, id)
		}
	}
	if expiresAt > now {
		m.confirmations[previewID] = quotaActivationConfirmation{Token: token, ExpiresAt: expiresAt}
	}
}

func (m *quotaActivationManager) confirmation(previewID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	confirmation, ok := m.confirmations[previewID]
	if !ok {
		return ""
	}
	if confirmation.ExpiresAt <= time.Now().Unix() {
		delete(m.confirmations, previewID)
		return ""
	}
	return confirmation.Token
}

func (m *quotaActivationManager) forgetConfirmation(previewID string) {
	m.mu.Lock()
	delete(m.confirmations, previewID)
	m.mu.Unlock()
}

func (m *quotaActivationManager) jobContext(jobID string) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.activeJobID = jobID
	return ctx
}

func (m *quotaActivationManager) finishJob(jobID string) {
	m.mu.Lock()
	if m.activeJobID == jobID {
		m.cancel = nil
		m.activeJobID = ""
	}
	m.mu.Unlock()
}

func tryAcquireQuotaProbeGate() bool {
	select {
	case <-quotaProbeGate:
		return true
	default:
		return false
	}
}

func releaseQuotaProbeGate() {
	select {
	case quotaProbeGate <- struct{}{}:
	default:
		panic("quota probe gate released without ownership")
	}
}

func evaluateQuotaActivation(account configuredAccount, credentialUsable bool, quota *quotaActivationQuota, quotaReadOK, force bool) quotaActivationDecision {
	if !isCodexAuthProvider(account.Provider) {
		return quotaActivationDecision{Reason: activationWrongProvider}
	}
	if account.Disabled || strings.EqualFold(strings.TrimSpace(account.RuntimeStatus), "disabled") {
		return quotaActivationDecision{Reason: activationDisabled}
	}
	if account.Expired || strings.EqualFold(strings.TrimSpace(account.RuntimeStatus), "expired") {
		return quotaActivationDecision{Reason: activationExpired}
	}
	if account.RuntimeUnavailable {
		return quotaActivationDecision{Reason: activationUnavailable}
	}
	if activationAccountKey(account) == "" || strings.TrimSpace(account.AuthIndex) == "" {
		return quotaActivationDecision{Reason: activationUnstableIdentity}
	}
	if !credentialUsable {
		return quotaActivationDecision{Reason: activationMissingCredential}
	}
	if !quotaReadOK || quota == nil {
		return quotaActivationDecision{Reason: activationQuotaReadFailed}
	}
	observedAt := quota.ObservedAt
	if observedAt <= 0 {
		observedAt = time.Now().Unix()
	}
	primaryReported, primary := evaluateFreshActivationWindow(quota.Primary, observedAt)
	secondaryReported, secondary := evaluateFreshActivationWindow(quota.Secondary, observedAt)
	if primary == activationUnknownQuota || secondary == activationUnknownQuota || (!primaryReported && !secondaryReported) {
		return quotaActivationDecision{Reason: activationUnknownQuota}
	}
	if force {
		return quotaActivationDecision{Eligible: true, Reason: activationEligible}
	}
	if primaryReported && primary != activationEligible {
		return quotaActivationDecision{Reason: activationPrimaryNotFresh}
	}
	if secondaryReported && secondary != activationEligible {
		return quotaActivationDecision{Reason: activationSecondaryNotFresh}
	}
	return quotaActivationDecision{Eligible: true, Reason: activationEligible}
}

func evaluateFreshActivationWindow(window quotaActivationWindow, observedAt int64) (bool, quotaActivationReason) {
	switch parseQuotaWindowPresence(string(window.Presence)) {
	case quotaWindowAbsent:
		return false, activationEligible
	case quotaWindowPresent:
		// Continue with the explicitly reported window.
	default:
		return false, activationUnknownQuota
	}
	if window.UsedPercent == nil || window.ResetAt == nil || normalizeUnixSeconds(*window.ResetAt) <= observedAt {
		return true, activationUnknownQuota
	}
	if math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) || *window.UsedPercent < 0 || *window.UsedPercent > 100 {
		return true, activationUnknownQuota
	}
	if window.UsedTokens != nil && *window.UsedTokens < 0 {
		return true, activationUnknownQuota
	}
	if (window.RemainingTokens == nil) != (window.LimitTokens == nil) {
		return true, activationUnknownQuota
	}
	if window.RemainingTokens != nil && (*window.RemainingTokens < 0 || *window.LimitTokens <= 0 || *window.RemainingTokens > *window.LimitTokens) {
		return true, activationUnknownQuota
	}
	if window.UsedTokens != nil && window.LimitTokens != nil && (*window.UsedTokens > *window.LimitTokens || *window.UsedTokens != *window.LimitTokens-*window.RemainingTokens) {
		return true, activationUnknownQuota
	}
	if (window.LimitWindowSeconds == nil) != (window.ResetAfterSeconds == nil) {
		return true, activationUnknownQuota
	}
	activeCountdown := false
	if window.LimitWindowSeconds != nil {
		if *window.LimitWindowSeconds <= 0 || *window.ResetAfterSeconds < 0 || *window.ResetAfterSeconds > *window.LimitWindowSeconds {
			return true, activationUnknownQuota
		}
		expectedReset := observedAt + *window.ResetAfterSeconds
		if absInt64(normalizeUnixSeconds(*window.ResetAt)-expectedReset) > 30 {
			return true, activationUnknownQuota
		}
		activeCountdown = *window.ResetAfterSeconds < *window.LimitWindowSeconds
	}
	if *window.UsedPercent > 0 || (window.UsedTokens != nil && *window.UsedTokens > 0) || activeCountdown {
		return true, activationPrimaryNotFresh
	}
	if window.RemainingTokens != nil && *window.RemainingTokens != *window.LimitTokens {
		return true, activationUnknownQuota
	}
	return true, activationEligible
}

func activationAccountKey(account configuredAccount) string {
	authIndex := strings.TrimSpace(account.AuthIndex)
	if authIndex == "" {
		return ""
	}
	identity := strings.Join([]string{"codex", authIndex, strings.TrimSpace(account.AuthID)}, "\x1f")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func activationInventoryFingerprint(account configuredAccount) string {
	value := strings.Join([]string{
		activationAccountKey(account), account.AuthID, account.AuthIndex, account.AuthFile, account.Source, account.Email, account.Name,
		strconv.FormatInt(account.AuthFileMTime, 10),
		strconv.FormatBool(account.Disabled), strconv.FormatBool(account.Expired),
		strings.TrimSpace(account.RuntimeStatus), strconv.FormatBool(account.RuntimeUnavailable),
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func activationCycleKey(account configuredAccount, quota quotaActivationQuota) string {
	parts := []string{
		activationAccountKey(account),
		activationWindowCycleMarker(quota.Primary),
		activationWindowCycleMarker(quota.Secondary),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func activationCycleKeyForReservation(ctx context.Context, db *sql.DB, account configuredAccount, quota quotaActivationQuota) (string, error) {
	baseKey := activationCycleKey(account, quota)
	observedAt := quota.ObservedAt
	if observedAt <= 0 {
		observedAt = time.Now().Unix()
	}
	var predecessorKey string
	var boundary int64
	err := db.QueryRowContext(ctx, `
SELECT cycle_key,next_cycle_after
FROM quota_activation_cycles
WHERE account_key=? AND status IN ('dispatch_intent','verified','partial','sent_unknown')
ORDER BY rowid DESC
LIMIT 1`, activationAccountKey(account)).Scan(&predecessorKey, &boundary)
	if errors.Is(err, sql.ErrNoRows) {
		return baseKey, nil
	}
	if err != nil {
		return "", err
	}
	// A prior ambiguous or still-active dispatch guards the whole account, not
	// merely one payload shape. This prevents a changed presence/duration shape
	// or force mode from bypassing sent-unknown idempotency.
	if boundary <= 0 || observedAt <= boundary {
		return predecessorKey, nil
	}
	if !activationQuotaIsFreshFullDuration(quota) {
		return baseKey, nil
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{baseKey, predecessorKey, strconv.FormatInt(boundary, 10)}, "\x00")))
	return hex.EncodeToString(sum[:]), nil
}

func activationQuotaIsFreshFullDuration(quota quotaActivationQuota) bool {
	reported := 0
	for _, window := range []quotaActivationWindow{quota.Primary, quota.Secondary} {
		switch parseQuotaWindowPresence(string(window.Presence)) {
		case quotaWindowAbsent:
			continue
		case quotaWindowPresent:
			reported++
			if !activationWindowIsFreshFullDuration(window) {
				return false
			}
		default:
			return false
		}
	}
	return reported > 0
}

func activationCycleBoundary(before, after quotaActivationQuota) int64 {
	if after.ObservedAt <= 0 {
		return 0
	}
	boundary := int64(0)
	targets := 0
	for _, pair := range [][2]quotaActivationWindow{{before.Primary, after.Primary}, {before.Secondary, after.Secondary}} {
		if parseQuotaWindowPresence(string(pair[0].Presence)) != quotaWindowPresent {
			continue
		}
		targets++
		if !activationWindowMetadataValid(pair[1], after.ObservedAt) || pair[0].LimitWindowSeconds == nil || pair[1].LimitWindowSeconds == nil || *pair[0].LimitWindowSeconds != *pair[1].LimitWindowSeconds || pair[1].ResetAt == nil {
			return 0
		}
		resetAt := normalizeUnixSeconds(*pair[1].ResetAt)
		if resetAt > boundary {
			boundary = resetAt
		}
	}
	if targets == 0 {
		return 0
	}
	return boundary
}

func activationWindowCycleMarker(window quotaActivationWindow) string {
	switch parseQuotaWindowPresence(string(window.Presence)) {
	case quotaWindowAbsent:
		return "absent"
	case quotaWindowPresent:
		// A fresh unused window reports a floating reset_at. Its duration and
		// explicit presence are the stable cycle identity until usage anchors it.
		if activationWindowIsFreshFullDuration(window) {
			return "present:fresh:duration:" + strconv.FormatInt(*window.LimitWindowSeconds, 10)
		}
		parts := []string{"present:active"}
		if window.LimitWindowSeconds != nil {
			parts = append(parts, "duration:"+strconv.FormatInt(*window.LimitWindowSeconds, 10))
		}
		if window.ResetAt != nil {
			parts = append(parts, "reset:"+strconv.FormatInt(normalizeUnixSeconds(*window.ResetAt), 10))
		}
		return strings.Join(parts, ":")
	default:
		return "unknown"
	}
}

func activationWindowIsFreshFullDuration(window quotaActivationWindow) bool {
	if parseQuotaWindowPresence(string(window.Presence)) != quotaWindowPresent || window.LimitWindowSeconds == nil || window.ResetAfterSeconds == nil {
		return false
	}
	if *window.LimitWindowSeconds <= 0 || *window.ResetAfterSeconds != *window.LimitWindowSeconds {
		return false
	}
	if window.UsedPercent == nil || *window.UsedPercent != 0 {
		return false
	}
	return window.UsedTokens == nil || *window.UsedTokens == 0
}

func quotaActivationQuotaFromRun(run quotaTriggerRun) quotaActivationQuota {
	return quotaActivationQuota{
		ObservedAt: time.Now().Unix(),
		Primary: quotaActivationWindow{
			Presence: run.PrimaryWindowPresence, UsedPercent: run.PrimaryUsedPercent, ResetAt: run.PrimaryResetAt,
			UsedTokens: run.PrimaryUsedTokens, RemainingTokens: run.PrimaryRemaining, LimitTokens: run.PrimaryLimit,
			LimitWindowSeconds: run.PrimaryLimitWindowSeconds, ResetAfterSeconds: run.PrimaryResetAfterSeconds,
		},
		Secondary: quotaActivationWindow{
			Presence: run.SecondaryWindowPresence, UsedPercent: run.SecondaryUsedPercent, ResetAt: run.SecondaryResetAt,
			UsedTokens: run.SecondaryUsedTokens, RemainingTokens: run.SecondaryRemaining, LimitTokens: run.SecondaryLimit,
			LimitWindowSeconds: run.SecondaryLimitWindowSeconds, ResetAfterSeconds: run.SecondaryResetAfterSeconds,
		},
	}
}

func activationInventory() ([]configuredAccount, string, error) {
	globalCodexAuthSource.invalidate()
	inventory, err := readCodexHostAuthInventory()
	if err == nil {
		return inventory, configuredAccountListRevision(inventory), nil
	}
	files := configuredCodexFileInventory(readConfiguredAuthFiles())
	if len(files) == 0 {
		return nil, "", fmt.Errorf("read Codex auth inventory: %w", err)
	}
	return files, configuredAccountListRevision(files), nil
}

func stableActivationAuthIndexes(inventory []configuredAccount) map[string]struct{} {
	counts := make(map[string]int, len(inventory))
	for _, account := range inventory {
		key := normalizeAccountAlias(account.AuthIndex)
		if key != "" {
			counts[key]++
		}
	}
	stable := make(map[string]struct{}, len(counts))
	for key, count := range counts {
		if count == 1 {
			stable[key] = struct{}{}
		}
	}
	return stable
}

func resolveActivationCredential(account configuredAccount) (triggerAuthAccount, bool) {
	probe := invalidAuthRow{
		AuthID: account.AuthID, AuthIndex: account.AuthIndex, AuthFile: account.AuthFile,
		AuthSourceKind: account.AuthSourceKind,
	}
	files := readTriggerAuthAccounts()
	fileInventory := make([]configuredAccount, 0, len(files))
	for _, item := range files {
		fileInventory = append(fileInventory, item.configuredAccount)
	}
	if matched, ok := matchCodexHostAuthInventoryExact(probe, fileInventory); ok {
		for _, item := range files {
			if activationAccountKey(item.configuredAccount) == activationAccountKey(matched) && strings.TrimSpace(item.AccessToken) != "" {
				item.configuredAccount = mergeActivationAccountMetadata(account, item.configuredAccount)
				item.AccessToken = strings.TrimSpace(item.AccessToken)
				return item, true
			}
		}
	}
	if normalizeAuthSourceKind(account.AuthSourceKind) == authSourceKindFile && strings.TrimSpace(account.AuthFile) != "" {
		matches := make([]triggerAuthAccount, 0, 1)
		for _, item := range files {
			if normalizeAccountAlias(item.AuthFile) == normalizeAccountAlias(account.AuthFile) && strings.TrimSpace(item.AccessToken) != "" {
				matches = append(matches, item)
			}
		}
		if len(matches) == 1 {
			matched := matches[0]
			matched.configuredAccount = mergeActivationAccountMetadata(account, matched.configuredAccount)
			matched.AccessToken = strings.TrimSpace(matched.AccessToken)
			return matched, true
		}
	}
	if strings.TrimSpace(account.AuthIndex) == "" {
		return triggerAuthAccount{}, false
	}
	raw, err := hostAuthCaller("host.auth.get", hostAuthGetRequest{AuthIndex: account.AuthIndex})
	if err != nil {
		return triggerAuthAccount{}, false
	}
	var response hostAuthGetResponse
	if json.Unmarshal(raw, &response) != nil || len(response.JSON) == 0 {
		return triggerAuthAccount{}, false
	}
	var doc map[string]any
	if json.Unmarshal(response.JSON, &doc) != nil {
		return triggerAuthAccount{}, false
	}
	accessToken := strings.TrimSpace(firstNonEmptyString(stringFromAny(doc["access_token"]), stringFromAny(doc["accessToken"]), stringFromAny(doc["token"])))
	if accessToken == "" {
		return triggerAuthAccount{}, false
	}
	return triggerAuthAccount{
		configuredAccount: account,
		AccessToken:       accessToken,
		ChatGPTAccountID:  strings.TrimSpace(firstNonEmptyString(stringFromAny(doc["chatgpt_account_id"]), stringFromAny(doc["chatgptAccountId"]), stringFromAny(doc["account_id"]), stringFromAny(doc["accountId"]))),
	}, true
}

func mergeActivationAccountMetadata(authoritative, credential configuredAccount) configuredAccount {
	credential.AuthIndex = authoritative.AuthIndex
	credential.AuthID = authoritative.AuthID
	credential.Source = authoritative.Source
	credential.Provider = authoritative.Provider
	credential.Email = authoritative.Email
	credential.Name = authoritative.Name
	credential.Disabled = authoritative.Disabled
	credential.Expired = authoritative.Expired
	credential.PlanType = firstNonEmptyString(authoritative.PlanType, credential.PlanType)
	credential.RuntimeStatus = authoritative.RuntimeStatus
	credential.RuntimeMessage = authoritative.RuntimeMessage
	credential.RuntimeUnavailable = authoritative.RuntimeUnavailable
	if authoritative.AuthFile != "" {
		credential.AuthFile = authoritative.AuthFile
	}
	if authoritative.AuthFileMTime != 0 {
		credential.AuthFileMTime = authoritative.AuthFileMTime
	}
	return credential
}

func readActivationQuota(ctx context.Context, account triggerAuthAccount, cfg pluginConfig) (quotaActivationQuota, quotaTriggerRun, error) {
	run := executeQuotaUsageRequest(ctx, nil, account, cfg)
	if run.Status != "success" || run.HTTPStatus < 200 || run.HTTPStatus >= 300 {
		message := run.Error
		if message == "" {
			message = "quota request failed"
		}
		return quotaActivationQuota{}, run, errors.New(sanitizeTriggerError(message))
	}
	return quotaActivationQuotaFromRun(run), run, nil
}

func (m *quotaActivationManager) startPreview(ctx context.Context, req quotaActivationPreviewRequest) (quotaActivationStartResponse, int, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	authIndexes, err := normalizeActivationAuthIndexes(req.AuthIndexes)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusBadRequest, err
	}
	if req.Force && len(authIndexes) == 0 {
		return quotaActivationStartResponse{}, http.StatusBadRequest, errors.New("force preview requires explicit auth_indexes")
	}
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	if active, err := activeActivationJob(ctx, db); err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	} else if active != "" {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("another quota activation job is active")
	}
	jobID, err := randomOpaqueToken(18)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	now := time.Now()
	_, err = db.ExecContext(ctx, `
INSERT INTO quota_activation_jobs (job_id,job_type,state,force,created_at,updated_at)
VALUES (?, 'preview', 'queued', ?, ?, ?)`, jobID, boolInt(req.Force), now.Unix(), now.Unix())
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	go m.runPreview(m.jobContext(jobID), jobID, req.Force, authIndexes)
	return quotaActivationStartResponse{PreviewID: jobID, State: "queued", PollURL: activationPreviewPollURL(jobID)}, http.StatusAccepted, nil
}

func (m *quotaActivationManager) runPreview(ctx context.Context, jobID string, force bool, selected map[string]struct{}) {
	defer m.finishJob(jobID)
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='running',updated_at=? WHERE job_id=? AND state='queued'`, now, jobID); err != nil {
		finishActivationJobError(context.Background(), db, jobID, err)
		return
	}
	inventory, revision, err := activationInventory()
	if err != nil {
		finishActivationJobError(ctx, db, jobID, err)
		return
	}
	if len(inventory) > quotaActivationMaxItems {
		finishActivationJobError(ctx, db, jobID, errors.New("auth inventory exceeds 2000 records"))
		return
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET inventory_revision=?,total_accounts=?,updated_at=? WHERE job_id=?`, revision, len(inventory), time.Now().Unix(), jobID); err != nil {
		finishActivationJobError(ctx, db, jobID, err)
		return
	}
	cfg := m.config()
	stableIndexes := stableActivationAuthIndexes(inventory)
	sem := make(chan struct{}, cfg.QuotaTriggerMaxConcurrency)
	var wg sync.WaitGroup
	var jobErr error
	var jobErrOnce sync.Once
	for _, account := range inventory {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			_, identityStable := stableIndexes[normalizeAccountAlias(account.AuthIndex)]
			row := previewActivationAccount(ctx, account, identityStable, force, selected, cfg)
			if err := insertActivationAccount(ctx, db, jobID, row); err != nil {
				jobErrOnce.Do(func() { jobErr = err })
				return
			}
			if _, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET completed_accounts=completed_accounts+1,updated_at=? WHERE job_id=?`, time.Now().Unix(), jobID); err != nil {
				jobErrOnce.Do(func() { jobErr = err })
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		finishActivationJobError(context.Background(), db, jobID, ctx.Err())
		return
	}
	if jobErr != nil {
		finishActivationJobError(ctx, db, jobID, jobErr)
		return
	}
	token, err := randomOpaqueToken(24)
	if err != nil {
		finishActivationJobError(ctx, db, jobID, err)
		return
	}
	digest := activationTokenDigest(token)
	completedAt := time.Now()
	expiresAt := completedAt.Add(quotaActivationPreviewTTL).Unix()
	m.rememberConfirmation(jobID, token, expiresAt)
	result, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='completed',confirmation_digest=?,expires_at=?,updated_at=? WHERE job_id=? AND state='running'`, digest, expiresAt, completedAt.Unix(), jobID)
	if err != nil {
		m.forgetConfirmation(jobID)
		finishActivationJobError(ctx, db, jobID, err)
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		m.forgetConfirmation(jobID)
	}
}

func previewActivationAccount(ctx context.Context, account configuredAccount, identityStable, force bool, selected map[string]struct{}, cfg pluginConfig) quotaActivationAccountResult {
	row := activationAccountResultFromConfigured(account)
	row.Status = "checking"
	if !identityStable {
		row.Reason = activationUnstableIdentity
		row.Status = "skipped"
		return row
	}
	if len(selected) > 0 {
		if _, ok := selected[account.AuthIndex]; !ok {
			row.Reason = activationNotSelected
			row.Status = "skipped"
			return row
		}
	}
	credential, credentialOK := resolveActivationCredential(account)
	preDecision := evaluateQuotaActivation(account, credentialOK, &quotaActivationQuota{
		Primary:   quotaActivationWindow{Presence: quotaWindowPresent, UsedPercent: float64Pointer(0), ResetAt: int64Pointer(time.Now().Add(time.Hour).Unix())},
		Secondary: quotaActivationWindow{Presence: quotaWindowAbsent},
	}, true, force)
	if !preDecision.Eligible {
		row.Reason = preDecision.Reason
		row.Status = "skipped"
		return row
	}
	quota, _, quotaErr := readActivationQuota(ctx, credential, cfg)
	if quotaErr == nil {
		row.Before = &quota
	}
	decision := evaluateQuotaActivation(account, credentialOK, row.Before, quotaErr == nil, force)
	row.Eligible = decision.Eligible
	row.Reason = decision.Reason
	if decision.Eligible {
		row.Status = "eligible"
	} else if decision.Reason == activationQuotaReadFailed {
		row.Status = "failed"
		row.Error = sanitizeTriggerError(quotaErr)
	} else {
		row.Status = "skipped"
	}
	return row
}

func (m *quotaActivationManager) startRun(ctx context.Context, req quotaActivationRunRequest) (quotaActivationStartResponse, int, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	req.PreviewID = strings.TrimSpace(req.PreviewID)
	req.ConfirmationToken = strings.TrimSpace(req.ConfirmationToken)
	if req.PreviewID == "" || req.ConfirmationToken == "" {
		return quotaActivationStartResponse{}, http.StatusBadRequest, errors.New("preview_id and confirmation_token are required")
	}
	selected, err := normalizeActivationAuthIndexes(req.AuthIndexes)
	if err != nil || len(selected) == 0 {
		if err == nil {
			err = errors.New("auth_indexes must contain at least one entry")
		}
		return quotaActivationStartResponse{}, http.StatusBadRequest, err
	}
	if !tryAcquireQuotaProbeGate() {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("periodic or one-shot quota probe is already running")
	}
	releaseGate := true
	defer func() {
		if releaseGate {
			releaseQuotaProbeGate()
		}
	}()
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	if active, activeErr := activeActivationJob(ctx, db); activeErr != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, activeErr
	} else if active != "" {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("another quota activation job is active")
	}
	runID, err := randomOpaqueToken(18)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	now := time.Now().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	var state, digest, revision string
	var forceInt, expiresAt, consumedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT state,confirmation_digest,inventory_revision,force,expires_at,confirmation_consumed_at FROM quota_activation_jobs WHERE job_id=? AND job_type='preview'`, req.PreviewID).
		Scan(&state, &digest, &revision, &forceInt, &expiresAt, &consumedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return quotaActivationStartResponse{}, http.StatusNotFound, errors.New("preview not found")
		}
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	if state != "completed" || expiresAt <= now {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("preview is incomplete or expired")
	}
	if consumedAt != 0 {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("preview confirmation was already consumed")
	}
	if !activationTokenMatches(req.ConfirmationToken, digest) {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("confirmation token is invalid")
	}
	rows, err := tx.QueryContext(ctx, `SELECT auth_index,eligible FROM quota_activation_job_accounts WHERE job_id=?`, req.PreviewID)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	eligible := make(map[string]bool)
	for rows.Next() {
		var authIndex string
		var allowed int
		if err := rows.Scan(&authIndex, &allowed); err != nil {
			_ = rows.Close()
			return quotaActivationStartResponse{}, http.StatusInternalServerError, err
		}
		eligible[authIndex] = allowed != 0
	}
	if err := rows.Close(); err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	for authIndex := range selected {
		if !eligible[authIndex] {
			return quotaActivationStartResponse{}, http.StatusBadRequest, errors.New("selected auth_index is not preview-eligible")
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE quota_activation_jobs SET confirmation_consumed_at=?,updated_at=? WHERE job_id=? AND confirmation_consumed_at=0`, now, now, req.PreviewID)
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return quotaActivationStartResponse{}, http.StatusConflict, errors.New("preview confirmation was already consumed")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO quota_activation_jobs (job_id,job_type,state,force,source_preview_id,inventory_revision,created_at,updated_at,total_accounts)
VALUES (?, 'run', 'queued', ?, ?, ?, ?, ?, ?)`, runID, forceInt, req.PreviewID, revision, now, now, len(selected))
	if err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	for authIndex := range selected {
		_, err = tx.ExecContext(ctx, `
INSERT INTO quota_activation_job_accounts (
 job_id,account_key,auth_id,auth_index,source,provider,email,name,auth_file,auth_file_mtime,plan_type,
 inventory_fingerprint,eligible,reason,status,before_quota_json
)
SELECT ?,account_key,auth_id,auth_index,source,provider,email,name,auth_file,auth_file_mtime,plan_type,
 inventory_fingerprint,eligible,reason,'queued',before_quota_json
FROM quota_activation_job_accounts WHERE job_id=? AND auth_index=? AND eligible=1`, runID, req.PreviewID, authIndex)
		if err != nil {
			return quotaActivationStartResponse{}, http.StatusInternalServerError, err
		}
	}
	if err := tx.Commit(); err != nil {
		return quotaActivationStartResponse{}, http.StatusInternalServerError, err
	}
	rollback = false
	m.forgetConfirmation(req.PreviewID)
	releaseGate = false
	go m.runActivation(m.jobContext(runID), runID, forceInt != 0)
	return quotaActivationStartResponse{RunID: runID, State: "queued", PollURL: activationRunPollURL(runID)}, http.StatusAccepted, nil
}

func (m *quotaActivationManager) runActivation(ctx context.Context, runID string, force bool) {
	defer releaseQuotaProbeGate()
	defer m.finishJob(runID)
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='running',updated_at=? WHERE job_id=? AND state='queued'`, time.Now().Unix(), runID); err != nil {
		finishActivationJobError(context.Background(), db, runID, err)
		return
	}
	accounts, err := loadActivationAccounts(ctx, db, runID)
	if err != nil {
		finishActivationJobError(ctx, db, runID, err)
		return
	}
	inventory, _, err := activationInventory()
	if err != nil {
		finishActivationJobError(ctx, db, runID, err)
		return
	}
	cfg := m.config()
	sem := make(chan struct{}, cfg.QuotaTriggerMaxConcurrency)
	var wg sync.WaitGroup
	for _, row := range accounts {
		row := row
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			executeQuotaActivationAccount(ctx, db, runID, row, inventory, force, cfg)
			_, _ = db.ExecContext(context.Background(), `UPDATE quota_activation_jobs SET completed_accounts=completed_accounts+1,updated_at=? WHERE job_id=?`, time.Now().Unix(), runID)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		markUnfinishedActivationAccountsUnknown(context.Background(), db, runID, ctx.Err())
	}
	_, _ = db.ExecContext(context.Background(), `UPDATE quota_activation_jobs SET state='completed',updated_at=? WHERE job_id=? AND state='running'`, time.Now().Unix(), runID)
}

func executeQuotaActivationAccount(ctx context.Context, db *sql.DB, runID string, row quotaActivationAccountResult, inventory []configuredAccount, force bool, cfg pluginConfig) {
	started := time.Now().Unix()
	if err := updateActivationAccountState(ctx, db, runID, row.AccountKey, "revalidating", "", 0, "", nil, started, 0); err != nil {
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, "failed_before_send", activationEligible, 0, err, nil, "")
		return
	}
	current, ok := findExactActivationAccount(row, inventory)
	if !ok || activationInventoryFingerprint(current) != row.InventoryFingerprint {
		finishActivationAccount(ctx, db, runID, row.AccountKey, "skipped", activationInventoryChanged, 0, nil, nil, "")
		return
	}
	credential, credentialOK := resolveActivationCredential(current)
	if !credentialOK {
		finishActivationAccount(ctx, db, runID, row.AccountKey, "skipped", activationMissingCredential, 0, nil, nil, "")
		return
	}
	before, _, quotaErr := readActivationQuota(ctx, credential, cfg)
	decision := evaluateQuotaActivation(current, true, quotaPointer(before, quotaErr), quotaErr == nil, force)
	if !decision.Eligible {
		reason := decision.Reason
		if reason == activationPrimaryNotFresh || reason == activationSecondaryNotFresh || reason == activationUnknownQuota {
			reason = activationNoLongerEligible
		}
		finishActivationAccount(ctx, db, runID, row.AccountKey, "skipped", reason, 0, quotaErr, quotaPointer(before, quotaErr), "")
		return
	}
	cycleKey, cycleErr := activationCycleKeyForReservation(ctx, db, current, before)
	if cycleErr != nil {
		finishActivationAccount(ctx, db, runID, row.AccountKey, "failed_before_send", activationEligible, 0, cycleErr, &before, "")
		return
	}
	reserved, reserveErr := reserveActivationCycle(ctx, db, row.AccountKey, cycleKey, runID)
	if reserveErr != nil {
		finishActivationAccount(ctx, db, runID, row.AccountKey, "failed_before_send", activationEligible, 0, reserveErr, &before, cycleKey)
		return
	}
	if !reserved {
		finishActivationAccount(ctx, db, runID, row.AccountKey, "skipped", activationDuplicateCycle, 0, nil, &before, cycleKey)
		return
	}
	if err := updateActivationAccountState(ctx, db, runID, row.AccountKey, "reserved", activationEligible, 0, cycleKey, &before, started, 0); err != nil {
		_ = updateActivationCycle(context.Background(), db, row.AccountKey, cycleKey, "failed_before_send")
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, "failed_before_send", activationEligible, 0, err, &before, cycleKey)
		return
	}
	probe := executeQuotaProbeRequest(ctx, db, credential, cfg)
	_ = recordQuotaTriggerRun(context.Background(), db, probe)
	_ = applyQuotaTriggerAccountState(context.Background(), db, probe)
	if probe.Status != "success" || probe.HTTPStatus < 200 || probe.HTTPStatus >= 300 {
		status := quotaProbeFailureActivationStatus(probe.HTTPStatus)
		_ = updateActivationCycle(context.Background(), db, row.AccountKey, cycleKey, status)
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, status, activationEligible, probe.HTTPStatus, errors.New(probe.Error), &before, cycleKey)
		return
	}
	if err := updateActivationAccountState(context.Background(), db, runID, row.AccountKey, "dispatched", activationEligible, probe.HTTPStatus, cycleKey, &before, started, 0); err != nil {
		_ = updateActivationCycle(context.Background(), db, row.AccountKey, cycleKey, "sent_unknown")
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, "sent_unknown", activationEligible, probe.HTTPStatus, err, &before, cycleKey)
		return
	}
	if !waitActivationGrace(ctx, quotaActivationPropagationGrace) {
		_ = updateActivationCycle(context.Background(), db, row.AccountKey, cycleKey, "sent_unknown")
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, "sent_unknown", activationEligible, probe.HTTPStatus, ctx.Err(), &before, cycleKey)
		return
	}
	after, _, afterErr := readActivationQuota(ctx, credential, cfg)
	if afterErr != nil {
		_ = updateActivationCycle(context.Background(), db, row.AccountKey, cycleKey, "sent_unknown")
		finishActivationAccount(context.Background(), db, runID, row.AccountKey, "sent_unknown", activationEligible, probe.HTTPStatus, afterErr, &before, cycleKey)
		return
	}
	status := classifyActivationVerification(before, after)
	_ = updateActivationCycleOutcome(context.Background(), db, row.AccountKey, cycleKey, status, before, after)
	finishActivationAccountWithAfter(context.Background(), db, runID, row.AccountKey, status, activationEligible, probe.HTTPStatus, nil, &before, &after, cycleKey)
}

func quotaPointer(quota quotaActivationQuota, err error) *quotaActivationQuota {
	if err != nil {
		return nil
	}
	return &quota
}

func findExactActivationAccount(row quotaActivationAccountResult, inventory []configuredAccount) (configuredAccount, bool) {
	probe := invalidAuthRow{AuthID: row.AuthID, AuthIndex: row.AuthIndex, AuthFile: row.AuthFile}
	account, ok := matchCodexHostAuthInventoryExact(probe, inventory)
	if !ok || activationAccountKey(account) != row.AccountKey {
		return configuredAccount{}, false
	}
	return account, true
}

func classifyActivationVerification(before, after quotaActivationQuota) string {
	observedAt := after.ObservedAt
	if observedAt <= 0 {
		observedAt = time.Now().Unix()
	}
	targets := 0
	verified := 0
	for _, pair := range [][2]quotaActivationWindow{{before.Primary, after.Primary}, {before.Secondary, after.Secondary}} {
		if parseQuotaWindowPresence(string(pair[0].Presence)) != quotaWindowPresent {
			continue
		}
		targets++
		if activationWindowHasEvidence(pair[0], pair[1], observedAt) {
			verified++
		}
	}
	if targets > 0 && verified == targets {
		return "verified"
	}
	if verified > 0 {
		return "partial"
	}
	return "sent_unknown"
}

func activationWindowHasEvidence(before, after quotaActivationWindow, observedAt int64) bool {
	if !activationWindowMetadataValid(after, observedAt) {
		return false
	}
	if after.UsedPercent != nil && *after.UsedPercent > 0 && *after.UsedPercent > pointerFloat64Value(before.UsedPercent) {
		return true
	}
	if after.UsedTokens != nil && *after.UsedTokens > 0 && *after.UsedTokens > pointerInt64Value(before.UsedTokens) {
		return true
	}
	if !activationWindowIsFreshFullDuration(before) || after.LimitWindowSeconds == nil || after.ResetAfterSeconds == nil || after.ResetAt == nil {
		return false
	}
	if *after.LimitWindowSeconds != *before.LimitWindowSeconds || *after.ResetAfterSeconds >= *after.LimitWindowSeconds {
		return false
	}
	return true
}

func activationWindowMetadataValid(window quotaActivationWindow, observedAt int64) bool {
	if parseQuotaWindowPresence(string(window.Presence)) != quotaWindowPresent {
		return false
	}
	if window.UsedPercent != nil && (math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) || *window.UsedPercent < 0 || *window.UsedPercent > 100) {
		return false
	}
	if window.UsedTokens != nil && *window.UsedTokens < 0 {
		return false
	}
	if (window.RemainingTokens == nil) != (window.LimitTokens == nil) {
		return false
	}
	if window.RemainingTokens != nil {
		if *window.RemainingTokens < 0 || *window.LimitTokens <= 0 || *window.RemainingTokens > *window.LimitTokens {
			return false
		}
		if window.UsedTokens != nil && (*window.UsedTokens > *window.LimitTokens || *window.UsedTokens != *window.LimitTokens-*window.RemainingTokens) {
			return false
		}
	}
	if (window.LimitWindowSeconds == nil) != (window.ResetAfterSeconds == nil) {
		return false
	}
	if window.LimitWindowSeconds != nil {
		if *window.LimitWindowSeconds <= 0 || *window.ResetAfterSeconds < 0 || *window.ResetAfterSeconds > *window.LimitWindowSeconds || window.ResetAt == nil {
			return false
		}
		resetAt := normalizeUnixSeconds(*window.ResetAt)
		if resetAt <= observedAt || absInt64(resetAt-(observedAt+*window.ResetAfterSeconds)) > 30 {
			return false
		}
	}
	return true
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func quotaProbeFailureActivationStatus(httpStatus int) string {
	switch httpStatus {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests:
		return "failed_before_send"
	default:
		// Unknown 4xx responses are not automatically safe to retry. A timeout,
		// conflict, or other unrecognized response may arrive after upstream has
		// accepted enough of the request to consume quota.
		return "sent_unknown"
	}
}

func pointerFloat64Value(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func pointerInt64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func waitActivationGrace(ctx context.Context, grace time.Duration) bool {
	if grace <= 0 {
		return true
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func reserveActivationCycle(ctx context.Context, db *sql.DB, accountKey, cycleKey, runID string) (bool, error) {
	now := time.Now().Unix()
	// Keep reservation as one write statement. A deferred transaction that reads
	// before writing cannot safely wait for a sibling writer in SQLite: upgrading
	// its read lock would deadlock, so SQLite returns BUSY immediately instead of
	// honoring busy_timeout. The atomic upsert lets concurrent account workers
	// wait briefly at the write boundary without serializing their network I/O.
	result, err := db.ExecContext(ctx, `
INSERT INTO quota_activation_cycles(account_key,cycle_key,run_id,status,reserved_at,updated_at)
VALUES (?, ?, ?, 'dispatch_intent', ?, ?)
ON CONFLICT(account_key,cycle_key) DO UPDATE SET
 run_id=excluded.run_id,
 status='dispatch_intent',
 reserved_at=excluded.reserved_at,
 updated_at=excluded.updated_at
WHERE quota_activation_cycles.status='failed_before_send'`, accountKey, cycleKey, runID, now, now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func updateActivationCycle(ctx context.Context, db *sql.DB, accountKey, cycleKey, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE quota_activation_cycles SET status=?,updated_at=? WHERE account_key=? AND cycle_key=?`, status, time.Now().Unix(), accountKey, cycleKey)
	return err
}

func updateActivationCycleOutcome(ctx context.Context, db *sql.DB, accountKey, cycleKey, status string, before, after quotaActivationQuota) error {
	boundary := activationCycleBoundary(before, after)
	_, err := db.ExecContext(ctx, `UPDATE quota_activation_cycles SET status=?,next_cycle_after=?,updated_at=? WHERE account_key=? AND cycle_key=?`, status, boundary, time.Now().Unix(), accountKey, cycleKey)
	return err
}

func activationAccountResultFromConfigured(account configuredAccount) quotaActivationAccountResult {
	accountKey := activationAccountKey(account)
	if accountKey == "" {
		accountKey = "unstable:" + activationInventoryFingerprint(account)
	}
	return quotaActivationAccountResult{
		AccountKey: accountKey, AuthID: account.AuthID, AuthIndex: account.AuthIndex,
		Source: account.Source, Provider: account.Provider, Email: account.Email,
		Name: account.Name, AuthFile: account.AuthFile, PlanType: account.PlanType,
		AuthFileMTime: account.AuthFileMTime, InventoryFingerprint: activationInventoryFingerprint(account),
		Reason: activationUnknownQuota,
	}
}

func insertActivationAccount(ctx context.Context, db *sql.DB, jobID string, row quotaActivationAccountResult) error {
	beforeJSON := marshalActivationQuota(row.Before)
	_, err := db.ExecContext(ctx, `
INSERT INTO quota_activation_job_accounts (
 job_id,account_key,auth_id,auth_index,source,provider,email,name,auth_file,auth_file_mtime,plan_type,
 inventory_fingerprint,eligible,reason,status,http_status,cycle_key,before_quota_json,after_quota_json,error,started_at,finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, row.AccountKey, row.AuthID, row.AuthIndex, row.Source, row.Provider, row.Email, row.Name,
		row.AuthFile, row.AuthFileMTime, row.PlanType, row.InventoryFingerprint, boolInt(row.Eligible), string(row.Reason), row.Status,
		row.HTTPStatus, row.CycleKey, beforeJSON, marshalActivationQuota(row.After), sanitizeTriggerError(row.Error), parseActivationTime(row.StartedAt), parseActivationTime(row.FinishedAt))
	return err
}

func loadActivationAccounts(ctx context.Context, db *sql.DB, jobID string) ([]quotaActivationAccountResult, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_key,auth_id,auth_index,source,provider,email,name,auth_file,auth_file_mtime,plan_type,
 inventory_fingerprint,eligible,reason,status,http_status,cycle_key,before_quota_json,after_quota_json,error,started_at,finished_at
FROM quota_activation_job_accounts WHERE job_id=? ORDER BY auth_index,account_key`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotaActivationAccountResult
	for rows.Next() {
		var row quotaActivationAccountResult
		var eligible int
		var reason, beforeJSON, afterJSON string
		var startedAt, finishedAt int64
		if err := rows.Scan(&row.AccountKey, &row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.Email, &row.Name,
			&row.AuthFile, &row.AuthFileMTime, &row.PlanType, &row.InventoryFingerprint, &eligible, &reason, &row.Status,
			&row.HTTPStatus, &row.CycleKey, &beforeJSON, &afterJSON, &row.Error, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		row.Eligible = eligible != 0
		row.Reason = quotaActivationReason(reason)
		row.Before = unmarshalActivationQuota(beforeJSON)
		row.After = unmarshalActivationQuota(afterJSON)
		row.StartedAt = unixTime(startedAt)
		row.FinishedAt = unixTime(finishedAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadActivationJob(ctx context.Context, db *sql.DB, jobType, jobID string) (quotaActivationJob, error) {
	var job quotaActivationJob
	var force int
	var created, updated, expires, consumed int64
	err := db.QueryRowContext(ctx, `
SELECT job_id,job_type,state,force,source_preview_id,inventory_revision,created_at,updated_at,expires_at,
 confirmation_consumed_at,total_accounts,completed_accounts,error
FROM quota_activation_jobs WHERE job_id=? AND job_type=?`, jobID, jobType).Scan(
		&job.ID, &job.Type, &job.State, &force, &job.SourcePreviewID, &job.InventoryRevision, &created, &updated, &expires,
		&consumed, &job.TotalAccounts, &job.CompletedAccounts, &job.Error)
	if err != nil {
		return quotaActivationJob{}, err
	}
	job.Force = force != 0
	job.CreatedAt = unixTime(created)
	job.UpdatedAt = unixTime(updated)
	job.ExpiresAt = unixTime(expires)
	job.ConfirmationConsumedAt = unixTime(consumed)
	job.Accounts, err = loadActivationAccounts(ctx, db, jobID)
	if err != nil {
		return quotaActivationJob{}, err
	}
	for _, account := range job.Accounts {
		if account.Eligible {
			job.EligibleAccounts++
		}
		switch {
		case account.Reason == activationUnknownQuota:
			job.UnknownAccounts++
		case account.Status == "failed" || account.Status == "failed_before_send" || account.Status == "sent_unknown" || account.Status == "partial":
			job.FailedAccounts++
		case account.Status == "skipped":
			job.SkippedAccounts++
		}
	}
	if job.Type == "preview" && job.State == "completed" && consumed == 0 && expires > time.Now().Unix() {
		job.ConfirmationToken = globalQuotaActivation.confirmation(job.ID)
	}
	return job, nil
}

func recoverInterruptedQuotaActivationJobs(ctx context.Context, db *sql.DB) error {
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='failed',error='plugin restarted; preview must be run again',updated_at=? WHERE job_type='preview' AND confirmation_consumed_at=0 AND state IN ('queued','running','completed')`, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_cycles AS cycle SET status='sent_unknown',updated_at=? WHERE status='dispatch_intent' AND EXISTS (SELECT 1 FROM quota_activation_job_accounts AS account WHERE account.job_id=cycle.run_id AND account.account_key=cycle.account_key AND account.status IN ('reserved','dispatched'))`, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_cycles AS cycle SET status='failed_before_send',updated_at=? WHERE status='dispatch_intent' AND EXISTS (SELECT 1 FROM quota_activation_job_accounts AS account WHERE account.job_id=cycle.run_id AND account.account_key=cycle.account_key AND account.status IN ('queued','revalidating'))`, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_job_accounts SET status='sent_unknown',error='plugin restarted after dispatch intent',finished_at=? WHERE status IN ('reserved','dispatched')`, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE quota_activation_job_accounts SET status='failed_before_send',error='plugin restarted before dispatch',finished_at=? WHERE status IN ('queued','revalidating') AND job_id IN (SELECT job_id FROM quota_activation_jobs WHERE job_type='run' AND state IN ('queued','running'))`, now); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='completed',error='plugin restarted during execution; unfinished dispatches were recovered conservatively',completed_accounts=total_accounts,updated_at=? WHERE job_type='run' AND state IN ('queued','running')`, now)
	return err
}

func activeActivationJob(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT job_id FROM quota_activation_jobs WHERE state IN ('queued','running') ORDER BY created_at LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func finishActivationJobError(ctx context.Context, db *sql.DB, jobID string, err error) {
	_, _ = db.ExecContext(ctx, `UPDATE quota_activation_jobs SET state='failed',error=?,updated_at=? WHERE job_id=?`, sanitizeTriggerError(err), time.Now().Unix(), jobID)
}

func updateActivationAccountState(ctx context.Context, db *sql.DB, runID, accountKey, status string, reason quotaActivationReason, httpStatus int, cycleKey string, before *quotaActivationQuota, startedAt, finishedAt int64) error {
	_, err := db.ExecContext(ctx, `UPDATE quota_activation_job_accounts SET status=?,reason=?,http_status=?,cycle_key=?,before_quota_json=CASE WHEN ?<>'' THEN ? ELSE before_quota_json END,started_at=CASE WHEN ?>0 THEN ? ELSE started_at END,finished_at=CASE WHEN ?>0 THEN ? ELSE finished_at END WHERE job_id=? AND account_key=?`,
		status, string(reason), httpStatus, cycleKey, marshalActivationQuota(before), marshalActivationQuota(before), startedAt, startedAt, finishedAt, finishedAt, runID, accountKey)
	return err
}

func finishActivationAccount(ctx context.Context, db *sql.DB, runID, accountKey, status string, reason quotaActivationReason, httpStatus int, err error, before *quotaActivationQuota, cycleKey string) {
	finishActivationAccountWithAfter(ctx, db, runID, accountKey, status, reason, httpStatus, err, before, nil, cycleKey)
}

func finishActivationAccountWithAfter(ctx context.Context, db *sql.DB, runID, accountKey, status string, reason quotaActivationReason, httpStatus int, err error, before, after *quotaActivationQuota, cycleKey string) {
	_, _ = db.ExecContext(ctx, `UPDATE quota_activation_job_accounts SET status=?,reason=?,http_status=?,cycle_key=?,before_quota_json=CASE WHEN ?<>'' THEN ? ELSE before_quota_json END,after_quota_json=?,error=?,finished_at=? WHERE job_id=? AND account_key=?`,
		status, string(reason), httpStatus, cycleKey, marshalActivationQuota(before), marshalActivationQuota(before), marshalActivationQuota(after), sanitizeTriggerError(err), time.Now().Unix(), runID, accountKey)
}

func markUnfinishedActivationAccountsUnknown(ctx context.Context, db *sql.DB, runID string, cause error) {
	now := time.Now().Unix()
	_, _ = db.ExecContext(ctx, `UPDATE quota_activation_cycles AS cycle SET status=CASE WHEN EXISTS (SELECT 1 FROM quota_activation_job_accounts AS account WHERE account.job_id=cycle.run_id AND account.account_key=cycle.account_key AND account.status IN ('reserved','dispatched')) THEN 'sent_unknown' ELSE 'failed_before_send' END,updated_at=? WHERE cycle.run_id=? AND cycle.status='dispatch_intent'`, now, runID)
	_, _ = db.ExecContext(ctx, `UPDATE quota_activation_job_accounts SET status=CASE WHEN status IN ('reserved','dispatched') THEN 'sent_unknown' ELSE 'failed_before_send' END,error=?,finished_at=? WHERE job_id=? AND status IN ('queued','revalidating','reserved','dispatched')`, sanitizeTriggerError(cause), now, runID)
}

func marshalActivationQuota(quota *quotaActivationQuota) string {
	if quota == nil {
		return ""
	}
	raw, _ := json.Marshal(quota)
	return string(raw)
}

func unmarshalActivationQuota(raw string) *quotaActivationQuota {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var quota quotaActivationQuota
	if json.Unmarshal([]byte(raw), &quota) != nil {
		return nil
	}
	return &quota
}

func randomOpaqueToken(bytesCount int) (string, error) {
	buf := make([]byte, bytesCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func activationTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func activationTokenMatches(token, expectedDigest string) bool {
	actual, err := hex.DecodeString(activationTokenDigest(token))
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(expectedDigest)
	if err != nil || len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func normalizeActivationAuthIndexes(values []string) (map[string]struct{}, error) {
	if len(values) > quotaActivationMaxItems {
		return nil, errors.New("auth_indexes exceeds 2000 entries")
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("auth_indexes cannot contain empty values")
		}
		out[value] = struct{}{}
	}
	return out, nil
}

func activationPreviewPollURL(id string) string {
	return "/v0/management/plugins/" + pluginID + "/quota-activation/preview?id=" + id
}

func activationRunPollURL(id string) string {
	return "/v0/management/plugins/" + pluginID + "/quota-activation/run?id=" + id
}

func handleQuotaActivationManagement(req managementRequest) managementResponse {
	base := "/v0/management/plugins/" + pluginID + "/quota-activation/"
	ctx := context.Background()
	switch req.Path {
	case base + "preview":
		if strings.EqualFold(req.Method, http.MethodPost) {
			var body quotaActivationPreviewRequest
			if len(req.Body) > 0 && json.Unmarshal(req.Body, &body) != nil {
				return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "invalid JSON body"})
			}
			response, status, err := globalQuotaActivation.startPreview(ctx, body)
			if err != nil {
				return activationAPIError(status, err)
			}
			return jsonResponse(status, response)
		}
		if strings.EqualFold(req.Method, http.MethodGet) {
			return pollQuotaActivationJob(ctx, "preview", firstQuery(req.Query, "id", ""))
		}
		return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	case base + "run":
		if strings.EqualFold(req.Method, http.MethodPost) {
			var body quotaActivationRunRequest
			if len(req.Body) == 0 || json.Unmarshal(req.Body, &body) != nil {
				return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "invalid JSON body"})
			}
			response, status, err := globalQuotaActivation.startRun(ctx, body)
			if err != nil {
				return activationAPIError(status, err)
			}
			return jsonResponse(status, response)
		}
		if strings.EqualFold(req.Method, http.MethodGet) {
			return pollQuotaActivationJob(ctx, "run", firstQuery(req.Query, "id", ""))
		}
		return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

func pollQuotaActivationJob(ctx context.Context, jobType, jobID string) managementResponse {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "id is required"})
	}
	db, _, err := globalStore.open(ctx)
	if err != nil {
		return activationAPIError(http.StatusInternalServerError, err)
	}
	job, err := loadActivationJob(ctx, db, jobType, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
	if err != nil {
		return activationAPIError(http.StatusInternalServerError, err)
	}
	return jsonResponse(http.StatusOK, job)
}

func activationAPIError(status int, err error) managementResponse {
	code := "activation_failed"
	switch status {
	case http.StatusBadRequest:
		code = "bad_request"
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusConflict:
		code = "conflict"
	}
	return jsonResponse(status, map[string]any{"error": code, "message": sanitizeTriggerError(err)})
}

func pruneQuotaActivationState(ctx context.Context, db *sql.DB, cutoff int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_activation_job_accounts WHERE job_id IN (SELECT job_id FROM quota_activation_jobs WHERE updated_at < ? AND state NOT IN ('queued','running'))`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM quota_activation_jobs WHERE updated_at < ? AND state NOT IN ('queued','running')`, cutoff)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM quota_activation_cycles
WHERE updated_at < ? AND (
  status='failed_before_send' OR
  EXISTS (
    SELECT 1 FROM quota_activation_cycles AS newer
    WHERE newer.account_key=quota_activation_cycles.account_key
      AND newer.rowid>quota_activation_cycles.rowid
  )
)`, cutoff); err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func parseActivationTime(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed.Unix()
}

func float64Pointer(value float64) *float64 { return &value }
func int64Pointer(value int64) *int64       { return &value }

func sortedActivationAuthIndexes(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

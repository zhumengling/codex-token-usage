package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	xaiStateUnauthorized  = "unauthorized"
	xaiStateForbidden     = "forbidden"
	xaiStateFreeExhausted = "free_usage_exhausted"
	xaiStateRateLimited   = "rate_limited"
)

type xaiAccountStateRow struct {
	StateKey         string `json:"state_key"`
	AuthID           string `json:"auth_id"`
	AuthIndex        string `json:"auth_index"`
	Source           string `json:"source"`
	Provider         string `json:"provider"`
	State            string `json:"state"`
	Reason           string `json:"reason"`
	ObservedAt       int64  `json:"observed_at"`
	ObservedAtText   string `json:"observed_at_text"`
	ResetAt          int64  `json:"reset_at"`
	ResetAtText      string `json:"reset_at_text"`
	SecondsRemaining int64  `json:"seconds_remaining"`
	Active           bool   `json:"active"`
	LastStatusCode   int    `json:"last_status_code"`
	AuthFile         string `json:"auth_file,omitempty"`
	AuthFileMTime    int64  `json:"auth_file_mtime,omitempty"`
}

func isXAISchedulerRequest(req schedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "xai") {
		return true
	}
	return len(req.Providers) == 1 && strings.EqualFold(strings.TrimSpace(req.Providers[0]), "xai")
}

func xaiStateForRecord(rec usageRecord, status int, now int64) (state, reason string, resetAt int64) {
	if !rec.Failed {
		return "", "", 0
	}
	parsed := parseXAIError(status, rec.Failure.Body)
	switch parsed.Kind {
	case xaiErrorUnauthorized, xaiErrorTokenExpired, xaiErrorTokenRevoked:
		return xaiStateUnauthorized, xaiErrorReason(status, parsed, "xAI credential is invalid"), 0
	case xaiErrorPermissionDenied, xaiErrorAccountUnavailable:
		return xaiStateForbidden, xaiErrorReason(status, parsed, "xAI access is denied"), 0
	case xaiErrorFreeUsageExhausted:
		return xaiStateFreeExhausted, xaiErrorReason(status, parsed, "xAI free usage is exhausted"), now + int64((24 * time.Hour).Seconds())
	case xaiErrorRateLimited:
		return xaiStateRateLimited, xaiErrorReason(status, parsed, "temporary xAI throttling"), xaiRetryAfterUnix(rec.ResponseHeaders, now)
	default:
		return "", "", 0
	}
}

func xaiFreeUsageExhaustedBody(body string) bool {
	return parseXAIError(0, body).Kind == xaiErrorFreeUsageExhausted
}

func xaiErrorReason(status int, parsed xaiParsedError, fallback string) string {
	evidence := firstNonEmptyString(parsed.Code, parsed.Message)
	if parsed.Code != "" && parsed.Message != "" && !strings.EqualFold(parsed.Code, parsed.Message) {
		evidence = parsed.Code + ": " + parsed.Message
	}
	evidence = sanitizeTriggerError(evidence)
	if evidence == "" {
		evidence = fallback
	}
	if status > 0 {
		return fmt.Sprintf("%d %s: %s", status, parsed.Kind, evidence)
	}
	return fmt.Sprintf("%s: %s", parsed.Kind, evidence)
}

func xaiRetryAfterUnix(headers map[string][]string, now int64) int64 {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), "retry-after") {
			continue
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
				if seconds > int64((15 * time.Minute).Seconds()) {
					seconds = int64((15 * time.Minute).Seconds())
				}
				return now + seconds
			}
			if parsed, err := http.ParseTime(value); err == nil && parsed.Unix() > now {
				resetAt := parsed.Unix()
				maxReset := now + int64((15 * time.Minute).Seconds())
				if resetAt > maxReset {
					resetAt = maxReset
				}
				return resetAt
			}
		}
	}
	return now + int64(time.Minute.Seconds())
}

func xaiAuthFileStateForRecord(rec usageRecord) (string, int64) {
	if authFile := fileNameIfJSON(rec.AuthFile); authFile != "" {
		return authFileStateForName(authFile)
	}
	if authFile := firstNonEmptyString(fileNameIfJSON(rec.AuthIndex), fileNameIfJSON(rec.Source), fileNameIfJSON(rec.AuthID)); authFile != "" {
		return authFileStateForName(authFile)
	}
	configured := readConfiguredXAIAccounts()
	emailCounts := configuredEmailCounts(configured)
	for _, cfg := range configured {
		if aliasesOverlap(normalizeAccountAliases(rec.AuthIndex, rec.AuthID, rec.Source), configuredAccountMatchAliases(cfg, emailCounts)) {
			return cfg.AuthFile, cfg.AuthFileMTime
		}
	}
	return "", 0
}

func xaiStateKeyForRecord(rec usageRecord, authFile string) string {
	if authFile != "" {
		return normalizeAccountAlias(authFile)
	}
	return normalizeAccountAlias(firstNonEmptyString(rec.AuthID, rec.AuthIndex, rec.Source))
}

func recordXAIStateIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if !strings.EqualFold(trim(rec.Provider), "xai") {
		return nil
	}
	now := rec.RequestedAt.Unix()
	if now <= 0 {
		now = time.Now().Unix()
	}
	if !rec.Failed && successfulStatusCode(status) {
		changed, err := clearRecoveredXAIState(ctx, db, rec)
		if err != nil {
			return err
		}
		if changed {
			globalSchedulerState.invalidate()
		}
		return nil
	}
	state, reason, resetAt := xaiStateForRecord(rec, status, now)
	if state == "" {
		return nil
	}
	authFile, authFileMTime := xaiAuthFileStateForRecord(rec)
	stateKey := xaiStateKeyForRecord(rec, authFile)
	if stateKey == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO xai_account_states (
  state_key, auth_id, auth_index, source, provider, state, reason, observed_at,
  reset_at, active, last_status_code, auth_file, auth_file_mtime
) VALUES (?, ?, ?, ?, 'xai', ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(state_key) DO UPDATE SET
  auth_id=excluded.auth_id,
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider='xai',
  state=excluded.state,
  reason=excluded.reason,
  observed_at=excluded.observed_at,
  reset_at=excluded.reset_at,
  active=1,
  last_status_code=excluded.last_status_code,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime`,
		stateKey, trim(rec.AuthID), trim(rec.AuthIndex), trim(rec.Source), state, reason, now, resetAt, status, authFile, authFileMTime)
	if err == nil {
		globalSchedulerState.setRestricted("xai", true)
	}
	return err
}

func clearRecoveredXAIState(ctx context.Context, db *sql.DB, rec usageRecord) (bool, error) {
	authFile, _ := xaiAuthFileStateForRecord(rec)
	aliases := normalizeAccountAliases(authFile, rec.AuthID, rec.AuthIndex, rec.Source)
	changed := false
	for _, alias := range aliases {
		result, err := db.ExecContext(ctx, `
UPDATE xai_account_states SET active=0
WHERE active=1
AND state IN (?, ?, ?)
AND (lower(state_key)=? OR lower(auth_id)=? OR lower(auth_index)=? OR lower(source)=? OR lower(auth_file)=?)`,
			xaiStateUnauthorized, xaiStateForbidden, xaiStateRateLimited,
			alias, alias, alias, alias, alias)
		if err != nil {
			return false, err
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			changed = true
		}
	}
	return changed, nil
}

func expireXAIStates(ctx context.Context, db *sql.DB, now int64) error {
	_, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE active=1 AND reset_at>0 AND reset_at<=?`, now)
	return err
}

func queryActiveXAIStates(ctx context.Context, db *sql.DB, now int64) ([]xaiAccountStateRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT state_key, auth_id, auth_index, source, provider, state, reason, observed_at, reset_at,
active, last_status_code, auth_file, auth_file_mtime
FROM xai_account_states
WHERE active=1 AND (reset_at=0 OR reset_at>?)
ORDER BY CASE WHEN reset_at=0 THEN 1 ELSE 0 END, reset_at, observed_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]xaiAccountStateRow, 0)
	for rows.Next() {
		var row xaiAccountStateRow
		var active int
		if err := rows.Scan(&row.StateKey, &row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.State, &row.Reason,
			&row.ObservedAt, &row.ResetAt, &active, &row.LastStatusCode, &row.AuthFile, &row.AuthFileMTime); err != nil {
			return nil, err
		}
		row.Active = active != 0
		row.ObservedAtText = unixTime(row.ObservedAt)
		if row.ResetAt > 0 {
			row.ResetAtText = unixTime(row.ResetAt)
			row.SecondsRemaining = maxInt64(0, row.ResetAt-now)
		} else {
			row.ResetAtText = "认证文件恢复后解除"
			row.SecondsRemaining = -1
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func xaiStateAliases(row xaiAccountStateRow) []string {
	return normalizeAccountAliases(row.StateKey, row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
}

func filterMissingXAIStateRows(rows []xaiAccountStateRow, configured []configuredAccount, authDirReadable bool) []xaiAccountStateRow {
	if !authDirReadable || len(rows) == 0 {
		return rows
	}
	aliases := configuredAliasSet(configured)
	strict := configuredStrictAliasSet(configured)
	out := rows[:0]
	for _, row := range rows {
		fileAliases := fileBackedCleanupAliases(row.StateKey, row.AuthIndex, row.Source, row.AuthFile)
		if len(fileAliases) > 0 {
			if aliasesContainAny(strict, fileAliases...) {
				out = append(out, row)
			}
			continue
		}
		if aliasesContainAny(aliases, row.StateKey, row.AuthID, row.AuthIndex, row.Source, row.AuthFile) {
			out = append(out, row)
		}
	}
	return out
}

func applyXAIStates(accounts []accountRow, states []xaiAccountStateRow) {
	for i := range accounts {
		for _, state := range states {
			if !aliasesOverlap(accountAliases(accounts[i]), xaiStateAliases(state)) {
				continue
			}
			accounts[i].XAIState = state.State
			accounts[i].XAIStateReason = state.Reason
			accounts[i].XAIStateObservedAt = state.ObservedAtText
			accounts[i].XAIStateResetAt = state.ResetAt
			accounts[i].XAIStateResetAtText = state.ResetAtText
			accounts[i].XAIStateSecondsRemaining = state.SecondsRemaining
			accounts[i].XAILastStatusCode = state.LastStatusCode
			break
		}
	}
}

func clearReplacedOrMissingXAIStates(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredXAIAccounts()
	if globalXAIAuthSource.authoritative() {
		states, err := queryActiveXAIStates(ctx, db, time.Now().Unix())
		if err != nil {
			return err
		}
		visible := filterMissingXAIStateRows(states, configured, true)
		keep := make(map[string]struct{}, len(visible))
		for _, state := range visible {
			keep[state.StateKey] = struct{}{}
		}
		for _, state := range states {
			if _, ok := keep[state.StateKey]; ok {
				continue
			}
			if _, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE state_key=?`, state.StateKey); err != nil {
				return err
			}
		}
	}
	for _, cfg := range configured {
		if cfg.AuthFileMTime <= 0 {
			continue
		}
		_, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE active=1 AND auth_file=? AND ?>observed_at`, cfg.AuthFile, cfg.AuthFileMTime)
		if err != nil {
			return err
		}
	}
	return nil
}

func candidateMatchesXAIState(candidate schedulerAuthCandidate, states []xaiAccountStateRow) bool {
	aliases := schedulerCandidateAliases(candidate)
	for _, state := range states {
		if aliasesOverlap(aliases, xaiStateAliases(state)) {
			return true
		}
	}
	return false
}

func earliestXAIStateReset(states []xaiAccountStateRow, now int64) int64 {
	var earliest int64
	for _, state := range states {
		if state.ResetAt <= now {
			continue
		}
		if earliest == 0 || state.ResetAt < earliest {
			earliest = state.ResetAt
		}
	}
	return earliest
}

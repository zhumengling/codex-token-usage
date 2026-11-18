package main

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strconv"
	"strings"
)

type providerQuotaWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ObservedAt  int64   `json:"observed_at"`
	ResetAt     int64   `json:"reset_at,omitempty"`
	ExpiresAt   int64   `json:"expires_at"`
}

type providerQuotaRow struct {
	Provider  string               `json:"provider"`
	Primary   *providerQuotaWindow `json:"primary,omitempty"`
	Secondary *providerQuotaWindow `json:"secondary,omitempty"`
}

const maxProviderQuotaTimestamp = 253402300799
const providerQuotaWithoutResetTTL = 24 * 60 * 60

func (window *providerQuotaWindow) expiry() int64 {
	if window.ResetAt > 0 {
		return window.ResetAt
	}
	return window.ObservedAt + providerQuotaWithoutResetTTL
}

func providerQuotaHeaders(rec usageRecord, status int, window string) (*float64, *int64) {
	if status != http.StatusTooManyRequests && (rec.Failed || (status != 0 && (status < 200 || status >= 300))) {
		return nil, nil
	}
	prefix := "x-codex-" + window + "-"
	percent, err := strconv.ParseFloat(providerQuotaHeader(rec.ResponseHeaders, prefix+"used-percent"), 64)
	if err != nil || math.IsNaN(percent) || percent < 0 || percent > 100 {
		return nil, nil
	}
	reset, err := strconv.ParseInt(providerQuotaHeader(rec.ResponseHeaders, prefix+"reset-at"), 10, 64)
	if err != nil || reset <= 0 || reset > maxProviderQuotaTimestamp {
		after, err := strconv.ParseInt(providerQuotaHeader(rec.ResponseHeaders, prefix+"reset-after-seconds"), 10, 64)
		observedAt := rec.RequestedAt.Unix()
		if err != nil || observedAt <= 0 || after < 0 || after > maxProviderQuotaTimestamp-observedAt {
			return &percent, nil
		}
		reset = observedAt + after
	}
	return &percent, &reset
}

func providerQuotaHeader(headers map[string][]string, key string) string {
	var value string
	for name, values := range headers {
		if !strings.EqualFold(name, key) {
			continue
		}
		for _, candidate := range values {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" || (value != "" && value != candidate) {
				return ""
			}
			value = candidate
		}
	}
	return value
}

func queryProviderQuotas(ctx context.Context, db *sql.DB, entries []providerConfigEntry) ([]providerQuotaRow, error) {
	quotas := make([]providerQuotaRow, 0)
	indexes := map[string]int{}
	for _, entry := range entries {
		if entry.Provider == "Codex" {
			indexes[entry.Name] = len(quotas)
			quotas = append(quotas, providerQuotaRow{Provider: entry.Name})
		}
	}
	if len(quotas) == 0 {
		return quotas, nil
	}
	query := `WITH observations AS (
    SELECT id, requested_at, ` + codexProviderNameSQL(entries) + ` AS endpoint,
      primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
    FROM usage_events
    WHERE (primary_used_percent IS NOT NULL OR secondary_used_percent IS NOT NULL)
      AND ((failed = 0 AND (status_code = 0 OR status_code BETWEEN 200 AND 299)) OR status_code = 429)
      AND ` + codexAPIKeyUsageSQL(entries) + `
  ), windows AS (
    SELECT id, requested_at, endpoint, 1 AS slot, primary_used_percent AS used, primary_reset_at AS reset_at
      FROM observations WHERE primary_used_percent BETWEEN 0 AND 100
    UNION ALL
    SELECT id, requested_at, endpoint, 2 AS slot, secondary_used_percent AS used, secondary_reset_at AS reset_at
      FROM observations WHERE secondary_used_percent BETWEEN 0 AND 100
  ), ranked AS (
    SELECT *, row_number() OVER (PARTITION BY endpoint, slot ORDER BY requested_at DESC, id DESC) AS latest
      FROM windows WHERE endpoint <> ?
  )
  SELECT endpoint, slot, used, requested_at, reset_at FROM ranked WHERE latest = 1`
	rows, err := db.QueryContext(ctx, query, unmatchedCodexProvider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint string
		var slot int
		var reset sql.NullInt64
		var observation providerQuotaWindow
		if err := rows.Scan(&endpoint, &slot, &observation.UsedPercent, &observation.ObservedAt, &reset); err != nil {
			return nil, err
		}
		if reset.Valid && reset.Int64 > 0 && reset.Int64 <= maxProviderQuotaTimestamp {
			observation.ResetAt = reset.Int64
		}
		observation.ExpiresAt = observation.expiry()
		if index, exists := indexes[endpoint]; exists {
			if slot == 1 {
				quotas[index].Primary = &observation
			} else {
				quotas[index].Secondary = &observation
			}
		}
	}
	return quotas, rows.Err()
}

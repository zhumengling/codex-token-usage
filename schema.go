package main

import (
	"context"
	"database/sql"
	"errors"
)

var errNoRows = errors.New("no rows")

const schemaSQL = `
CREATE TABLE IF NOT EXISTS usage_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  requested_at INTEGER NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  executor_type TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  alias TEXT NOT NULL DEFAULT '',
  api_key TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  auth_type TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  reasoning_effort TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  primary_used_percent REAL,
  primary_reset_at INTEGER,
  secondary_used_percent REAL,
  secondary_reset_at INTEGER,
  primary_used_tokens INTEGER,
  primary_remaining_tokens INTEGER,
  primary_limit_tokens INTEGER,
  secondary_used_tokens INTEGER,
  secondary_remaining_tokens INTEGER,
  secondary_limit_tokens INTEGER
);
CREATE INDEX IF NOT EXISTS idx_usage_events_requested_at ON usage_events(requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_auth ON usage_events(auth_index, auth_id, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events(model, alias, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_requested_auth_id ON usage_events(requested_at, auth_id);
CREATE INDEX IF NOT EXISTS idx_usage_events_requested_source ON usage_events(requested_at, source);
CREATE INDEX IF NOT EXISTS idx_usage_events_quota_scan ON usage_events(requested_at, failed, status_code);
CREATE INDEX IF NOT EXISTS idx_usage_events_api_key_requested ON usage_events(api_key, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_provider_requested ON usage_events(provider, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_status_requested ON usage_events(status_code, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_requested_id_desc ON usage_events(requested_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_lower_auth_index_requested ON usage_events(lower(auth_index), requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_lower_auth_id_requested ON usage_events(lower(auth_id), requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_lower_source_requested ON usage_events(lower(source), requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_provider_model_requested ON usage_events(provider, model, alias, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_api_key_provider_requested ON usage_events(api_key, provider, requested_at);
CREATE TABLE IF NOT EXISTS account_protection_reservations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  plan_type TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_account_protection_reservations_expiry ON account_protection_reservations(expires_at);
CREATE INDEX IF NOT EXISTS idx_account_protection_reservations_auth ON account_protection_reservations(auth_index, auth_id, source, expires_at);
CREATE TABLE IF NOT EXISTS xai_account_states (
  state_key TEXT PRIMARY KEY,
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT 'xai',
  state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  observed_at INTEGER NOT NULL,
  reset_at INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  last_status_code INTEGER NOT NULL DEFAULT 0,
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_xai_account_states_active_reset ON xai_account_states(active, reset_at);
CREATE INDEX IF NOT EXISTS idx_xai_account_states_auth ON xai_account_states(auth_index, auth_id, source);
CREATE TABLE IF NOT EXISTS summary_cache (
  cache_key TEXT PRIMARY KEY,
  window TEXT NOT NULL DEFAULT '',
  limit_count INTEGER NOT NULL DEFAULT 0,
  cached_at INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  revision TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  data_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_summary_cache_cached_at ON summary_cache(cached_at);
CREATE TABLE IF NOT EXISTS store_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS autoban_bans (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  window TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  banned_at INTEGER NOT NULL,
  reset_at INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_status_code INTEGER NOT NULL DEFAULT 429,
  primary_used_percent REAL,
  primary_reset_at INTEGER,
  secondary_used_percent REAL,
  secondary_reset_at INTEGER,
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0,
  released_at INTEGER NOT NULL DEFAULT 0,
  release_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_autoban_bans_active_reset ON autoban_bans(active, reset_at);
CREATE TABLE IF NOT EXISTS invalid_auths (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  invalidated_at INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_status_code INTEGER NOT NULL DEFAULT 401,
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0,
  auth_source_kind TEXT NOT NULL DEFAULT 'legacy'
);
CREATE INDEX IF NOT EXISTS idx_invalid_auths_active ON invalid_auths(active);
CREATE TABLE IF NOT EXISTS quota_trigger_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0,
  mode TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  http_status INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL,
  primary_used_percent REAL,
  primary_reset_at INTEGER,
  secondary_used_percent REAL,
  secondary_reset_at INTEGER,
  primary_used_tokens INTEGER,
  primary_remaining_tokens INTEGER,
  primary_limit_tokens INTEGER,
  secondary_used_tokens INTEGER,
  secondary_remaining_tokens INTEGER,
  secondary_limit_tokens INTEGER,
  primary_window_presence TEXT NOT NULL DEFAULT '',
  primary_limit_window_seconds INTEGER,
  primary_reset_after_seconds INTEGER,
  secondary_window_presence TEXT NOT NULL DEFAULT '',
  secondary_limit_window_seconds INTEGER,
  secondary_reset_after_seconds INTEGER
);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_account ON quota_trigger_runs(auth_index, auth_id, source, auth_file, finished_at);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_finished_at ON quota_trigger_runs(finished_at);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_status_finished ON quota_trigger_runs(status, finished_at);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_auth_file_finished ON quota_trigger_runs(auth_file, finished_at);
CREATE TABLE IF NOT EXISTS quota_activation_jobs (
  job_id TEXT PRIMARY KEY,
  job_type TEXT NOT NULL,
  state TEXT NOT NULL,
  force INTEGER NOT NULL DEFAULT 0,
  source_preview_id TEXT NOT NULL DEFAULT '',
  inventory_revision TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  confirmation_digest TEXT NOT NULL DEFAULT '',
  confirmation_consumed_at INTEGER NOT NULL DEFAULT 0,
  total_accounts INTEGER NOT NULL DEFAULT 0,
  completed_accounts INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_quota_activation_jobs_state ON quota_activation_jobs(job_type, state, updated_at);
CREATE INDEX IF NOT EXISTS idx_quota_activation_jobs_expiry ON quota_activation_jobs(expires_at);
CREATE TABLE IF NOT EXISTS quota_activation_job_accounts (
  job_id TEXT NOT NULL,
  account_key TEXT NOT NULL,
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0,
  plan_type TEXT NOT NULL DEFAULT '',
  inventory_fingerprint TEXT NOT NULL DEFAULT '',
  eligible INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  http_status INTEGER NOT NULL DEFAULT 0,
  cycle_key TEXT NOT NULL DEFAULT '',
  before_quota_json TEXT NOT NULL DEFAULT '',
  after_quota_json TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(job_id, account_key),
  FOREIGN KEY(job_id) REFERENCES quota_activation_jobs(job_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_quota_activation_accounts_job_status ON quota_activation_job_accounts(job_id, status);
CREATE INDEX IF NOT EXISTS idx_quota_activation_accounts_auth ON quota_activation_job_accounts(auth_index, auth_id, auth_file);
CREATE TABLE IF NOT EXISTS quota_activation_cycles (
  account_key TEXT NOT NULL,
  cycle_key TEXT NOT NULL,
  run_id TEXT NOT NULL,
  status TEXT NOT NULL,
  reserved_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  next_cycle_after INTEGER NOT NULL DEFAULT 0,
  active_observed_at INTEGER NOT NULL DEFAULT 0,
  refresh_observed_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(account_key, cycle_key)
);
CREATE INDEX IF NOT EXISTS idx_quota_activation_cycles_updated ON quota_activation_cycles(updated_at);
`

const insertSQL = `
INSERT INTO usage_events (
  requested_at, provider, executor_type, model, alias, api_key, auth_id, auth_index, auth_type, source,
  reasoning_effort, service_tier, latency_ms, ttft_ms, failed, status_code, input_tokens, output_tokens, reasoning_tokens,
  cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
  primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func ensureQuotaActivationColumns(ctx context.Context, db *sql.DB) error {
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(quota_activation_cycles)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	columns := []struct {
		name string
		sql  string
	}{
		{name: "next_cycle_after", sql: `ALTER TABLE quota_activation_cycles ADD COLUMN next_cycle_after INTEGER NOT NULL DEFAULT 0`},
		{name: "active_observed_at", sql: `ALTER TABLE quota_activation_cycles ADD COLUMN active_observed_at INTEGER NOT NULL DEFAULT 0`},
		{name: "refresh_observed_at", sql: `ALTER TABLE quota_activation_cycles ADD COLUMN refresh_observed_at INTEGER NOT NULL DEFAULT 0`},
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, column.sql); err != nil {
			return err
		}
	}
	return nil
}

func ensureInvalidAuthColumns(ctx context.Context, db *sql.DB) error {
	existing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(invalid_auths)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !existing["auth_source_kind"] {
		if _, err := db.ExecContext(ctx, `ALTER TABLE invalid_auths ADD COLUMN auth_source_kind TEXT NOT NULL DEFAULT 'legacy'`); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `
UPDATE invalid_auths
SET auth_source_kind = CASE lower(trim(auth_source_kind))
  WHEN 'file' THEN 'file'
  WHEN 'runtime_only' THEN 'runtime_only'
  ELSE 'legacy'
END
WHERE auth_source_kind <> CASE lower(trim(auth_source_kind))
  WHEN 'file' THEN 'file'
  WHEN 'runtime_only' THEN 'runtime_only'
  ELSE 'legacy'
END`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_invalid_auths_source_kind_active ON invalid_auths(auth_source_kind, active)`)
	return err
}

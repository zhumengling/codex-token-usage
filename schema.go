package main

import "errors"

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
  secondary_reset_at INTEGER
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
  auth_file_mtime INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_invalid_auths_active ON invalid_auths(active);
CREATE TABLE IF NOT EXISTS quota_trigger_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  auth_file TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  http_status INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL,
  primary_used_percent REAL,
  primary_reset_at INTEGER,
  secondary_used_percent REAL,
  secondary_reset_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_account ON quota_trigger_runs(auth_index, auth_id, source, auth_file, finished_at);
CREATE INDEX IF NOT EXISTS idx_quota_trigger_runs_finished_at ON quota_trigger_runs(finished_at);
`

const insertSQL = `
INSERT INTO usage_events (
  requested_at, provider, executor_type, model, alias, api_key, auth_id, auth_index, auth_type, source,
  reasoning_effort, service_tier, latency_ms, ttft_ms, failed, status_code, input_tokens, output_tokens, reasoning_tokens,
  cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
  primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

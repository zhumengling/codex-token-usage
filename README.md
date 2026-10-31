# CPA Token Usage

CPA Token Usage is a CLIProxyAPI plugin for Codex account operation dashboards and AI provider usage analytics.

Current version: `0.1.39`

## Features

- Codex account pool dashboard with pagination, saved sorting, quota bars, 7d/month quota estimates, cost estimates, and light/dark compatible UI.
- AI provider pages grouped by CPA endpoint name, separated from Codex OAuth account-pool pricing and quota calculations.
- Codex 429 auto-ban support with `reset_at` based recovery.
- 401 invalid-auth protection until the auth JSON file is replaced or removed.
- Suspicious external quota consumption detection for shared or resold accounts.
- Optional periodic Codex quota trigger that sends a tiny real Codex request to refresh/start server-reported quota windows.
- Authenticated Management UI/API workflow for previewing and activating fresh Codex quota windows exactly once per observed account cycle.
- Runtime diagnostics and local alerts are exposed in summary JSON for troubleshooting and plugin-store validation.
- CSV / JSON export support; the dashboard exposes account export buttons and the backend can export accounts, providers, models, and recent requests.
- Built-in price fallbacks plus automatic LiteLLM model price updates.
- Manual Chinese / English language switch saved in the browser.
- xAI account-pool dashboard for xAI OAuth JSON credentials, with xAI-specific 401/403/429 and free-usage-exhausted states.
- Claude subscription pool: Claude OAuth logins get an account-pool page like Codex and xAI, with the 5h / 7d utilization from Claude response headers converted into a USD limit at API prices and at a fitted cache-hit price. Like Codex and xAI accounts, the logins leave the AI overview; Claude API keys stay there.
- xAI accounts are read through CPA `host.auth.list/get/get_runtime` when available, with filesystem fallback for older CPA versions; account rows classify Free, Super, and Heavy tiers from auth metadata.
- Non-standard Codex credential import converts ChatGPT Session, sub2api/account-product, 9router, Codex auth.json, AxonHub, Codex-Manager, and generic nested token JSON through CPA `host.auth.save`, with preview, conflict detection, and no-refresh-token warnings.
- Optional Codex/xAI Session affinity for scheduler requests: the same Session can stay on the same account; without a usable binding, filtered candidates follow CPA `routing.strategy` (`fill-first` or `round-robin`).
- Optional account-protection scheduling for Codex OAuth accounts: per-plan concurrency hard limits and rolling-window Token soft demotion.
- Account-protection and error filtering preserve CPA `fill-first` or `round-robin` selection within the highest-priority candidate tier.
- Configured accounts with no real requests display zero quota even when background health probes have captured quota headers.
- Provider-aware cache read/write normalization keeps OpenAI-compatible and Anthropic-style usage, cache hit rates, and cost estimates consistent.
- Summary cache keys are canonicalized and bounded in memory and SQLite for long-running installations.

## Install Manually

Download the matching release zip, then place the dynamic library under the CLIProxyAPI plugin directory:

```text
plugins/linux/amd64/codex-token-usage.so
plugins/windows/amd64/codex-token-usage.dll
plugins/darwin/arm64/codex-token-usage.dylib
```

Restart CLIProxyAPI after replacing the file.

## Configuration

The plugin is configured under:

```yaml
plugins:
  enabled: true
  configs:
    codex-token-usage:
      enabled: true
      priority: 120

      开启定时额度触发（不建议账号多的情况下开启）: false
      触发间隔分钟: 10
      触发模式: probe
      最大并发账号数: 1
      单账号超时秒数: 20
      单账号最小冷却分钟: 10

      同一个Session优先固定到同一个账号: true
      自动更新模型价格表: true
      模型价格更新间隔小时: 6
      模型价格表地址: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
      模型价格更新超时秒数: 20

      开启 Claude 订阅额度估算: true
      用量保留天数: 90
      额度触发记录保留天数: 30
      请求明细保留天数: 30

      开启账号保护调度（可能会影响缓存）: false
      Free 并发上限: 2
      Plus 并发上限: 5
      K12 并发上限: 5
      Team 并发上限: 5
      Pro 并发上限: 10
      Free 5 分钟 Token 上限: 2000000
      Plus 5 分钟 Token 上限: 8000000
      K12 5 分钟 Token 上限: 8000000
      Team 5 分钟 Token 上限: 8000000
      Pro 5 分钟 Token 上限: 12000000
      账号保护 Token 窗口秒数: 300
      账号保护预约超时秒数: 900
```

English config keys are also accepted:

```yaml
quota_trigger_enabled: false
quota_trigger_interval_minutes: 10
quota_trigger_mode: probe
quota_trigger_max_concurrency: 1
quota_trigger_timeout_seconds: 20
quota_trigger_min_account_cooldown_minutes: 10
scheduler_session_affinity_enabled: true
model_price_auto_update_enabled: true
model_price_update_interval_hours: 6
model_price_update_url: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
model_price_update_timeout_seconds: 20
claude_quota_enabled: true
usage_retention_days: 90
quota_trigger_retention_days: 30
request_detail_retention_days: 30
account_protection_enabled: false
account_protection_free_concurrency: 2
account_protection_plus_concurrency: 5
account_protection_k12_concurrency: 5
account_protection_team_concurrency: 5
account_protection_pro_concurrency: 10
account_protection_free_token_limit: 2000000
account_protection_plus_token_limit: 8000000
account_protection_k12_token_limit: 8000000
account_protection_team_token_limit: 8000000
account_protection_pro_token_limit: 12000000
account_protection_token_window_seconds: 300
account_protection_reservation_ttl_seconds: 900
```

Quota trigger defaults to off and is not recommended for large account pools. `probe` mode sends a real minimal Codex model request, so it can consume a small amount of tokens and may affect quota. Probe results are account-health inputs: 401 and 402 update invalid-auth state, 403 follows the same repeated-failure threshold as normal traffic, 429 creates an auto-ban, and a successful probe clears recovered state. Accounts already restricted by 401, 402, 403, or 429 remain eligible for health rechecks after the configured cooldown, including when an older quota snapshot is still full. The legacy Chinese key `开启定时额度触发` and the legacy `quota` mode remain accepted for compatibility.

## One-shot quota-window activation

The dashboard action **Activate quota windows once** is independent of the periodic trigger and works while `quota_trigger_enabled` remains `false`:

1. Preview reads quota without model generation and lists every exact Codex auth record separately, including multiple seats with the same email.
2. The default decision requires an enabled, unexpired Codex credential with at least one explicitly reported quota window and every reported window completely fresh. An explicitly `null` window is absent and does not block another valid reported window; omitted presence, zero reported windows, contradictory values, positive usage/tokens, or a countdown shorter than the server-reported duration are not fresh eligibility.
3. Window names are opaque API slots, not duration promises: the UI shows each window's server-reported `limit_window_seconds`, `reset_after_seconds`, presence, usage, and reset time. For example, an account may report a seven-day `primary_window` and an explicitly null `secondary_window`.
4. After explicit acknowledgement, a confirmed run revalidates the exact auth identity and quota, reserves a stable cycle key, and sends one fixed compact Codex request per selected account. A fresh full-duration window's moving `reset_at` is not part of that key. A later fresh cycle becomes distinct either after a prior safe window boundary has passed or after durable valid observations show the guarded cycle active and then show every reported window full/fresh again. The active-to-fresh policy intentionally accepts one authoritative fresh server read after prior active evidence so event-driven or manual resets need not wait for a scheduled boundary. That refresh observation is persisted once on the predecessor, making its successor key stable across preview, run revalidation, restart, and definite pre-send retry. Repeated fresh reads cannot mint another successor; the successor must itself be observed active before a later fresh read can create another generation. An ambiguous send without active-to-fresh evidence or a safe elapsed boundary stays blocked rather than risking a duplicate.
5. The result reports `verified`, `partial`, `failed_before_send`, `sent_unknown`, or an explainable skip. Verification requires positive usage/tokens or a full-duration-to-shorter-countdown transition for every reported target window. Reset-time movement alone is not evidence, explicitly absent windows do not force `partial`, and ambiguous or partially verified sends are never retried automatically.

The request is the smallest fixed request currently used by this plugin; it is a real request and can consume a small amount of quota. The API does not enforce an exact one-token output, so this feature makes no exact-token-cost claim. Force recovery mode bypasses only the fresh-window decision and requires explicit auth indexes; it never bypasses disabled, expired, provider, identity, credential, unknown-presence, zero-window, contradictory-quota, or quota-read safety checks.

Management routes (all protected by CPA Management authentication) are:

```text
POST /v0/management/plugins/codex-token-usage/quota-activation/preview
GET  /v0/management/plugins/codex-token-usage/quota-activation/preview?id=<preview-id>
POST /v0/management/plugins/codex-token-usage/quota-activation/run
GET  /v0/management/plugins/codex-token-usage/quota-activation/run?id=<run-id>
```

Example preview body:

```json
{"force": false, "auth_indexes": []}
```

A completed preview returns a short-lived one-time confirmation token. Pass that token, the preview ID, and an explicit subset of preview-eligible auth indexes to the run endpoint. Preview/run state and cycle reservations are persisted in the plugin SQLite database. Credentials, internal auth IDs/file names, authorization headers, cookies, and raw upstream bodies are not returned; credentials, headers, cookies, and raw bodies are not persisted. A periodic trigger round and a one-shot run share an exclusion gate and cannot dispatch concurrently.

Rollback is local: disable/remove this plugin and restart CPA if required. This stops future actions but cannot undo a successful upstream request or its intended quota-window effect.

## Claude subscription quota

Anthropic reports a Claude subscription's usage only as a percentage per window (`anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization`, and model-scoped windows such as `7d_sonnet`). CPA forwards these headers with every Claude usage record; the plugin stores a reading whenever a window's value or reset time changes, plus a heartbeat every ten minutes. It sends no extra upstream requests.

The **Claude pool** tab (the account-pool page, like the Codex and xAI pools) shows the 5h and 7d windows in the account table and a 7-day quota card. It and `GET /v0/management/plugins/codex-token-usage/claude-quota` relate each reading to the API-priced cost of the same login's requests since the window started:

```text
utilization% = (base + k × cache_read) / limit × 100
```

`base` is input, output and cache-write cost, `cache_read` is cache-hit cost, both at the model price table's API prices. Two tiers are reported:

- **API price** fixes `k = 1`, so cache hits count at the API cache-read price.
- **Fitted cache price** solves for `k` by least squares over the moments the reported percentage stepped up, with one intercept per window cycle (or per part of one, see below). `k` is the share of the API cache-read price the subscription actually charges; it needs at least six steps and cycles in which the cache share varies, and reports a standard error and confidence.

Both tiers give the window limit, the current cycle's spend and the remaining amount. Fits use the last 28 days. Windows scoped to something other than a model family (for example `7d_oi`) are shown without an estimate.

Use of the same login outside this CPA (signed in directly, or on claude.ai) raises the percentage with no request here to account for it, which would make the limit look smaller. The shortest unscoped window (5h) therefore screens its steps: it fits them in blocks of five by least absolute deviations, leaves out each block that rose more than three robust standard deviations, and at least one percentage point, above the median block, and refits the rest until nothing changes. The 7d and model-scoped windows, whose steps are too coarse to screen, leave out every step that overlaps a left-out 5h step or that no 5h step covers. The steps after a left-out one get an intercept of their own. Each tier reports `other_usd`, the current cycle's usage the requests here do not account for (utilization × limit − spend), and each window `screened_steps`; the pool shows the outside usage under the window. A stretch at the start or end of the 28 days in which 80% of the blocks are left out, lasting 12 hours and 20 steps, is kept instead: outside usage comes and goes, and a faster rate that holds that long is more likely a change for the change detection below.

Anthropic can change a window's limit or the price of cache hits at any time, and a fit over steps on both sides of such a change blends the two. Both tiers therefore fit only the steps after the most recent change, if one is found. A change is the split of the screened steps that best explains the residuals of one shared fit (a sup-Wald test). It counts once 20 steps have followed it, the statistic reaches 100, and the fit after it charges the steps since the change at least 10% more or less than the fit before it does. Slow drift in the traffic mix stays below that bar in simulations. The steps after a change are screened again against a fit of their own. The route reports a change as `change` (when it happened, the steps left out, the rate ratio and the earlier cache price), and the pool shows it under the window.

Disable sampling with `claude_quota_enabled: false` (or `开启 Claude 订阅额度估算: false`). Samples follow `usage_retention_days`.

## Model Price Table

The plugin includes a small built-in fallback price table. By default it also downloads and refreshes the full LiteLLM-style model price table from:

```text
https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
```

The downloaded file is stored under the current CPA user's data directory:

```text
$HOME/.cli-proxy-api/plugins/codex-token-usage/model_prices.cache
```

The file is about 1.5 MB and is not bundled into release zips, so plugin binaries stay smaller and prices can be refreshed without rebuilding the plugin.

To override the location, set:

```bash
CPA_MODEL_PRICE_FILE=/path/to/model_prices.json
```

`CPA_TOKEN_USAGE_DIR` overrides the shared plugin data directory used by both `usage.db` and, unless `CPA_MODEL_PRICE_FILE` is set, `model_prices.cache`. The cache content is JSON, but the non-JSON extension prevents CPA from treating it as an authentication file. Existing plugin-owned `model_prices.json` files are migrated automatically. `CPA_CONFIG_PATH` (or the legacy `CPA_CONFIG_FILE`) overrides the CPA config path. Otherwise the plugin follows CPA's `-config` / `--config` process argument. Without either override, the canonical `$HOME/.cli-proxy-api/config.yaml` is preferred when present, followed by an existing `$HOME/config.yaml` or `config.yaml` in the process working directory; the final fallback remains `$HOME/.cli-proxy-api/config.yaml`.

## Data Safety

- Access tokens, refresh tokens, id tokens, and API keys are not written to summary JSON, UI, alert output, or exports.
- Exported account labels that look like API keys are masked as `sk-****abcd`.
- Local alert data is generated inside summary/export responses only; this version does not send webhooks.
- Auth JSON files are read only for account identity, provider classification, quota trigger access, and replacement detection. Tokens are used in memory for trigger requests and are not written to summary/export data.

## Build

```bash
go test ./...
./build.sh
./package-release.sh dist
```

Release assets are named in the CLIProxyAPI plugin store format:

```text
codex-token-usage_0.1.39_linux_amd64.zip
codex-token-usage_0.1.39_linux_arm64.zip
codex-token-usage_0.1.39_windows_amd64.zip
codex-token-usage_0.1.39_darwin_amd64.zip
codex-token-usage_0.1.39_darwin_arm64.zip
checksums.txt
```

## Plugin Store Checklist

- Build and upload all required OS / architecture zip files.
- Include `checksums.txt`.
- Add screenshots for the Codex account pool, AI provider overview, and a selected AI endpoint page.
- Document default-off quota trigger behavior and real-probe token cost risk.
- Confirm `go test ./...` passes before publishing.

## Common Issues

- `未注册 / 未生效`: confirm the file is under the correct plugin directory and restart CLIProxyAPI.
- `401`: the auth JSON is invalid and will not be used until replaced or removed.
- `429`: the account is temporarily auto-banned until the observed reset time.
- Provider not visible: confirm the endpoint still exists in CPA config and refresh the dashboard.
- Price missing: check `model_prices.cache` status in the summary JSON and the model price update error if present.

## License

MIT

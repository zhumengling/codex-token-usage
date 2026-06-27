# CPA Token Usage

CPA Token Usage is a CLIProxyAPI plugin for Codex account operation dashboards.

It records usage events, estimates model cost, tracks Codex 5h and 7d/month quota windows, marks invalid credentials, detects suspicious external quota consumption, and helps auto-disable accounts after 429 quota errors.

## Features

- Codex account pool dashboard with pagination, sorting, quota bars, and cost estimates.
- 429 auto-ban support with reset-at based recovery.
- 401 invalid-auth protection until the auth JSON file is replaced or removed.
- Optional quota trigger for refreshing observed Codex quota snapshots.
- Provider pages for non-Codex OpenAI-compatible endpoints.
- Built-in model price fallbacks with optional local override.

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
      开启定时额度触发: false
      触发间隔分钟: 10
      触发模式: quota
      最大并发账号数: 1
      单账号超时秒数: 20
      单账号最小冷却分钟: 10
```

Quota trigger defaults to off. `quota` mode only queries quota state. `probe` mode can consume a small amount of tokens and should be enabled deliberately.

## Optional Price Override

The plugin includes a default price table. To override or extend it, place a LiteLLM-style JSON file at:

```text
/root/plugins/codex-token-usage/model_prices.json
```

or set:

```bash
CPA_MODEL_PRICE_FILE=/path/to/model_prices.json
```

Do not put API keys, access tokens, refresh tokens, or auth JSON content in this file.

## Build

```bash
go test ./...
./build.sh
./package-release.sh dist
```

Release assets are named in the CLIProxyAPI plugin store format:

```text
codex-token-usage_0.1.6_linux_amd64.zip
checksums.txt
```

## License

MIT

# 发布到 CPA 插件商店

CPA 插件商店读取官方仓库 `router-for-me/CLIProxyAPI-Plugins-Store` 的 `registry.json`。插件本体不提交到官方仓库，而是放在你自己的 GitHub Releases。

## 1. 发布 GitHub Release

1. 推送源码到 `https://github.com/zhumengling/codex-token-usage`。
2. 创建版本标签，例如：

   ```bash
   git tag v0.1.13
   git push origin v0.1.13
   ```

3. GitHub Actions 会生成 release 资产：

   ```text
   codex-token-usage_0.1.13_linux_amd64.zip
   codex-token-usage_0.1.13_linux_arm64.zip
   codex-token-usage_0.1.13_darwin_amd64.zip
   codex-token-usage_0.1.13_darwin_arm64.zip
   codex-token-usage_0.1.13_windows_amd64.zip
   checksums.txt
   ```

如果某个平台 workflow 失败，可以先只保留成功的平台资产，但插件商店覆盖面会变小。

完整模型价格表不会打入 release zip。插件运行时会按配置从 LiteLLM 兼容价格表 URL 自动更新 `/root/plugins/codex-token-usage/model_prices.json`。

## 2. 向官方插件商店提交 PR

Fork `https://github.com/router-for-me/CLIProxyAPI-Plugins-Store`，在 `registry.json` 里新增：

```json
{
  "id": "codex-token-usage",
  "name": "CPA Token Usage",
  "description": "Codex account usage, quota tracking, 429 autoban and provider cost dashboard.",
  "author": "zhumengling",
  "repository": "https://github.com/zhumengling/codex-token-usage",
  "homepage": "https://github.com/zhumengling/codex-token-usage",
  "license": "MIT",
  "tags": ["Usage", "Codex", "Quota", "Dashboard"]
}
```

然后向官方仓库发 Pull Request。官方合并后，CPA 管理面板的插件商店就能看到它。

## 3. 发布新版本

1. 修改 `pluginVersion`。
2. 创建新 tag，例如 `v0.1.13`。
3. 等 Release 资产生成。
4. 通常不需要更新插件商店 `registry.json`；CPA 会读取仓库 latest release。`version` 只是 latest release 查询失败时的旧版显示 fallback。

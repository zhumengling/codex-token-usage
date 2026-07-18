package main

const dashboardBody = `</style>
</head>
<body>
<main>
  <div class="hero">
    <div><h1>CPA Token Usage</h1><div class="hint" id="pool-hero-hint">按账号聚合 CPA usage：Token 消耗、缓存率、请求健康、5h/7d 额度窗口和最近异常。</div></div>
    <div class="controls">
      <input id="key" class="fallback-key" type="password" autocomplete="current-password" aria-label="CPA 管理密码备用输入" placeholder="管理密钥备用输入">
      <button id="auth-import-open" class="ghost" type="button">账号 JSON 导入</button>
      <button id="batch-proxy-open" class="ghost" type="button">批量写入代理</button>
      <button id="quota-activation-open" class="ghost" type="button">一次性启动额度窗口</button>
      <select id="language" data-no-i18n aria-label="语言"><option value="zh">中文</option><option value="en">English</option></select>
      <select id="window" aria-label="统计窗口"><option value="24h">最近 24 小时</option><option value="today">今天</option><option value="7d">最近 7 天</option><option value="30d">最近 30 天</option><option value="all">全部</option></select>
      <button id="export-logs" class="ghost">导出日志</button>
      <button id="refresh">刷新</button>
    </div>
  </div>
  <div id="status" class="status" role="status" aria-live="polite"></div>
  <div class="tabs" role="tablist" aria-label="统计页面">
    <div class="tab-strip" id="tab-strip">
      <button class="tab active" data-target="codex" role="tab" aria-selected="true">Codex 账号池<span class="tab-count" id="tab-codex-count">-</span></button>
      <button class="tab" data-target="xai" role="tab" aria-selected="false" hidden>xAI 账号池<span class="tab-count" id="tab-xai-count">-</span></button>
      <button class="tab" data-target="providers" role="tab" aria-selected="false">AI 总览<span class="tab-count" id="tab-provider-count">-</span></button>
      <span id="provider-tabs" class="tab-strip"></span>
    </div>
    <div class="provider-picker" id="provider-picker">
      <button class="ghost" id="provider-picker-button" type="button">显示接入点</button>
      <div class="picker-panel" id="provider-picker-panel"></div>
    </div>
  </div>
  <section data-page="codex" class="page-on">
  <div class="command-grid">
    <section class="section"><h2><span>运行总览</span><span class="mini">请求 / Token / 缓存 / 限流</span></h2><div class="section-body"><div class="cards">
      <div class="metric" style="--accent:var(--blue)"><div class="label">请求数</div><div class="value" id="m-requests">-</div><div class="sub" id="m-success">成功率 -</div></div>
      <div class="metric" style="--accent:var(--cyan)"><div class="label">总 Token</div><div class="value" id="m-total">-</div><div class="sub" id="m-total-sub">Codex 账号池合计</div></div>
      <div class="metric" style="--accent:var(--blue)"><div class="label">费用估算</div><div class="value" id="m-cost">-</div><div class="sub" id="m-cost-sub">按模型价格估算</div></div>
      <div class="metric" style="--accent:var(--cyan)"><div class="label">输入 Token</div><div class="value" id="m-input">-</div><div class="sub" id="m-input-share">占比 -</div></div>
      <div class="metric" style="--accent:var(--violet)"><div class="label">输出 Token</div><div class="value" id="m-output">-</div><div class="sub" id="m-output-share">占比 -</div></div>
      <div class="metric" style="--accent:var(--orange)"><div class="label">缓存 Token</div><div class="value" id="m-cache">-</div><div class="sub" id="m-cache-share">缓存率 -</div></div>
      <div class="metric" style="--accent:var(--red)"><div class="label">429 次数</div><div class="value bad" id="m-429">-</div><div class="sub" id="m-429-sub">限流/额度打满</div></div>
      <div class="metric" style="--accent:var(--red)"><div class="label" id="m-bans-label">自动禁用</div><div class="value bad" id="m-bans">-</div><div class="sub" id="m-bans-sub">Codex 429 Auto Ban</div></div>
      <div class="metric" style="--accent:var(--blue)"><div class="label">活跃账号</div><div class="value" id="m-accounts">-</div><div class="sub">可识别账号</div></div>
      <div class="metric" id="m-quota-card" style="--accent:var(--cyan)"><div class="label">7d/月剩余额度</div><div class="value" id="m-7d-remaining">-</div><div class="sub" id="m-7d-remaining-sub">按账号额度快照估算</div></div>
      <div class="metric" id="m-trigger-card" style="--accent:var(--violet)"><div class="label">额度触发</div><div class="value" id="m-trigger">-</div><div class="sub" id="m-trigger-sub">默认关闭</div></div>
      <div class="metric" style="--accent:var(--cyan)"><div class="label">Top 账号占比</div><div class="value" id="m-topshare">-</div><div class="sub">Token 集中度</div></div>
      <div class="metric" style="--accent:var(--blue)"><div class="label">平均耗时</div><div class="value" id="m-latency">-</div><div class="sub" id="m-latency-sub">慢请求 -</div></div>
      <div class="metric" style="--accent:var(--cyan)"><div class="label">首 Token</div><div class="value" id="m-ttft">-</div><div class="sub" id="m-ttft-sub">慢首包 -</div></div>
      <div class="metric" style="--accent:var(--violet)"><div class="label">输出速度</div><div class="value" id="m-throughput">-</div><div class="sub">输出 Token / 秒</div></div>
    </div></div></section>
    <section class="section"><h2><span>风险洞察</span><span class="mini">健康 / 异常 / 集中度</span></h2><div class="section-body"><div id="insights" class="insights"></div></div></section>
  </div>
  <div class="layout">
    <section class="section"><h2><span>用量趋势</span><span class="mini">请求数 / 总 Token / 输出 Token</span></h2><div class="section-body"><svg id="trend" class="chart" viewBox="0 0 900 270" preserveAspectRatio="none"></svg><div class="legend"><span><i class="dot" style="background:var(--blue)"></i>请求</span><span><i class="dot" style="background:var(--cyan)"></i>总 Token</span><span><i class="dot" style="background:var(--orange)"></i>输出 Token</span></div></div></section>
    <section class="section"><h2><span>Token 结构</span><span class="mini">缓存命中率 = Read / 总输入</span></h2><div class="section-body"><div class="mix" id="token-mix"></div></div></section>
  </div>
  <section class="section" style="margin-top:8px"><h2><span>模型排行</span><span class="mini">仅 Codex 账号池</span></h2><div class="scroll model-table-wrap"><table><thead><tr><th>模型</th><th>别名</th><th>Provider</th><th>请求</th><th>总 Token</th><th>费用</th><th>性能</th><th>输入</th><th>输出</th><th>缓存</th><th>缓存率</th></tr></thead><tbody id="models"></tbody></table></div></section>
  <section class="section" style="margin-top:8px"><h2><span>账号池运营台</span><span class="mini" id="account-scope">搜索、排序、分页承载大量账号</span></h2><div class="section-body"><div class="account-toolbar"><input id="account-filter" aria-label="搜索账号、邮箱或 AuthIndex" placeholder="搜索账号、邮箱或 AuthIndex"><select id="account-sort" aria-label="账号排序方式"><option value="tokens">按 Token</option><option value="cost">按费用</option><option value="quotaRemain">按 7d/月余量</option><option value="quotaTotal">按 7d/月总额度</option><option value="latency">按平均耗时</option><option value="invalid">按 401 失效</option><option value="workspace">按 402 工作区</option><option value="external">按外部消耗</option><option value="trigger">按触发状态</option><option value="quota">按额度已用</option><option value="cache">按缓存率</option><option value="429">按 429</option><option value="success">按成功率</option><option value="recent">按最近使用</option></select><div class="column-controls" id="account-columns"><label><input type="checkbox" data-col="perf" checked>性能</label><label><input type="checkbox" data-col="cache" checked>缓存</label><label><input type="checkbox" data-col="quota5h" checked>5h</label><label><input type="checkbox" data-col="status" checked>状态</label></div><span class="spacer"></span><select id="account-page-size" aria-label="账号每页数量"><option value="25">25 / 页</option><option value="50">50 / 页</option><option value="100">100 / 页</option></select><button id="account-prev" class="ghost" aria-label="上一页账号">上一页</button><span id="account-page-label" class="page-label">1 / 1</span><button id="account-next" class="ghost" aria-label="下一页账号">下一页</button></div><div class="account-summary-grid"><div class="account-summary-card"><span>当前结果</span><b id="account-loaded">-</b></div><div class="account-summary-card"><span>费用合计</span><b id="account-cost-total">-</b></div><div class="account-summary-card"><span>风险账号</span><b id="account-risk">-</b></div><button id="invalid-auth-card" class="account-summary-card account-summary-action invalid-auth-action" type="button" aria-label="管理 401 失效账号"><span>401 失效</span><b id="account-invalid-auth">-</b><small id="account-invalid-auth-hint">点击管理</small></button><button id="workspace-deactivated-card" class="account-summary-card account-summary-action workspace-deactivated-action" type="button" aria-label="管理 402 工作区失效账号"><span>402 工作区</span><b id="account-workspace-deactivated">-</b><small id="account-workspace-deactivated-hint">点击管理</small></button><button id="autoban-release-card" class="account-summary-card account-summary-action autoban-release-action" type="button" aria-label="管理 429 禁用账号"><span>429 禁用</span><b id="account-429-bans">-</b><small id="account-429-bans-hint">点击解除</small></button><div class="account-summary-card"><span>疑似外部消耗</span><b id="account-external-use">-</b></div><div class="account-summary-card"><span>触发异常</span><b id="account-trigger-failed">-</b></div><div class="account-summary-card"><span>额度最高</span><b id="account-quota-hot">-</b></div><div class="account-summary-card"><span>缓存最低</span><b id="account-cache-low">-</b></div></div><div class="scroll account-table-wrap"><table class="account-table"><thead><tr><th>账号</th><th>请求</th><th>成功率</th><th data-col="perf">性能</th><th>总 Token / 费用</th><th data-col="cache">缓存</th><th data-col="quota5h">5h 窗口</th><th>7d/月窗口 / 额度预估</th><th>429</th><th>最近</th><th data-col="status">状态</th></tr></thead><tbody id="account-table"></tbody></table></div></div></section>
  <section class="section" style="margin-top:8px"><h2><span>自动禁用状态</span><span class="mini">429 按 reset_at 恢复，401/402 处理认证文件后解除</span></h2><div class="section-body"><div class="autoban-toolbar"><span id="autoban-scope" class="autoban-scope">显示 0 / 0 个自动禁用账号</span><select id="autoban-page-size" aria-label="自动禁用每页数量"><option value="10">10 / 页</option><option value="25">25 / 页</option><option value="50">50 / 页</option></select><button id="autoban-prev" class="ghost" aria-label="上一页自动禁用账号">上一页</button><span id="autoban-page-label" class="page-label">1 / 1</span><button id="autoban-next" class="ghost" aria-label="下一页自动禁用账号">下一页</button></div><div class="scroll autoban-table-wrap"><table><thead><tr><th>账号</th><th>AuthIndex</th><th>窗口</th><th>原因</th><th>封禁时间</th><th>解禁时间</th><th>剩余</th><th>5h</th><th>7d</th></tr></thead><tbody id="autobans"></tbody></table></div></div></section>
  <section class="section" style="margin-top:8px"><h2><span>最近请求</span><span class="mini">Codex 最近 30 条</span></h2><div class="scroll recent-table-wrap"><table><thead><tr><th>模型</th><th>耗时</th><th>Tokens</th><th>费用</th><th>详情</th></tr></thead><tbody id="recent"></tbody></table></div></section>
  </section>
  <section data-page="providers">
    <div class="command-grid">
      <section class="section"><h2><span>AI 接入点总览</span><span class="mini">不计入 Codex 账号池价格和额度</span></h2><div class="section-body"><div class="cards">
        <div class="metric" style="--accent:var(--blue)"><div class="label">请求数</div><div class="value" id="pm-requests">-</div><div class="sub" id="pm-success">成功率 -</div></div>
        <div class="metric" style="--accent:var(--cyan)"><div class="label">总 Token</div><div class="value" id="pm-total">-</div><div class="sub">其他 AI Provider 合计</div></div>
        <div class="metric" style="--accent:var(--blue)"><div class="label">费用估算</div><div class="value" id="pm-cost">-</div><div class="sub" id="pm-cost-sub">按模型价格估算</div></div>
        <div class="metric" style="--accent:var(--cyan)"><div class="label">输入 Token</div><div class="value" id="pm-input">-</div><div class="sub" id="pm-input-share">占比 -</div></div>
        <div class="metric" style="--accent:var(--violet)"><div class="label">输出 Token</div><div class="value" id="pm-output">-</div><div class="sub" id="pm-output-share">占比 -</div></div>
        <div class="metric" style="--accent:var(--orange)"><div class="label">缓存 Token</div><div class="value" id="pm-cache">-</div><div class="sub" id="pm-cache-share">缓存率 -</div></div>
        <div class="metric" style="--accent:var(--red)"><div class="label">429 次数</div><div class="value bad" id="pm-429">-</div><div class="sub">Provider 限流</div></div>
        <div class="metric" style="--accent:var(--blue)"><div class="label">接入点</div><div class="value" id="pm-providers">-</div><div class="sub">Provider / endpoint</div></div>
        <div class="metric" style="--accent:var(--cyan)"><div class="label">模型数</div><div class="value" id="pm-models">-</div><div class="sub">按模型聚合</div></div>
        <div class="metric" style="--accent:var(--violet)"><div class="label">Top 接入点</div><div class="value" id="pm-topshare">-</div><div class="sub">Token 集中度</div></div>
        <div class="metric" style="--accent:var(--blue)"><div class="label">平均耗时</div><div class="value" id="pm-latency">-</div><div class="sub" id="pm-latency-sub">慢请求 -</div></div>
        <div class="metric" style="--accent:var(--cyan)"><div class="label">首 Token</div><div class="value" id="pm-ttft">-</div><div class="sub" id="pm-ttft-sub">慢首包 -</div></div>
        <div class="metric" style="--accent:var(--violet)"><div class="label">输出速度</div><div class="value" id="pm-throughput">-</div><div class="sub">输出 Token / 秒</div></div>
      </div></div></section>
      <section class="section"><h2><span>Token 结构</span><span class="mini">其他 AI Provider</span></h2><div class="section-body"><div class="mix" id="provider-token-mix"></div></div></section>
    </div>
    <div class="layout">
      <section class="section"><h2><span>Provider / 接入点总览</span><span class="mini">按 Provider 名称聚合，不进入 Codex 账号池</span></h2><div class="scroll"><table><thead><tr><th>Provider</th><th>请求</th><th>成功率</th><th>性能</th><th>总 Token</th><th>费用</th><th>输入</th><th>输出</th><th>缓存</th><th>缓存率</th><th>账号数</th><th>模型数</th><th>429</th><th>最近</th></tr></thead><tbody id="providers"></tbody></table></div></section>
      <section class="section"><h2><span>用量趋势</span><span class="mini">其他 AI Provider</span></h2><div class="section-body"><svg id="provider-trend" class="chart" viewBox="0 0 900 270" preserveAspectRatio="none"></svg><div class="legend"><span><i class="dot" style="background:var(--blue)"></i>请求</span><span><i class="dot" style="background:var(--cyan)"></i>总 Token</span><span><i class="dot" style="background:var(--orange)"></i>输出 Token</span></div></div></section>
    </div>
    <section class="section" style="margin-top:8px"><h2><span>CPA 多 Key 用量</span><span class="mini">按 CPA 对外 Key 聚合模型、协议和 Token 额度</span></h2><div class="scroll key-summary-table-wrap"><table><thead><tr><th>Key</th><th>协议</th><th>接入点</th><th>请求</th><th>成功率</th><th>Token / 费用</th><th>模型数</th><th>429</th><th>最近</th></tr></thead><tbody id="key-summaries"></tbody></table></div></section>
    <section class="section" style="margin-top:8px"><h2><span>模型排行</span><span class="mini">其他 AI Provider</span></h2><div class="scroll model-table-wrap"><table><thead><tr><th>模型</th><th>别名</th><th>Provider</th><th>请求</th><th>总 Token</th><th>费用</th><th>性能</th><th>输入</th><th>输出</th><th>缓存</th><th>缓存率</th></tr></thead><tbody id="provider-models"></tbody></table></div></section>
    <section class="section" style="margin-top:8px"><h2><span>最近请求</span><span class="mini">其他 AI Provider 最近 30 条</span></h2><div class="scroll recent-table-wrap"><table><thead><tr><th>模型</th><th>耗时</th><th>Tokens</th><th>费用</th><th>详情</th></tr></thead><tbody id="provider-recent"></tbody></table></div></section>
  </section>
  <div id="provider-pages"></div>
</main>
<div id="auth-import-modal" class="modal-backdrop" hidden>
  <div class="modal-panel auth-import-panel" role="dialog" aria-modal="true" aria-labelledby="auth-import-title">
    <div class="modal-head"><h2 id="auth-import-title">非标准账号 JSON 批量导入</h2><button id="auth-import-close" class="icon-button ghost" type="button" aria-label="关闭账号 JSON 导入">×</button></div>
    <div class="modal-body">
      <label class="form-row auth-import-file-row"><span>文件</span><input id="auth-import-files" type="file" accept=".json,.txt,application/json,text/plain" multiple></label>
      <label class="form-row auth-import-text-row"><span>账号内容</span><textarea id="auth-import-text" spellcheck="false" placeholder="粘贴 ChatGPT Session、sub2api、9router、Codex auth.json、AxonHub、Codex-Manager 或卖家卡密文本。"></textarea></label>
      <label class="auth-import-overwrite"><input id="auth-import-overwrite" type="checkbox">覆盖同名 CPA 认证文件</label>
      <div class="modal-note">内容只发送到当前 CPA 插件进行本地转换，通过 <code>host.auth.save</code> 保存；预览结果不会返回或显示 Token。</div>
      <div id="auth-import-status" class="modal-status" role="status" aria-live="polite">等待粘贴或选择账号文件。</div>
      <div id="auth-import-results" class="auth-import-results"></div>
    </div>
    <div class="modal-actions">
      <button id="auth-import-clear" class="ghost" type="button">清空</button>
      <button id="auth-import-preview" class="ghost" type="button">识别预览</button>
      <button id="auth-import-commit" type="button">导入 CPA</button>
    </div>
  </div>
</div>
<div id="batch-proxy-modal" class="modal-backdrop" hidden>
  <div class="modal-panel" role="dialog" aria-modal="true" aria-labelledby="batch-proxy-title">
    <div class="modal-head"><h2 id="batch-proxy-title">批量写入 Codex 代理</h2><button id="batch-proxy-close" class="icon-button ghost" type="button" aria-label="关闭批量写入代理">×</button></div>
    <div class="modal-body">
      <label class="form-row"><span>代理地址</span><input id="batch-proxy-url" autocomplete="off" placeholder="socks5://username:password@proxy_ip:port/"></label>
      <div class="modal-note">只写入 Codex 认证文件的 <code>proxy_url</code> 字段。填写 <code>direct</code> 可批量直连，留空不会执行。</div>
      <div id="batch-proxy-status" class="modal-status" role="status" aria-live="polite">等待输入代理地址。</div>
    </div>
    <div class="modal-actions">
      <button id="batch-proxy-clear" class="ghost danger-ghost" type="button">清除所有代理</button>
      <button id="batch-proxy-preview" class="ghost" type="button">预览数量</button>
      <button id="batch-proxy-apply" type="button">应用</button>
    </div>
  </div>
</div>
<div id="invalid-auth-modal" class="modal-backdrop" hidden>
  <div class="modal-panel invalid-auth-panel" role="dialog" aria-modal="true" aria-labelledby="invalid-auth-title">
    <div class="modal-head"><div class="modal-title-actions"><h2 id="invalid-auth-title">管理 401 失效账号</h2><button id="invalid-auth-delete-all" class="ghost danger-ghost compact-danger" type="button">处理所有 401 账号</button></div><button id="invalid-auth-close" class="icon-button ghost" type="button" aria-label="关闭 401 管理">×</button></div>
    <div class="modal-body">
      <div class="invalid-auth-toolbar"><span id="invalid-auth-summary">已选 0 / 共 0 个</span><button id="invalid-auth-refresh" class="ghost" type="button">刷新</button></div>
      <div id="invalid-auth-oauth-url" class="modal-note" hidden></div>
      <div id="invalid-auth-status" class="modal-status" role="status" aria-live="polite">等待选择 401 账号。</div>
      <div id="invalid-auth-list" class="invalid-auth-list"></div>
      <div class="invalid-auth-pager"><button id="invalid-auth-prev" class="ghost" type="button">上一页</button><span id="invalid-auth-page-label" class="page-label">1 / 1</span><button id="invalid-auth-next" class="ghost" type="button">下一页</button></div>
    </div>
    <div class="modal-actions">
      <button id="invalid-auth-select-page" class="ghost" type="button">全选当前页</button>
      <button id="invalid-auth-delete-selected" class="ghost danger-ghost" type="button">处理选中</button>
      <button id="invalid-auth-close-bottom" class="ghost" type="button">关闭</button>
    </div>
  </div>
</div>
<div id="workspace-deactivated-modal" class="modal-backdrop" hidden>
  <div class="modal-panel workspace-deactivated-panel invalid-auth-panel" role="dialog" aria-modal="true" aria-labelledby="workspace-deactivated-title">
    <div class="modal-head"><div class="modal-title-actions"><h2 id="workspace-deactivated-title">管理 402 工作区失效账号</h2><button id="workspace-deactivated-delete-all" class="ghost danger-ghost compact-danger" type="button">删除所有 402 账号</button></div><button id="workspace-deactivated-close" class="icon-button ghost" type="button" aria-label="关闭 402 管理">×</button></div>
    <div class="modal-body">
      <div class="invalid-auth-toolbar"><span id="workspace-deactivated-summary">已选 0 / 共 0 个</span><button id="workspace-deactivated-refresh" class="ghost" type="button">刷新</button></div>
      <div id="workspace-deactivated-status" class="modal-status" role="status" aria-live="polite">等待选择 402 账号。</div>
      <div id="workspace-deactivated-list" class="invalid-auth-list workspace-deactivated-list"></div>
      <div class="invalid-auth-pager"><button id="workspace-deactivated-prev" class="ghost" type="button">上一页</button><span id="workspace-deactivated-page-label" class="page-label">1 / 1</span><button id="workspace-deactivated-next" class="ghost" type="button">下一页</button></div>
    </div>
    <div class="modal-actions">
      <button id="workspace-deactivated-select-page" class="ghost" type="button">全选当前页</button>
      <button id="workspace-deactivated-delete-selected" class="ghost danger-ghost" type="button">删除选中</button>
      <button id="workspace-deactivated-close-bottom" class="ghost" type="button">关闭</button>
    </div>
  </div>
</div>
<div id="autoban-release-modal" class="modal-backdrop" hidden>
  <div class="modal-panel autoban-release-panel invalid-auth-panel" role="dialog" aria-modal="true" aria-labelledby="autoban-release-title">
    <div class="modal-head"><div class="modal-title-actions"><h2 id="autoban-release-title">管理 429 禁用账号</h2><button id="autoban-release-all" class="ghost danger-ghost compact-danger" type="button">解除所有 429</button></div><button id="autoban-release-close" class="icon-button ghost" type="button" aria-label="关闭 429 管理">×</button></div>
    <div class="modal-body">
      <div class="invalid-auth-toolbar"><span id="autoban-release-summary">已选 0 / 共 0 个</span><button id="autoban-release-refresh" class="ghost" type="button">刷新</button></div>
      <div id="autoban-release-status" class="modal-status" role="status" aria-live="polite">等待选择 429 账号。</div>
      <div id="autoban-release-list" class="invalid-auth-list autoban-release-list"></div>
      <div class="invalid-auth-pager"><button id="autoban-release-prev" class="ghost" type="button">上一页</button><span id="autoban-release-page-label" class="page-label">1 / 1</span><button id="autoban-release-next" class="ghost" type="button">下一页</button></div>
    </div>
    <div class="modal-actions">
      <button id="autoban-release-select-page" class="ghost" type="button">全选当前页</button>
      <button id="autoban-release-selected" class="ghost danger-ghost" type="button">解除选中</button>
      <button id="autoban-release-close-bottom" class="ghost" type="button">关闭</button>
    </div>
  </div>
</div>
<div id="quota-activation-modal" class="modal-backdrop" hidden>
  <div class="modal-panel quota-activation-panel" role="dialog" aria-modal="true" aria-labelledby="quota-activation-title">
    <div class="modal-head"><h2 id="quota-activation-title">一次性启动 Codex 额度窗口</h2><button id="quota-activation-close" class="icon-button ghost" type="button" aria-label="关闭一次性额度启动">×</button></div>
    <div class="modal-body">
      <div class="modal-note">先预览，不会发送模型请求。默认仅选择至少一个明确上报窗口且所有上报窗口均完全新鲜的账号；执行时会发送一个最小真实 Codex 请求，不保证恰好消耗一个 Token。</div>
      <label class="quota-activation-force"><input id="quota-activation-force" type="checkbox">强制恢复模式（必须明确选择账号，可绕过新鲜窗口检查）</label>
      <div id="quota-activation-status" class="modal-status" role="status" aria-live="polite">等待预览新鲜账号。</div>
      <div class="quota-activation-summary" id="quota-activation-summary"></div>
      <div class="quota-activation-table-wrap"><table><thead><tr><th>选择</th><th>账号</th><th>Primary 上报窗口（前 → 后）</th><th>Secondary 上报窗口（前 → 后）</th><th>判定 / 结果</th></tr></thead><tbody id="quota-activation-results"></tbody></table></div>
      <div class="quota-activation-pagination"><button id="quota-activation-prev" class="ghost" type="button">上一页</button><span id="quota-activation-page">1 / 1</span><button id="quota-activation-next" class="ghost" type="button">下一页</button></div>
      <label class="quota-activation-ack"><input id="quota-activation-ack" type="checkbox">我确认执行会消耗少量真实 Codex 额度，并且成功请求无法撤销。</label>
    </div>
    <div class="modal-actions">
      <button id="quota-activation-preview" class="ghost" type="button">预览新鲜账号</button>
      <button id="quota-activation-run" type="button" disabled>仅执行选中账号一次</button>
      <button id="quota-activation-close-bottom" class="ghost" type="button">关闭</button>
    </div>
  </div>
</div>
<div id="log-export-modal" class="modal-backdrop" hidden>
  <div class="modal-panel log-export-panel" role="dialog" aria-modal="true" aria-labelledby="log-export-title">
    <div class="modal-head"><h2 id="log-export-title">导出日志</h2><button id="log-export-close" class="icon-button ghost" type="button" aria-label="关闭导出日志">×</button></div>
    <div class="modal-body log-export-grid">
      <label class="form-row"><span>账号</span><select id="log-export-account"><option value="">全部账号</option></select></label>
      <label class="form-row"><span>接入点</span><select id="log-export-provider"><option value="">全部接入点</option></select></label>
      <label class="form-row"><span>日期</span><input id="log-export-date" type="date"></label>
      <label class="form-row"><span>模型</span><select id="log-export-model"><option value="">全部模型</option></select></label>
      <label class="form-row"><span>状态</span><select id="log-export-status"><option value="all">全部状态</option><option value="success">成功</option><option value="failed">失败</option><option value="401">401</option><option value="402">402</option><option value="403">403</option><option value="429">429</option><option value="5xx">5xx</option></select></label>
      <label class="form-row"><span>格式</span><select id="log-export-format"><option value="csv">CSV</option><option value="json">JSON</option></select></label>
      <div id="log-export-status-text" class="modal-status" role="status" aria-live="polite">按当前页面范围导出请求日志。</div>
    </div>
    <div class="modal-actions">
      <button id="log-export-apply" type="button">导出日志</button>
      <button id="log-export-close-bottom" class="ghost" type="button">关闭</button>
    </div>
  </div>
</div>
<script>
`

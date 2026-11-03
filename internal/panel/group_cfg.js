/* 分组配置共享组件（window.GroupCfg）
 * ---------------------------------------------------------------------------
 * 主页的分组卡片和号池分组页做的是同一件事：查看/轮换分组密钥、配代理、开关
 * 共享池、编辑提示词规则。这些以前只长在分组页上，主页只能跳过去看。现在两边
 * 都渲染 GroupCfg.cards()，用的是同一份 DOM 与同一套事件，改一次两页同步生效。
 *
 * 宿主页面只需：
 *   1) 引入 group_cfg.css / group_cfg.js
 *   2) document.body 里插入 GroupCfg.modals()（三个配置弹窗）
 *   3) GroupCfg.mount({...}) 注入上下文
 *   4) 在要放配置卡的位置调 GroupCfg.render(hostId, group, opts)
 *
 * 上下文（mount 的参数）：
 *   api(path, opt)      调面板 API，失败时 throw（err.body 里带服务端报文）
 *   toast(msg, kind)    提示条
 *   getGroup()          当前分组对象；公共池返回 null
 *   getPublicProxy()    公共池代理；未配置返回 null
 *   reload()            配置变更后刷新宿主页（号池列表 / 分组徽标要跟着变）
 *   confirm(title, text, okText, fn)  二次确认；fn 里做真正的删除请求
 *   copyText(text)      复制到剪贴板
 *
 * 约定：所有按钮的 data-act 以 gc- 开头，宿主页的委托（data-act="acc" /
 * "ge…" 之类）不会误命中；模块自己挂 document 级的 click / change 委托。
 */
(function () {
  'use strict';

  function $(id) { return document.getElementById(id); }
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function openVeil(id) { const el = $(id); if (el) el.classList.add('on'); }
  function closeVeil(id) { const el = $(id); if (el) el.classList.remove('on'); }

  let E = null;                        // 宿主上下文
  const revealed = {};                 // 分组 id → 已拉取的完整密钥（只在内存，刷新页面即清）
  let proxyGroup = 'public';           // 代理弹窗作用域：'public' 或分组 id
  let proxyType = 'socks5h';
  let promptGroup = '';                // 提示词弹窗当前分组 id
  let promptDraft = { system: [], user: [], replaces: [] };
  let lastHost = '';                   // 上次渲染的容器，配置变更后就地重绘
  let lastOpts = null;

  function cur() { return E && E.getGroup ? E.getGroup() : null; }
  function curPublicProxy() { return (E && E.getPublicProxy && E.getPublicProxy()) || null; }
  function toast(m, k) { if (E && E.toast) E.toast(m, k); }

  /* ── 四张配置卡 ──────────────────────────────────────────────────────── */
  function keyCard(g) {
    const shown = revealed[g.id] || '';
    const hint = shown ? esc(shown) : (g.has_key ? esc(g.key_hint || '已生成') : '尚未生成');
    const btns = g.has_key ? '<button class="xs ghost" data-act="gc-reveal-key">' + (shown ? '隐藏' : '显示') + '</button><button class="xs ghost" data-act="gc-copy-key">复制</button>' : '';
    return '<div class="card"><div class="card-h"><h3>分组密钥</h3><span class="tag accent">独立鉴权</span></div>' +
      '<div class="kline"><code>' + hint + '</code>' + btns + '<button class="xs" data-act="gc-rotate-key">' + (g.has_key ? '轮换' : '生成') + '</button></div>' +
      '<div class="hint">点击「显示」查看完整密钥；旧版本创建的分组只存了哈希，需轮换一次后才能查看。</div><div class="card-f"><span>Bearer 此密钥只能访问本组</span></div></div>';
  }
  function proxyCard(isPub, p) {
    const scopeAttr = isPub ? ' data-gc-public="1"' : '';
    const clr = p ? '<button class="xs ghost danger" data-act="gc-clear-proxy"' + scopeAttr + '>清除</button>' : '';
    return '<div class="card"><div class="card-h"><h3>网络代理</h3>' + (p ? '<span class="tag ok">已配置</span>' : '<span class="tag mute">未配置</span>') + '</div>' +
      (p ? '<div class="proxy-line"><span class="proto ' + esc(p.type) + '">' + esc(String(p.type).toUpperCase()) + '</span><code>' + esc(p.host) + ':' + p.port + '</code></div><div class="hint">覆盖聊天、OAuth、余额、模型探测与全部任务。</div>'
        : '<div class="hint">' + (isPub ? '未配置时公共池（未分组账号）使用服务器直连出口；共享分组账号仍走各自分组代理。' : '未配置时本组使用服务器直连出口。') + '</div>') +
      '<div class="card-f"><span>' + (isPub ? '未分组账号全部出站请求' : '本组全部出站请求') + '</span><span class="grow"></span>' + clr + '<button class="xs ghost" data-act="gc-edit-proxy"' + scopeAttr + '>' + (p ? '编辑' : '配置') + '</button></div></div>';
  }
  function shareCard(g) {
    return '<div class="card"><div class="card-h"><h3>共享池</h3><span class="tag ' + (g.shared_pool || g.call_public ? 'warn' : 'mute') + '">' + (g.shared_pool || g.call_public ? '已开启共享' : '双向隔离') + '</span></div>' +
      '<label class="switch-row"><span class="sw-text"><b>允许本组账号进入公共池</b><em>默认关闭</em></span><input type="checkbox" data-act="gc-toggle-shared"' + (g.shared_pool ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
      '<label class="switch-row"><span class="sw-text"><b>允许本组调用公共池账号</b><em>本组与公共池账号共用候选；默认关闭</em></span><input type="checkbox" data-act="gc-toggle-call"' + (g.call_public ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
      '<div class="hint">' + (g.shared_pool ? '公共池可命中本组账号。' : '') + (g.call_public ? '本组账号与公共池账号进同一候选集，按权重一起抽签。' : '') + (!g.shared_pool && !g.call_public ? '本组与公共池双向隔离。' : '') + '</div></div>';
  }
  function promptCard(g) {
    const on = !!g.prompt_enabled;
    const n = g.prompt_chars || 0;
    const ec = g.prompt_entries || 0, rc = g.prompt_replaces || 0;
    const bits = [];
    if (ec) bits.push(ec + ' 条提示词');
    if (rc) bits.push(rc + ' 条替换');
    return '<div class="card"><div class="card-h"><h3>提示词规则</h3>' + (on ? '<span class="tag accent">已启用 · ' + (bits.length ? bits.join(' · ') : '空规则') + '</span>' : '<span class="tag mute">继承全局</span>') + '</div>' +
      '<label class="switch-row"><span class="sw-text"><b>启用本组专属提示词规则</b><em>' + (on ? '系统/用户提示词与关键词替换对本组密钥请求生效' : '沿用配置页 prompt.mode 设置') + '</em></span><input type="checkbox" data-act="gc-toggle-prompt"' + (on ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
      (on && g.prompt_preview ? '<div class="hint" style="margin-top:8px">系统提示词预览：<code style="font:11px var(--mono);color:var(--ink-2)">' + esc(g.prompt_preview) + '</code></div>' : '') +
      '<div class="card-f"><span>只影响本组密钥的 /v1/chat/completions 与 /v1/responses' + (on && n ? ' · ' + n + ' 字' : '') + '</span><span class="grow"></span><button class="xs ghost" data-act="gc-edit-prompt">' + (on ? '编辑' : '配置') + '</button></div></div>';
  }
  /* cards 返回卡片组 HTML；o.public 为真表示公共池（无密钥/共享卡，代理作用域是 public）。 */
  function cards(g, o) {
    o = o || {};
    if (!g || o.public) {
      const p = o.proxy !== undefined ? o.proxy : curPublicProxy();
      return '<div class="gcfg">' +
        '<div class="card"><div class="card-h"><h3>公共池</h3><span class="tag mute">config.api_key</span></div><div class="hint">公共池沿用现有主密钥；分组密钥不能反向访问公共池。</div></div>' +
        proxyCard(true, p) +
        '<div class="card"><div class="card-h"><h3>共享来源</h3><span class="tag ' + (o.sharedCount ? 'warn' : 'mute') + '">' + (o.sharedCount || 0) + ' 个分组</span></div><div class="hint">只有开启「进入公共池」的分组才会出现在这里。</div></div></div>';
    }
    const p = o.proxy !== undefined ? o.proxy : (g.proxy || null);
    return '<div class="gcfg">' + keyCard(g) + proxyCard(false, p) + shareCard(g) + promptCard(g) + '</div>';
  }
  /* render 把卡片画进宿主的一个容器；后续配置变更时 refresh() 会就地重绘。 */
  function render(hostId, g, o) {
    const el = $(hostId);
    o = o || {};
    // 没有分组又不是公共池（例如主页「新建分组」弹窗）：清空容器并解除绑定，
    // 否则后续 refresh() 会把公共池卡片画进这个弹窗。
    if (!g && !o.public) { if (el) el.innerHTML = ''; lastHost = ''; lastOpts = null; return; }
    lastHost = hostId; lastOpts = o;
    if (el) el.innerHTML = cards(g, o);
  }
  function refresh() {
    if (!lastHost) return;
    render(lastHost, cur(), lastOpts);
  }

  /* ── 三个配置弹窗（由宿主页面插入一次）──────────────────────────────── */
  function modals() {
    return '' +
      '<div class="veil gc-veil" id="gcProxyVeil"><div class="dlg" style="width:600px">' +
      '<header><h3>网络代理</h3><div class="hint" id="gcProxyScope"></div></header>' +
      '<div class="body"><div class="seg" id="gcProxyTypes"><button data-type="http" data-act="gc-proxy-type">HTTP</button><button data-type="socks5" data-act="gc-proxy-type">SOCKS5</button><button data-type="socks5h" data-act="gc-proxy-type">SOCKS5H</button></div>' +
      '<div class="proxy-hint" id="gcProxyHint"></div>' +
      '<div class="grid2"><label class="fld"><span class="lb">主机</span><input id="gcProxyHost" placeholder="127.0.0.1"></label><label class="fld"><span class="lb">端口</span><input id="gcProxyPort" type="number" placeholder="1080"></label></div>' +
      '<div class="grid2"><label class="fld"><span class="lb">用户名（可选）</span><input id="gcProxyUser"></label><label class="fld"><span class="lb">密码（可选）</span><input id="gcProxyPass" type="password"></label></div>' +
      '<div class="testbox" id="gcProxyTestBox" hidden></div><div class="hint">代理不可用时本组请求直接失败，不回落直连。</div></div>' +
      '<footer><button class="ghost" data-act="gc-proxy-test">测试连接</button><button class="ghost danger" id="gcProxyClear" data-act="gc-clear-proxy" hidden>清除代理（直连）</button><span class="grow"></span><button data-act="gc-close" data-target="gcProxyVeil">取消</button><button class="primary" data-act="gc-proxy-save">保存代理</button></footer>' +
      '</div></div>' +

      '<div class="veil gc-veil" id="gcPromptVeil"><div class="dlg" style="width:820px">' +
      '<header><h3>提示词规则 <span id="gcPromptGroupName" class="hint"></span></h3><div class="hint">开启后，本组密钥的 /v1/chat/completions 与 /v1/responses 请求按以下规则改写：系统提示词删除客户端全部 system / developer 后按配置顺序拼接注入；用户提示词作为 user 消息插到对话最前；关键词替换对请求消息文本生效（先替换、再注入，不会改动网关自己的提示词）。保存后下一个请求立即生效，无需重启。</div></header>' +
      '<div class="body"><label class="switch-row" style="margin-bottom:2px"><span class="sw-text"><b>启用本组提示词规则</b><em>关闭时保留全部规则但不再生效</em></span><input type="checkbox" id="gcPromptEnabled"><span class="track"><i></i></span></label>' +
      '<div class="pr-sec"><div class="pr-sec-h"><b>系统提示词</b><em>多条按顺序拼接（空行分隔），整体替换客户端 system</em><span class="grow"></span><button class="xs ghost" data-act="gc-prompt-add" data-role="system">＋ 添加</button></div><div id="gcPromptSystemList"></div></div>' +
      '<div class="pr-sec"><div class="pr-sec-h"><b>用户提示词</b><em>作为 user 消息注入到对话最前</em><span class="grow"></span><button class="xs ghost" data-act="gc-prompt-add" data-role="user">＋ 添加</button></div><div id="gcPromptUserList"></div></div>' +
      '<div class="pr-sec"><div class="pr-sec-h"><b>关键词替换</b><em>请求里出现的关键词 → 替换为（留空 = 删除该词）</em><span class="grow"></span><button class="xs ghost" data-act="gc-prompt-add" data-role="replace">＋ 添加</button></div><div id="gcPromptReplaceList"></div></div>' +
      '<div class="hint" id="gcPromptCount" style="margin-top:10px">系统 0 条 · 用户 0 条 · 关键词 0 条 · 共 0 字</div></div>' +
      '<footer><span class="grow"></span><button data-act="gc-close" data-target="gcPromptVeil">取消</button><button class="primary" data-act="gc-prompt-save">保存（立即生效）</button></footer>' +
      '</div></div>' +

      '<div class="veil gc-veil" id="gcKeyVeil" data-lock="1"><div class="dlg" style="width:560px">' +
      '<header><h3>分组密钥</h3><div class="hint">之后可在分组配置卡上点「显示」随时查看；重新生成后旧密钥立即失效。</div></header>' +
      '<div class="body"><div class="key-big" id="gcKeyBig"></div><div class="modal-warn">此密钥只能访问本组账号，不能访问公共池或其他分组。</div></div>' +
      '<footer><button class="ghost" data-act="gc-copy-key">复制密钥</button><span class="grow"></span><button class="primary" data-act="gc-close" data-target="gcKeyVeil">我已保存</button></footer>' +
      '</div></div>';
  }

  /* ── 代理弹窗 ────────────────────────────────────────────────────────── */
  function proxyHint(t) {
    if (t === 'http') return 'HTTP CONNECT；目标域名由本机解析。';
    if (t === 'socks5') return 'SOCKS5（本地 DNS）：先解析目标域名再由代理转发 IP。';
    return 'SOCKS5H（远程 DNS）：域名交给代理解析，适合避免 DNS 泄漏。';
  }
  function updateProxyUI() {
    document.querySelectorAll('#gcProxyTypes button').forEach(function (b) { b.classList.toggle('on', b.dataset.type === proxyType); });
    $('gcProxyHint').textContent = proxyHint(proxyType);
    const p = $('gcProxyPort');
    if (!p.value || p.value === '1080' || p.value === '8080') p.value = proxyType === 'http' ? 8080 : 1080;
  }
  function openProxy(g) {
    proxyGroup = g ? g.id : 'public';
    const p = g ? (g.proxy || null) : curPublicProxy();
    $('gcProxyScope').textContent = g
      ? '本组全部出站请求（聊天、OAuth、余额、模型探测、签到/保活/旅行/活跃任务）都走此代理。'
      : '公共池（未分组账号）全部出站请求都走此代理；共享分组账号仍走各自分组代理。';
    $('gcProxyClear').hidden = !p;
    proxyType = p ? p.type : 'socks5h';
    $('gcProxyHost').value = p ? p.host : '';
    $('gcProxyPort').value = p ? p.port : 1080;
    $('gcProxyUser').value = p ? (p.username || '') : '';
    $('gcProxyPass').value = '';
    $('gcProxyTestBox').hidden = true; $('gcProxyTestBox').className = 'testbox';
    updateProxyUI(); openVeil('gcProxyVeil');
  }
  function proxyBody() {
    return {
      type: proxyType,
      host: $('gcProxyHost').value.trim(),
      port: parseInt($('gcProxyPort').value, 10) || 0,
      username: $('gcProxyUser').value.trim(),
      password: $('gcProxyPass').value,
    };
  }
  /* 测试结果固定两行：连通性（耗时 / 状态 / 目标站点）+ 出口 IP 与归属地。
     出口 IP 由服务端经同一代理访问 ipip.net 取回——CSP 是 connect-src 'self'，
     前端不能直连第三方，也不该让浏览器实测（那样测的是浏览器出口）。 */
  function renderProxyTest(box, r) {
    box.className = 'testbox ' + (r.ok ? 'ok' : 'err');
    box.textContent = '';
    const l1 = document.createElement('div');
    l1.textContent = r.ok
      ? '连接成功 · ' + r.latency_ms + ' ms · HTTP ' + r.status + ' · ' + r.target
      : '连接失败：' + (r.error || '未知错误');
    const l2 = document.createElement('div');
    if (r.exit_ip) {
      l2.className = 'l2';
      l2.innerHTML = '出口 IP：<b>' + esc(r.exit_ip) + '</b>' + (r.exit_region ? ' · ' + esc(r.exit_region) : ' · 未返回归属地') + (r.ip_latency_ms != null ? ' · ' + r.ip_latency_ms + ' ms' : '');
    } else {
      l2.className = 'l2 warn';
      l2.textContent = '出口 IP：未取到' + (r.ip_error ? '（' + r.ip_error + '）' : '');
    }
    box.append(l1, l2);
  }
  function scopeGroup() { return proxyGroup === 'public' ? null : cur(); }

  /* ── 提示词规则编辑器 ────────────────────────────────────────────────── */
  const PROMPT_ROLE_NAME = { system: '系统提示词', user: '用户提示词' };
  function promptTextRow(kind, text, idx) {
    const ph = kind === 'system' ? '例如：你是一个只回答数据查询问题的助手，回答直接、简短。' : '例如：先用一句话给结论，再列要点。';
    return '<div class="pr-row"><textarea class="code-area" data-pr-text data-kind="' + kind + '" data-idx="' + idx + '" rows="3" placeholder="' + ph + '">' + esc(text || '') + '</textarea>' +
      '<button class="xs ghost danger pr-del" data-act="gc-prompt-del" data-kind="' + kind + '" data-idx="' + idx + '">删除</button></div>';
  }
  function promptReplaceRow(r, idx) {
    const scope = r.scope || 'all';
    const opt = function (v, t) { return '<option value="' + v + '"' + (scope === v ? ' selected' : '') + '>' + t + '</option>'; };
    return '<div class="pr-row pr-kv">' +
      '<input data-pr-match data-idx="' + idx + '" placeholder="关键词" value="' + esc(r.match || '') + '">' +
      '<span class="pr-arrow">→</span>' +
      '<input data-pr-with data-idx="' + idx + '" placeholder="替换为（留空 = 删除）" value="' + esc(r.with || '') + '">' +
      '<select data-pr-scope data-idx="' + idx + '">' + opt('all', '全部消息') + opt('system', '仅 system') + opt('user', '仅 user') + '</select>' +
      '<button class="xs ghost danger pr-del" data-act="gc-prompt-del" data-kind="replace" data-idx="' + idx + '">删除</button></div>';
  }
  function renderPromptRows() {
    const d = promptDraft;
    $('gcPromptSystemList').innerHTML = d.system.length ? d.system.map(function (x, i) { return promptTextRow('system', x, i); }).join('') : '<div class="pr-empty">还没有' + PROMPT_ROLE_NAME.system + '，点右上角「添加」</div>';
    $('gcPromptUserList').innerHTML = d.user.length ? d.user.map(function (x, i) { return promptTextRow('user', x, i); }).join('') : '<div class="pr-empty">还没有' + PROMPT_ROLE_NAME.user + '，点右上角「添加」</div>';
    $('gcPromptReplaceList').innerHTML = d.replaces.length ? d.replaces.map(function (x, i) { return promptReplaceRow(x, i); }).join('') : '<div class="pr-empty">还没有关键词替换规则，点右上角「添加」</div>';
  }
  /* 增删前先把当前 DOM 收进 draft：否则「删第 2 条」时第 3 条刚输入的内容会被
     上一轮 DOM 覆盖（行下标整体前移）。 */
  function readPromptRows() {
    const d = promptDraft;
    d.system = Array.prototype.map.call(document.querySelectorAll('#gcPromptSystemList [data-pr-text]'), function (el) { return el.value; });
    d.user = Array.prototype.map.call(document.querySelectorAll('#gcPromptUserList [data-pr-text]'), function (el) { return el.value; });
    d.replaces = Array.prototype.map.call(document.querySelectorAll('#gcPromptReplaceList .pr-row'), function (row) {
      return {
        match: row.querySelector('[data-pr-match]').value,
        with: row.querySelector('[data-pr-with]').value,
        scope: row.querySelector('[data-pr-scope]').value,
      };
    });
  }
  function promptCounts() {
    const d = promptDraft;
    const sys = d.system.filter(function (s) { return s.trim(); }).length;
    const usr = d.user.filter(function (s) { return s.trim(); }).length;
    const rep = d.replaces.filter(function (r) { return (r.match || '').trim(); }).length;
    const chars = d.system.concat(d.user).reduce(function (n, s) { return n + (s || '').length; }, 0);
    return { sys: sys, usr: usr, rep: rep, chars: chars };
  }
  function updatePromptCount() {
    const c = promptCounts();
    $('gcPromptCount').textContent = '系统 ' + c.sys + ' 条 · 用户 ' + c.usr + ' 条 · 关键词 ' + c.rep + ' 条 · 共 ' + c.chars + ' 字';
  }
  /* 列表接口只回条数与预览，正文在打开编辑器时按需 GET 一次。
     forceEnable：从卡片开关「开启」进入时强制勾选，用户看到内容后再保存。 */
  async function openPrompt(g, forceEnable) {
    if (!g) return;
    promptGroup = g.id;
    promptDraft = { system: [], user: [], replaces: [] };
    $('gcPromptGroupName').textContent = '· ' + g.name;
    const r = await E.api('groups/' + g.id + '/prompt');
    const entries = Array.isArray(r.entries) ? r.entries : [];
    entries.forEach(function (e) {
      if (!e) return;
      if (e.role === 'user') promptDraft.user.push(e.text || ''); else promptDraft.system.push(e.text || '');
    });
    // 旧版只有单条正文：补成一条系统提示词，保证编辑器打开就能看到原文。
    if (!entries.length && r.text) promptDraft.system.push(r.text);
    if (!promptDraft.system.length && !promptDraft.user.length) promptDraft.system.push('');
    promptDraft.replaces = (Array.isArray(r.replaces) ? r.replaces : []).map(function (x) {
      return { match: (x && x.match) || '', with: (x && x.with) || '', scope: (x && x.scope) || 'all' };
    });
    $('gcPromptEnabled').checked = forceEnable ? true : !!r.enabled;
    renderPromptRows();
    updatePromptCount();
    openVeil('gcPromptVeil');
  }
  async function savePrompt() {
    const g = cur();
    if (!g) return;
    readPromptRows();
    const d = promptDraft;
    const entries = [];
    d.system.forEach(function (x) { if (x.trim()) entries.push({ role: 'system', text: x }); });
    d.user.forEach(function (x) { if (x.trim()) entries.push({ role: 'user', text: x }); });
    const replaces = d.replaces.filter(function (r) { return (r.match || '').trim(); }).map(function (r) { return { match: r.match, with: r.with, scope: r.scope }; });
    const enabled = $('gcPromptEnabled').checked;
    if (enabled && !entries.length && !replaces.length) { toast('已开启开关，请至少填写一条提示词或关键词替换（或关闭开关再保存）', 'err'); return; }
    try {
      await E.api('groups/' + g.id + '/prompt', { method: 'PUT', body: JSON.stringify({ enabled: enabled, entries: entries, replaces: replaces }) });
    } catch (e) {
      refresh(); // 保存失败：卡片开关退回服务端真实状态
      throw e;
    }
    closeVeil('gcPromptVeil'); await E.reload(); refresh();
    toast(enabled ? '已保存，下一个请求立即生效' : '已关闭本组提示词规则，恢复沿用全局设置', enabled ? 'ok' : 'warn');
  }

  /* ── 事件 ────────────────────────────────────────────────────────────── */
  async function handle(act, el) {
    if (act === 'gc-close') { closeVeil(el.dataset.target); return; }
    if (act === 'gc-reveal-key') {
      const g = cur(); if (!g) return;
      if (revealed[g.id]) delete revealed[g.id];
      else revealed[g.id] = (await E.api('groups/' + g.id + '/key')).key;
      refresh(); return;
    }
    if (act === 'gc-copy-key') {
      const g = cur(); if (!g) return;
      const id = el.dataset.g || (g && g.id);
      const k = (g && revealed[g.id]) || (await E.api('groups/' + id + '/key')).key;
      E.copyText(k); return;
    }
    if (act === 'gc-rotate-key') {
      const g = cur(); if (!g) return;
      const r = await E.api('groups/' + g.id + '/key/rotate', { method: 'POST' });
      delete revealed[g.id];
      $('gcKeyBig').textContent = r.key; openVeil('gcKeyVeil');
      await E.reload(); refresh();
      toast('新密钥已生成，旧密钥已失效', 'warn'); return;
    }
    if (act === 'gc-clear-proxy') {
      const g = el.dataset.gcPublic ? null : scopeGroup();
      const name = g ? '「' + g.name + '」' : '公共池';
      E.confirm('清除代理', '清除后' + name + '的出站请求将改为服务器直连。', '清除', async function () {
        await E.api('groups/' + (g ? g.id : 'public') + '/proxy', { method: 'DELETE' });
        closeVeil('gcProxyVeil'); await E.reload(); refresh(); toast('代理已清除，恢复直连', 'ok');
      });
      return;
    }
    if (act === 'gc-edit-proxy') { openProxy(el.dataset.gcPublic ? null : cur()); return; }
    if (act === 'gc-proxy-type') { proxyType = el.dataset.type; updateProxyUI(); return; }
    if (act === 'gc-proxy-save') {
      await E.api('groups/' + proxyGroup + '/proxy', { method: 'PUT', body: JSON.stringify(proxyBody()) });
      closeVeil('gcProxyVeil'); await E.reload(); refresh(); toast('代理已保存', 'ok'); return;
    }
    if (act === 'gc-proxy-test') {
      const box = $('gcProxyTestBox'); box.hidden = false; box.className = 'testbox'; box.textContent = '正在测试…（连通性 + 出口 IP）';
      const r = await E.api('groups/' + proxyGroup + '/proxy/test', { method: 'POST', body: JSON.stringify(proxyBody()) });
      renderProxyTest(box, r); return;
    }
    if (act === 'gc-edit-prompt') { await openPrompt(cur()); return; }
    if (act === 'gc-prompt-save') { await savePrompt(); return; }
    if (act === 'gc-prompt-add') {
      readPromptRows();
      const kind = el.dataset.role;
      if (kind === 'replace') promptDraft.replaces.push({ match: '', with: '', scope: 'all' });
      else if (kind === 'user') promptDraft.user.push('');
      else promptDraft.system.push('');
      renderPromptRows(); updatePromptCount();
      const list = $(kind === 'replace' ? 'gcPromptReplaceList' : (kind === 'user' ? 'gcPromptUserList' : 'gcPromptSystemList'));
      const focus = list.querySelectorAll('input,textarea');
      if (focus.length) focus[focus.length - 1].focus();
      return;
    }
    if (act === 'gc-prompt-del') {
      readPromptRows();
      const kind = el.dataset.kind, idx = Number(el.dataset.idx) || 0;
      if (kind === 'replace') promptDraft.replaces.splice(idx, 1);
      else if (kind === 'user') promptDraft.user.splice(idx, 1);
      else promptDraft.system.splice(idx, 1);
      renderPromptRows(); updatePromptCount(); return;
    }
  }

  async function onClick(ev) {
    const el = ev.target && ev.target.closest ? ev.target.closest('[data-act]') : null;
    if (!el) return;
    const act = el.dataset.act || '';
    if (act.indexOf('gc-') !== 0) return;
    ev.preventDefault();
    try { await handle(act, el); }
    catch (e) { toast((e && e.message) || String(e), 'err'); }
  }

  /* 开关只认 change：点标签文字也会触发 change，用 click 会被触发两次。 */
  async function onChange(ev) {
    const el = ev.target;
    const act = (el && el.dataset && el.dataset.act) || '';
    if (act !== 'gc-toggle-shared' && act !== 'gc-toggle-call' && act !== 'gc-toggle-prompt') return;
    try {
      const g = cur();
      if (!g) return;
      if (act === 'gc-toggle-prompt') {
        if (el.checked) { await openPrompt(g, true); return; } // 开启必须先确认内容
        // 关闭时把规则原样回传：只发 enabled=false 的旧版报文会走兼容分支、
        // 清掉新版的 entries/replaces（下次开启变成空规则）。
        const cur1 = await E.api('groups/' + g.id + '/prompt');
        await E.api('groups/' + g.id + '/prompt', { method: 'PUT', body: JSON.stringify({ enabled: false, entries: cur1.entries || [], replaces: cur1.replaces || [] }) });
        await E.reload(); refresh();
        toast('已关闭本组提示词规则，恢复沿用全局设置', 'warn');
        return;
      }
      const body = { name: g.name, color: g.color, shared_pool: g.shared_pool, call_public: g.call_public };
      if (act === 'gc-toggle-shared') body.shared_pool = el.checked; else body.call_public = el.checked;
      await E.api('groups/' + g.id, { method: 'PATCH', body: JSON.stringify(body) });
      await E.reload(); refresh();
      toast('共享设置已更新', 'ok');
    } catch (e) {
      el.checked = !el.checked;               // 失败回滚开关视觉
      toast((e && e.message) || String(e), 'err');
    }
  }

  function mount(env) {
    E = env || {};
    const pv = $('gcPromptVeil');
    if (pv) pv.addEventListener('input', function () { readPromptRows(); updatePromptCount(); });
    document.addEventListener('click', onClick);
    document.addEventListener('change', onChange);
  }

  /* showKey 给宿主页用：新建分组或轮换后直接弹一次完整密钥（只此一次可见）。 */
  function showKey(key) { const el = $('gcKeyBig'); if (el) el.textContent = key; openVeil('gcKeyVeil'); }
  window.GroupCfg = { cards: cards, render: render, refresh: refresh, modals: modals, mount: mount, showKey: showKey };
})();

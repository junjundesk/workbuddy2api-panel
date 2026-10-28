'use strict';
const $ = id => document.getElementById(id);
const COLORS = ['#5b7cfa','#3ddc97','#f5b544','#c084fc','#f0655f','#38bdf8','#fb923c','#a3e635'];
const state = { groups: [], accounts: [], selected: 'public', editing: null, groupColor: COLORS[0], proxyGroup: null, proxyType: 'socks5h', importGroup: null, importTab: 'single', confirm: null, publicProxy: null, sel: new Set(), revealed: {}, moveUids: [] };

function esc(s) { return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;'); }
function fmt(n) { return Number(n || 0).toLocaleString('zh-CN'); }
function fmtTokens(n) { n = Number(n || 0); if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M'; if (n >= 1000) return (n / 1000).toFixed(0) + 'k'; return String(n); }
function toast(msg, cls) { const el = document.createElement('div'); el.className = 'tst ' + (cls || ''); el.textContent = msg; $('toasts').appendChild(el); setTimeout(() => el.remove(), 3600); }
// 不能叫 open/close：顶层函数声明会覆盖 window.open，导致新标签页深链报 classList of null。
function openVeil(id) { $(id).classList.add('on'); }
function closeVeil(id) { $(id).classList.remove('on'); }
function copyText(t) { if (!t) return; const ta = document.createElement('textarea'); ta.value = t; ta.style.position = 'fixed'; ta.style.opacity = '0'; document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); toast('已复制', 'ok'); } catch (e) { toast('复制失败', 'err'); } ta.remove(); }
function groupOf(id) { return state.groups.find(g => g.id === id); }
function selectedGroup() { return groupOf(state.selected); }
function publicAccounts() { const shared = new Set(state.groups.filter(g => g.shared_pool).map(g => g.id)); return state.accounts.filter(a => !a.group || shared.has(a.group)); }
function groupAccounts(id) { return state.accounts.filter(a => a.group === id); }

async function api(path, opts) {
  opts = opts || {};
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem('wb2api.key');
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body && !(opts.body instanceof FormData)) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openVeil('keyGate'); throw new Error('密钥无效'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
async function load() {
  try {
    const [gd, ov] = await Promise.all([api('groups'), api('overview')]);
    state.groups = gd.groups || [];
    state.accounts = ov.accounts || [];
    state.publicProxy = gd.public_proxy || null;
    const live = new Set(state.accounts.map(a => a.uid));
    Array.from(state.sel).forEach(u => { if (!live.has(u)) state.sel.delete(u); });
    if (state.selected !== 'public' && !groupOf(state.selected)) state.selected = 'public';
    $('navState').textContent = '服务正常';
    renderAll();
    return true;
  } catch (e) {
    $('navState').textContent = '加载失败';
    if (String(e.message).indexOf('密钥') < 0) toast('加载失败：' + e.message, 'err');
    return false;
  }
}
function statusOf(a) {
  if (a.disabled) return { cls: 'off', text: '已禁用', tag: 'bad' };
  if (a.paused) return { cls: 'paused', text: '已暂停', tag: 'mute' };
  if (a.cooling) return { cls: 'cool', text: '冷却 ' + (a.cool_remaining_sec ? Math.ceil(a.cool_remaining_sec / 60) + ' 分' : ''), tag: 'warn' };
  return { cls: 'ok', text: '可用', tag: 'ok' };
}
function renderStats() {
  const pub = publicAccounts();
  const groupAcc = state.accounts.filter(a => a.group);
  let keys = 0, proxies = 0, healthy = 0;
  state.groups.forEach(g => { if (g.has_key) keys++; if (g.proxy) proxies++; }); if (state.publicProxy) proxies++;
  state.accounts.forEach(a => { if (a.group && !a.disabled && !a.cooling && !a.paused) healthy++; });
  const shared = state.groups.filter(g => g.shared_pool).length;
  $('stats').innerHTML =
    '<div class="stat accent"><div class="v">' + pub.length + '</div><div class="k">公共池可见<span>未分组 ' + state.accounts.filter(a => !a.group).length + ' + 共享 ' + (pub.length - state.accounts.filter(a => !a.group).length) + '</span></div></div>' +
    '<div class="stat"><div class="v">' + state.groups.length + '</div><div class="k">分组数量<span>共享 ' + shared + '</span></div></div>' +
    '<div class="stat good"><div class="v">' + groupAcc.length + '</div><div class="k">分组账号<span>可用 ' + healthy + '</span></div></div>' +
    '<div class="stat ' + (keys === state.groups.length ? 'good' : 'warn') + '"><div class="v">' + keys + '/' + state.groups.length + '</div><div class="k">分组密钥<span>独立鉴权</span></div></div>' +
    '<div class="stat"><div class="v">' + proxies + '</div><div class="k">代理出口<span>HTTP / SOCKS5 / SOCKS5H</span></div></div>';
  $('topMeta').textContent = '公共池 ' + pub.length + ' · ' + state.groups.length + ' 个分组 · ' + groupAcc.length + ' 个分组账号';
}
function renderRail() {
  let html = '<button class="pool-item public ' + (state.selected === 'public' ? 'on' : '') + '" data-act="select" data-id="public"><span class="gdot"></span><span><b>公共号池</b><em>未分组 + 共享分组</em></span><span class="cnt">' + publicAccounts().length + '</span></button>';
  html += '<div class="rail-sec">专属分组 <span>' + state.groups.length + '</span><button data-act="new-group">＋</button></div>';
  state.groups.forEach(g => {
    const bits = [g.member_count + ' 个账号', g.shared_pool ? '共享' : '独立'];
    if (g.call_public) bits.push('可回源');
    if (!g.has_key) bits.push('缺密钥');
    html += '<button class="pool-item ' + (state.selected === g.id ? 'on' : '') + '" data-act="select" data-id="' + esc(g.id) + '"><span class="gdot" style="--c:' + esc(g.color || '#5b7cfa') + '"></span><span><b>' + esc(g.name) + '</b><em>' + esc(bits.join(' · ')) + '</em></span><span class="cnt">' + g.member_count + '</span></button>';
  });
  $('railList').innerHTML = html;
}
function keyCard(g) {
  const shown = state.revealed[g.id];
  const hint = shown ? esc(shown) : (g.has_key ? esc(g.key_hint || '已生成') : '尚未生成');
  const btns = g.has_key ? '<button class="xs ghost" data-act="reveal-key">' + (shown ? '隐藏' : '显示') + '</button><button class="xs ghost" data-act="copy-group-key">复制</button>' : '';
  return '<div class="card"><div class="card-h"><h3>分组密钥</h3><span class="tag accent">独立鉴权</span></div>' +
    '<div class="kline"><code>' + hint + '</code>' + btns + '<button class="xs" data-act="rotate-key">' + (g.has_key ? '轮换' : '生成') + '</button></div>' +
    '<div class="hint">点击「显示」查看完整密钥；旧版本创建的分组只存了哈希，需轮换一次后才能查看。</div><div class="card-f"><span>Bearer 此密钥只能访问本组</span></div></div>';
}
function proxyCard(g) {
  const isPub = !g;
  const p = isPub ? state.publicProxy : g.proxy;
  const clr = p ? '<button class="xs ghost danger" data-act="clear-proxy">清除</button>' : '';
  return '<div class="card"><div class="card-h"><h3>网络代理</h3>' + (p ? '<span class="tag ok">已配置</span>' : '<span class="tag mute">未配置</span>') + '</div>' +
    (p ? '<div class="proxy-line"><span class="proto ' + esc(p.type) + '">' + esc(p.type.toUpperCase()) + '</span><code>' + esc(p.host) + ':' + p.port + '</code></div><div class="hint">覆盖聊天、OAuth、余额、模型探测与全部任务。</div>' : '<div class="hint">' + (isPub ? '未配置时公共池（未分组账号）使用服务器直连出口；共享分组账号仍走各自分组代理。' : '未配置时本组使用服务器直连出口。') + '</div>') +
    '<div class="card-f"><span>' + (isPub ? '未分组账号全部出站请求' : '本组全部出站请求') + '</span><span class="grow"></span>' + clr + '<button class="xs ghost" data-act="edit-proxy">' + (p ? '编辑' : '配置') + '</button></div></div>';
}
function shareCard(g) {
  return '<div class="card"><div class="card-h"><h3>共享池</h3><span class="tag ' + (g.shared_pool || g.call_public ? 'warn' : 'mute') + '">' + (g.shared_pool || g.call_public ? '已开启共享' : '双向隔离') + '</span></div>' +
    '<label class="switch-row"><span class="sw-text"><b>允许本组账号进入公共池</b><em>默认关闭</em></span><input type="checkbox" data-act="toggle-shared"' + (g.shared_pool ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
    '<label class="switch-row"><span class="sw-text"><b>允许本组调用公共池账号</b><em>本组无可用号时回源；默认关闭</em></span><input type="checkbox" data-act="toggle-call"' + (g.call_public ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
    '<div class="hint">' + (g.shared_pool ? '公共池可命中本组账号。' : '') + (g.call_public ? '本组可回源公共池。' : '') + (!g.shared_pool && !g.call_public ? '本组与公共池双向隔离。' : '') + '</div></div>';
}
// accActs 账号行操作，与主页账号池（app.js renderAccounts）同口径：
// 企业版不显示签到 / 任务（上游不支持），冻结 → 解冻、暂停 → 恢复选号、否则 → 暂停选号。
// 任务 / 模型弹窗逻辑较重，复用主页实现：新标签页深链 /panel/?open=tasks&uid=...。
function accActs(a, mode) {
  const u = esc(a.uid);
  const b = (act, label, cls, title) => '<button class="xs ' + (cls || 'ghost') + '" data-act="acc" data-a="' + act + '" data-uid="' + u + '"' + (title ? ' title="' + esc(title) + '"' : '') + '>' + label + '</button>';
  const frozen = a.disabled || a.cooling;
  let h = '';
  if (!a.enterprise) h += b('checkin', a.checkin_done ? '已签' : '签到', '', a.checkin_done ? '今日已签到；点击可重新签到并刷新余额' : '');
  h += b('balance', a.enterprise ? '额度' : '余额');
  if (!a.enterprise) h += b('tasks', '任务', '', '在控制台打开任务面板');
  h += b('models', '模型', '', '在控制台打开模型面板');
  h += frozen ? b('revive', '解冻', 'primary') : (a.paused ? b('resume', '恢复选号', 'primary') : b('pause', '暂停选号', '', '退出选号，但照常签到 / 保活 / 刷新余额'));
  if (!a.disabled) h += b('disable', '禁用');
  h += '<button class="xs ghost" data-act="move-account" data-uid="' + u + '" title="移动到公共池或其他分组">移动</button>';
  if (mode !== 'group' && a.group) h += '<button class="xs ghost" data-act="select" data-id="' + esc(a.group) + '">前往分组</button>';
  h += '<button class="xs ghost" data-act="export-account" data-uid="' + u + '" title="导出该账号 JSON（含凭证）">导出</button>';
  h += '<button class="xs ghost danger" data-act="remove-account" data-uid="' + u + '">移除</button>';
  return h;
}
function credText(a) {
  if (a.credits_total === -1) return '不限';
  if (a.credits == null) return '—';
  return fmt(a.credits) + (a.credits_total > 0 ? '<span style="color:var(--ink-3)">/' + fmt(a.credits_total) + '</span>' : '');
}
function rowsHtml(list, mode) {
  if (!list.length) return '<tr><td colspan="10"><div class="empty"><div class="big">' + (mode === 'group' ? '这个分组还没有账号' : '没有账号') + '</div>点击「添加账号」或「导入 JSON」。</div></td></tr>';
  return list.map(a => {
    const st = statusOf(a);
    const acts = accActs(a, mode);
    const tags = (a.realm === 'global' ? ' <span class="tag accent">国际版</span>' : '') + (a.enterprise ? ' <span class="tag accent">企业版</span>' : '');
    const note = a.reason ? '<div class="id" style="margin-top:3px">' + esc(a.reason) + '</div>' : '';
    return '<tr class="' + st.cls + '"><td class="mark"><i></i></td><td class="ck"><input type="checkbox" data-act="sel-one" data-uid="' + esc(a.uid) + '"' + (state.sel.has(a.uid) ? ' checked' : '') + '></td><td><div class="who"><div class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + tags + '</div><div class="id">' + esc(a.uid) + '</div></div></td><td><span class="tag ' + st.tag + '">' + st.text + '</span>' + note + '</td><td class="num">' + credText(a) + '</td><td class="num">' + fmt(a.success_count) + ' / ' + fmt(a.err_total) + '</td><td class="num">' + (a.in_flight || 0) + '</td><td class="num">' + (a.token_usage && a.token_usage.total_tokens ? fmtTokens(a.token_usage.total_tokens) : '—') + '</td><td class="num">' + (a.realm || 'cn').toUpperCase() + '</td><td><div class="acts">' + acts + '</div></td></tr>';
  }).join('');
}
// 分组批量操作，与主页分组卡片同口径：签到跳过禁用 / 企业版；任务执行只跑能做成长任务的号。
function taskEligible(a) { return !a.disabled && !a.enterprise && a.realm !== 'global'; }
function groupBulkBtns(list) {
  if (!list.length) return '';
  let h = '';
  if (list.some(taskEligible)) h += '<button class="xs primary" data-act="group-tasks" title="扫描本组账号的成长任务待办并排队执行（并发 = 本组账号数），进度在任务中心查看">任务执行</button>';
  h += '<button class="xs" data-act="group-bulk" data-what="checkin" title="逐个签到本组账号（跳过禁用 / 企业版）">本组签到</button>';
  h += '<button class="xs" data-act="group-bulk" data-what="balance" title="逐个刷新本组账号余额">本组余额</button>';
  return h;
}
function accountsBox(list, mode) {
  return '<div class="box"><header><h3>' + (mode === 'group' ? '分组账号' : '公共池账号') + ' <span class="hint">' + (mode === 'group' ? '仅本组密钥可见' : '未分组 + 共享') + '</span></h3><span class="grow"></span>' + (mode === 'group' ? groupBulkBtns(list) : '') + '<span class="hint" id="selInfo"></span><button class="xs" data-act="move-sel" disabled>移动所选</button><button class="xs" data-act="export-sel" disabled title="导出所选账号 JSON（含凭证），可在其他实例直接导入">导出所选</button><button class="xs" data-act="add-account">添加账号</button><button class="xs" data-act="import">导入 JSON</button></header><div class="acc-wrap"><table class="acc"><thead><tr><th class="mark"></th><th class="ck"><input type="checkbox" data-act="sel-all" title="全选当前列表"></th><th>账号</th><th>状态</th><th>积分</th><th>成功 / 失败</th><th>在途</th><th>用量</th><th>域</th><th></th></tr></thead><tbody>' + rowsHtml(list, mode) + '</tbody></table></div><div class="table-foot">共 ' + list.length + ' 个账号</div></div>';
}
function renderDetail() {
  const g = selectedGroup();
  if (!g) {
    const list = publicAccounts();
    $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:var(--accent)"></span><div><h1>公共号池</h1><div class="sub">未分组账号 + 已进入共享池的分组账号</div></div></div><div class="dh-acts"><span class="tag accent">默认池</span></div></div>' +
      '<div class="gp-cards"><div class="card"><div class="card-h"><h3>公共池</h3><span class="tag mute">config.api_key</span></div><div class="hint">公共池沿用现有主密钥；分组密钥不能反向访问公共池。</div></div>' +
      proxyCard(null) +
      '<div class="card"><div class="card-h"><h3>共享来源</h3><span class="tag ' + (state.groups.filter(x => x.shared_pool).length ? 'warn' : 'mute') + '">' + state.groups.filter(x => x.shared_pool).length + ' 个分组</span></div><div class="hint">只有开启「进入公共池」的分组才会出现在这里。</div></div></div>' + accountsBox(list, 'public');
    return;
  }
  const list = groupAccounts(g.id);
  $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:' + esc(g.color || '#5b7cfa') + '"></span><div><h1>' + esc(g.name) + '</h1><div class="sub">' + esc(g.id) + ' · ' + g.member_count + ' 个账号</div></div></div><div class="dh-acts"><span class="tag ' + (g.shared_pool ? 'warn' : 'accent') + '">' + (g.shared_pool ? '共享分组' : '独立分组') + '</span><button class="xs" data-act="edit-group">编辑</button><button class="xs danger" data-act="delete-group">删除</button></div></div>' +
    '<div class="gp-cards">' + keyCard(g) + proxyCard(g) + shareCard(g) + '</div>' + accountsBox(list, 'group');
}
function renderAll() { renderStats(); renderRail(); renderDetail(); updateSelUI(); }
function visibleAccounts() { const g = selectedGroup(); return g ? groupAccounts(g.id) : publicAccounts(); }
function updateSelUI() {
  const list = visibleAccounts(); const n = list.filter(a => state.sel.has(a.uid)).length;
  const info = $('selInfo'); if (info) info.textContent = n ? '已选 ' + n : '';
  document.querySelectorAll('[data-act="move-sel"],[data-act="export-sel"]').forEach(b => { b.disabled = !n; });
  const all = document.querySelector('[data-act="sel-all"]'); if (all) { all.checked = n > 0 && n === list.length; all.indeterminate = n > 0 && n < list.length; }
}
function selectedVisible() { return visibleAccounts().filter(a => state.sel.has(a.uid)).map(a => a.uid); }
async function exportAccounts(uids) {
  if (!uids.length) { toast('请先勾选账号', 'err'); return; }
  const r = await api('accounts/export', { method: 'POST', body: JSON.stringify({ uids }) });
  const data = { format: r.format, version: r.version, exported_at: r.exported_at, accounts: r.accounts };
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob); const link = document.createElement('a');
  const ts = new Date().toISOString().replace(/[-:T]/g, '').slice(0, 14);
  link.href = url; link.download = uids.length === 1 ? 'workbuddy-' + uids[0] + '.json' : 'workbuddy-accounts-' + ts + '.json';
  document.body.appendChild(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  toast('已导出 ' + r.accounts.length + ' 个账号' + (r.missing && r.missing.length ? '（' + r.missing.length + ' 个不存在已跳过）' : '') + '；文件含登录凭证，请妥善保管', 'warn');
}
function openMoveModal(uids) {
  state.moveUids = uids;
  const cur = new Set(uids.map(u => { const a = state.accounts.find(x => x.uid === u); return a ? (a.group || '') : ''; }));
  const only = cur.size === 1 ? Array.from(cur)[0] : null;
  const opts = [{ id: '', name: '公共号池（未分组）' }].concat(state.groups.map(g => ({ id: g.id, name: g.name })));
  $('moveTarget').innerHTML = opts.map(o => '<option value="' + esc(o.id) + '"' + (o.id === only ? ' disabled' : '') + '>' + esc(o.name) + (o.id === only ? '（当前）' : '') + '</option>').join('');
  const first = opts.find(o => o.id !== only); $('moveTarget').value = first ? first.id : '';
  $('moveHint').textContent = '将 ' + uids.length + ' 个账号移动到目标号池（一个账号只能属于一个分组）。';
  openVeil('moveVeil');
}
function showKeyModal(key) { $('keyBig').textContent = key; openVeil('keyVeil'); }
function openGroupModal(edit) {
  state.editing = edit ? edit.id : null;
  state.groupColor = edit ? (edit.color || COLORS[0]) : COLORS[state.groups.length % COLORS.length];
  $('groupTitle').textContent = edit ? '编辑分组' : '新建分组';
  $('groupName').value = edit ? edit.name : '';
  $('groupShared').checked = !!(edit && edit.shared_pool);
  $('groupCallPublic').checked = !!(edit && edit.call_public);
  $('groupColors').innerHTML = COLORS.map(c => '<button class="swatch ' + (c === state.groupColor ? 'on' : '') + '" style="--c:' + c + '" data-act="group-color" data-color="' + c + '"></button>').join('');
  openVeil('groupVeil');
}
// 测试结果固定两行：连通性（耗时 / 状态 / 目标站点）+ 出口 IP 与归属地。
// 出口 IP 由服务端经同一代理访问 ipip.net 取回——CSP 是 connect-src 'self'，
// 前端不能直连第三方，也不该让浏览器实测（那样测的是浏览器出口而不是服务器出口）。
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
    l2.innerHTML = '出口 IP：<b>' + esc(r.exit_ip) + '</b>' +
      (r.exit_region ? ' · ' + esc(r.exit_region) : ' · 未返回归属地') +
      (r.ip_latency_ms != null ? ' · ' + r.ip_latency_ms + ' ms' : '');
  } else {
    l2.className = 'l2 warn';
    l2.textContent = '出口 IP：未取到' + (r.ip_error ? '（' + r.ip_error + '）' : '');
  }
  box.append(l1, l2);
}
function proxyHint(t) { if (t === 'http') return 'HTTP CONNECT；目标域名由本机解析。'; if (t === 'socks5') return 'SOCKS5（本地 DNS）：先解析目标域名再由代理转发 IP。'; return 'SOCKS5H（远程 DNS）：域名交给代理解析，适合避免 DNS 泄漏。'; }
function openProxyModal(g) {
  state.proxyGroup = g ? g.id : 'public';
  const p = g ? g.proxy : state.publicProxy;
  $('proxyScope').textContent = g ? '本组全部出站请求（聊天、OAuth、余额、模型探测、签到/保活/旅行/活跃任务）都走此代理。' : '公共池（未分组账号）全部出站请求都走此代理；共享分组账号仍走各自分组代理。';
  $('proxyClear').hidden = !p;
  state.proxyType = p ? p.type : 'socks5h';
  $('proxyHost').value = p ? p.host : '';
  $('proxyPort').value = p ? p.port : 1080;
  $('proxyUser').value = p ? (p.username || '') : '';
  $('proxyPass').value = '';
  $('proxyTestBox').hidden = true; $('proxyTestBox').className = 'testbox';
  updateProxyUI(); openVeil('proxyVeil');
}
function updateProxyUI() {
  document.querySelectorAll('#proxyTypes button').forEach(b => b.classList.toggle('on', b.dataset.type === state.proxyType));
  $('proxyHint').textContent = proxyHint(state.proxyType);
  const p = $('proxyPort'); if (!p.value || p.value === '1080' || p.value === '8080') p.value = state.proxyType === 'http' ? 8080 : 1080;
}
function openImportModal() {
  state.importGroup = state.selected === 'public' ? null : state.selected;
  $('importGroupName').textContent = state.importGroup ? '· ' + groupOf(state.importGroup).name : '· 公共号池';
  $('importSingle').value = ''; $('importBatch').value = ''; $('importFile').value = '';
  switchImportTab('single'); updateImportParse(); openVeil('importVeil');
}
function switchImportTab(t) { state.importTab = t; document.querySelectorAll('#importVeil .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === t)); $('importSinglePanel').hidden = t !== 'single'; $('importBatchPanel').hidden = t !== 'batch'; if ($('importParse')) updateImportParse(); }
function parseAccounts(text) {
  try {
    const d = JSON.parse(text); let arr = Array.isArray(d) ? d : (d && Array.isArray(d.accounts) ? d.accounts : (d && typeof d === 'object' ? [d] : []));
    return arr.filter(x => x && (x.uid || (x.account && x.account.uid)) && (x.access_token || x.accessToken || (x.auth && x.auth.accessToken)));
  } catch (e) { return []; }
}
// 实时提示识别到的账号数；文件选择后读入批量文本框（原先选文件没有任何效果）。
function updateImportParse() {
  const text = state.importTab === 'single' ? $('importSingle').value : $('importBatch').value;
  const n = text.trim() ? parseAccounts(text).length : -1;
  $('importParse').textContent = n < 0 ? '等待输入…' : (n ? '识别到 ' + n + ' 个账号' : '没有识别到有效账号（需要 uid + access_token/accessToken）');
}
$('importSingle').addEventListener('input', updateImportParse);
$('importBatch').addEventListener('input', updateImportParse);
// 支持一次多选多个 JSON 文件：逐个解析后按 uid 去重合并成一个数组放进批量文本框。
// 解析失败 / 无有效账号的文件单独计数提示，不影响其他文件。
$('importFile').addEventListener('change', async () => {
  const files = Array.from($('importFile').files || []); if (!files.length) return;
  const merged = new Map(); const bad = [];
  for (const f of files) {
    const arr = parseAccounts(await f.text());
    if (!arr.length) { bad.push(f.name); continue; }
    arr.forEach(x => merged.set(x.uid || (x.account && x.account.uid), x));
  }
  $('importBatch').value = merged.size ? JSON.stringify(Array.from(merged.values()), null, 1) : '';
  updateImportParse();
  $('importParse').textContent = files.length + ' 个文件，识别到 ' + merged.size + ' 个账号（已按 uid 去重）' + (bad.length ? '；无法识别：' + bad.join('、') : '');
});
function openConfirm(title, body, label, fn) { $('confirmTitle').textContent = title; $('confirmBody').innerHTML = body; $('confirmYes').textContent = label; state.confirm = fn; openVeil('confirmVeil'); }

document.addEventListener('click', async e => {
  const el = e.target.closest('[data-act]'); if (!el) return;
  const act = el.dataset.act;
  try {
    if (act === 'select') { state.selected = el.dataset.id; state.sel.clear(); renderAll(); return; }
    if (act === 'sel-one') { if (el.checked) state.sel.add(el.dataset.uid); else state.sel.delete(el.dataset.uid); updateSelUI(); return; }
    if (act === 'sel-all') { const on = el.checked; visibleAccounts().forEach(a => { if (on) state.sel.add(a.uid); else state.sel.delete(a.uid); }); document.querySelectorAll('[data-act="sel-one"]').forEach(c => { c.checked = on; }); updateSelUI(); return; }
    if (act === 'export-sel') { await exportAccounts(selectedVisible()); return; }
    if (act === 'export-account') { await exportAccounts([el.dataset.uid]); return; }
    if (act === 'move-sel') { const u = selectedVisible(); if (!u.length) { toast('请先勾选账号', 'err'); return; } openMoveModal(u); return; }
    if (act === 'move-do') {
      const target = $('moveTarget').value; const uids = state.moveUids.slice(); let ok = 0, fail = 0, lastErr = '';
      for (const uid of uids) {
        try { await api('accounts/' + encodeURIComponent(uid) + '/group', { method: 'POST', body: JSON.stringify({ group: target }) }); ok++; }
        catch (e2) { fail++; lastErr = e2.message; }
      }
      closeVeil('moveVeil'); state.sel.clear(); await load();
      const tg = target ? groupOf(target) : null;
      toast('已移动 ' + ok + ' 个账号到' + (tg ? '「' + tg.name + '」' : '公共号池') + (fail ? '，失败 ' + fail + '（' + lastErr + '）' : ''), fail ? 'err' : 'ok'); return;
    }
    if (act === 'reveal-key') {
      const g = selectedGroup(); if (!g) return;
      if (state.revealed[g.id]) { delete state.revealed[g.id]; renderDetail(); updateSelUI(); return; }
      const r = await api('groups/' + g.id + '/key'); state.revealed[g.id] = r.key; renderDetail(); updateSelUI(); return;
    }
    if (act === 'copy-group-key') {
      const g = selectedGroup(); if (!g) return;
      const k = state.revealed[g.id] || (await api('groups/' + g.id + '/key')).key; copyText(k); return;
    }
    if (act === 'clear-proxy' || act === 'proxy-clear') {
      const g = act === 'proxy-clear' ? (state.proxyGroup === 'public' ? null : groupOf(state.proxyGroup)) : selectedGroup();
      const id = g ? g.id : 'public';
      openConfirm('清除代理', '<div class="hint">清除后' + (g ? '「' + esc(g.name) + '」' : '公共池') + '的出站请求将改为服务器直连。</div>', '清除', async () => {
        await api('groups/' + id + '/proxy', { method: 'DELETE' }); closeVeil('confirmVeil'); closeVeil('proxyVeil'); await load(); toast('代理已清除，恢复直连', 'ok');
      }); return;
    }
    if (act === 'new-group') { openGroupModal(null); return; }
    if (act === 'edit-group') { const g = selectedGroup(); if (g) openGroupModal(g); return; }
    if (act === 'group-color') { state.groupColor = el.dataset.color; document.querySelectorAll('#groupColors .swatch').forEach(b => b.classList.toggle('on', b.dataset.color === state.groupColor)); return; }
    if (act === 'group-save') {
      const body = { name: $('groupName').value.trim(), color: state.groupColor, shared_pool: $('groupShared').checked, call_public: $('groupCallPublic').checked };
      if (!body.name) { toast('请填写分组名称', 'err'); return; }
      if (state.editing) { await api('groups/' + state.editing, { method: 'PATCH', body: JSON.stringify(body) }); toast('分组已更新', 'ok'); }
      else { const r = await api('groups', { method: 'POST', body: JSON.stringify(body) }); state.selected = r.group.id; showKeyModal(r.key); toast('分组已创建', 'ok'); }
      closeVeil('groupVeil'); await load(); return;
    }
    if (act === 'delete-group') {
      const g = selectedGroup(); if (!g) return;
      const body = '<div class="hint">删除 <b>' + esc(g.name) + '</b>？该组有 ' + g.member_count + ' 个账号。</div><label class="radio"><input type="radio" name="delMode" value="move" checked> 移到公共池（推荐）</label><label class="radio"><input type="radio" name="delMode" value="delete"> 完全删除（含 auth 文件，不可恢复）</label>';
      openConfirm('删除分组', body, '删除分组', async () => {
        const mode = document.querySelector('input[name="delMode"]:checked').value;
        await api('groups/' + g.id + '?account_action=' + mode, { method: 'DELETE' });
        state.selected = 'public'; closeVeil('confirmVeil'); await load(); toast('分组已删除', 'ok');
      }); return;
    }
    if (act === 'rotate-key') {
      const g = selectedGroup(); if (!g) return;
      const r = await api('groups/' + g.id + '/key/rotate', { method: 'POST' });
      delete state.revealed[g.id]; showKeyModal(r.key); await load(); toast('新密钥已生成，旧密钥已失效', 'warn'); return;
    }
    if (act === 'toggle-shared' || act === 'toggle-call') {
      const g = selectedGroup(); if (!g) return;
      const body = { name: g.name, color: g.color, shared_pool: g.shared_pool, call_public: g.call_public };
      if (act === 'toggle-shared') body.shared_pool = el.checked; else body.call_public = el.checked;
      await api('groups/' + g.id, { method: 'PATCH', body: JSON.stringify(body) });
      await load(); toast('共享设置已更新', 'ok'); return;
    }
    if (act === 'edit-proxy') { openProxyModal(selectedGroup() || null); return; }
    if (act === 'proxy-type') { state.proxyType = el.dataset.type; updateProxyUI(); return; }
    if (act === 'proxy-save') {
      const body = { type: state.proxyType, host: $('proxyHost').value.trim(), port: parseInt($('proxyPort').value, 10) || 0, username: $('proxyUser').value.trim(), password: $('proxyPass').value };
      await api('groups/' + state.proxyGroup + '/proxy', { method: 'PUT', body: JSON.stringify(body) });
      closeVeil('proxyVeil'); await load(); toast('代理已保存', 'ok'); return;
    }
    if (act === 'proxy-test') {
      const body = { type: state.proxyType, host: $('proxyHost').value.trim(), port: parseInt($('proxyPort').value, 10) || 0, username: $('proxyUser').value.trim(), password: $('proxyPass').value };
      const box = $('proxyTestBox'); box.hidden = false; box.className = 'testbox'; box.textContent = '正在测试…（连通性 + 出口 IP）';
      const r = await api('groups/' + state.proxyGroup + '/proxy/test', { method: 'POST', body: JSON.stringify(body) });
      renderProxyTest(box, r);
      return;
    }
    if (act === 'import') { openImportModal(); return; }
    if (act === 'import-tab') { switchImportTab(el.dataset.tab); return; }
    if (act === 'import-do') {
      const text = state.importTab === 'single' ? $('importSingle').value : $('importBatch').value;
      const arr = parseAccounts(text);
      if (!arr.length) { toast('没有识别到有效账号', 'err'); return; }
      const fd = new FormData(); fd.append('file', new Blob([JSON.stringify(arr)], { type: 'application/json' }), 'accounts.json');
      if (state.importGroup) fd.append('group', state.importGroup);
      const r = await api('import/cockpit', { method: 'POST', body: fd });
      closeVeil('importVeil'); await load(); if (r.errors && r.errors.length) console.warn('import errors:', r.errors);
      toast('导入完成：成功 ' + r.imported + '，跳过 ' + r.skipped + (r.errors && r.errors.length ? '（' + r.errors[0] + '）' : ''), r.imported ? 'ok' : 'warn'); return;
    }
    if (act === 'add-account') {
      const g = selectedGroup(); $('addGroupName').textContent = g ? '· ' + g.name : '· 公共号池'; $('addWait').hidden = true; openVeil('addVeil');
      state.addGroup = g ? g.id : ''; return;
    }
    if (act === 'start-login') {
      const realm = $('addRealm').value || 'cn';
      const r = await api('login/start', { method: 'POST', body: JSON.stringify({ realm, group: state.addGroup || '' }) });
      $('addUrl').textContent = r.url; $('addWait').hidden = false;
      const timer = setInterval(async () => {
        try {
          const p = await api('login/poll?state=' + encodeURIComponent(r.state));
          if (p.done) { clearInterval(timer); closeVeil('addVeil'); await load(); toast('账号已添加' + (p.group ? '到分组' : '到公共池'), 'ok'); }
        } catch (e) { /* 继续轮询 */ }
      }, 3000);
      return;
    }
    if (act === 'acc') {
      const u = el.dataset.uid, a = el.dataset.a, p = 'accounts/' + encodeURIComponent(u) + '/';
      if (a === 'tasks' || a === 'models') { window.open('/panel/?open=' + a + '&uid=' + encodeURIComponent(u) + '#accounts', '_blank'); return; }
      if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
      el.disabled = true;
      try {
        if (a === 'checkin') { const r = await api(p + 'checkin', { method: 'POST' }); toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok'); }
        else if (a === 'balance') { const r = await api(p + 'balance', { method: 'POST' }); toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok'); }
        else if (a === 'revive') { await api(p + 'revive', { method: 'POST' }); toast('已解冻', 'ok'); }
        else if (a === 'pause') { await api(p + 'pause', { method: 'POST' }); toast('已暂停选号（签到 / 保活照常）', 'ok'); }
        else if (a === 'resume') { await api(p + 'resume', { method: 'POST' }); toast('已恢复选号', 'ok'); }
        else if (a === 'disable') { await api(p + 'disable', { method: 'POST' }); toast('已禁用', 'ok'); }
      } finally { el.disabled = false; await load(); }
      return;
    }
    if (act === 'group-bulk') {
      const g = selectedGroup(); if (!g || el.disabled) return;
      const what = el.dataset.what;
      const items = groupAccounts(g.id).filter(a => !a.disabled && !(what === 'checkin' && a.enterprise));
      if (!items.length) { toast('本组没有可执行的账号', 'err'); return; }
      const label = el.textContent; el.disabled = true;
      let ok = 0, fail = 0;
      try {
        for (let i = 0; i < items.length; i++) {
          el.textContent = label + ' ' + (i + 1) + '/' + items.length;
          try { await api('accounts/' + encodeURIComponent(items[i].uid) + '/' + what, { method: 'POST' }); ok++; } catch (e2) { fail++; }
        }
      } finally { el.disabled = false; el.textContent = label; }
      toast(label + '完成：成功 ' + ok + (fail ? '，失败 ' + fail : ''), fail ? 'err' : 'ok');
      await load(); return;
    }
    if (act === 'group-tasks') {
      const g = selectedGroup(); if (!g || el.disabled) return;
      const items = groupAccounts(g.id).filter(taskEligible);
      if (!items.length) { toast('本组没有可执行成长任务的账号', 'err'); return; }
      const conc = items.length; // 并发 = 本组可执行账号数，账号内串行
      if (!confirm('扫描「' + g.name + '」' + items.length + ' 个账号的待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
      // 先同步开空白标签页，避免 await 之后再 window.open 被浏览器当弹窗拦截。
      const tab = window.open('about:blank', '_blank');
      el.disabled = true;
      try {
        const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc, uids: items.map(a => a.uid), label: g.name }) });
        if (!r.started) { if (tab) tab.close(); toast(r.message || '本组没有待办任务', 'ok'); return; }
        toast(g.name + '：队列已启动 ' + r.total + ' 项（并发 ' + (r.conc || conc) + '），进度见任务中心', 'ok');
        if (tab) tab.location.href = '/panel/#taskscenter'; else window.open('/panel/#taskscenter', '_blank');
      } catch (e2) { if (tab) tab.close(); throw e2; }
      finally { el.disabled = false; }
      return;
    }
    if (act === 'move-account') { openMoveModal([el.dataset.uid]); return; }
    if (act === 'remove-account') {
      const uid = el.dataset.uid;
      openConfirm('移除账号', '<div class="hint">移除 ' + esc(uid) + ' 将删除池状态与 auths/ 下的凭证文件，且不可恢复。</div>', '移除', async () => {
        await api('accounts/' + encodeURIComponent(uid) + '/remove', { method: 'POST' }); closeVeil('confirmVeil'); await load(); toast('账号已移除', 'ok');
      }); return;
    }
    if (act === 'close') { closeVeil(el.dataset.target); return; }
    if (act === 'confirm-yes') { const fn = state.confirm; state.confirm = null; if (fn) await fn(); return; }
    if (act === 'copy-key') { copyText($('keyBig').textContent); return; }
    if (act === 'gate-ok') { const v = $('gateKey').value.trim(); if (!v) return; localStorage.setItem('wb2api.key', v); closeVeil('keyGate'); load(); return; }
  } catch (err) { toast(err.message || String(err), 'err'); }
});

// 刷新：原来直接 onclick = load，数据没变时页面看起来毫无反应；现在给出进行中状态与结果提示。
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh'); if (b.disabled) return;
  b.disabled = true; b.textContent = '刷新中…';
  try { if (await load()) toast('已刷新 · ' + state.accounts.length + ' 个账号', 'ok'); }
  finally { b.disabled = false; b.textContent = '刷新'; }
};
$('btnTheme').onclick = () => { const cur = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light'; document.documentElement.dataset.theme = cur; localStorage.setItem('wb2gp.theme', cur); };
if (localStorage.getItem('wb2gp.theme')) document.documentElement.dataset.theme = localStorage.getItem('wb2gp.theme');
load();

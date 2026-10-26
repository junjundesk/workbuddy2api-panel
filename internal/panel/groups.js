'use strict';
const $ = id => document.getElementById(id);
const COLORS = ['#5b7cfa','#3ddc97','#f5b544','#c084fc','#f0655f','#38bdf8','#fb923c','#a3e635'];
const state = { groups: [], accounts: [], selected: 'public', editing: null, groupColor: COLORS[0], proxyGroup: null, proxyType: 'socks5h', importGroup: null, importTab: 'single', confirm: null };

function esc(s) { return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;'); }
function fmt(n) { return Number(n || 0).toLocaleString('zh-CN'); }
function fmtTokens(n) { n = Number(n || 0); if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M'; if (n >= 1000) return (n / 1000).toFixed(0) + 'k'; return String(n); }
function toast(msg, cls) { const el = document.createElement('div'); el.className = 'tst ' + (cls || ''); el.textContent = msg; $('toasts').appendChild(el); setTimeout(() => el.remove(), 3600); }
function open(id) { $(id).classList.add('on'); }
function close(id) { $(id).classList.remove('on'); }
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
  if (r.status === 401) { open('keyGate'); throw new Error('密钥无效'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
async function load() {
  try {
    const [gd, ov] = await Promise.all([api('groups'), api('overview')]);
    state.groups = gd.groups || [];
    state.accounts = ov.accounts || [];
    if (state.selected !== 'public' && !groupOf(state.selected)) state.selected = 'public';
    $('navState').textContent = '服务正常';
    renderAll();
  } catch (e) {
    $('navState').textContent = '加载失败';
    if (String(e.message).indexOf('密钥') < 0) toast('加载失败：' + e.message, 'err');
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
  state.groups.forEach(g => { if (g.has_key) keys++; if (g.proxy) proxies++; });
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
  const hint = g.has_key ? esc(g.key_hint || '已生成') : '尚未生成';
  return '<div class="card"><div class="card-h"><h3>分组密钥</h3><span class="tag accent">独立鉴权</span></div>' +
    '<div class="kline"><code>' + hint + '</code><button class="xs" data-act="rotate-key">' + (g.has_key ? '轮换' : '生成') + '</button></div>' +
    '<div class="hint">分组密钥只显示一次；此处仅保留掩码。</div><div class="card-f"><span>Bearer 此密钥只能访问本组</span></div></div>';
}
function proxyCard(g) {
  const p = g.proxy;
  return '<div class="card"><div class="card-h"><h3>网络代理</h3>' + (p ? '<span class="tag ok">已配置</span>' : '<span class="tag mute">未配置</span>') + '</div>' +
    (p ? '<div class="proxy-line"><span class="proto ' + esc(p.type) + '">' + esc(p.type.toUpperCase()) + '</span><code>' + esc(p.host) + ':' + p.port + '</code></div><div class="hint">覆盖聊天、OAuth、余额、模型探测与全部任务。</div>' : '<div class="hint">未配置时本组使用服务器直连出口。</div>') +
    '<div class="card-f"><span>本组全部出站请求</span><button class="xs ghost" data-act="edit-proxy">' + (p ? '编辑' : '配置') + '</button></div></div>';
}
function shareCard(g) {
  return '<div class="card"><div class="card-h"><h3>共享池</h3><span class="tag ' + (g.shared_pool || g.call_public ? 'warn' : 'mute') + '">' + (g.shared_pool || g.call_public ? '已开启共享' : '双向隔离') + '</span></div>' +
    '<label class="switch-row"><span class="sw-text"><b>允许本组账号进入公共池</b><em>默认关闭</em></span><input type="checkbox" data-act="toggle-shared"' + (g.shared_pool ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
    '<label class="switch-row"><span class="sw-text"><b>允许本组调用公共池账号</b><em>本组无可用号时回源；默认关闭</em></span><input type="checkbox" data-act="toggle-call"' + (g.call_public ? ' checked' : '') + '><span class="track"><i></i></span></label>' +
    '<div class="hint">' + (g.shared_pool ? '公共池可命中本组账号。' : '') + (g.call_public ? '本组可回源公共池。' : '') + (!g.shared_pool && !g.call_public ? '本组与公共池双向隔离。' : '') + '</div></div>';
}
function rowsHtml(list, mode) {
  if (!list.length) return '<tr><td colspan="9"><div class="empty"><div class="big">' + (mode === 'group' ? '这个分组还没有账号' : '没有账号') + '</div>点击「添加账号」或「导入 JSON」。</div></td></tr>';
  return list.map(a => {
    const st = statusOf(a);
    const acts = mode === 'group'
      ? '<button data-act="move-account" data-uid="' + esc(a.uid) + '">移出</button><button class="danger" data-act="remove-account" data-uid="' + esc(a.uid) + '">移除</button>'
      : (a.group ? '<button data-act="select" data-id="' + esc(a.group) + '">前往分组</button>' : '<button class="danger" data-act="remove-account" data-uid="' + esc(a.uid) + '">移除</button>');
    return '<tr class="' + st.cls + '"><td class="mark"><i></i></td><td><div class="who"><div class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</div><div class="id">' + esc(a.uid) + '</div></div></td><td><span class="tag ' + st.tag + '">' + st.text + '</span></td><td class="num">' + fmt(a.credits) + '</td><td class="num">' + fmt(a.success_count) + ' / ' + fmt(a.err_total) + '</td><td class="num">' + (a.in_flight || 0) + '</td><td class="num">' + (a.token_usage && a.token_usage.total_tokens ? fmtTokens(a.token_usage.total_tokens) : '—') + '</td><td class="num">' + (a.realm || 'cn').toUpperCase() + '</td><td><div class="acts">' + acts + '</div></td></tr>';
  }).join('');
}
function accountsBox(list, mode) {
  return '<div class="box"><header><h3>' + (mode === 'group' ? '分组账号' : '公共池账号') + ' <span class="hint">' + (mode === 'group' ? '仅本组密钥可见' : '未分组 + 共享') + '</span></h3><span class="grow"></span><button class="xs" data-act="add-account">添加账号</button><button class="xs" data-act="import">导入 JSON</button></header><div class="acc-wrap"><table class="acc"><thead><tr><th class="mark"></th><th>账号</th><th>状态</th><th>积分</th><th>成功 / 失败</th><th>在途</th><th>用量</th><th>域</th><th></th></tr></thead><tbody>' + rowsHtml(list, mode) + '</tbody></table></div><div class="table-foot">共 ' + list.length + ' 个账号</div></div>';
}
function renderDetail() {
  const g = selectedGroup();
  if (!g) {
    const list = publicAccounts();
    $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:var(--accent)"></span><div><h1>公共号池</h1><div class="sub">未分组账号 + 已进入共享池的分组账号</div></div></div><div class="dh-acts"><span class="tag accent">默认池</span></div></div>' +
      '<div class="gp-cards"><div class="card"><div class="card-h"><h3>公共池</h3><span class="tag mute">config.api_key</span></div><div class="hint">公共池沿用现有主密钥；分组密钥不能反向访问公共池。</div></div>' +
      '<div class="card"><div class="card-h"><h3>网络出口</h3><span class="tag mute">直连</span></div><div class="hint">公共池账号不套用分组代理。</div></div>' +
      '<div class="card"><div class="card-h"><h3>共享来源</h3><span class="tag ' + (state.groups.filter(x => x.shared_pool).length ? 'warn' : 'mute') + '">' + state.groups.filter(x => x.shared_pool).length + ' 个分组</span></div><div class="hint">只有开启「进入公共池」的分组才会出现在这里。</div></div></div>' + accountsBox(list, 'public');
    return;
  }
  const list = groupAccounts(g.id);
  $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:' + esc(g.color || '#5b7cfa') + '"></span><div><h1>' + esc(g.name) + '</h1><div class="sub">' + esc(g.id) + ' · ' + g.member_count + ' 个账号</div></div></div><div class="dh-acts"><span class="tag ' + (g.shared_pool ? 'warn' : 'accent') + '">' + (g.shared_pool ? '共享分组' : '独立分组') + '</span><button class="xs" data-act="edit-group">编辑</button><button class="xs danger" data-act="delete-group">删除</button></div></div>' +
    '<div class="gp-cards">' + keyCard(g) + proxyCard(g) + shareCard(g) + '</div>' + accountsBox(list, 'group');
}
function renderAll() { renderStats(); renderRail(); renderDetail(); }
function showKeyModal(key) { $('keyBig').textContent = key; open('keyVeil'); }
function openGroupModal(edit) {
  state.editing = edit ? edit.id : null;
  state.groupColor = edit ? (edit.color || COLORS[0]) : COLORS[state.groups.length % COLORS.length];
  $('groupTitle').textContent = edit ? '编辑分组' : '新建分组';
  $('groupName').value = edit ? edit.name : '';
  $('groupShared').checked = !!(edit && edit.shared_pool);
  $('groupCallPublic').checked = !!(edit && edit.call_public);
  $('groupColors').innerHTML = COLORS.map(c => '<button class="swatch ' + (c === state.groupColor ? 'on' : '') + '" style="--c:' + c + '" data-act="group-color" data-color="' + c + '"></button>').join('');
  open('groupVeil');
}
function proxyHint(t) { if (t === 'http') return 'HTTP CONNECT；目标域名由本机解析。'; if (t === 'socks5') return 'SOCKS5（本地 DNS）：先解析目标域名再由代理转发 IP。'; return 'SOCKS5H（远程 DNS）：域名交给代理解析，适合避免 DNS 泄漏。'; }
function openProxyModal(g) {
  state.proxyGroup = g.id;
  const p = g.proxy;
  state.proxyType = p ? p.type : 'socks5h';
  $('proxyHost').value = p ? p.host : '';
  $('proxyPort').value = p ? p.port : 1080;
  $('proxyUser').value = p ? (p.username || '') : '';
  $('proxyPass').value = '';
  $('proxyTestBox').hidden = true; $('proxyTestBox').className = 'testbox';
  updateProxyUI(); open('proxyVeil');
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
  switchImportTab('single'); open('importVeil');
}
function switchImportTab(t) { state.importTab = t; document.querySelectorAll('#importVeil .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === t)); $('importSinglePanel').hidden = t !== 'single'; $('importBatchPanel').hidden = t !== 'batch'; }
function parseAccounts(text) {
  try {
    const d = JSON.parse(text); let arr = Array.isArray(d) ? d : (d && Array.isArray(d.accounts) ? d.accounts : (d && typeof d === 'object' ? [d] : []));
    return arr.filter(x => x && (x.uid || (x.account && x.account.uid)) && (x.access_token || x.accessToken || (x.auth && x.auth.accessToken)));
  } catch (e) { return []; }
}
function openConfirm(title, body, label, fn) { $('confirmTitle').textContent = title; $('confirmBody').innerHTML = body; $('confirmYes').textContent = label; state.confirm = fn; open('confirmVeil'); }

document.addEventListener('click', async e => {
  const el = e.target.closest('[data-act]'); if (!el) return;
  const act = el.dataset.act;
  try {
    if (act === 'select') { state.selected = el.dataset.id; renderAll(); return; }
    if (act === 'new-group') { openGroupModal(null); return; }
    if (act === 'edit-group') { const g = selectedGroup(); if (g) openGroupModal(g); return; }
    if (act === 'group-color') { state.groupColor = el.dataset.color; document.querySelectorAll('#groupColors .swatch').forEach(b => b.classList.toggle('on', b.dataset.color === state.groupColor)); return; }
    if (act === 'group-save') {
      const body = { name: $('groupName').value.trim(), color: state.groupColor, shared_pool: $('groupShared').checked, call_public: $('groupCallPublic').checked };
      if (!body.name) { toast('请填写分组名称', 'err'); return; }
      if (state.editing) { await api('groups/' + state.editing, { method: 'PATCH', body: JSON.stringify(body) }); toast('分组已更新', 'ok'); }
      else { const r = await api('groups', { method: 'POST', body: JSON.stringify(body) }); state.selected = r.group.id; showKeyModal(r.key); toast('分组已创建', 'ok'); }
      close('groupVeil'); await load(); return;
    }
    if (act === 'delete-group') {
      const g = selectedGroup(); if (!g) return;
      const body = '<div class="hint">删除 <b>' + esc(g.name) + '</b>？该组有 ' + g.member_count + ' 个账号。</div><label class="radio"><input type="radio" name="delMode" value="move" checked> 移到公共池（推荐）</label><label class="radio"><input type="radio" name="delMode" value="delete"> 完全删除（含 auth 文件，不可恢复）</label>';
      openConfirm('删除分组', body, '删除分组', async () => {
        const mode = document.querySelector('input[name="delMode"]:checked').value;
        await api('groups/' + g.id + '?account_action=' + mode, { method: 'DELETE' });
        state.selected = 'public'; close('confirmVeil'); await load(); toast('分组已删除', 'ok');
      }); return;
    }
    if (act === 'rotate-key') {
      const g = selectedGroup(); if (!g) return;
      const r = await api('groups/' + g.id + '/key/rotate', { method: 'POST' });
      showKeyModal(r.key); await load(); toast('新密钥已生成，旧密钥已失效', 'warn'); return;
    }
    if (act === 'toggle-shared' || act === 'toggle-call') {
      const g = selectedGroup(); if (!g) return;
      const body = { name: g.name, color: g.color, shared_pool: g.shared_pool, call_public: g.call_public };
      if (act === 'toggle-shared') body.shared_pool = el.checked; else body.call_public = el.checked;
      await api('groups/' + g.id, { method: 'PATCH', body: JSON.stringify(body) });
      await load(); toast('共享设置已更新', 'ok'); return;
    }
    if (act === 'edit-proxy') { const g = selectedGroup(); if (g) openProxyModal(g); return; }
    if (act === 'proxy-type') { state.proxyType = el.dataset.type; updateProxyUI(); return; }
    if (act === 'proxy-save') {
      const body = { type: state.proxyType, host: $('proxyHost').value.trim(), port: parseInt($('proxyPort').value, 10) || 0, username: $('proxyUser').value.trim(), password: $('proxyPass').value };
      await api('groups/' + state.proxyGroup + '/proxy', { method: 'PUT', body: JSON.stringify(body) });
      close('proxyVeil'); await load(); toast('代理已保存', 'ok'); return;
    }
    if (act === 'proxy-test') {
      const body = { type: state.proxyType, host: $('proxyHost').value.trim(), port: parseInt($('proxyPort').value, 10) || 0, username: $('proxyUser').value.trim(), password: $('proxyPass').value };
      const box = $('proxyTestBox'); box.hidden = false; box.className = 'testbox'; box.textContent = '正在测试…';
      const r = await api('groups/' + state.proxyGroup + '/proxy/test', { method: 'POST', body: JSON.stringify(body) });
      box.className = 'testbox ' + (r.ok ? 'ok' : 'err');
      box.textContent = r.ok ? ('连接成功 · ' + r.latency_ms + ' ms · HTTP ' + r.status + ' · ' + r.target) : ('连接失败：' + r.error);
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
      close('importVeil'); await load(); toast('导入完成：成功 ' + r.imported + '，跳过 ' + r.skipped, r.imported ? 'ok' : 'warn'); return;
    }
    if (act === 'add-account') {
      const g = selectedGroup(); $('addGroupName').textContent = g ? '· ' + g.name : '· 公共号池'; $('addWait').hidden = true; open('addVeil');
      state.addGroup = g ? g.id : ''; return;
    }
    if (act === 'start-login') {
      const realm = $('addRealm').value || 'cn';
      const r = await api('login/start', { method: 'POST', body: JSON.stringify({ realm, group: state.addGroup || '' }) });
      $('addUrl').textContent = r.url; $('addWait').hidden = false;
      const timer = setInterval(async () => {
        try {
          const p = await api('login/poll?state=' + encodeURIComponent(r.state));
          if (p.done) { clearInterval(timer); close('addVeil'); await load(); toast('账号已添加' + (p.group ? '到分组' : '到公共池'), 'ok'); }
        } catch (e) { /* 继续轮询 */ }
      }, 3000);
      return;
    }
    if (act === 'move-account') {
      const uid = el.dataset.uid;
      await api('accounts/' + encodeURIComponent(uid) + '/group', { method: 'POST', body: JSON.stringify({ group: '' }) });
      await load(); toast('已移到公共池', 'ok'); return;
    }
    if (act === 'remove-account') {
      const uid = el.dataset.uid;
      openConfirm('移除账号', '<div class="hint">从池中移除 ' + esc(uid) + '，不会删除 auth 文件。</div>', '移除', async () => {
        await api('accounts/' + encodeURIComponent(uid) + '/remove', { method: 'POST' }); close('confirmVeil'); await load(); toast('账号已移除', 'ok');
      }); return;
    }
    if (act === 'close') { close(el.dataset.target); return; }
    if (act === 'confirm-yes') { const fn = state.confirm; state.confirm = null; if (fn) await fn(); return; }
    if (act === 'copy-key') { copyText($('keyBig').textContent); return; }
    if (act === 'gate-ok') { const v = $('gateKey').value.trim(); if (!v) return; localStorage.setItem('wb2api.key', v); close('keyGate'); load(); return; }
  } catch (err) { toast(err.message || String(err), 'err'); }
});

$('btnRefresh').onclick = load;
$('btnTheme').onclick = () => { const cur = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light'; document.documentElement.dataset.theme = cur; localStorage.setItem('wb2gp.theme', cur); };
if (localStorage.getItem('wb2gp.theme')) document.documentElement.dataset.theme = localStorage.getItem('wb2gp.theme');
load();

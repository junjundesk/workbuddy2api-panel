/* 账号级批量动作：主页分组卡片与「号池分组」页共用同一套实现。
   ────────────────────────────────────────────────────────────────
   为什么抽出来：这两页原先各写一份「移动所选 / 导出所选 / 移除所选 / 添加账号 /
   导入 JSON」，按钮文案、确认语气、失败处理很快就不一致——改一页另一页永远跟不上。
   这里把按钮、弹窗、请求收在一处，页面只负责回答三件事：
     这个作用域里勾了哪些号（selUids）、做完怎么刷新（reload）、号池清单是什么（groups）。

   页面契约（mount 传入 env）：
     container     弹窗挂载容器 id（页面里放一个空 div）
     api           (path, opts) => Promise；失败 throw，err.body 带后端详情
     toast         (msg, cls)
     groups()      => [{ id, name, member_count, shared_pool }]
     publicCount() => 公共号池账号数（导出「按号池」时用）
     selUids(scope)        => [uid]   该作用域当前勾选的账号
     clearSel(scope, uids) => void    删除成功后清掉勾选
     reload()              => Promise 重新拉数据（移动 / 删除 / 导入后调用）
     confirm(title, text)  => 可选，同步返回 true/false；不传用原生 confirm
   作用域只是字符串：主页每个分组卡片一个（'_public' 或分组 id），分组页只有一个（'gp'）。
   模块不猜账号归属，要数据一律问页面。 */
window.AcctActions = (function () {
  'use strict';
  var env = null;
  var st = {
    // pickUids：行内单号按钮（移动 / 导出）临时指定的账号。设了就优先于「勾选集合」，
    // 这样同一个弹窗既能服务工具条的「移动所选」，也能服务行内某一个号的「移动」。
    pickUids: [],
    moveUids: [], moveScope: '',
    exportScope: '', exportTab: 'accounts', exportPools: {},
    importScope: '', importGroup: '', importTab: 'single',
    addGroup: '', pollTimer: 0
  };

  function $(id) { return document.getElementById(id); }
  function esc(s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  function openVeil(id) { var v = $(id); if (v) v.classList.add('on'); }
  function closeVeil(id) { var v = $(id); if (v) v.classList.remove('on'); }
  function say(m, c) { if (env && env.toast) env.toast(m, c); }
  function groups() { try { return (env && env.groups && env.groups()) || []; } catch (e) { return []; } }
  function publicCount() { try { return (env && env.publicCount) ? Number(env.publicCount() || 0) : 0; } catch (e) { return 0; } }
  function selUids(scope) { try { return (env && env.selUids ? env.selUids(scope) : []) || []; } catch (e) { return []; } }
  function clearSel(scope, uids) { try { if (env && env.clearSel) env.clearSel(scope, uids); } catch (e) { /* 只是清勾选，失败不影响主流程 */ } }
  function reload() { try { return env && env.reload ? Promise.resolve(env.reload()) : Promise.resolve(); } catch (e) { return Promise.resolve(); } }
  function picked(scope) {
    if (st.pickUids && st.pickUids.length) return st.pickUids.slice();
    return selUids(scope);
  }
  function groupName(id) { var gs = groups(); for (var i = 0; i < gs.length; i++) if (gs[i].id === id) return gs[i].name; return ''; }

  /* ── 工具条：一行按钮，disabled 由 sync 按勾选数刷新 ───────────────── */
  function bar(o) {
    o = o || {};
    var scope = String(o.scope || 'default');
    var gid = o.group == null ? '' : String(o.group);
    var at = ' data-aa-scope="' + esc(scope) + '" data-aa-group="' + esc(gid) + '"';
    return '<div class="aa-bar">' +
      '<span class="aa-info" data-aa-info="' + esc(scope) + '"></span>' +
      '<button class="aa-b" data-aa="move"' + at + ' disabled title="把勾选的账号移动到另一个号池（一个账号只属于一个分组）">移动所选</button>' +
      '<button class="aa-b" data-aa="export"' + at + ' disabled title="导出勾选账号的 JSON（含登录凭证），可在其他实例直接导入">导出所选</button>' +
      '<button class="aa-b danger" data-aa="remove"' + at + ' disabled title="删除勾选账号：池状态与 auths/ 下的凭证文件一并删除，不可恢复">移除所选</button>' +
      '<button class="aa-b" data-aa="add"' + at + ' title="浏览器 OAuth 登录，新账号直接落入当前号池（走该组代理）">添加账号</button>' +
      '<button class="aa-b" data-aa="import"' + at + ' title="粘贴或选择 JSON 文件批量导入账号">导入 JSON</button>' +
      '</div>';
  }

  // sync 只刷一个作用域：主页有多张卡片，各卡片的勾选互不相干。
  function sync(scope) {
    var n = selUids(scope).length;
    var info = document.querySelector('[data-aa-info="' + scope + '"]');
    if (info) info.textContent = n ? '已选 ' + n : '';
    var btns = document.querySelectorAll('[data-aa="move"],[data-aa="export"],[data-aa="remove"]');
    for (var i = 0; i < btns.length; i++) {
      var b = btns[i];
      if (b.getAttribute('data-aa-scope') !== scope) continue;
      b.disabled = !n;
      // 删除不可恢复，按钮上直接写数量，避免「以为只删一个」。
      if (b.getAttribute('data-aa') === 'remove') b.textContent = n ? '移除所选 (' + n + ')' : '移除所选';
    }
  }
  function syncAll() {
    var seen = {}, btns = document.querySelectorAll('[data-aa="move"]');
    for (var i = 0; i < btns.length; i++) {
      var s = btns[i].getAttribute('data-aa-scope') || '';
      if (seen[s]) continue;
      seen[s] = 1; sync(s);
    }
  }

  /* ── 移动 ─────────────────────────────────────────────────────────── */
  function openMove(scope, gid, only) {
    st.pickUids = (only && only.length) ? only.slice() : [];
    var uids = picked(scope);
    if (!uids.length) { say('请先勾选账号', 'err'); return; }
    // 单号操作没有「作用域」概念：成功后不清任何卡片的勾选。
    st.moveScope = (only && only.length) ? '' : scope; st.moveUids = uids.slice();
    var gs = groups(), cur = gid || '';
    var html = '<option value="">公共号池（未分组）</option>';
    for (var i = 0; i < gs.length; i++) {
      var g = gs[i];
      html += '<option value="' + esc(g.id) + '"' + (g.id === cur ? ' disabled' : '') + '>' + esc(g.name) + (g.id === cur ? '（当前）' : '') + '</option>';
    }
    var box = $('aaMoveTarget');
    box.innerHTML = html;
    // 默认落到第一个「不是当前」的目标：当前分组 disabled，选中它没有意义。
    var pick = '';
    for (var j = 0; j < box.options.length; j++) if (!box.options[j].disabled) { pick = box.options[j].value; break; }
    box.value = pick;
    $('aaMoveHint').textContent = '将 ' + uids.length + ' 个账号移动到目标号池（一个账号只能属于一个分组）。';
    openVeil('aaMoveVeil');
  }

  async function doMove() {
    var uids = st.moveUids.slice(), target = $('aaMoveTarget').value;
    if (!uids.length) { closeVeil('aaMoveVeil'); return; }
    var btn = $('aaMoveDo');
    if (btn) { btn.disabled = true; btn.textContent = '移动中…'; }
    var ok = 0, fail = 0, last = '';
    try {
      for (var i = 0; i < uids.length; i++) {
        try { await env.api('accounts/' + encodeURIComponent(uids[i]) + '/group', { method: 'POST', body: JSON.stringify({ group: target }) }); ok++; }
        catch (e) { fail++; last = e.message || String(e); }
      }
      closeVeil('aaMoveVeil');
      clearSel(st.moveScope, uids);
      await reload();
      var tn = target ? groupName(target) : '';
      say('已移动 ' + ok + ' 个账号到' + (tn ? '「' + tn + '」' : '公共号池') + (fail ? '，失败 ' + fail + '（' + last + '）' : ''), fail ? 'err' : 'ok');
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = '移动'; }
      syncAll();
    }
  }

  /* ── 移除 ─────────────────────────────────────────────────────────── */
  async function doRemove(scope, only) {
    var uids = (only && only.length) ? only.slice() : selUids(scope);
    if (!uids.length) { say('请先勾选账号', 'err'); return; }
    var text = '将删除 ' + uids.length + ' 个账号：池状态与 auths/ 下的凭证文件一并删除，且不可恢复。\n\n确认删除？';
    var yes = env && env.confirm ? env.confirm('移除账号', text) : window.confirm(text);
    if (!yes) return;
    var b = document.querySelector('[data-aa="remove"][data-aa-scope="' + scope + '"]');
    if (b) { b.disabled = true; b.textContent = '移除中…'; }
    try {
      var r = await env.api('accounts/remove_batch', { method: 'POST', body: JSON.stringify({ uids: uids }) });
      var ok = (r.removed || []).length, bad = (r.failed || []).length, fe = (r.file_errors || []).length;
      clearSel(scope, r.removed || []);
      say('已移除 ' + ok + ' 个账号' + (bad ? '，失败 ' + bad : '') + (fe ? '，凭证文件删除失败 ' + fe : ''), bad || fe ? 'err' : 'ok');
      await reload();
    } catch (e) {
      say('移除失败：' + (e.message || '未知错误'), 'err');
    } finally {
      syncAll();
    }
  }

  /* ── 导出 ─────────────────────────────────────────────────────────── */
  function openExport(scope, gid, only) {
    st.pickUids = (only && only.length) ? only.slice() : [];
    st.exportScope = scope; st.exportGroup = gid || ''; st.exportPools = {};
    // 有勾选（或指定了单号）就默认导勾选；没有退到按号池。
    st.exportTab = picked(scope).length ? 'accounts' : 'pools';
    renderExport();
    openVeil('aaExportVeil');
  }

  function exportPools() {
    var out = [{ id: 'public', name: '公共号池', meta: '未分组 + 共享分组', n: publicCount() }];
    var gs = groups();
    for (var i = 0; i < gs.length; i++) {
      out.push({ id: gs[i].id, name: gs[i].name, meta: (gs[i].shared_pool ? '共享' : '独立') + ' · ' + (gs[i].member_count || 0) + ' 个账号', n: gs[i].member_count || 0 });
    }
    return out;
  }

  function renderExport() {
    var tabs = document.querySelectorAll('#aaExportVeil [data-aa-etab]');
    for (var i = 0; i < tabs.length; i++) tabs[i].classList.toggle('on', tabs[i].getAttribute('data-aa-etab') === st.exportTab);
    $('aaExportPoolsPanel').hidden = st.exportTab !== 'pools';
    $('aaExportSelPanel').hidden = st.exportTab !== 'accounts';
    var pools = exportPools();
    $('aaExportPools').innerHTML = pools.map(function (p) {
      return '<label class="aa-pool"><input type="checkbox" data-aa-pool="' + esc(p.id) + '"' + (st.exportPools[p.id] ? ' checked' : '') + '>' +
        '<span><b>' + esc(p.name) + '</b> <span class="aa-hint">' + esc(p.meta) + '</span></span></label>';
    }).join('');
    var uids = picked(st.exportScope);
    $('aaExportSel').innerHTML = uids.length
      ? uids.map(function (u) { return '<div class="aa-hint"><code>' + esc(u) + '</code></div>'; }).join('')
      : '<div class="aa-hint">当前列表还没有勾选账号，请先用行首复选框勾选。</div>';
    var n = 0;
    if (st.exportTab === 'pools') {
      pools.forEach(function (p) { if (st.exportPools[p.id]) n += p.n; });
      $('aaExportPreview').textContent = n ? '将导出约 ' + n + ' 个账号' : '未勾选号池';
    } else {
      n = uids.length;
      $('aaExportPreview').textContent = n ? '将导出 ' + n + ' 个账号' : '未勾选账号';
    }
  }

  async function doExport() {
    var uids = [], gs = [];
    if (st.exportTab === 'pools') {
      for (var k in st.exportPools) if (st.exportPools[k]) gs.push(k);
      if (!gs.length) { say('请先勾选号池', 'err'); return; }
    } else {
      uids = picked(st.exportScope);
      if (!uids.length) { say('请先勾选账号', 'err'); return; }
    }
    var btn = $('aaExportDo');
    if (btn) { btn.disabled = true; btn.textContent = '导出中…'; }
    try {
      var r = await env.api('accounts/export', { method: 'POST', body: JSON.stringify({ uids: uids, groups: gs }) });
      var data = { format: r.format, version: r.version, exported_at: r.exported_at, accounts: r.accounts };
      var blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
      var url = URL.createObjectURL(blob), link = document.createElement('a');
      var ts = new Date().toISOString().replace(/[-:T]/g, '').slice(0, 14);
      var one = r.accounts.length === 1;
      link.href = url;
      link.download = one ? 'workbuddy-' + r.accounts[0].uid + '.json' : 'workbuddy-accounts-' + ts + '.json';
      document.body.appendChild(link); link.click(); link.remove();
      setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
      closeVeil('aaExportVeil');
      say('已导出 ' + r.accounts.length + ' 个账号' + (r.missing && r.missing.length ? '（' + r.missing.length + ' 个不存在已跳过）' : '') + '；文件含登录凭证，请妥善保管', 'warn');
    } catch (e) {
      say('导出失败：' + (e.message || '未知错误'), 'err');
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = '导出 JSON'; }
    }
  }

  /* ── 导入 ─────────────────────────────────────────────────────────── */
  function openImport(scope, gid) {
    st.importScope = scope; st.importGroup = gid || '';
    var nm = gid ? groupName(gid) : '';
    $('aaImportGroupName').textContent = nm ? '· ' + nm : '· 公共号池';
    $('aaImportSingle').value = ''; $('aaImportBatch').value = ''; $('aaImportFile').value = '';
    switchImport('single');
    openVeil('aaImportVeil');
  }
  function switchImport(t) {
    st.importTab = t;
    var tabs = document.querySelectorAll('#aaImportVeil [data-aa-itab]');
    for (var i = 0; i < tabs.length; i++) tabs[i].classList.toggle('on', tabs[i].getAttribute('data-aa-itab') === t);
    $('aaImportSinglePanel').hidden = t !== 'single';
    $('aaImportBatchPanel').hidden = t !== 'batch';
    updateParse();
  }
  // 兼容三种来源：单个账号对象 / 数组 / {accounts:[...]}；也兼容插件 auths 原始嵌套
  // （account.uid + auth.accessToken）。识别不到 uid + token 的一律丢弃，避免把半截数据打给后端。
  function parseAccounts(text) {
    try {
      var d = JSON.parse(text);
      var arr = Array.isArray(d) ? d : (d && Array.isArray(d.accounts) ? d.accounts : (d && typeof d === 'object' ? [d] : []));
      return arr.filter(function (x) {
        return x && (x.uid || (x.account && x.account.uid)) && (x.access_token || x.accessToken || (x.auth && x.auth.accessToken));
      });
    } catch (e) { return []; }
  }
  function updateParse() {
    var box = $('aaImportParse');
    if (!box) return;
    var text = st.importTab === 'single' ? $('aaImportSingle').value : $('aaImportBatch').value;
    if (!text.trim()) { box.textContent = '等待输入…'; return; }
    var n = parseAccounts(text).length;
    box.textContent = n ? '识别到 ' + n + ' 个账号' : '没有识别到有效账号（需要 uid + access_token/accessToken；也兼容 auths 原文件的嵌套形式）';
  }
  async function doImport() {
    var text = st.importTab === 'single' ? $('aaImportSingle').value : $('aaImportBatch').value;
    var arr = parseAccounts(text);
    if (!arr.length) { say('没有识别到有效账号', 'err'); return; }
    var btn = $('aaImportDo');
    if (btn) { btn.disabled = true; btn.textContent = '导入中…'; }
    try {
      var fd = new FormData();
      fd.append('file', new Blob([JSON.stringify(arr)], { type: 'application/json' }), 'accounts.json');
      if (st.importGroup) fd.append('group', st.importGroup);
      var r = await env.api('import/cockpit', { method: 'POST', body: fd });
      closeVeil('aaImportVeil');
      await reload();
      var errs = r.errors || [];
      // toast 放不下全部原因：最多带 2 条，其余只计数，完整列表去控制台看。
      var errText = errs.length ? '（' + errs.slice(0, 2).join('；') + (errs.length > 2 ? ' 等 ' + errs.length + ' 条' : '') + '）' : '';
      say('导入完成：成功 ' + r.imported + '，跳过 ' + r.skipped + errText, r.imported ? 'ok' : 'warn');
    } catch (e) {
      say('导入失败：' + (e.message || '未知错误'), 'err');
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = '开始导入'; }
    }
  }

  /* ── 添加账号（浏览器 OAuth）：新号直接落当前号池，OAuth 请求走该组代理 ── */
  function openAdd(scope, gid) {
    st.addGroup = gid || '';
    var nm = gid ? groupName(gid) : '';
    $('aaAddGroupName').textContent = nm ? '· ' + nm : '· 公共号池';
    $('aaAddUrl').textContent = ''; $('aaAddWait').hidden = true;
    openVeil('aaAddVeil');
  }
  function stopPoll() { if (st.pollTimer) { clearInterval(st.pollTimer); st.pollTimer = 0; } }
  async function startLogin() {
    var realm = $('aaAddRealm').value || 'cn';
    var btn = $('aaAddStart');
    if (btn) btn.disabled = true;
    try {
      var r = await env.api('login/start', { method: 'POST', body: JSON.stringify({ realm: realm, group: st.addGroup || '' }) });
      $('aaAddUrl').textContent = r.url;
      $('aaAddWait').hidden = false;
      stopPoll();
      st.pollTimer = setInterval(async function () {
        try {
          var p = await env.api('login/poll?state=' + encodeURIComponent(r.state));
          if (p.done) {
            stopPoll(); closeVeil('aaAddVeil');
            await reload();
            say('账号已添加' + (p.group ? '到分组' : '到公共池'), 'ok');
          }
        } catch (e) { /* 轮询失败继续等，账号没授权完是常态 */ }
      }, 3000);
    } catch (e) {
      say('获取授权链接失败：' + (e.message || '未知错误'), 'err');
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  /* ── 弹窗：一次渲染好挂进页面，避免每页各写一遍 HTML ─────────────── */
  function modals() {
    var mv = '<div class="veil" id="aaMoveVeil"><div class="dlg" style="width:460px">' +
      '<header><h3>移动账号</h3><div class="aa-hint" id="aaMoveHint"></div></header>' +
      '<div class="body"><label class="aa-fld"><span>目标号池</span><select class="aa-in" id="aaMoveTarget"></select></label>' +
      '<div class="aa-hint">移动后立即按目标号池的密钥、代理与共享策略生效，账号凭证与状态保持不变。</div></div>' +
      '<footer><span class="aa-grow"></span><button class="aa-b" data-aa-close="aaMoveVeil">取消</button>' +
      '<button class="aa-b primary" id="aaMoveDo" data-aa-act="move-do">移动</button></footer></div></div>';

    var ex = '<div class="veil" id="aaExportVeil"><div class="dlg" style="width:620px">' +
      '<header><h3>导出账号 JSON<div class="aa-tabs"><button class="aa-tab" data-aa-etab="accounts">按勾选账号</button><button class="aa-tab" data-aa-etab="pools">按号池</button></div></h3>' +
      '<div class="aa-hint">导出内容含登录凭证（access / refresh token），可在其他实例直接导入。请妥善保管。</div></header>' +
      '<div class="body"><div id="aaExportSelPanel"><div class="aa-hint" style="margin-bottom:8px">导出当前列表里勾选的账号。</div><div id="aaExportSel"></div></div>' +
      '<div id="aaExportPoolsPanel" hidden><div class="aa-hint" style="margin-bottom:8px">勾选要导出的号池（可多选，跨分组一起导出）。「公共号池」= 未分组账号 + 开启共享的分组账号。</div><div class="aa-pools" id="aaExportPools"></div></div>' +
      '<div class="aa-preview" id="aaExportPreview"></div></div>' +
      '<footer><span class="aa-grow"></span><button class="aa-b" data-aa-close="aaExportVeil">取消</button>' +
      '<button class="aa-b primary" id="aaExportDo" data-aa-act="export-do">导出 JSON</button></footer></div></div>';

    var im = '<div class="veil" id="aaImportVeil"><div class="dlg" style="width:680px">' +
      '<header><h3>导入账号 <span class="aa-hint" id="aaImportGroupName"></span><div class="aa-tabs">' +
      '<button class="aa-tab" data-aa-itab="single">单账号 JSON</button><button class="aa-tab" data-aa-itab="batch">批量 JSON</button></div></h3></header>' +
      '<div class="body"><div id="aaImportSinglePanel"><div class="aa-hint">粘贴单个账号对象，兼容 cockpit 与 auth 原始字段。</div>' +
      '<textarea class="aa-ta" id="aaImportSingle" rows="8" placeholder=\'{"uid":"...","access_token":"..."}\'></textarea></div>' +
      '<div id="aaImportBatchPanel" hidden><div class="aa-hint">数组 [...] 或 {"accounts":[...]}；可一次选择多个 JSON 文件，自动合并去重。</div>' +
      '<input type="file" id="aaImportFile" accept=".json,application/json" multiple style="margin:8px 0">' +
      '<textarea class="aa-ta" id="aaImportBatch" rows="8" placeholder=\'[{"uid":"...","access_token":"..."}]\'></textarea></div>' +
      '<div class="aa-hint" id="aaImportParse" style="margin-top:8px">等待输入…</div></div>' +
      '<footer><span class="aa-grow"></span><button class="aa-b" data-aa-close="aaImportVeil">取消</button>' +
      '<button class="aa-b primary" id="aaImportDo" data-aa-act="import-do">开始导入</button></footer></div></div>';

    var ad = '<div class="veil" id="aaAddVeil"><div class="dlg" style="width:560px">' +
      '<header><h3>添加账号 <span class="aa-hint" id="aaAddGroupName"></span></h3></header>' +
      '<div class="body"><div class="aa-hint">浏览器 OAuth 登录，账号会直接落入当前号池；授权请求走该组代理。</div><div style="height:10px"></div>' +
      '<label class="aa-fld"><span>版本</span><select class="aa-in" id="aaAddRealm"><option value="cn">国内版 CN</option><option value="global">国际版 Global</option></select></label>' +
      '<button class="aa-b primary" id="aaAddStart" data-aa-act="start-login">获取授权链接</button>' +
      '<div id="aaAddWait" hidden><div class="aa-url" id="aaAddUrl"></div><div class="aa-dots">等待授权完成…</div></div></div>' +
      '<footer><span class="aa-grow"></span><button class="aa-b" data-aa-close="aaAddVeil">关闭</button></footer></div></div>';

    return mv + ex + im + ad;
  }

  /* ── 事件：一个 click 委托 + 一个 change 委托，页面不用再绑 ────────── */
  function onAction(el) {
    var act = el.getAttribute('data-aa-act');
    if (act === 'move-do') return doMove();
    if (act === 'export-do') return doExport();
    if (act === 'import-do') return doImport();
    if (act === 'start-login') return startLogin();
  }

  function onClick(ev) {
    var t = ev.target;
    if (!t || !t.closest) return;
    var b = t.closest('[data-aa]');
    if (b) {
      var act = b.getAttribute('data-aa');
      var scope = b.getAttribute('data-aa-scope') || '';
      var gid = b.getAttribute('data-aa-group') || '';
      if (act === 'move') openMove(scope, gid);
      else if (act === 'export') openExport(scope, gid);
      else if (act === 'remove') { var p = doRemove(scope); if (p && p.catch) p.catch(function () {}); }
      else if (act === 'add') openAdd(scope, gid);
      else if (act === 'import') openImport(scope, gid);
      return;
    }
    var et = t.closest('[data-aa-etab]');
    if (et) { st.exportTab = et.getAttribute('data-aa-etab'); renderExport(); return; }
    var it = t.closest('[data-aa-itab]');
    if (it) { switchImport(it.getAttribute('data-aa-itab')); return; }
    var a = t.closest('[data-aa-act]');
    if (a) { var q = onAction(a); if (q && q.catch) q.catch(function () {}); return; }
    var c = t.closest('[data-aa-close]');
    if (c) { var id = c.getAttribute('data-aa-close'); if (id === 'aaAddVeil') stopPoll(); closeVeil(id); return; }
    // 点遮罩空白处关闭；弹窗内部点击不冒泡处理。
    if (t.classList && t.classList.contains('veil') && /^aa[A-Z]/.test(t.id || '')) {
      if (t.id === 'aaAddVeil') stopPoll();
      closeVeil(t.id);
    }
  }

  function onChange(ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var pool = t.getAttribute('data-aa-pool');
    if (pool) { st.exportPools[pool] = !!t.checked; renderExport(); return; }
    if (t.id === 'aaImportSingle' || t.id === 'aaImportBatch') { updateParse(); return; }
    if (t.id === 'aaImportFile') readFiles(t);
  }

  // 支持一次多选多个 JSON 文件：逐个解析后按 uid 去重合并进批量框；解析不出账号的文件单独提示。
  async function readFiles(input) {
    var files = Array.prototype.slice.call(input.files || []);
    if (!files.length) return;
    var merged = {}, order = [], bad = [];
    for (var i = 0; i < files.length; i++) {
      var arr = parseAccounts(await files[i].text());
      if (!arr.length) { bad.push(files[i].name); continue; }
      for (var j = 0; j < arr.length; j++) {
        var uid = arr[j].uid || (arr[j].account && arr[j].account.uid);
        if (!merged[uid]) order.push(uid);
        merged[uid] = arr[j];
      }
    }
    $('aaImportBatch').value = order.length ? JSON.stringify(order.map(function (u) { return merged[u]; }), null, 1) : '';
    updateParse();
    $('aaImportParse').textContent = files.length + ' 个文件，识别到 ' + order.length + ' 个账号（已按 uid 去重）' + (bad.length ? '；无法识别：' + bad.join('、') : '');
  }

  function mount(e) {
    env = e || {};
    if (!env.api) throw new Error('AcctActions.mount 需要 api');
    var host = env.container ? $(env.container) : null;
    if (host) host.innerHTML = modals();
    if (!mount.bound) {
      document.addEventListener('click', onClick);
      document.addEventListener('change', onChange);
      mount.bound = true;
    }
    syncAll();
  }

  // 行内单号按钮也走同一套弹窗：只传 [uid]，弹窗照常渲染、照常打同一个接口。
  function actOn(kind, uid, gid) {
    if (kind === 'move') return openMove('', gid, [uid]);
    if (kind === 'export') return openExport('', gid, [uid]);
    if (kind === 'remove') return doRemove('', [uid]);
    if (kind === 'add') return openAdd('', gid);
    if (kind === 'import') return openImport('', gid);
  }

  return { bar: bar, sync: sync, syncAll: syncAll, modals: modals, mount: mount, actOn: actOn };
})();

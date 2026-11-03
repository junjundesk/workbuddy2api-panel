'use strict';
const $ = id => document.getElementById(id);
const COLORS = ['#5b7cfa','#3ddc97','#f5b544','#c084fc','#f0655f','#38bdf8','#fb923c','#a3e635'];
// 移动 / 导出 / 导入的临时状态都归共享的 acct_actions.js，这里只留本页自己的：
// 侧栏批量勾选（selPools）、分组编辑、确认弹窗、公共池代理回显、行内勾选（sel）。
const state = { groups: [], accounts: [], selected: 'public', editing: null, selPools: new Set(), groupColor: COLORS[0], proxyGroup: null, proxyType: 'socks5h', promptGroup: null, promptDraft: { system: [], user: [], replaces: [] }, confirm: null, publicProxy: null, sel: new Set(), revealed: {} };

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
/* hash 深链：#g=<分组 id>。主页分组卡片的「分组页」按钮就靠它直接落到该组，
   否则用户还得在侧栏里自己再找一遍。切分组时用 replaceState 回写 hash，
   保持可收藏 / 可分享，也不会在浏览器历史里堆一串分组切换记录。 */
let hashDone = false;
function hashTarget() {
  const m = /^#g=(.+)$/.exec(location.hash || '');
  if (!m) return '';
  // 手改 / 截断的 hash（#g=%zz）会让 decodeURIComponent 抛 URIError，被 load() 的
  // try 接住后整页显示「加载失败」——这里吞掉，落回公共池即可。
  try { return decodeURIComponent(m[1]); } catch (e) { return ''; }
}
function syncHash() {
  const want = state.selected === 'public' ? '#public' : '#g=' + encodeURIComponent(state.selected);
  if (location.hash !== want) { try { history.replaceState(null, '', want); } catch (e) {} }
}
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
  if (!r.ok) {
    // 失效账号的余额接口回 502，但响应体里带 dead / moved_to；挂到错误对象上，
    // 调用方才能提示「已移入失效池」而不是干巴巴一句 502。
    const err = new Error(d.error || ('HTTP ' + r.status));
    err.status = r.status; err.body = d;
    throw err;
  }
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
    // 首次加载按 hash 落组（分组不存在就留在公共池，不报错——深链可能来自已删分组）。
    // 之后每次 load 只把 hash 校准到当前选中，避免刷新 / 轮询把用户弹回别的组。
    if (!hashDone) { hashDone = true; const want = hashTarget(); if (want && groupOf(want)) state.selected = want; }
    syncHash();
    $('navState').textContent = '服务正常';
    renderAll();
    return true;
  } catch (e) {
    $('navState').textContent = '加载失败';
    if (String(e.message).indexOf('密钥') < 0) toast('加载失败：' + e.message, 'err');
    return false;
  }
}
/* ── 账号表：渲染与排序都来自 pool_list.js（共享模块） ─────────────────────
   与主页账号池是同一份实现（列集合 = PRESET.group）。以后加列、改状态文案、
   改按钮顺序、改排序口径只动 pool_list.js，两页同时生效——主页不用跟着改。
   本页只保留差异开关：勾选独立成列（表头能全选）、用量只给总量、操作外包一层
   .acts 便于窄屏换行。 */
const GP_SORT = PoolList.createSort('wb2api.gpSort');
function gpOpts(mode) {
  return {
    mode: 'group', style: 'group', sort: GP_SORT, actsWrap: 'div',
    usageDetail: false, sel: state.sel,
    emptyTitle: mode === 'group' ? '这个分组还没有账号' : '没有账号',
  };
}

function renderStats() {
  const pub = publicAccounts();
  const groupAcc = state.accounts.filter(a => a.group);
  let keys = 0, proxies = 0, healthy = 0;
  state.groups.forEach(g => { if (g.has_key) keys++; if (g.proxy) proxies++; }); if (state.publicProxy) proxies++;
  state.accounts.forEach(a => { if (a.group && PoolList.statusOf(a).cls === 'ok') healthy++; });
  const shared = state.groups.filter(g => g.shared_pool).length;
  $('stats').innerHTML =
    '<div class="stat accent"><div class="v">' + pub.length + '</div><div class="k">公共池可见<span>未分组 ' + state.accounts.filter(a => !a.group).length + ' + 共享 ' + (pub.length - state.accounts.filter(a => !a.group).length) + '</span></div></div>' +
    '<div class="stat"><div class="v">' + state.groups.length + '</div><div class="k">分组数量<span>共享 ' + shared + '</span></div></div>' +
    '<div class="stat good"><div class="v">' + groupAcc.length + '</div><div class="k">分组账号<span>可用 ' + healthy + '</span></div></div>' +
    '<div class="stat ' + (keys === state.groups.length ? 'good' : 'warn') + '"><div class="v">' + keys + '/' + state.groups.length + '</div><div class="k">分组密钥<span>独立鉴权</span></div></div>' +
    '<div class="stat"><div class="v">' + proxies + '</div><div class="k">代理出口<span>HTTP / SOCKS5 / SOCKS5H</span></div></div>';
  $('topMeta').textContent = '公共池 ' + pub.length + ' · ' + state.groups.length + ' 个分组 · ' + groupAcc.length + ' 个分组账号';
}
// poolRow 一个号池行：左侧勾选框（用于按号池导出）+ 右侧按钮（切换当前视图）。
// 勾选框与按钮分开两个元素：checkbox 不能放在 button 内部，事件会互相干扰。
function poolRow(id, name, meta, count, cls, color) {
  const checked = state.selPools.has(id) ? ' checked' : '';
  return '<div class="pool-row"><input type="checkbox" data-act="sel-pool" data-id="' + esc(id) + '"' + checked + ' title="勾选此号池以便批量导出">' +
    '<button class="pool-item ' + cls + (state.selected === id ? ' on' : '') + '" data-act="select" data-id="' + esc(id) + '">' +
    (color ? '<span class="gdot" style="--c:' + esc(color) + '"></span>' : '<span class="gdot"></span>') +
    '<span><b>' + esc(name) + '</b><em>' + esc(meta) + '</em></span><span class="cnt">' + count + '</span></button></div>';
}
function renderRail() {
  let html = poolRow('public', '公共号池', '未分组 + 共享分组', publicAccounts().length, 'public', '');
  html += '<div class="rail-sec">专属分组 <span>' + state.groups.length + '</span><button data-act="new-group">＋</button></div>';
  state.groups.forEach(g => {
    const bits = [g.member_count + ' 个账号', g.shared_pool ? '共享' : '独立'];
    if (g.call_public) bits.push('共用公共池');
    if (g.prompt_enabled) bits.push('自定义提示词');
    if (!g.has_key) bits.push('缺密钥');
    html += poolRow(g.id, g.name, bits.join(' · '), g.member_count, '', g.color || '#5b7cfa');
  });
  $('railList').innerHTML = html;
  updatePoolSelUI();
}
// updatePoolSelUI 同步号池勾选状态与「导出所选号池」按钮的可用性。
function updatePoolSelUI() {
  const btn = $('railExport'); if (!btn) return;
  const n = state.selPools.size;
  btn.disabled = !n;
  btn.textContent = n ? '导出所选号池（' + n + '）' : '导出所选号池';
}
// 测活结果：成功带上模型 / 耗时 / 回复片段；失败按阶段（模型目录 / 对话）给出原因。
// 服务端对上游失败仍回 200 + ok:false（与代理测试同口径），因此这里按 r.ok 选 toast 颜色。
function aliveToast(r) {
  if (r && r.dead) return '测活失败：账号已失效（401），已移入「' + (r.moved_to || '失效池') + '」';
  if (!r || !r.ok) {
    const where = r && r.stage === 'catalog' ? '模型目录' : '对话';
    return '测活失败（' + where + '）：' + ((r && r.error) || '未知错误');
  }
  let s = '测活通过';
  if (r.model) s += ' · ' + r.model;
  if (r.latency_ms != null) s += ' · ' + r.latency_ms + 'ms';
  return s + (r.reply ? '：' + r.reply : '');
}
// 账号行（状态标签、积分、操作按钮）全部由 pool_list.js 生成，这里不再各自拼 HTML。
// 分组批量操作，与主页分组卡片同口径：签到跳过禁用 / 企业版；任务执行只跑能做成长任务的号。
function taskEligible(a) { return !a.disabled && !a.enterprise && a.realm !== 'global'; }
function groupBulkBtns(list) {
  if (!list.length) return '';
  let h = '';
  if (list.some(taskEligible)) h += '<button class="xs primary" data-act="group-tasks" title="扫描本组账号的成长任务待办并排队执行（并发 = 本组账号数），进度在本页弹窗里跟">任务执行</button>';
  // 本组测活只出现在分组（独立号池）：公共池是混合来源、测活对象不明确，且它是默认池不宜一键打满。
  h += '<button class="xs" data-act="group-alive" title="逐个向本组账号发一句 hi 验证能否正常对话（每号消耗少量积分；不改选号 / 冷却状态），结果在弹窗清单里逐条展示">本组测活</button>';
  h += '<button class="xs" data-act="group-bulk" data-what="checkin" title="逐个签到本组账号（跳过禁用 / 企业版）">本组签到</button>';
  h += '<button class="xs" data-act="group-bulk" data-what="balance" title="逐个刷新本组账号余额">本组余额</button>';
  return h;
}
/* ── 本组测活：逐个账号发一句 hi，结果在弹窗的可滚动清单里逐条更新 ──────
   与行内单号「测活」同一接口（POST accounts/{uid}/alive），差别只在编排：
   一次跑整组、并发 ALIVE_CONC、每行独立落状态，跑完出汇总。
   走前端编排而不是后端批量接口：单号结果本来就一条一条回来，天然满足
   「清单逐条推进」；一次人工诊断动作也不值得引入服务端任务态。 */
const ALIVE_CONC = 4; // 并发上限：上游是流式对话，压太高容易把本组代理 / 上游限流打满
const aliveState = { running: false, stop: false, rows: [], name: '' };

// aliveRowHtml 一行清单的初始态：空心点 + 待测。
function aliveRowHtml(a) {
  const nick = a.nickname || a.uid.slice(0, 12);
  return '<div class="alive-row">' +
    '<span class="qdot wait"></span>' +
    '<span class="who"><div class="nm">' + esc(nick) + '</div><div class="id">' + esc(a.uid) + '</div></span>' +
    '<span class="st">待测</span>' +
    '<span class="msg">—</span></div>';
}
// aliveFlat 压平空白并按字符截断。上游错误常带整段 HTML（401 页面）与换行，
// 塞进单行清单既撑破行高又是注入面——压平 + 截断后统一 esc，一律当纯文本。
function aliveFlat(s, n) {
  const t = String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  return t.length > n ? t.slice(0, n) + '…' : t;
}
// aliveRowText 单号结果文案：成功带模型 / 耗时 / 回复预览，失败按阶段（模型目录 / 对话）给原因。
// 与 aliveToast 同口径，但这里输出 HTML——弹窗里模型与耗时用等宽字对齐。
// 上游回来的 error / reply 一律 esc：这些字段内容不受我们控制（错误页甚至是整段 HTML）。
function aliveRowText(r) {
  if (r && r.dead) return '账号已失效（401）· 已移入「' + esc(r.moved_to || '失效池') + '」';
  if (!r || !r.ok) {
    const where = r && r.stage === 'catalog' ? '模型目录' : '对话';
    return '失败（' + where + '）：' + (esc(aliveFlat(r && r.error, 160)) || '未知错误');
  }
  const bits = [];
  if (r.model) bits.push('<span class="mono">' + esc(r.model) + '</span>');
  if (r.latency_ms != null) bits.push('<span class="mono">' + r.latency_ms + 'ms</span>');
  if (r.ttfb_ms) bits.push('首字 <span class="mono">' + r.ttfb_ms + 'ms</span>');
  if (r.reply) bits.push(esc(r.reply));
  return bits.length ? bits.join(' · ') : '通过（无回复正文）';
}
// aliveSetRow 改写一行：state 取 wait/run/done/err/skip，与 .qdot 五态一一对应。
function aliveSetRow(row, state, text, msgHtml) {
  if (!row) return;
  row.className = 'alive-row' + (state === 'run' ? ' run' : state === 'done' ? ' done' : state === 'err' ? ' err' : '');
  const dot = row.querySelector('.qdot');
  if (dot) dot.className = 'qdot ' + (state || 'wait');
  const st = row.querySelector('.st');
  if (st) st.textContent = text;
  const msg = row.querySelector('.msg');
  if (msg) msg.innerHTML = msgHtml;
}
// aliveProgress 进度条与汇总：通过 / 失败 / 未测都算终态，口径同一。
function aliveProgress() {
  const rows = aliveState.rows, n = rows.length;
  let fin = 0, ok = 0, fail = 0, skip = 0;
  rows.forEach(r => {
    if (r.state === 'done') { fin++; ok++; }
    else if (r.state === 'err') { fin++; fail++; }
    else if (r.state === 'skip') { fin++; skip++; }
  });
  $('aliveBar').style.width = (n ? Math.round(fin / n * 100) : 0) + '%';
  $('aliveProgText').textContent = (aliveState.running ? '测活中 ' : '已结束 ') + fin + ' / ' + n;
  $('aliveSum').textContent = '通过 ' + ok + (fail ? ' · 失败 ' + fail : '') + (skip ? ' · 未测 ' + skip : '');
}
function stopAlive() {
  if (!aliveState.running) return;
  aliveState.stop = true;
  $('aliveStop').disabled = true;
  toast('已停止测活：剩余账号不再测试，在途的会跑完', 'warn');
}
/* ── 批量签到 / 余额：与主页「全部签到」同一套弹窗交互 ─────────────────────
   主页点「全部签到」弹的是逐号清单，分组页这边若还停在按钮上的数字进度，两页就对不上；
   更实际的原因是 401 失效归组只写在响应体里（dead / moved_to），只有逐号渲染
   才看得见「已移入失效池」，一次性的汇总 toast 会把它吞掉。 */
const BATCH_CONC = 4;
const batchState = { running: false, stop: false, rows: [], kind: '', name: '' };

// batchRowHtml 一行清单的初始态：空心点 + 待执行；skip 的行直接给跳过原因。
function batchRowHtml(a, hint) {
  const nick = a.nickname || a.uid.slice(0, 12);
  return '<div class="alive-row">' +
    '<span class="qdot wait"></span>' +
    '<span class="who"><div class="nm">' + esc(nick) + '</div><div class="id">' + esc(a.uid) + '</div></span>' +
    '<span class="st">' + esc(hint || '待执行') + '</span>' +
    '<span class="msg">—</span></div>';
}
// batchCredHtml 余额片段：剩余 / 总额；企业版总额是 -1（不限量），只显示剩余。
function batchCredHtml(r) {
  if (r.credits == null) return r.balance_error ? esc(aliveFlat(r.balance_error, 160)) : '无余额数据';
  const total = r.credits_total > 0 ? '<span class="mono"> / ' + r.credits_total + '</span>'
    : (r.credits_total === -1 ? ' <span class="mono">/ 不限</span>' : '');
  let s = '余额 <span class="mono">' + r.credits + '</span>' + total;
  if (r.balance_error) s += ' · 余额查询失败：' + esc(aliveFlat(r.balance_error, 120));
  return s;
}
// batchRowResult 把单号返回翻译成「状态点 + 状态词 + 说明」。dead / moved_to 是
// 后端 401 失效归组的契约字段：没有 moved_to 就只说「已失效」，不谎称已移组。
function batchRowResult(kind, r) {
  if (!r) return { state: 'err', st: '失败', msg: '没有返回结果' };
  if (r.dead) {
    const to = r.moved_to ? '「' + esc(r.moved_to) + '」' : '失效分组';
    return { state: 'err', st: '已失效', msg: '账号已失效（401）· 已移入' + to };
  }
  if (!r.ok) {
    const why = r.error || r.message || r.balance_error;
    return { state: 'err', st: '失败', msg: esc(aliveFlat(why, 160)) || '未知错误' };
  }
  if (kind === 'balance') return { state: 'done', st: '已刷新', msg: batchCredHtml(r) };
  // 签到：企业版无签到体系（端点只刷额度）；checkin_done=false 且有 message 才是真失败。
  if (r.enterprise) return { state: 'done', st: '企业版', msg: '无签到体系，仅刷新额度 · ' + batchCredHtml(r) };
  if (r.checkin_done) return { state: 'done', st: '已签', msg: '今日已签 · ' + batchCredHtml(r) };
  const why = r.checkin_message ? esc(aliveFlat(r.checkin_message, 140)) : '未签到';
  return { state: 'err', st: '未签', msg: '签到失败：' + why + ' · ' + batchCredHtml(r) };
}
// batchProgress 进度与汇总：done / err / skip 都算终态，口径与测活一致。
function batchProgress() {
  const rows = batchState.rows, n = rows.length;
  let fin = 0, ok = 0, fail = 0, skip = 0, dead = 0;
  rows.forEach(r => {
    if (r.state === 'done') { fin++; ok++; }
    else if (r.state === 'err') { fin++; fail++; if (r.dead) dead++; }
    else if (r.state === 'skip') { fin++; skip++; }
  });
  $('batchBar').style.width = (n ? Math.round(fin / n * 100) : 0) + '%';
  const verb = batchState.kind === 'balance' ? '刷新' : '签到';
  $('batchProgText').textContent = (batchState.running ? verb + '中 ' : '已结束 ') + fin + ' / ' + n;
  $('batchSum').textContent = '成功 ' + ok + ' · 失败 ' + fail + (dead ? '（其中失效 ' + dead + '）' : '') + (skip ? ' · 跳过 ' + skip : '');
}
// runAccountBatch 批量入口：items = [{ a: 账号, skip: '跳过原因' }]。
// 运行中再触发（含关掉弹窗后再点）只重开弹窗看进度，不重复开跑。
async function runAccountBatch(opts) {
  if (batchState.running) { openVeil('batchVeil'); return; }
  const items = opts.items || [];
  if (!items.length) { toast('没有可执行的账号', 'err'); return; }
  batchState.running = true; batchState.stop = false;
  batchState.kind = opts.kind; batchState.name = opts.name || '';
  batchState.rows = items.map(it => ({ a: it.a, skip: it.skip || '', state: it.skip ? 'skip' : 'wait', dead: false }));
  $('batchTitle').textContent = opts.title || '批量执行';
  $('batchScope').textContent = opts.scope || '';
  $('batchDesc').textContent = opts.desc || '';
  $('batchList').innerHTML = items.map(it => batchRowHtml(it.a, it.skip || '待执行')).join('');
  $('batchStop').disabled = false;
  openVeil('batchVeil');

  const nodes = Array.from($('batchList').children);
  // 跳过行先落终态，进度条从第一秒就反映「这批里有多少根本不跑」。
  batchState.rows.forEach((row, i) => {
    if (row.state === 'skip') aliveSetRow(nodes[i], 'skip', '跳过', esc(row.skip));
  });
  batchProgress();

  const path = batchState.kind === 'balance' ? 'balance' : 'checkin';
  let cursor = 0;
  const worker = async () => {
    for (;;) {
      if (batchState.stop) return;
      const i = cursor++;
      if (i >= items.length) return;
      const row = batchState.rows[i];
      if (row.state === 'skip') continue;
      row.state = 'run';
      aliveSetRow(nodes[i], 'run', path === 'balance' ? '刷新中…' : '签到中…', '请求上游…');
      batchProgress();
      let r;
      try { r = await api('accounts/' + encodeURIComponent(row.a.uid) + '/' + path, { method: 'POST' }); }
      // 单号失败只落到这一行，不中断整批；错误响应里的 dead / moved_to 一并带出来。
      catch (e) { r = Object.assign({ ok: false, error: e.message || String(e) }, (e && e.body) || {}); }
      const out = batchRowResult(path, r);
      row.state = out.state; row.dead = !!(r && r.dead);
      aliveSetRow(nodes[i], out.state, out.st, out.msg);
      batchProgress();
    }
  };
  try {
    await Promise.all(Array.from({ length: Math.min(BATCH_CONC, items.length) }, worker));
  } finally {
    // 收尾必须走 finally：异常时若 running 卡在 true，之后点「本组签到」只会重开弹窗。
    batchState.rows.forEach((row, i) => {
      if (row.state === 'wait') { row.state = 'skip'; aliveSetRow(nodes[i], 'skip', '已停止', '未执行'); }
    });
    batchState.running = false;
    $('batchStop').disabled = true;
    batchProgress();
  }
  const ok = batchState.rows.filter(r => r.state === 'done').length;
  const fail = batchState.rows.filter(r => r.state === 'err').length;
  const dead = batchState.rows.filter(r => r.dead).length;
  const skipN = batchState.rows.filter(r => r.state === 'skip').length;
  toast((opts.title || '批量执行') + ' 完成：成功 ' + ok + (fail ? '，失败 ' + fail : '') + (dead ? '，' + dead + ' 个账号已失效并移入失效池' : '') + (skipN ? '，跳过 ' + skipN : ''), fail ? 'err' : 'ok');
  await load(); // 签到 / 余额都会改池内积分与签到态，必须刷新
}
function stopBatch() {
  if (!batchState.running) return;
  batchState.stop = true;
  $('batchStop').disabled = true;
  toast('已停止：剩余账号不再执行，在途的会跑完', 'warn');
}

// runGroupAlive 分组详情「本组测活」：整组进清单 → 并发 ALIVE_CONC 逐号测 → 行级实时更新。
// 运行中再点（含关掉弹窗后再点）只重开弹窗看进度，不重复开测；测活对象含禁用 / 冷却 / 暂停号
// （测活是纯诊断动作，正好用来看「冷却中的号上游到底还行不行」）。
async function runGroupAlive(g) {
  if (aliveState.running) { openVeil('aliveVeil'); return; }
  const items = groupAccounts(g.id);
  if (!items.length) { toast('本组没有可测活的账号', 'err'); return; }
  aliveState.running = true; aliveState.stop = false; aliveState.name = g.name;
  aliveState.rows = items.map(a => ({ a: a, state: 'wait' }));
  $('aliveTitle').textContent = '· ' + g.name + '（' + items.length + ' 个账号）';
  $('aliveList').innerHTML = items.map(aliveRowHtml).join('');
  $('aliveStop').disabled = false;
  openVeil('aliveVeil');
  aliveProgress();

  const nodes = Array.from($('aliveList').children);
  let cursor = 0;
  const worker = async () => {
    for (;;) {
      if (aliveState.stop) return;
      const i = cursor++;
      if (i >= items.length) return;
      const row = aliveState.rows[i];
      row.state = 'run';
      aliveSetRow(nodes[i], 'run', '测活中…', '发送 hi 并等待回复…');
      aliveProgress();
      let r;
      try { r = await api('accounts/' + encodeURIComponent(row.a.uid) + '/alive', { method: 'POST' }); }
      catch (e) { r = { ok: false, stage: 'chat', error: e.message || String(e) }; } // 网络层失败只落到这一行，不中断整组
      row.state = r && r.ok ? 'done' : 'err';
      aliveSetRow(nodes[i], row.state, row.state === 'done' ? '通过' : '失败', aliveRowText(r));
      aliveProgress();
    }
  };
  await Promise.all(Array.from({ length: Math.min(ALIVE_CONC, items.length) }, worker));

  // 停止后没轮到的行转「已停止」；在途的照常跑完再落终态（不强杀上游连接）。
  aliveState.rows.forEach((row, i) => {
    if (row.state === 'wait') { row.state = 'skip'; aliveSetRow(nodes[i], 'skip', '已停止', '未测'); }
  });
  aliveState.running = false;
  $('aliveStop').disabled = true;
  aliveProgress();
  const ok = aliveState.rows.filter(r => r.state === 'done').length;
  const fail = aliveState.rows.filter(r => r.state === 'err').length;
  toast(g.name + ' 测活结束：通过 ' + ok + (fail ? '，失败 ' + fail : ''), fail ? 'err' : 'ok');
  await load(); // 测活不改池状态，刷新只为让余额变化立刻可见
}
function accountsBox(list, mode) {
  const opts = gpOpts(mode);
  const ordered = GP_SORT.sort(list);
  return '<div class="box"><header><h3>' + (mode === 'group' ? '分组账号' : '公共池账号') + ' <span class="hint">' + (mode === 'group' ? '仅本组密钥可见' : '未分组 + 共享') + '</span></h3><span class="grow"></span>' + (mode === 'group' ? groupBulkBtns(list) : '') + '</header><div class="gbar">' + AcctActions.bar({ scope: 'gp', group: mode === 'group' && selectedGroup() ? selectedGroup().id : '' }) + '</div><div class="acc-wrap"><table class="acc">' + PoolList.head(opts) + '<tbody>' + PoolList.rows(ordered, opts) + '</tbody></table></div><div class="table-foot">共 ' + list.length + ' 个账号' + (GP_SORT.key ? ' · 按' + GP_SORT.label() + '排序' : '') + '</div></div>';
}
function renderDetail() {
  const g = selectedGroup();
  // 配置卡（密钥 / 代理 / 共享池 / 提示词）由 group_cfg.js 渲染，和主页分组卡片是同一份 DOM；
  // 分组页只负责放一个容器，配置项的增删改不用再动这里。
  if (!g) {
    const list = publicAccounts();
    $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:var(--accent)"></span><div><h1>公共号池</h1><div class="sub">未分组账号 + 已进入共享池的分组账号</div></div></div><div class="dh-acts"><span class="tag accent">默认池</span></div></div>' +
      '<div id="gcCards" class="gc-cards"></div>' + accountsBox(list, 'public');
    GroupCfg.render('gcCards', null, { public: true, sharedCount: state.groups.filter(x => x.shared_pool).length });
    return;
  }
  const list = groupAccounts(g.id);
  $('detail').innerHTML = '<div class="detail-head"><div class="dh-title"><span class="group-mark" style="--c:' + esc(g.color || '#5b7cfa') + '"></span><div><h1>' + esc(g.name) + '</h1><div class="sub">' + esc(g.id) + ' · ' + g.member_count + ' 个账号</div></div></div><div class="dh-acts"><span class="tag ' + (g.shared_pool ? 'warn' : 'accent') + '">' + (g.shared_pool ? '共享分组' : '独立分组') + '</span><button class="xs" data-act="edit-group">编辑</button><button class="xs danger" data-act="delete-group">删除</button></div></div>' +
    '<div id="gcCards" class="gc-cards"></div>' + accountsBox(list, 'group');
  GroupCfg.render('gcCards', g);
}
function renderAll() { renderStats(); renderRail(); renderDetail(); updateSelUI(); }
function visibleAccounts() { const g = selectedGroup(); return g ? groupAccounts(g.id) : publicAccounts(); }
function updateSelUI() {
  const list = visibleAccounts(); const n = list.filter(a => state.sel.has(a.uid)).length;
  const all = document.querySelector('[data-act="sel-all"]'); if (all) { all.checked = n > 0 && n === list.length; all.indeterminate = n > 0 && n < list.length; }
  // 账号级动作条（移动 / 导出 / 移除 / 添加 / 导入）由共享的 acct_actions.js 渲染，
  // 勾选一变就让它重算按钮启停，避免页面自己再维护一份 disabled 逻辑。
  if (window.AcctActions) AcctActions.syncAll();
}
function selectedVisible() { return visibleAccounts().filter(a => state.sel.has(a.uid)).map(a => a.uid); }
/* ── 积分任务：与主页 app.js 同一套交互 ─────────────────────────────────
   主页点行内「任务」是在本页弹窗（openTasks → loadTasks；接受 / 领取 / 一键
   完成都直接打在弹窗里）。分组页原先用 window.open('/panel/?open=tasks&uid=…')
   深链回主页：运维正在看某个分组，一跳转就丢了当前号池、勾选与分组详情，回来
   还得重新点一遍。这里按「和主页逻辑相同」把实现原样搬过来——同样四个接口、
   同样的按钮语义、同样的行渲染与文案，唯一差别是数据刷新走本页 load()。
   下面 AUTO_TASKS / autoTaskHint 与 app.js 逐字一致：改一处就要同步改另一处，
   否则同一个任务在主页有「一键完成」、在分组页却只剩「接受」。
   （「模型」按钮仍是深链：那是另一套模型表弹窗，不在本次改动范围内。） */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'WorkBuddy_Valorant': '召唤「无畏契约拼豆专家」+ 真实对话（任务指定专家，实测点亮）',
  'expert_use_maidehao': '召唤「中国电商运营专家」+ 真实对话（任务指定专家）',
  'expert_use_douyinyin': '召唤「抖音策略师」+ 真实对话（任务指定专家）',
  'expert_use_shangjia_xuanchuan': '召唤「商家宣传页与海报设计师」+ 真实对话（任务指定专家）',
  'expert_use_bowangwang': '召唤「直播电商教练」+ 真实对话（任务指定专家）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

// autoTaskHint 可一键完成任务的说明文字；未知码返回空串（= 只展示操作指引）。
function autoTaskHint(code) {
  if (AUTO_TASKS[code]) return AUTO_TASKS[code];
  if (/^expert_use_/.test(String(code))) return '召唤任务指定的专家 + 完成一次真实对话（活动任务，后端按任务 jump_url 解析专家）';
  return '';
}

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  openVeil('taskVeil');
  loadTasks();
}
function closeTasks() { closeVeil('taskVeil'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast('已接受 ' + n + ' 个，' + r.failed.length + ' 个被上游拒绝（可重试）', 'err');
    } else {
      toast(n ? '已接受 ' + n + ' 个任务' : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast('执行完成：成功 ' + okN + ' 项，跳过 ' + skipN + ' 项' + (errN ? '，失败 ' + errN + ' 项' : ''), errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : autoTaskHint(t.task_code) ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(autoTaskHint(t.task_code)) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += '（进度 ' + r.progress_before + ' → ' + r.progress_after + '）';
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      load();
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') load();
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});


/* ── 账号实时模型列表：与主页 app.js 同一套实现 ─────────────────────────
   与主页「模型与档位」页的区别：那一页是「该域首个可用账号」的目录（网关缓存
   口径），这里回答「这个账号自己从最上游能看到哪些模型」——不同套餐 / 域 / 灰度
   可见模型可以不同。每次打开 / 重新拉取都真实打上游，并与同账号上次结果对比。
   原先这个按钮是 window.open('/panel/?open=models&uid=…')：和「任务」一样把运维
   甩去主页，现在同页弹窗。 */
let accModelUID = null;
function stripRealm(id) { return String(id || '').replace(/^(cn|global):/, ''); }
// rateCell 积分倍率单元格：活动价优先（原价加删除线 + 活动标签），无活动价原样显示。
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}
function openAccountModels(uid) {
  accModelUID = uid;
  $('accModelWho').textContent = uid.slice(0, 16);
  openVeil('accModelVeil');
  loadAccountModels();
}
function closeAccountModels() {
  closeVeil('accModelVeil');
  accModelUID = null;
}
$('btnCloseAccModel').onclick = closeAccountModels;
$('btnAccModelReload').onclick = loadAccountModels;

async function loadAccountModels() {
  if (!accModelUID) return;
  const st = $('accModelState'), tb = $('accModelBody'), tbl = $('accModelTable');
  const btn = $('btnAccModelReload');
  st.hidden = false; st.className = 'state';
  st.innerHTML = '<span class="dots">直连上游拉取中</span>';
  tbl.hidden = true;
  btn.disabled = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(accModelUID) + '/models');
    const list = d.models || [];
    const added = new Set((d.added || []).map(stripRealm));
    const removed = (d.removed || []).map(stripRealm);
    if (!list.length) {
      st.innerHTML = '<div class="empty">该账号上游未返回模型</div>';
      return;
    }
    st.hidden = true; tbl.hidden = false;
    tb.innerHTML = list.map(m => {
      const eff = (m.supported_efforts || []).slice();
      const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
      if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      const chg = added.has(stripRealm(m.id)) ? '<span class="tag ok">新增</span>' : '<span style="color:var(--ink-3)">—</span>';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + rateCell(m) + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        '<td>' + chg + '</td></tr>';
    }).join('');
    const parts = [list.length + ' 个模型', '域 ' + (d.realm || 'cn')];
    if (d.elapsed_ms != null) parts.push('耗时 ' + d.elapsed_ms + 'ms');
    parts.push(d.prev_at ? '对比上次 ' + new Date(d.prev_at).toLocaleTimeString() : '首次拉取（已建基线）');
    if (removed.length) parts.push('本次不再返回：' + removed.join('、'));
    $('accModelMeta').textContent = parts.join(' · ');
  } catch (e) {
    st.hidden = false; st.className = 'state';
    st.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  } finally { btn.disabled = false; }
}

/* ── 本组任务执行：队列进度在本页弹窗里跟 ───────────────────────────────
   原先启动队列后 window.open('/panel/#taskscenter')：一次「本组作业」把运维从当前
   分组带到主页任务中心，回来还得重新选分组。现在启动后直接在本页开进度弹窗，每 2s
   拉一次 GET tasks/queue（与任务中心同一个接口），跑完自动停轮询。
   关弹窗只停轮询、不停队列——服务端队列本来就和页面无生命周期绑定。 */
let queueTimer = null;
// 队列五态 → [圆点类, 文案]，与任务中心 / 本组测活同一套视觉语言。
const QUEUE_STATE = {
  pending: ['wait', '待执行'],
  running: ['run', '执行中'],
  done: ['done', '完成'],
  skipped: ['skip', '跳过'],
  error: ['err', '失败'],
};
function openQueueModal(label) {
  $('queueTitle').textContent = label ? '· ' + label : '';
  openVeil('queueVeil');
  loadQueue();
  if (!queueTimer) queueTimer = setInterval(loadQueue, 2000);
}
function stopQueuePoll() {
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
}
function closeQueueModal() { closeVeil('queueVeil'); stopQueuePoll(); }
$('btnQueueClose').onclick = closeQueueModal;
// 任务中心仍是新标签页，但改成「用户主动点」——不再作为队列启动的副作用。
$('btnQueueCenter').onclick = () => window.open('/panel/#taskscenter', '_blank');

async function loadQueue() {
  let q;
  try { q = await api('tasks/queue'); }
  catch (e) { stopQueuePoll(); $('queueProgText').textContent = '查询失败：' + e.message; return; }
  const items = q.items || [];
  const total = items.length;
  const n = s => items.filter(i => i.status === s).length;
  const done = n('done'), skip = n('skipped'), err = n('error'), run = n('running');
  const finished = done + skip + err;
  const bar = $('queueBar'), txt = $('queueProgText'), list = $('queueList'), sum = $('queueSum');
  bar.style.width = (total ? Math.round(finished * 100 / total) : 0) + '%';
  txt.textContent = total
    ? finished + ' / ' + total + ' 已结束' + (run ? ' · ' + run + ' 执行中' : '')
    : '准备中…';
  list.innerHTML = total ? items.map(it => {
    const st = QUEUE_STATE[it.status] || ['wait', it.status || '—'];
    const who = it.nickname || (it.uid || '').slice(0, 8);
    return '<div class="alive-row qrow"><span class="qdot ' + st[0] + '"></span>' +
      '<span title="' + esc(it.uid || '') + '">' + esc(who) + '</span>' +
      '<span class="note" title="' + esc(it.code || '') + '">' + esc(it.code || '—') + '</span>' +
      '<span class="note">' + esc(st[1] + (it.message ? ' · ' + it.message : '')) + '</span></div>';
  }).join('') : '<div class="hint" style="padding:14px 20px">队列为空</div>';
  sum.textContent = q.running ? '队列运行中…'
    : (skip || err ? '已结束：完成 ' + done + '，跳过 ' + skip + (err ? '，失败 ' + err : '') : '已结束：完成 ' + done + ' 项');
  // 队列跑完（且没有待执行的）就停轮询，避免弹窗开着空转。
  if (!q.running && finished >= total) stopQueuePoll();
}

function openConfirm(title, body, label, fn) { $('confirmTitle').textContent = title; $('confirmBody').innerHTML = body; $('confirmYes').textContent = label; state.confirm = fn; openVeil('confirmVeil'); }

document.addEventListener('click', async e => {
  const el = e.target.closest('[data-act]'); if (!el) return;
  const act = el.dataset.act;
  try {
    if (act === 'select') { state.selected = el.dataset.id; state.sel.clear(); renderAll(); syncHash(); return; }
    if (act === 'sort') { GP_SORT.toggle(el.dataset.key); renderDetail(); updateSelUI(); return; }
    if (act === 'sel-one') { if (el.checked) state.sel.add(el.dataset.uid); else state.sel.delete(el.dataset.uid); updateSelUI(); return; }
    if (act === 'sel-all') { const on = el.checked; visibleAccounts().forEach(a => { if (on) state.sel.add(a.uid); else state.sel.delete(a.uid); }); document.querySelectorAll('[data-act="sel-one"]').forEach(c => { c.checked = on; }); updateSelUI(); return; }
    if (act === 'sel-pool') {
      if (el.checked) state.selPools.add(el.dataset.id); else state.selPools.delete(el.dataset.id);
      updatePoolSelUI(); return;
    }
    if (act === 'sel-groups-all') {
      const ids = ['public'].concat(state.groups.map(g => g.id));
      const all = ids.every(id => state.selPools.has(id));
      ids.forEach(id => { if (all) state.selPools.delete(id); else state.selPools.add(id); });
      renderRail(); return;
    }
    // 导出 / 移动 / 移除 / 添加 / 导入 全部走共享的 acct_actions.js：弹窗与请求只有一份，
    // 这里只做「点到了哪个入口」的分发（行内单号按钮另走 act === 'acc'）。
    if (act === 'export-groups') { AcctActions.actOn('export'); return; }
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
    if (act === 'acc') {
      const u = el.dataset.uid, a = el.dataset.a, p = 'accounts/' + encodeURIComponent(u) + '/';
      // 「任务」「模型」都在本页弹窗（与主页同一套实现）：深链回主页会把运维从
      // 当前分组踢走，回来还得重新点一遍分组。账号行的按钮一律不再跳页。
      if (a === 'tasks') { openTasks(u); return; }
      if (a === 'models') { openAccountModels(u); return; }
      // 行内的移动 / 导出 / 移除和工具条上的「移动所选」共用同一个弹窗与同一个接口，
      // 只是把作用域换成「就这一个号」。
      if (a === 'move') { AcctActions.actOn('move', u, el.dataset.group || ''); return; }
      if (a === 'export') { AcctActions.actOn('export', u); return; }
      if (a === 'remove') { await AcctActions.actOn('remove', u); return; }
      // 公共池里显示账号所属分组时，直接在本页切过去，不再新开标签。
      if (a === 'gogroup') { const gid = el.dataset.g; if (gid && groupOf(gid)) { state.selected = gid; state.sel.clear(); renderAll(); syncHash(); } return; }
      if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
      el.disabled = true;
      try {
        // 200 也可能带 dead：签到拿到了结果、但随后的余额查询撞 401，账号已被移组。
        if (a === 'checkin') {
          const r = await api(p + 'checkin', { method: 'POST' });
          if (r.dead) toast('账号已失效（401），已移入「' + (r.moved_to || '失效池') + '」', 'err');
          else toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
        }
        else if (a === 'balance') {
          const r = await api(p + 'balance', { method: 'POST' });
          if (r.dead) toast('账号已失效（401），已移入「' + (r.moved_to || '失效池') + '」', 'err');
          else toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
        }
        else if (a === 'alive') {
          // 测活可能在途数秒：按钮显示进行中文案，结果（含上游失败）走 toast。
          el.textContent = '测活中…';
          try { const r = await api(p + 'alive', { method: 'POST' }); toast(aliveToast(r), r.ok ? 'ok' : 'err'); }
          finally { el.textContent = '测活'; }
        }
        else if (a === 'revive') { await api(p + 'revive', { method: 'POST' }); toast('已解冻', 'ok'); }
        else if (a === 'pause') { await api(p + 'pause', { method: 'POST' }); toast('已暂停选号（签到 / 保活照常）', 'ok'); }
        else if (a === 'resume') { await api(p + 'resume', { method: 'POST' }); toast('已恢复选号', 'ok'); }
        else if (a === 'disable') { await api(p + 'disable', { method: 'POST' }); toast('已禁用', 'ok'); }
      } catch (e) {
        // 余额接口对失效号回 502，dead / moved_to 在错误体里；不接住就只剩一句 502。
        const mv = e.body && e.body.moved_to;
        toast(e.body && e.body.dead ? '账号已失效（401），已移入「' + (mv || '失效池') + '」' : (e.message || '操作失败'), 'err');
      } finally { el.disabled = false; await load(); }
      return;
    }
    if (act === 'group-alive') {
      const g = selectedGroup(); if (!g) return;
      await runGroupAlive(g); return;
    }
    if (act === 'alive-stop') { stopAlive(); return; }
    if (act === 'batch-stop') { stopBatch(); return; }
    if (act === 'group-bulk') {
      const g = selectedGroup(); if (!g || el.disabled) return;
      const what = el.dataset.what === 'balance' ? 'balance' : 'checkin';
      // 原来是在按钮上跑数字进度、失败只累加一个计数：401 失效号到底是谁、有没有
      // 移进失效池都看不见。改成和测活 / 主页「全部签到」同一套弹窗清单。
      // 跳过项不预先过滤，保留成「跳过」行，运维才能确认禁用号 / 企业版没被漏跑。
      const items = groupAccounts(g.id).map(a => ({
        a: a,
        skip: a.disabled ? '已禁用' : (what === 'checkin' && a.enterprise ? '企业版' : ''),
      }));
      if (!items.some(it => !it.skip)) { toast('本组没有可执行的账号', 'err'); return; }
      await runAccountBatch({
        kind: what,
        title: (what === 'checkin' ? '本组签到' : '本组余额') + ' · ' + g.name,
        scope: '（' + items.length + ' 个账号）',
        desc: '并发 ' + BATCH_CONC + '；跳过已禁用' + (what === 'checkin' ? '与企业版' : '') + '；401 失效号自动移入失效池。',
        items: items,
      });
      return;
    }
    if (act === 'group-tasks') {
      const g = selectedGroup(); if (!g || el.disabled) return;
      const items = groupAccounts(g.id).filter(taskEligible);
      if (!items.length) { toast('本组没有可执行成长任务的账号', 'err'); return; }
      const conc = items.length; // 并发 = 本组可执行账号数，账号内串行
      if (!confirm('扫描「' + g.name + '」' + items.length + ' 个账号的待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
      el.disabled = true;
      try {
        const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc, uids: items.map(a => a.uid), label: g.name }) });
        if (!r.started) { toast(r.message || '本组没有待办任务', 'ok'); return; }
        toast(g.name + '：队列已启动 ' + r.total + ' 项（并发 ' + (r.conc || conc) + '）', 'ok');
        // 少了「先开空白标签页防拦截」那一步：现在根本不开新标签，进度在本页弹窗里跟。
        openQueueModal(g.name);
      } finally { el.disabled = false; }
      return;
    }
    if (act === 'close') {
      closeVeil(el.dataset.target);
      return;
    }
    if (act === 'confirm-yes') { const fn = state.confirm; state.confirm = null; if (fn) await fn(); return; }
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
// 配置弹窗（代理 / 提示词 / 密钥）由共享组件注入，group_cfg.js 自己挂 document 级委托。
$('gcModals').innerHTML = GroupCfg.modals();
GroupCfg.mount({
  api: api,
  toast: toast,
  getGroup: () => selectedGroup(),
  getPublicProxy: () => state.publicProxy,
  reload: load,
  confirm: (title, text, okText, fn) => openConfirm(title, '<div class="hint">' + esc(text) + '</div>', okText, async () => { closeVeil("confirmVeil"); await fn(); }),
  copyText: copyText,
});
// 账号级动作（移动 / 导出 / 移除 / 添加账号 / 导入 JSON）与主页是同一份 acct_actions.js：
// 分组页只有一个列表，作用域固定 'gp'；勾选、刷新走本页现有实现。
$('aaModals').innerHTML = AcctActions.modals();
AcctActions.mount({
  container: 'aaModals',
  api: api,
  toast: toast,
  groups: () => state.groups.map(g => ({ id: g.id, name: g.name, member_count: g.member_count, shared_pool: g.shared_pool })),
  publicCount: () => publicAccounts().length,
  selUids: () => selectedVisible(),
  clearSel: (scope, uids) => { (uids || []).forEach(u => state.sel.delete(u)); updateSelUI(); },
  reload: load,
  // 删除确认用浏览器原生 confirm：富文本确认弹窗是回调式的，取消时没有完成信号，
  // 硬接成 Promise 会让调用方永远挂在 await 上。原生 confirm 同步返回 true/false。
});
load();

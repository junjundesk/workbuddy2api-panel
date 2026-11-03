/* pool_list.js —— 号池账号列表的唯一实现（主页账号池 + 号池分组页共用）。

   为什么独立成文件：这两个页面都要展示同一批账号，字段、状态口径、行内操作
   本来就该一致。以前各写一份，改「状态文案 / 加一个操作按钮 / 排序口径」要改两处，
   漏一处就变成两页显示不一样。现在列表的行、表头、排序、操作按钮只在这里定义，
   页面只提供容器与少量开关（列集合、勾选属性、按钮 data 属性风格）。

   设计取舍：
   - 行/表头返回 HTML 字符串（两页都是整块 innerHTML 重绘，没有虚拟 DOM 成本）；
   - 排序状态按页面各自持久化（storageKey 参数），主页与分组页可以按不同视角看；
   - 空值（没查到积分、从没跑过）永远沉底，升序降序都不占第一屏；
   - 所有上游来的字段一律 esc，错误页/昵称都可能带 HTML。 */
(function () {
  'use strict';

  // 状态按可运维性排，不是字典序；未知状态排最后。
  var STATUS_RANK = { ok: 0, cool: 1, paused: 2, off: 3 };
  var SORT_LABEL = { name: '账号', status: '状态', credits: '积分', calls: '成功 / 失败', inflight: '在途', tokens: '用量', realm: '域' };

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  function fmt(n) { return Number(n || 0).toLocaleString('zh-CN'); }
  function fmtTokens(n) {
    n = Number(n || 0);
    if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(0) + 'k';
    return String(n);
  }
  // fmtCount 精确到一位：999999 → 1m 而不是 1000k。
  function fmtCount(tokens) {
    if (tokens == null || tokens === '') return '—';
    var n = Number(tokens);
    if (!isFinite(n) || n < 0) return '—';
    if (n < 1000) return String(Math.round(n));
    var units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
    var unit = units[0];
    for (var i = 0; i < units.length; i++) if (n >= units[i][1]) unit = units[i];
    var value = n / unit[1];
    var rounded = Number(value.toFixed(1));
    var next = units[units.indexOf(unit) + 1];
    if (next && rounded >= 1000) { unit = next; value = n / unit[1]; rounded = Number(value.toFixed(1)); }
    return rounded + unit[0];
  }
  function fmtLatency(ms) {
    if (ms == null || ms === '') return '—';
    var n = Number(ms);
    if (!isFinite(n) || n <= 0) return '—';
    return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
  }
  function fmtRate(rate) {
    if (rate == null || rate === '') return '—';
    var n = Number(rate);
    if (!isFinite(n) || n < 0) return '—';
    return n.toFixed(1) + 'tok/s';
  }
  function parseTime(v) {
    var s = String(v || '');
    if (!s || s.indexOf('0001-') === 0) return 0;
    var ms = Date.parse(s);
    return isFinite(ms) ? ms : 0;
  }
  function fmtTime(ms) {
    var d = new Date(ms), p = function (n) { return String(n).padStart(2, '0'); };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }
  function dur(sec) {
    sec = Math.max(0, Math.round(sec));
    var h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
    return h ? h + '时' + String(m).padStart(2, '0') + '分'
      : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
  }
  function ago(iso) {
    var t = parseTime(iso);
    if (!t) return '—';
    var s = (Date.now() - t) / 1000;
    if (s < 0) return '刚刚';
    if (s < 60) return Math.floor(s) + ' 秒前';
    if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
    if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
    return Math.floor(s / 86400) + ' 天前';
  }

  /* ── 状态 ──────────────────────────────────────────────────────────────
     口径：封号 > 禁用 > 暂停 > 冷却 > 可用。
     冷却时长取 max(剩余冷却, 熔断截止, 降权截止)：三个字段来自不同机制，
     只看 cool_remaining_sec 会在熔断期间显示成「可用」。 */
  function coolSec(a, now) {
    var bl = (parseTime(a.breaker_until) - now) / 1000;
    var dg = (parseTime(a.degrade_until) - now) / 1000;
    return Math.max(Number(a.cool_remaining_sec) || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  }
  function coolKind(a, sec, now) {
    var bl = (parseTime(a.breaker_until) - now) / 1000;
    var dg = (parseTime(a.degrade_until) - now) / 1000;
    var own = Number(a.cool_remaining_sec) || 0;
    if (bl > Math.max(own, dg > 0 ? dg : 0)) return '熔断';
    if (dg > own) return '连败降权';
    return a.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却';
  }
  function statusOf(a) {
    var now = Date.now();
    if (a.banned) return { cls: 'off', tag: 'bad', text: '封号', title: '上游 11140 request illegal：已封号，积分不计入池统计' };
    if (a.disabled) return { cls: 'off', tag: 'bad', text: '已禁用', title: '已禁用：不参与选号，保号任务默认跳过' };
    if (a.paused) return { cls: 'paused', tag: 'warn', text: '已暂停选号', title: '已暂停选号：仍照常签到 / 保活 / 刷新余额' };
    var cool = coolSec(a, now);
    if (cool > 0) return { cls: 'cool', tag: 'warn', text: coolKind(a, cool, now) + ' · ' + dur(cool), title: '临时不可选号：' + fmtTime(now + cool * 1000) + ' 后可重试' };
    return { cls: 'ok', tag: 'ok', text: '可用', title: '可用' };
  }

  /* ── 积分 ──────────────────────────────────────────────────────────────
     企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出。
     此时剩余额度不参与展示 / 百分比，直接标「不限」。 */
  function credOf(a, maxCred) {
    var unlimited = a.credits_total === -1;
    var text = unlimited ? '不限'
      : (a.credits == null ? '—' : fmt(a.credits) + (a.credits_total > 0 ? '<span class="of">/' + fmt(a.credits_total) + '</span>' : ''));
    var pct = unlimited ? 100
      : (a.credits_total > 0 ? Math.min(100, Math.round((a.credits || 0) / a.credits_total * 100))
        : Math.round((a.credits || 0) / Math.max(1, maxCred || 1) * 100));
    var tip;
    if (a.banned) tip = '封号账号（11140）：余额不可用，不计入池积分统计';
    else if (unlimited) tip = '企业版不限量（上游 limitNum=-1）';
    else if (a.credits_total > 0) tip = (a.enterprise ? '企业版剩余额度 ' : '剩余 ') + a.credits + ' / ' + (a.enterprise ? '分配 ' : '总额 ') + a.credits_total + '（' + pct + '%）';
    else tip = '积分（相对池内最高）';
    // 成本台账：每模型实测单价，运维据此看「为什么总选它」。
    var costs = (a.model_costs || []).filter(function (c) { return c && c.model; });
    if (costs.length) {
      tip += '\n实测单价（credits/1K）：\n' + costs.map(function (c) {
        return '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k);
      }).join('\n');
    }
    return { text: text, pct: pct, tip: tip, unlimited: unlimited };
  }

  // rateLimitRows：模型级限流，比状态标签更细——状态是「这个号现在能不能用」，
  // 这里是「哪些模型被封到什么时候」，运维排查 429 时第一眼要看它。
  function rateLimitRows(a, now) {
    var rows = Array.isArray(a.rate_limited_models) ? a.rate_limited_models.filter(function (r) { return r && r.model; }) : [];
    if (!rows.length) return '';
    return '<div class="rate-limits">' + rows.map(function (row) {
      var model = String(row.model), kind = String(row.kind || 'rate_limit');
      var resetAt = parseTime(row.reset_at), until = parseTime(row.until), deadline = resetAt || until;
      var remain = deadline > now ? Math.round((deadline - now) / 1000) : 0;
      var detail, title;
      if (kind === 'model_unavailable') {
        detail = remain ? '预计 ' + dur(remain) + ' 后重试' : '等待重新探测';
        title = model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtTime(deadline) : '');
      } else {
        detail = resetAt ? '预计 ' + fmtTime(resetAt) + ' 解封' + (remain ? '（剩余 ' + dur(remain) + '）' : '')
          : (until ? '预计 ' + fmtTime(until) + ' 恢复（剩余 ' + dur(remain) + '）' : '预计解封时间未知');
        title = model + '\n上游重置：' + (resetAt ? fmtTime(resetAt) : '时间未知');
        if (until && resetAt && until < resetAt) {
          detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
          title += '\n网关最早重试：' + fmtTime(until);
        }
      }
      return '<div class="rate-limit ' + (kind === 'model_unavailable' ? 'model-unavailable' : '') +
        '" title="' + esc(title) + '"><b>' + esc(model) + '</b><span>' + esc(detail) + '</span></div>';
    }).join('') + '</div>';
  }

  /* ── 排序 ──────────────────────────────────────────────────────────────
     点表头：升序 → 降序 → 恢复默认（后端顺序，即加入顺序）。
     相同值回落到原下标：否则每次刷新同分账号到处跳，勾选也跟着乱。 */
  function sortValue(a, key) {
    switch (key) {
      case 'name': return (a.nickname || a.uid || '').toLowerCase();
      case 'status': { var r = STATUS_RANK[statusOf(a).cls]; return typeof r === 'number' ? r : 9; }
      case 'credits': return a.credits_total === -1 ? Number.MAX_SAFE_INTEGER : (a.credits == null ? null : a.credits);
      case 'calls': return (a.success_count || 0) * 1e9 + (a.err_total || 0);
      case 'inflight': return a.in_flight || 0;
      case 'tokens': return (a.token_usage && a.token_usage.total_tokens) || null;
      case 'realm': return a.realm || 'cn';
      default: return null;
    }
  }
  function sortList(list, st) {
    if (!st || !st.key) return list;
    var key = st.key, dir = st.dir;
    var base = new Map();
    list.forEach(function (a, i) { base.set(a.uid, i); });
    return list.slice().sort(function (x, y) {
      var vx = sortValue(x, key), vy = sortValue(y, key);
      var nx = vx === null || vx === undefined, ny = vy === null || vy === undefined;
      if (nx && ny) return base.get(x.uid) - base.get(y.uid);
      if (nx) return 1;
      if (ny) return -1;
      var c;
      if (typeof vx === 'string' || typeof vy === 'string') c = String(vx).localeCompare(String(vy), 'zh-Hans-CN');
      else c = vx === vy ? 0 : (vx < vy ? -1 : 1);
      return c ? c * dir : base.get(x.uid) - base.get(y.uid);
    });
  }
  function sortLabel(key) { return SORT_LABEL[key] || key; }
  function sortTh(st, key, label) {
    var on = !!(st && st.key === key);
    var arrow = on ? (st.dir === 1 ? '▲' : '▼') : '⇅';
    return '<th class="' + (on ? 'on' : '') + '" data-act="sort" data-key="' + key +
      '" title="按' + label + '排序：点一次升序、再点降序、第三次恢复默认">' + label + '<i class="sar">' + arrow + '</i></th>';
  }
  // createSort 一页一个实例：各自 localStorage key，各自记忆排序偏好。
  function createSort(storageKey) {
    var st = { key: '', dir: -1 };
    try {
      var v = JSON.parse(localStorage.getItem(storageKey) || 'null');
      if (v && typeof v.key === 'string' && v.key) st = { key: v.key, dir: v.dir === 1 ? 1 : -1 };
    } catch (e) { /* 隐私模式 / 脏数据：退回默认顺序 */ }
    function save() { try { localStorage.setItem(storageKey, JSON.stringify(st)); } catch (e) {} }
    return {
      get key() { return st.key; },
      get dir() { return st.dir; },
      sort: function (list) { return sortList(list, st); },
      th: function (key, label) { return sortTh(st, key, label); },
      label: function () { return sortLabel(st.key); },
      // toggle 三态：换列 → 默认方向（文本列升序、数值列降序）→ 反向 → 取消。
      toggle: function (key) {
        if (st.key !== key) st = { key: key, dir: (key === 'name' || key === 'realm') ? 1 : -1 };
        else if (st.dir === -1) st.dir = 1;
        else st = { key: '', dir: -1 };
        save();
        return st;
      },
    };
  }

  /* ── 行内操作 ──────────────────────────────────────────────────────────
     两页共用同一套按钮与顺序，差别只在 data 属性：
     主页行委托读 data-a / data-u，分组页读 data-act="acc" / data-a / data-uid。 */
  function acts(a, o) {
    var now = Date.now();
    var frozen = a.disabled || coolSec(a, now) > 0;
    function btn(key, label, cls, title, extra) {
      var attrs;
      if (o.style === 'group') attrs = ' data-act="acc" data-a="' + key + '" data-uid="' + esc(a.uid) + '"';
      else attrs = ' data-a="' + key + '" data-u="' + esc(a.uid) + '"';
      if (extra) Object.keys(extra).forEach(function (k) { attrs += ' data-' + k + '="' + esc(extra[k]) + '"'; });
      if (title) attrs += ' title="' + esc(title) + '"';
      return '<button class="xs ' + (cls || 'ghost') + '"' + attrs + '>' + label + '</button>';
    }
    var h = '';
    if (!a.enterprise) h += btn('checkin', a.checkin_done ? '已签' : '签到', '', a.checkin_done ? '今日已签到；点击可重新签到并刷新余额' : '');
    h += btn('balance', a.enterprise ? '额度' : '余额', '', a.enterprise ? '刷新企业版已分配额度（上游 get-enterprise-user-usage）' : '');
    if (!a.enterprise) h += btn('tasks', '任务', '', '查看 / 执行该账号的积分任务（弹窗，接受、领取、一键完成）');
    h += btn('models', '模型', '', '在控制台打开模型面板');
    h += btn('alive', '测活', '', '向该账号发一句 hi 验证能否正常对话（会消耗少量积分；不改选号 / 冷却状态）');
    h += frozen ? btn('revive', '解冻', 'primary')
      : (a.paused ? btn('resume', '恢复选号', 'primary')
        : btn('pause', '暂停选号', '', a.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额'));
    if (!a.disabled) h += btn('disable', '禁用');
    if (o.canMove) h += btn('move', '移动', '', '移动到公共池或其他分组');
    if (o.mode !== 'group' && a.group) h += btn('gogroup', '前往分组', '', '在「号池分组」页打开它所在的分组', { g: a.group });
    h += btn('export', '导出', '', '导出该账号 JSON（含登录凭证），可在其他实例导入');
    h += btn('remove', '移除', 'danger');
    return h;
  }

  /* ── 表格 ────────────────────────────────────────────────────────────── */
  var COLS = {
    mark: { th: '<th class="mark" aria-hidden="true"></th>', td: '<td class="mark" aria-hidden="true"><i></i></td>' },
    ck: {
      th: '<th class="ck"><input type="checkbox" data-act="sel-all" title="全选当前列表"></th>',
      td: function (a, o) {
        return '<td class="ck"><input type="checkbox" data-act="sel-one" data-uid="' + esc(a.uid) + '"' +
          (o.sel && o.sel.has(a.uid) ? ' checked' : '') + '></td>';
      },
    },
    name: {
      label: '账号', key: 'name',
      th: function (o) { return o.sort ? o.sort.th('name', '账号') : '<th>账号</th>'; },
      td: function (a, o) {
        var tags = (a.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') +
          (a.enterprise ? ' <span class="realm-tag">企业版</span>' : '');
        // 主页的勾选框就放在账号列里（省一列宽度）；分组页单独一列，便于表头全选。
        var pick = o.selInline ? '<label class="acc-pick" title="选中后可批量删除"><input type="checkbox" data-sel="' + esc(a.uid) + '"' +
          (o.sel && o.sel.has(a.uid) ? ' checked' : '') + '></label>' : '';
        var nick = a.nickname ? esc(a.nickname) : '<span class="dim">未命名</span>';
        var short = a.uid.length > 16 ? a.uid.slice(0, 16) + '…' : a.uid;
        return '<td class="who"><div class="nm">' + pick + nick + tags + '</div><div class="id">' + esc(short) + '</div></td>';
      },
    },
    status: {
      label: '状态', key: 'status',
      th: function (o) { return o.sort ? o.sort.th('status', '状态') : '<th>状态</th>'; },
      td: function (a, o, st) {
        var note = a.reason ? '<div class="note">' + esc(a.reason) + '</div>' : '';
        return '<td><span class="tag ' + st.tag + '" title="' + esc(st.title || st.text) + '">' + st.text + '</span>' + note +
          rateLimitRows(a, Date.now()) + '</td>';
      },
    },
    credits: {
      label: '积分', key: 'credits',
      th: function (o) { return o.sort ? o.sort.th('credits', '积分') : '<th>积分</th>'; },
      td: function (a, o, st, cr) {
        // 主页带余额条（一眼看出比例），分组页只要数字——同一份 tip，两种呈现。
        var bar = o.credBar ? '<div class="bar"><i style="width:' + cr.pct + '%"></i></div>' : '';
        return '<td class="cred num" title="' + esc(cr.tip) + '"><div class="n">' + cr.text + '</div>' + bar + '</td>';
      },
    },
    calls: {
      label: '成功 / 失败', key: 'calls',
      th: function (o) { return o.sort ? o.sort.th('calls', '成功 / 失败') : '<th>成功 / 失败</th>'; },
      td: function (a) {
        return '<td class="num">' + fmt(a.success_count) + ' <span class="dim">/</span> <span class="bad-txt">' + fmt(a.err_total) + '</span></td>';
      },
    },
    inflight: {
      label: '在途', key: 'inflight',
      th: function (o) { return o.sort ? o.sort.th('inflight', '在途') : '<th>在途</th>'; },
      td: function (a) { return '<td class="num">' + (a.in_flight || 0) + '</td>'; },
    },
    tokens: {
      label: '用量', key: 'tokens',
      th: function (o) { return o.sort ? o.sort.th('tokens', '用量') : '<th>用量</th>'; },
      td: function (a, o) {
        var tu = a.token_usage || {};
        if (!o.usageDetail) {
          return '<td class="num">' + (tu.total_tokens ? fmtTokens(tu.total_tokens) : '—') + '</td>';
        }
        var req = tu.request_count || 0;
        var total = fmtCount(tu.total_tokens);
        var latency = fmtLatency(tu.last_latency_ms);
        var rate = fmtRate(tu.last_tokens_per_second);
        var title = '最近一次：' + req + ' 次 / ' + total + ' / 延迟 ' + latency + ' / ' + rate;
        return '<td class="num usage-cell" title="' + esc(title) + '"><span class="usage-line" aria-label="' + esc(title) + '">' +
          '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
          '<span class="usage-item usage-total"><b>' + total + '</b>' + (total === '—' ? '' : '<em>tok</em>') + '</span>' +
          '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
          '<span class="usage-item usage-rate"><b>' + rate + '</b></span></span></td>';
      },
    },
    lastok: {
      label: '最近成功',
      th: function () { return '<th>最近成功</th>'; },
      td: function (a) { return '<td class="num dim">' + ago(a.last_success) + '</td>'; },
    },
    realm: {
      label: '域', key: 'realm',
      th: function (o) { return o.sort ? o.sort.th('realm', '域') : '<th>域</th>'; },
      td: function (a) { return '<td class="num">' + esc((a.realm || 'cn').toUpperCase()) + '</td>'; },
    },
    acts: {
      // 表头类由页面给：主页要 .c-acts（单表时压到最窄、不换行），分组页默认 .acth 即可。
      th: function (o) { return '<th class="' + (o.actsHeadClass || 'acth') + '"></th>'; },
      td: function (a, o) {
        var inner = acts(a, o);
        return o.actsWrap === 'div' ? '<td><div class="acts">' + inner + '</div></td>' : '<td class="acts">' + inner + '</td>';
      },
    },
  };

  var PRESET = {
    // 主页：勾选内嵌在账号列 + 最近成功 + 余额条；用量四项够细。
    overview: ['mark', 'name', 'status', 'credits', 'calls', 'inflight', 'tokens', 'lastok', 'acts'],
    // 分组页：勾选独立成列（表头能全选）、带域列、用量只给总量。
    group: ['mark', 'ck', 'name', 'status', 'credits', 'calls', 'inflight', 'tokens', 'realm', 'acts'],
  };

  function cols(o) { return o.columns || PRESET[o.mode] || PRESET.group; }
  function head(o) {
    var list = cols(o);
    var html = '';
    for (var i = 0; i < list.length; i++) {
      var c = COLS[list[i]];
      if (!c) continue;
      html += typeof c.th === 'function' ? c.th(o) : c.th;
    }
    return '<thead><tr>' + html + '</tr></thead>';
  }
  function row(a, o) {
    var st = statusOf(a);
    var cr = credOf(a, o.maxCred);
    var list = cols(o), html = '';
    for (var i = 0; i < list.length; i++) {
      var c = COLS[list[i]];
      if (!c) continue;
      html += typeof c.td === 'function' ? c.td(a, o, st, cr) : c.td;
    }
    return '<tr class="' + st.cls + '" title="uid: ' + esc(a.uid) + '">' + html + '</tr>';
  }
  function rows(list, o) {
    if (!list || !list.length) {
      return '<tr><td colspan="' + cols(o).length + '"><div class="empty"><div class="big">' +
        esc(o.emptyTitle || '没有账号') + '</div>' + esc(o.emptyHint || '点击「添加账号」或「导入 JSON」。') + '</div></td></tr>';
    }
    return list.map(function (a) { return row(a, o); }).join('');
  }
  function table(list, o) {
    return '<table class="' + (o.tableClass || 'acc') + '">' + (o.colgroup || '') + head(o) +
      '<tbody>' + rows(list, o) + '</tbody></table>';
  }

  window.PoolList = {
    esc: esc, fmt: fmt, fmtTokens: fmtTokens, fmtCount: fmtCount, fmtLatency: fmtLatency, fmtRate: fmtRate,
    dur: dur, ago: ago, fmtTime: fmtTime,
    statusOf: statusOf, coolSec: coolSec, credOf: credOf, rateLimitRows: rateLimitRows,
    sortValue: sortValue, sortList: sortList, sortTh: sortTh, sortLabel: sortLabel, createSort: createSort,
    acts: acts, head: head, row: row, rows: rows, table: table, COLS: COLS,
    columns: PRESET,
  };
})();

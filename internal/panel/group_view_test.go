package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 主页账号池按分组排列（方案 B）：公共池在前、分组按给定顺序。
// 公共池口径必须与号池分组页的 publicAccounts 一致：未分组 + 分组已删（兜底，分组页不兜）
// + 开启「进入公共池」的共享分组账号——共享组账号同时出现在自己的分组卡片与公共池卡片里。
// 标题计数口径与行内状态一致（禁用 > 暂停 > 冷却 > 可用），企业版不限量不计入积分合计。
func TestAppJSGroupAccountsForView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; grouping test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function groupAccountsForView');
const end = src.indexOf('const LS_ACC_FOLD');
if (start < 0 || end < 0) throw new Error('grouping helpers not found');
const ctx = { Date, Math, Map };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.g=groupAccountsForView; this.c=accGroupCounts;', ctx);
const groups = [{ id: 'ga', name: 'A', shared_pool: true }, { id: 'gb', name: 'B' }];
const list = [
  { uid: 'u1' }, { uid: 'u2', group: 'ga', credits: 10, credits_total: 100 },
  { uid: 'u3', group: 'gone' }, { uid: 'u4', group: 'ga', disabled: true, credits: 5, credits_total: 50 },
  { uid: 'u5', group: 'ga', paused: true }, { uid: 'u6', group: 'ga', cool_remaining_sec: 60 },
  { uid: 'u7', group: 'ga', credits_total: -1, credits: 999 },
];
const b = ctx.g(list, groups);
const c = ctx.c(b[1].items, Date.now());
process.stdout.write(JSON.stringify({
  keys: b.map(x => x.key), items: b.map(x => x.items.map(s => s.uid).join(',')),
  shared: b[1].shared, counts: [c.ok, c.cool, c.paused, c.off, c.credits, c.total],
  pub: [b[0].ungrouped, b[0].sharedN],
}));`
	f, err := os.CreateTemp(t.TempDir(), "group-view-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("grouping node test failed: %v\n%s", err, out)
	}
	// 公共池 = u1（未分组）+ u3（分组已删兜底）+ ga 共享组全部 5 个；
	// pub 计数拆开：ungrouped=2（u1/u3）、sharedN=5（ga 组）。
	const want = `{"keys":["_public","ga","gb"],"items":["u1,u2,u3,u4,u5,u6,u7","u2,u4,u5,u6,u7",""],"shared":true,"counts":[2,1,1,1,15,150],"pub":[2,5]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("grouping=%s want %s", out, want)
	}
}

// TestAppJSGroupCardBadge 分组卡片徽标：文案口径必须与号池分组页 rail 一致
// （共享 / 独立 · 共用公共池 · 缺密钥 / 自定义提示词），公共池再拆出未分组 / 共享来源。
// 徽标是「主页分组与号池分组页一致」的可见承诺，改成纯函数后单独锁住。
func TestAppJSGroupCardBadge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; badge test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function groupCardBadge');
const end = src.indexOf('const LS_ACC_FOLD');
if (start < 0 || end < 0) throw new Error('groupCardBadge not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.f=groupCardBadge;', ctx);
const cases = [
  [{ pub: true, ungrouped: 2, sharedN: 5 }, null],
  [{ pub: true, ungrouped: 2, sharedN: 0 }, null],
  [{ pub: true, ungrouped: 0, sharedN: 0 }, null],
  [{ shared: false, call: false }, { has_key: false }],
  [{ shared: true, call: true }, { has_key: true, prompt_enabled: true }],
  [{ shared: true, call: false }, { has_key: true, prompt_enabled: false }],
];
process.stdout.write(JSON.stringify(cases.map(c => ctx.f(c[0], c[1]))));`
	f, err := os.CreateTemp(t.TempDir(), "badge-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("badge node test failed: %v\n%s", err, out)
	}
	const want = `[{"text":"未分组 2 + 共享 5","cls":" accent"},{"text":"未分组 2","cls":""},{"text":"空池","cls":""},{"text":"独立 · 缺密钥","cls":" warn"},{"text":"共享 · 共用公共池 · 自定义提示词","cls":" accent"},{"text":"共享","cls":""}]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("badge=%s\nwant   %s", out, want)
	}
}

// TestGroupsJSRemoveSelected 分组页多选必须能一键移除。
//
// 早先勾完一堆账号只有「移动所选 / 导出所选」：要清失效号得一行行点「移除」，
// 每行各弹一次 confirm。现在按钮带勾选数、只确认一次、走 remove_batch 批量删，
// 删成功的从勾选里摘掉，失败的留下便于重试。
func TestGroupsJSRemoveSelected(t *testing.T) {
	path, err := filepath.Abs("groups.js")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		`data-act="remove-sel"`, // 工具条按钮
		`'移除所选 (' + n + ')'`,    // 勾选数直接写在按钮上
		`[data-act="move-sel"],[data-act="export-sel"],[data-act="remove-sel"]`, // 跟随勾选启停
		`if (act === 'remove-sel') { await removeSelected(); return; }`,
		`api('accounts/remove_batch'`,
		`(r.removed || []).forEach(u => state.sel.delete(u))`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("groups.js 缺少 %q：多选后应能一键移除所选账号", want)
		}
	}
	// 只确认一次：逐个 confirm 在清号时要点到手酸，也让「取消」语义不可预期。
	start := strings.Index(js, "async function removeSelected()")
	if start < 0 {
		t.Fatal("groups.js 缺少 removeSelected")
	}
	end := strings.Index(js[start:], "\n}\n")
	if end < 0 {
		t.Fatal("removeSelected 未闭合")
	}
	if n := strings.Count(js[start:start+end], "confirm("); n != 1 {
		t.Errorf("removeSelected 里 confirm 出现 %d 次，应只确认一次", n)
	}
}

// TestGroupsJSSortAccounts 分组内表头排序的口径。
//
// 运维在几百个号的组里找「谁快没积分了」「谁一直失败」靠排序，所以这里锁住三件
// 容易写错的事：企业版不限量（credits_total=-1）算最大值而不是最小；没查到积分的
// 账号永远沉底（升序降序都在最后，否则空值会白占一屏）；同分账号保持后端顺序，
// 否则每次刷新行都在跳，勾选跟着错位。
func TestGroupsJSSortAccounts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; sort test skipped")
	}
	// 排序已搬进共享模块 pool_list.js：整份模块在 vm 里跑，取 window.PoolList 直接验，
	// 断言的是两页真正共用的那份实现，而不是某一页的副本。
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 模块只依赖 window / localStorage：给最小桩即可（localStorage 用来记排序偏好）。
const ctx = { window: {}, localStorage: { getItem: () => null, setItem: () => {} } };
vm.createContext(ctx);
vm.runInContext(src, ctx);
const PL = ctx.window.PoolList;
if (!PL) throw new Error('window.PoolList missing');
const st = PL.createSort('test.sort');
const A = [
  { uid: 'a1', nickname: 'beta', credits: 50, success_count: 3, err_total: 1, in_flight: 2 },
  { uid: 'a2', nickname: 'Alpha', credits: 100, success_count: 9, err_total: 0, cool_remaining_sec: 600 },
  { uid: 'a3', nickname: 'delta', credits: null, success_count: 0, err_total: 0 },
  { uid: 'a4', nickname: 'gamma', credits_total: -1, credits: 5, success_count: 1, err_total: 2, disabled: true },
  { uid: 'a5', nickname: 'epsilon', credits: 100, success_count: 9, err_total: 0 },
];
const ids = l => l.map(a => a.uid).join(',');
// toggle 只能相邻切换：先落到该列，再翻到想要的方向。
function setS(key, dir) {
  for (let n = 0; n < 10 && (st.key !== key || st.dir !== dir); n++) st.toggle(key);
  if (st.key !== key || st.dir !== dir) throw new Error('cannot reach ' + key + '/' + dir + ', at ' + st.key + '/' + st.dir);
}
const out = { def: ids(st.sort(A)) };
setS('credits', -1); out.creditsDesc = ids(st.sort(A));
setS('credits', 1); out.creditsAsc = ids(st.sort(A));
setS('status', 1); out.statusAsc = ids(st.sort(A));
setS('calls', -1); out.callsDesc = ids(st.sort(A));
setS('name', 1); out.nameAsc = ids(st.sort(A));
setS('credits', 1);
out.th = st.th('credits', '积分');
out.thOff = st.th('name', '账号');
out.label = st.label();
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "sort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "pool_list.js").CombinedOutput()
	if err != nil {
		t.Fatalf("sort node test failed: %v\n%s", err, out)
	}
	var got struct {
		Def         string `json:"def"`
		CreditsDesc string `json:"creditsDesc"`
		CreditsAsc  string `json:"creditsAsc"`
		StatusAsc   string `json:"statusAsc"`
		CallsDesc   string `json:"callsDesc"`
		NameAsc     string `json:"nameAsc"`
		Th          string `json:"th"`
		ThOff       string `json:"thOff"`
		Label       string `json:"label"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("sort output not json: %v\n%s", err, out)
	}
	for _, c := range []struct{ name, got, want string }{
		{"默认顺序", got.Def, "a1,a2,a3,a4,a5"},
		{"积分降序", got.CreditsDesc, "a4,a2,a5,a1,a3"}, // 不限量最前、空值沉底、同分保序
		{"积分升序", got.CreditsAsc, "a1,a2,a5,a4,a3"},
		{"状态升序", got.StatusAsc, "a1,a3,a5,a2,a4"}, // ok → cool → paused → off
		{"成功降序", got.CallsDesc, "a2,a5,a1,a4,a3"},
		{"账号升序", got.NameAsc, "a2,a1,a3,a5,a4"},
		{"积分列名", got.Label, "积分"},
		{"排序中表头", got.Th, `<th class="on" data-act="sort" data-key="credits" title="按积分排序：点一次升序、再点降序、第三次恢复默认">积分<i class="sar">▲</i></th>`},
		{"未排序列", got.ThOff, `<th class="" data-act="sort" data-key="name" title="按账号排序：点一次升序、再点降序、第三次恢复默认">账号<i class="sar">⇅</i></th>`},
	} {
		if c.got != c.want {
			t.Errorf("%s=%q\nwant    %q", c.name, c.got, c.want)
		}
	}
}

// 号池列表（列、状态、积分、按钮、排序）只有 pool_list.js 一份实现：
// 两页都必须引它、都必须用 PoolList 渲染，且各自的副本要已经删干净——
// 否则「改一处两页生效」的承诺会在下一次改动里悄悄失效。
func TestPoolListSharedAcrossPages(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	index := read("index.html")
	groups := read("groups.html")
	app := read("app.js")
	gjs := read("groups.js")
	shared := read("pool_list.js")

	for _, c := range []struct{ name, src, want string }{
		{"主页引用共享模块", index, `<script src="pool_list.js"></script>`},
		{"分组页引用共享模块", groups, `<script src="/panel/pool_list.js"></script>`},
		{"共享模块导出", shared, "window.PoolList = {"},
		{"主页用共享表头", app, "PoolList.head("},
		{"主页用共享行", app, "PoolList.rows("},
		{"主页排序实例", app, "PoolList.createSort('wb2api.accSort')"},
		{"分组页用共享表头", gjs, "PoolList.head(opts)"},
		{"分组页用共享行", gjs, "PoolList.rows(ordered, opts)"},
		{"分组页排序实例", gjs, "PoolList.createSort('wb2api.gpSort')"},
	} {
		if !strings.Contains(c.src, c.want) {
			t.Errorf("%s：缺少 %q", c.name, c.want)
		}
	}
	// 旧的各页副本必须消失，否则下次有人改副本就又分叉了。
	for _, c := range []struct{ name, src, dead string }{
		{"主页行渲染副本", app, "function accRowHtml("},
		{"主页状态副本", app, "function statusOf(a)"},
		{"分组页行渲染副本", gjs, "function rowsHtml("},
		{"分组页按钮副本", gjs, "function accActs("},
		{"分组页状态副本", gjs, "function statusOf(a)"},
		{"分组页排序副本", gjs, "function sortAccounts("},
	} {
		if strings.Contains(c.src, c.dead) {
			t.Errorf("%s 仍然存在：%s", c.name, c.dead)
		}
	}
}

// 分组配置卡（密钥/代理/共享池/提示词）的内容由 group_cfg.js 决定，两页只是宿主。
// 这里在最小 DOM 沙箱里真跑一遍，防止卡片组成退化或公共池误挂分组密钥。
func TestGroupCfgCards(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; group cfg test skipped")
	}
	abs, err := filepath.Abs("group_cfg.js")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	out, err := exec.Command(node, filepath.Join("testdata", "gcfg_smoke.cjs"), abs).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))
	got := map[string]bool{}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	for _, c := range []struct {
		key  string
		want bool
		why  string
	}{
		{"grp", true, "分组卡缺少密钥/代理/共享池/提示词中的某一项"},
		{"pubHasKeyCard", false, "公共池卡片不该出现分组密钥"},
		{"pubScope", true, "公共池代理按钮缺少 data-gc-public，会错改当前分组"},
		{"pubShared", true, "公共池卡片缺少共享分组计数"},
		{"modals", true, "三个配置弹窗不完整"},
	} {
		if got[c.key] != c.want {
			t.Errorf("%s（%s=%v）", c.why, c.key, got[c.key])
		}
	}
}

// 两页共用同一份分组配置：都引入 group_cfg.js/css，都注入弹窗与上下文，旧副本已删净。
func TestGroupCfgSharedAcrossPages(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	index, gh, app, gjs := read("index.html"), read("groups.html"), read("app.js"), read("groups.js")
	shared := read("group_cfg.js")
	for _, c := range []struct{ name, src, want string }{
		{"主页引入样式", index, "/panel/group_cfg.css"},
		{"主页引入脚本", index, "group_cfg.js"},
		{"分组页引入样式", gh, "/panel/group_cfg.css"},
		{"分组页引入脚本", gh, "/panel/group_cfg.js"},
		{"共享模块导出", shared, "window.GroupCfg = {"},
		{"共享模块导出密钥弹窗", shared, "showKey: showKey"},
		{"主页注入弹窗", app, "GroupCfg.modals()"},
		{"主页注入上下文", app, "GroupCfg.mount("},
		{"主页渲染配置卡", app, "GroupCfg.render('grpCfg', g)"},
		{"分组页注入弹窗", gjs, "GroupCfg.modals()"},
		{"分组页注入上下文", gjs, "GroupCfg.mount("},
		{"分组页渲染配置卡", gjs, "GroupCfg.render('gcCards', g)"},
	} {
		if !strings.Contains(c.src, c.want) {
			t.Errorf("%s：缺少 %q", c.name, c.want)
		}
	}
	for _, c := range []struct{ name, src, dead string }{
		{"分组页密钥卡副本", gjs, "function keyCard("},
		{"分组页代理卡副本", gjs, "function proxyCard("},
		{"分组页提示词卡副本", gjs, "function promptCard("},
		{"分组页代理弹窗副本", gjs, "function openProxyModal("},
		{"分组页提示词行副本", gjs, "function renderPromptRows("},
	} {
		if strings.Contains(c.src, c.dead) {
			t.Errorf("%s 仍然存在：%s", c.name, c.dead)
		}
	}
}

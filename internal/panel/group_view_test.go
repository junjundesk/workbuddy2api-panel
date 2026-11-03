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
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf("const SORT_KEY = 'wb2api.gpSort';");
const end = src.indexOf('function renderStats(');
if (start < 0 || end < 0) throw new Error('sort block not found');
// statusOf 是排序的依赖（状态列按可运维性排），这里给个同口径的桩。
const ctx = {
  localStorage: { getItem: () => null, setItem: () => {} },
  statusOf: a => (a.disabled ? { cls: 'off' } : a.paused ? { cls: 'paused' } : a.cooling ? { cls: 'cool' } : { cls: 'ok' }),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + [
  '',
  'this.setSort=function(k,d){sortState.key=k;sortState.dir=d;};',
  'this.sortAccounts=sortAccounts;',
  'this.th=function(k,l){return sortTh(k,l);};',
  'this.label=sortStateLabel;',
].join('\n'), ctx);
const A = [
  { uid: 'a1', nickname: 'beta', credits: 50, success_count: 3, err_total: 1, in_flight: 2 },
  { uid: 'a2', nickname: 'Alpha', credits: 100, success_count: 9, err_total: 0, cooling: true },
  { uid: 'a3', nickname: 'delta', credits: null, success_count: 0, err_total: 0 },
  { uid: 'a4', nickname: 'gamma', credits_total: -1, credits: 5, success_count: 1, err_total: 2, disabled: true },
  { uid: 'a5', nickname: 'epsilon', credits: 100, success_count: 9, err_total: 0 },
];
const ids = l => l.map(a => a.uid).join(',');
const out = { def: ids(ctx.sortAccounts(A)) };
ctx.setSort('credits', -1); out.creditsDesc = ids(ctx.sortAccounts(A));
ctx.setSort('credits', 1); out.creditsAsc = ids(ctx.sortAccounts(A));
ctx.setSort('status', 1); out.statusAsc = ids(ctx.sortAccounts(A));
ctx.setSort('calls', -1); out.callsDesc = ids(ctx.sortAccounts(A));
ctx.setSort('name', 1); out.nameAsc = ids(ctx.sortAccounts(A));
ctx.setSort('credits', 1);
out.th = ctx.th('credits', '积分');
out.thOff = ctx.th('name', '账号');
out.label = ctx.label();
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "sort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "groups.js").CombinedOutput()
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

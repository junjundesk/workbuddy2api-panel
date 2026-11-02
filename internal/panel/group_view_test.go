package panel

import (
	"os"
	"os/exec"
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


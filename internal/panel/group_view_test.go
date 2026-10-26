package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 主页账号池按分组排列（方案 B）：公共池在前、分组按给定顺序；无分组 / 分组已删的账号归公共池；
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
	const want = `{"keys":["_public","ga","gb"],"items":["u1,u3","u2,u4,u5,u6,u7",""],"shared":true,"counts":[2,1,1,1,15,150]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("grouping=%s want %s", out, want)
	}
}

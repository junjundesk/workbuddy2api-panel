package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 号池多选 + 导出弹窗的前端行为：
//   - poolRow 渲染出勾选框与切换按钮（两个独立元素）；
//   - updatePoolSelUI 按已选号池数启用导出按钮并回填计数；
//   - renderExportModal 按已勾选号池给出预计导出数量。
func TestGroupsJSExportModal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS export test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function poolRow');
const end = src.indexOf('function openMoveModal');
if (start < 0 || end < 0) throw new Error('export helpers not found');
const els = {};
const mk = () => ({ innerHTML: '', textContent: '', hidden: false, disabled: false, checked: false, value: '', className: '', appendChild(){}, addEventListener(){}, querySelectorAll(){ return []; } });
const document = {
  getElementById: (id) => (els[id] = els[id] || mk()),
  querySelectorAll: () => [],
  createElement: () => mk(),
  addEventListener() {},
  documentElement: mk(),
  body: mk(),
};
const ctx = { document, localStorage: { getItem: () => null, setItem() {} }, console, JSON, Math, Date, Number, String, Object, Array, Map, Set, Blob: function(){}, URL: { createObjectURL: () => '', revokeObjectURL(){} }, encodeURIComponent, decodeURIComponent };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
const prelude = [
  "const state = { groups: [{id:'g1',name:'A',member_count:2,shared_pool:false},{id:'g2',name:'B',member_count:1,shared_pool:false}], accounts: [{uid:'u1'},{uid:'u2',group:'g1'},{uid:'u3',group:'g1'},{uid:'u4',group:'g2'}], selected:'g1', selPools:new Set(), exportPools:new Set(), exportTab:'pools', sel:new Set(), revealed:{} };",
  "const esc = s => String(s==null?'':s);",
  "const $ = id => document.getElementById(id);",
  "const groupOf = id => state.groups.find(g => g.id === id);",
  "const selectedGroup = () => groupOf(state.selected);",
  "const publicAccounts = () => state.accounts.filter(a => !a.group);",
  "const groupAccounts = id => state.accounts.filter(a => a.group === id);",
].join('\n');
const epilogue = [
  "this.renderRail = renderRail;",
  "this.updatePoolSelUI = updatePoolSelUI;",
  "this.renderExportModal = renderExportModal;",
  "this.poolRow = poolRow;",
  "this.state = state;",
].join('\n');
vm.runInContext(prelude + '\n' + src.slice(start, end) + '\n' + epilogue, ctx);
const row = ctx.poolRow('g1', 'A', '2 个账号', 2, '', '#5b7cfa');
if (!/data-act="sel-pool"/.test(row) || !/data-act="select"/.test(row)) throw new Error('poolRow missing sel/select actions: ' + row);
if (!/data-act="sel-pool" data-id="g1"/.test(row)) throw new Error('poolRow missing pool id: ' + row);
ctx.renderRail();
const rail = document.getElementById('railList').innerHTML;
const boxes = (rail.match(/data-act="sel-pool"/g) || []).length;
if (boxes !== 3) throw new Error('expected 3 pool checkboxes, got ' + boxes + '\n' + rail);
ctx.state.selPools = new Set(['public', 'g1']);
ctx.updatePoolSelUI();
const btn = document.getElementById('railExport');
if (btn.disabled) throw new Error('export button should be enabled');
if (!/2/.test(btn.textContent)) throw new Error('export button should show count: ' + btn.textContent);
ctx.state.exportPools = new Set(['public', 'g1']);
ctx.renderExportModal();
const prev = document.getElementById('exportPreview').textContent;
if (!/3/.test(prev)) throw new Error('preview should count 3 accounts: ' + prev);
const poolList = document.getElementById('exportPoolList').innerHTML;
if ((poolList.match(/data-act="export-pool"/g) || []).length !== 3) throw new Error('export pool list should have 3 rows: ' + poolList);
process.stdout.write('OK');`
	f, err := os.CreateTemp(t.TempDir(), "groups-export-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "groups.js").CombinedOutput()
	if err != nil {
		t.Fatalf("groups export node test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected output: %s", out)
	}
}

package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 提示词规则编辑器的前端行为：
//   - promptTextRow 按角色渲染 textarea + 删除按钮，正文走 HTML 转义；
//   - promptReplaceRow 回填关键词/替换值/作用范围（select 选中）；
//   - promptCounts 只统计非空条目，字数含系统与用户正文。
//
// 与 groups_export_test.go 同一套 node+vm 切片手法：groups.js 是 go:embed 静态
// 资源，Go 编译器只查语法（frontend_test.go），行为断言只能靠执行切片。
func TestGroupsJSPromptRuleRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS prompt rule test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PROMPT_ROLE_NAME');
const end = src.indexOf('function openImportModal');
if (start < 0 || end < 0) throw new Error('prompt rule helpers not found');
const els = {};
const mk = () => ({ innerHTML: '', textContent: '', value: '', checked: false, addEventListener() {}, querySelectorAll() { return []; }, focus() {} });
const document = { getElementById: id => (els[id] = els[id] || mk()), querySelectorAll: () => [], addEventListener() {} };
const ctx = { document, console, JSON, Math, Date, Number, String, Object, Array };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
const prelude = [
  "const state = { promptDraft: { system: [], user: [], replaces: [] } };",
  "const esc = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');",
  "const $ = id => document.getElementById(id);",
].join('\n');
const epilogue = [
  "this.promptTextRow = promptTextRow;",
  "this.promptReplaceRow = promptReplaceRow;",
  "this.promptCounts = promptCounts;",
  "this.state = state;",
].join('\n');
vm.runInContext(prelude + '\n' + src.slice(start, end) + '\n' + epilogue, ctx);

const row = ctx.promptTextRow('system', '<b>x</b>', 2);
if (!/data-kind="system"/.test(row) || !/data-idx="2"/.test(row)) throw new Error('text row missing role/idx: ' + row);
if (!/data-act="prompt-del"/.test(row)) throw new Error('text row missing delete: ' + row);
if (row.indexOf('&lt;b&gt;x&lt;/b&gt;') < 0) throw new Error('text row not escaped: ' + row);
if (row.indexOf('<b>x</b>') >= 0) throw new Error('text row leaked raw html: ' + row);

const rep = ctx.promptReplaceRow({ match: '旧', with: '新', scope: 'user' }, 1);
if (rep.indexOf('value="旧"') < 0 || rep.indexOf('value="新"') < 0) throw new Error('replace row values missing: ' + rep);
if (rep.indexOf('value="user" selected') < 0) throw new Error('replace row scope not selected: ' + rep);
if (!/data-act="prompt-del"/.test(rep) || !/data-idx="1"/.test(rep)) throw new Error('replace row missing delete/idx: ' + rep);

ctx.state.promptDraft = { system: ['a', ' '], user: ['b'], replaces: [{ match: 'k' }, { match: ' ' }] };
const c = ctx.promptCounts();
if (c.sys !== 1 || c.usr !== 1 || c.rep !== 1 || c.chars !== 3) throw new Error('counts mismatch: ' + JSON.stringify(c));
process.stdout.write('OK');`
	f, err := os.CreateTemp(t.TempDir(), "groups-prompt-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "groups.js").CombinedOutput()
	if err != nil {
		t.Fatalf("groups prompt node test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected output: %s", out)
	}
}

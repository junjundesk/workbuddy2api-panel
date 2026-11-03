package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 单分组配置导入导出的前端行为：
//   - cfgFileName 去掉路径/文件名非法字符，带日期后缀；
//   - parseGroupConfig 对格式/版本/代理参数给出可读错误，合法文件归一成展示用的摘要；
//   - groupConfigSummary 转义来源分组名、区分直连与含密码、说明"关闭但保留规则"。
//
// 与 groups_prompt_ui_test.go 同一套 node+vm 切片：只执行 cfgFileName→importGroupConfig
// 这段纯函数（不碰 DOM / 网络），renderDetail 之后的挂载逻辑由 frontend_test.go 的语法
// 检查与分组页冒烟覆盖。
func TestGroupConfigUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS group config UI test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync('groups.js', 'utf8');
const start = src.indexOf('function cfgFileName');
const end = src.indexOf('function renderDetail');
if (start < 0 || end < 0 || end < start) throw new Error('group config helpers not found');
const esc = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const ctx = { esc, console, JSON, Math, Date, Number, String, Object, Array, RegExp };
vm.createContext(ctx);
const epilogue = [
  'this.cfgFileName = cfgFileName;',
  'this.parseGroupConfig = parseGroupConfig;',
  'this.groupConfigSummary = groupConfigSummary;',
].join('\n');
vm.runInContext(src.slice(start, end) + '\n' + epilogue, ctx);

const name = ctx.cfgFileName('A/B C:中文');
if (!/^workbuddy-group-A-B-C-中文-\d{8}\.json$/.test(name)) throw new Error('filename sanitize: ' + name);

const okFile = JSON.stringify({
  format: 'workbuddy-group-config', version: 1,
  group: {
    name: '來源', shared_pool: true,
    proxy: { type: 'SOCKS5H', host: ' 10.0.0.2 ', port: 1080, username: 'u', password: 'p' },
    prompt: { enabled: true, entries: [{ role: 'system', text: 'hi' }], replaces: [{ match: 'a', with: 'b' }] },
  },
});
const cfg = ctx.parseGroupConfig(okFile);
if (cfg.proxy.type !== 'socks5h' || cfg.proxy.host !== '10.0.0.2' || cfg.proxy.port !== 1080) throw new Error('proxy normalize: ' + JSON.stringify(cfg.proxy));
if (!cfg.shared || cfg.callPublic) throw new Error('flags: ' + JSON.stringify(cfg));
if (cfg.prompt.entries.length !== 1 || cfg.prompt.replaces.length !== 1) throw new Error('prompt counts');

const legacy = ctx.parseGroupConfig(JSON.stringify({ group: { prompt: { enabled: true, text: '只有旧文本' } } }));
if (legacy.prompt.entries.length !== 0 || legacy.prompt.text !== '只有旧文本') throw new Error('legacy text dropped');

const rejects = [
  ['不是 JSON', '{oops', /JSON/],
  ['format 不符', JSON.stringify({ format: 'workbuddy-accounts', group: {} }), /format/],
  ['版本过高', JSON.stringify({ format: 'workbuddy-group-config', version: 2, group: {} }), /版本/],
  ['缺 group', JSON.stringify({ format: 'workbuddy-group-config', version: 1 }), /group/],
  ['代理类型', JSON.stringify({ group: { proxy: { type: 'ftp', host: 'h', port: 21 } } }), /代理类型/],
  ['代理端口', JSON.stringify({ group: { proxy: { type: 'http', host: 'h', port: 0 } } }), /端口/],
  ['缺 host', JSON.stringify({ group: { proxy: { type: 'http', host: ' ', port: 80 } } }), /host/],
];
for (const [label, text, re] of rejects) {
  let msg = '';
  try { ctx.parseGroupConfig(text); } catch (e) { msg = e.message; }
  if (!re.test(msg)) throw new Error(label + ' 未被拦住：' + (msg || '无错误'));
}

const html = ctx.groupConfigSummary(cfg);
if (html.indexOf('&lt;img') >= 0) throw new Error('summary escaped own text unexpectedly');
if (html.indexOf('socks5h://10.0.0.2:1080') < 0) throw new Error('summary proxy missing: ' + html);
if (html.indexOf('含密码') < 0) throw new Error('summary password badge missing');
if (html.indexOf('来源分组') < 0 || html.indexOf('共享池') < 0) throw new Error('summary rows missing');
if (html.indexOf('1 条提示词 · 1 条替换') < 0) throw new Error('summary prompt counts missing: ' + html);

const xss = ctx.groupConfigSummary(ctx.parseGroupConfig(JSON.stringify({ group: { name: '<img src=x>', proxy: { type: 'http', host: 'h', port: 1 } } })));
if (xss.indexOf('<img') >= 0) throw new Error('summary leaked raw html: ' + xss);

const direct = ctx.groupConfigSummary(ctx.parseGroupConfig(JSON.stringify({ group: { prompt: { enabled: false, text: 'x' } } })));
if (direct.indexOf('直连') < 0 || direct.indexOf('清除本组现有代理') < 0) throw new Error('direct proxy hint missing');
if (direct.indexOf('关闭') < 0 || direct.indexOf('规则保留') < 0 || direct.indexOf('1 条提示词') < 0) throw new Error('disabled prompt hint missing');

for (const want of ['data-act="cfg-export"', 'data-act="cfg-import"', 'cfgIOToolbar()']) {
  if (src.indexOf(want) < 0) throw new Error('groups.js 缺少 ' + want);
}
const page = fs.readFileSync('groups.html', 'utf8');
if (page.indexOf('id="cfgImportFile"') < 0) throw new Error('groups.html 缺少 cfgImportFile 文件选择框');
process.stdout.write('OK');`
	f, err := os.CreateTemp(t.TempDir(), "gcfg-io-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("group config UI node test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// 分组配置卡冒烟：在最小 DOM 沙箱里加载 group_cfg.js，检查组卡/公共池卡/弹窗的组成。
// 由 TestGroupCfgCards 调用：node testdata/gcfg_smoke.cjs <group_cfg.js>
const fs = require('fs'), vm = require('vm'), path = require('path');
const src = fs.readFileSync(path.resolve(process.argv[2]), 'utf8');
const sandbox = { console: console };
sandbox.window = sandbox;
sandbox.document = { getElementById: () => null, addEventListener: () => {} };
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'group_cfg.js' });
const G = sandbox.window.GroupCfg;
if (!G) { console.log(JSON.stringify({ error: 'no export' })); process.exit(0); }
const g = { id: 'g1', name: 'A', has_key: true, key_hint: 'wb-***', shared_pool: true, call_public: false,
  prompt_enabled: true, prompt_entries: 2, prompt_replaces: 1, prompt_chars: 12, prompt_preview: 'hi',
  proxy: { type: 'socks5h', host: '1.2.3.4', port: 1080 } };
const grp = G.cards(g);
const pub = G.cards(null, { public: true, sharedCount: 3 });
console.log(JSON.stringify({
  grp: ['分组密钥', '网络代理', '共享池', '提示词规则', 'data-act="gc-reveal-key"', 'data-act="gc-rotate-key"', 'gc-toggle-shared', 'gc-edit-prompt', 'SOCKS5H', '1.2.3.4:1080'].every(k => grp.indexOf(k) >= 0),
  pubHasKeyCard: pub.indexOf('<h3>分组密钥</h3>') >= 0,
  pubScope: pub.indexOf('data-gc-public="1"') >= 0,
  pubShared: pub.indexOf('3 个分组') >= 0,
  modals: ['gcProxyVeil', 'gcPromptVeil', 'gcKeyVeil'].every(k => G.modals().indexOf(k) >= 0),
}));

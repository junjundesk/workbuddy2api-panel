package panel

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 执行分组页实际加载的完整脚本，再通过点击委托验证新建和编辑。
// 仅校验语法或顶层求值，抓不到 openGroupModal/showKeyModal 这类点击后才触发的缺失函数。
func TestGroupsJSGroupModal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; group modal test skipped")
	}
	script := `const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const page = fs.readFileSync('groups.html', 'utf8');
const nodes = new Map(), listeners = {};
const copied = [];
let selectedText = '';
function element() {
  const classes = new Set();
  return {
    dataset: {}, style: {}, value: '', checked: false, textContent: '', children: [],
    classList: {
      add(c) { classes.add(c); }, remove(c) { classes.delete(c); },
      contains(c) { return classes.has(c); },
      toggle(c, on) { if (on) classes.add(c); else classes.delete(c); },
    },
    set innerHTML(html) { this.html = html; registerIDs(html); },
    get innerHTML() { return this.html || ''; },
    appendChild(child) { this.children.push(child); },
    addEventListener() {}, focus() { this.focused = true; }, remove() {},
    select() { selectedText = this.value; },
    closest(selector) { return selector === '[data-act]' && this.dataset.act ? this : null; },
    getAttribute(name) { return name === 'data-act' ? this.dataset.act : null; },
  };
}
function registerIDs(html) {
  for (const m of html.matchAll(/\bid="([^"]+)"/g)) {
    if (!nodes.has(m[1])) nodes.set(m[1], element());
  }
}
registerIDs(page);
function swatches() {
  return [...nodes.get('groupColors').innerHTML.matchAll(/<button\b[^>]*>/g)].map(m => {
    const el = element();
    const attr = name => (m[0].match(new RegExp(name + '="([^"]*)"')) || [])[1];
    el.dataset = { act: attr('data-act'), color: attr('data-color') };
    (attr('class') || '').split(/\s+/).forEach(c => el.classList.add(c));
    return el;
  });
}
let palette = [];
const ctx = {
  console, FormData: class FormData {},
  document: {
    getElementById: id => nodes.get(id) || null,
    createElement: element, documentElement: element(), body: element(),
    execCommand(command) { assert.equal(command, 'copy'); copied.push(selectedText); return true; },
    querySelector: () => null,
    querySelectorAll: selector => selector === '#groupColors .swatch' ? palette : [],
    addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
  },
  localStorage: { getItem: () => null, setItem() {} },
  location: { hash: '' }, history: { replaceState() {} },
  matchMedia: () => ({ matches: false }), addEventListener() {},
  fetch: () => new Promise(() => {}),
  setTimeout(fn, ms) { if (ms < 100) fn(); return 1; },
  clearTimeout() {}, setInterval: () => 1, clearInterval() {},
};
ctx.window = ctx;
vm.createContext(ctx);
// 与 HTML 保持同样的脚本集合和顺序；不加载主页 app.js 来掩盖缺失函数。
for (const m of page.matchAll(/<script src="\/panel\/([^"]+)"><\/script>/g)) {
  vm.runInContext(fs.readFileSync(m[1], 'utf8'), ctx, { filename: m[1] });
}
const state = vm.runInContext('state', ctx);
const colors = vm.runInContext('COLORS', ctx);
const existing = { id: 'g-existing', name: '原分组', color: colors[3], shared_pool: true, call_public: true };
const groups = [existing], requests = [];
let renders = 0, failure = '', beforeReload = null;
state.groups = groups.slice();
ctx.renderAll = () => { renders++; }; // 列表渲染不属于弹窗测试；保留实际 load() 刷新与选中逻辑。
const apiResponse = async (path, opts = {}) => {
  const method = opts.method || 'GET';
  const body = opts.body ? JSON.parse(opts.body) : null;
  requests.push({ path, method, body });
  if (failure) { const message = failure; failure = ''; throw new Error(message); }
  if (path === 'groups' && method === 'POST') {
    const group = { id: 'g-created', ...body }; groups.push(group);
    return { group, key: 'sk-test-group-key' };
  }
  if (path === 'groups/g-existing' && method === 'PATCH') { Object.assign(existing, body); return {}; }
  if (path === 'groups/g-created/key' && method === 'GET') return { key: 'sk-test-group-key' };
  if (path === 'groups' && method === 'GET') {
    if (beforeReload) { const inspect = beforeReload; beforeReload = null; await inspect(); }
    return { groups: groups.slice() };
  }
  if (path === 'overview' && method === 'GET') return { accounts: [], healthy: 0, total: 0 };
  throw new Error('unexpected API: ' + method + ' ' + path);
};
// 保留真实 api 以及 GroupCfg.mount 捕获的引用，只替换网络传输。
ctx.fetch = async (url, opts) => {
  assert(url.startsWith('/panel/api/'));
  const data = await apiResponse(url.slice('/panel/api/'.length), opts);
  return { ok: true, json: async () => data };
};
const el = id => { assert(nodes.has(id), 'missing DOM id: ' + id); return nodes.get(id); };
const errors = () => el('toasts').children.filter(n => n.className === 'tst err').map(n => n.textContent);
async function click(act, data = {}) {
  const target = element(); target.dataset = { act, ...data };
  for (const fn of listeners.click) await fn({ target, preventDefault() {} });
}
(async () => {
  await click('new-group');
  assert.deepEqual(errors(), [], '新建分组不应出现运行时错误');
  assert(el('groupVeil').classList.contains('on'));
  assert.equal(el('groupTitle').textContent, '新建分组');
  assert.equal(state.editing, null);
  assert.equal(el('groupName').value, '');
  assert.equal(el('groupShared').checked, false);
  assert.equal(el('groupCallPublic').checked, false);
  assert.equal(state.groupColor, colors[groups.length % colors.length]);
  palette = swatches();
  assert.deepEqual(palette.map(b => b.dataset.color), Array.from(colors));
  assert(palette.every(b => b.dataset.act === 'group-color'));
  assert.deepEqual(palette.filter(b => b.classList.contains('on')).map(b => b.dataset.color), [state.groupColor]);

  el('groupName').value = '   ';
  await click('group-save');
  assert.equal(requests.length, 0, '空名称不得提交');
  assert.equal(errors().pop(), '请填写分组名称');
  el('toasts').children = [];

  await click('group-color', { color: colors[2] });
  assert.equal(state.groupColor, colors[2]);
  assert.deepEqual(palette.filter(b => b.classList.contains('on')).map(b => b.dataset.color), [colors[2]]);
  el('groupName').value = '  新分组  ';
  el('groupShared').checked = true;
  beforeReload = async () => {
    assert.equal(renders, 0, '刷新完成前即应能复制密钥');
    assert(el('gcKeyVeil').classList.contains('on'));
    await click('gc-copy-key');
  };
  await click('group-save');
  assert.deepEqual(errors(), [], '创建成功后不应调用已移除的 showKeyModal');
  assert.deepEqual(requests[0], { path: 'groups', method: 'POST', body: { name: '新分组', color: colors[2], shared_pool: true, call_public: false } });
  assert(!el('groupVeil').classList.contains('on'));
  assert(el('gcKeyVeil').classList.contains('on'));
  assert.equal(el('gcKeyBig').textContent, 'sk-test-group-key');
  assert.equal(state.selected, 'g-created');
  assert.equal(renders, 1);
  assert.deepEqual(copied, ['sk-test-group-key'], '新组须在弹出密钥前进入本地状态');
  assert.equal(state.groups.filter(g => g.id === 'g-created').length, 1, '刷新不能重复追加新组');
  await click('gc-close', { target: 'gcKeyVeil' });
  assert(!el('gcKeyVeil').classList.contains('on'));

  state.selected = existing.id;
  await click('edit-group');
  assert.equal(state.editing, existing.id);
  assert.equal(el('groupTitle').textContent, '编辑分组');
  assert.equal(el('groupName').value, existing.name);
  assert.equal(state.groupColor, existing.color);
  assert.equal(el('groupShared').checked, true);
  assert.equal(el('groupCallPublic').checked, true);
  palette = swatches();
  assert.deepEqual(palette.filter(b => b.classList.contains('on')).map(b => b.dataset.color), [existing.color]);
  requests.length = 0;
  el('groupName').value = '已编辑';
  el('groupCallPublic').checked = false;
  await click('group-save');
  assert.deepEqual(errors(), []);
  assert.deepEqual(requests[0], { path: 'groups/g-existing', method: 'PATCH', body: { name: '已编辑', color: colors[3], shared_pool: true, call_public: false } });
  assert.equal(renders, 2);
  assert(!el('groupVeil').classList.contains('on'));
  assert(!el('gcKeyVeil').classList.contains('on'), '编辑不能弹出新密钥');

  await click('edit-group');
  requests.length = 0;
  el('groupName').value = '更新失败时保留';
  failure = '更新失败';
  await click('group-save');
  assert.equal(requests[0].method, 'PATCH');
  assert.equal(state.editing, existing.id);
  assert(el('groupVeil').classList.contains('on'));
  assert.equal(el('groupName').value, '更新失败时保留');
  assert.equal(errors().pop(), '更新失败');
  el('toasts').children = [];

  // 编辑后再新建必须清空编辑对象、名称、开关和颜色，不能误 PATCH 旧分组。
  await click('new-group');
  assert.equal(state.editing, null);
  assert.equal(el('groupName').value, '');
  assert.equal(el('groupShared').checked, false);
  assert.equal(el('groupCallPublic').checked, false);
  assert.equal(state.groupColor, colors[groups.length % colors.length]);
  requests.length = 0;
  el('groupName').value = '请求失败时保留';
  failure = '保存失败';
  await click('group-save');
  assert.equal(requests[0].method, 'POST');
  assert(el('groupVeil').classList.contains('on'));
  assert.equal(el('groupName').value, '请求失败时保留');
  assert.equal(errors().pop(), '保存失败');
  await click('close', { target: 'groupVeil' });
  assert(!el('groupVeil').classList.contains('on'));
  console.log('GROUP MODAL OK');
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	file := filepath.Join(t.TempDir(), "groups-modal.cjs")
	if err := os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("group modal node test failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("GROUP MODAL OK")) {
		t.Fatalf("group modal test did not finish: %s", out)
	}
}

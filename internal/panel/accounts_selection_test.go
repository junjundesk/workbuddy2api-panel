package panel

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 按主页实际顺序加载完整脚本，覆盖分组全选、半选、共享账号双向同步及批量动作计数。
// DOM 桩只实现本测试需要的控件、简单选择器与事件冒泡，不依赖额外的 npm 包。
func TestAppJSGroupSelection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; group selection test skipped")
	}
	script := `const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const page = fs.readFileSync("index.html", "utf8");
const nodes = new Map(), listeners = {}, storage = new Map();
const decode = s => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
function attrs(text) {
  return Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)].map(m => [m[1], decode(m[2] || "")]));
}
function element(tag = "div", at = {}, parent = null) {
  const classes = new Set((at.class || "").split(/\s+/).filter(Boolean));
  const el = {
    tag, id: at.id || "", parentElement: parent, children: [], listeners: {}, style: {},
    dataset: Object.fromEntries(Object.entries(at).filter(([k]) => k.startsWith("data-")).map(([k,v]) => [k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()), v])),
    checked: "checked" in at, disabled: "disabled" in at, indeterminate: false, value: "", textContent: "",
    classList: {
      add(c) { classes.add(c); }, remove(c) { classes.delete(c); }, contains(c) { return classes.has(c); },
      toggle(c, on = !classes.has(c)) { if (on) classes.add(c); else classes.delete(c); return on; },
    },
    getAttribute(k) { return at[k] == null ? null : at[k]; },
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    querySelectorAll(sel) { return descendants(this).filter(n => matches(n, sel)); },
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
    closest(sel) { for (let n = this; n; n = n.parentElement) if (matches(n, sel)) return n; return null; },
    appendChild(child) { child.parentElement = this; this.children.push(child); },
    focus() {}, remove() {},
    set innerHTML(html) { this.html = html; parse(html, this); },
    get innerHTML() { return this.html || ""; },
  };
  if (el.id) nodes.set(el.id, el);
  return el;
}
function matches(el, selector) {
  return selector.split(",").some(sel => {
    const m = sel.trim().match(/^([\w-]+)?(?:\.([\w-]+))?(?:\[([\w-]+)(?:="([^"]*)")?\])?$/);
    return !!m && (!m[1] || el.tag === m[1]) && (!m[2] || el.classList.contains(m[2])) &&
      (!m[3] || (el.getAttribute(m[3]) !== null && (m[4] === undefined || el.getAttribute(m[3]) === m[4])));
  });
}
function descendants(el) { return el.children.flatMap(n => [n, ...descendants(n)]); }
function parse(html, root) {
  root.children = [];
  const stack = [root];
  for (const m of html.matchAll(/<(\/?)([a-z][\w-]*)\b([^>]*)>/gi)) {
    const tag = m[2].toLowerCase();
    if (m[1]) {
      if (stack.length > 1 && stack[stack.length - 1].tag === tag) stack.pop();
      continue;
    }
    const parent = stack[stack.length - 1], el = element(tag, attrs(m[3]), parent);
    parent.children.push(el);
    if (!/^(input|br|hr|img|col|link|meta|wbr)$/.test(tag) && !m[3].endsWith("/")) stack.push(el);
  }
}
// 页面静态节点只需按 id 注册；动态卡片用实际 innerHTML 建树，保留 label/header 的父子关系。
for (const m of page.matchAll(/<([a-z][\w-]*)\b([^>]*\bid="[^"]+"[^>]*)>/gi)) element(m[1], attrs(m[2]));
function queryAll(sel) {
  return [...new Set([...nodes.values()].flatMap(n => [n, ...descendants(n)]))].filter(n => matches(n, sel));
}
const ctx = {
  console, URLSearchParams, FormData: class FormData {},
  document: {
    getElementById: id => nodes.get(id) || null, createElement: element,
    documentElement: element("html"), body: element("body"), head: element("head"), cookie: "",
    querySelectorAll: queryAll, querySelector: sel => queryAll(sel)[0] || null,
    addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
  },
  localStorage: { getItem: k => storage.get(k) || null, setItem: (k, v) => storage.set(k, v) },
  location: { hash: "#accounts", search: "", pathname: "/panel/" }, history: { replaceState() {} },
  matchMedia: () => ({ matches: false, addEventListener() {} }), addEventListener() {},
  fetch: () => new Promise(() => {}),
  setTimeout: () => 1, clearTimeout() {}, setInterval: () => 1, clearInterval() {},
};
ctx.window = ctx;
vm.createContext(ctx);
const scripts = [...page.matchAll(/<script src="([^"]+)"><\/script>/g)].map(m => m[1]);
assert.deepEqual(scripts, ["pool_list.js", "group_cfg.js", "acct_actions.js", "app.js"]);
for (const file of scripts) vm.runInContext(fs.readFileSync(file, "utf8"), ctx, { filename: file });
const run = code => vm.runInContext(code, ctx);
const groups = [
  { id: "ga", name: "共享组", shared_pool: true },
  { id: "gb", name: "独立组" },
  { id: "empty", name: "空组" },
];
const accounts = [
  { uid: "u1" }, { uid: "u2", group: "ga" }, { uid: "u3", group: "ga", disabled: true },
  { uid: "u4", group: "gb", paused: true }, { uid: "u5", group: "gb" },
  { uid: "orphan", group: "deleted" },
];
ctx.fixture = { accounts, groups };
run("overviewData = fixture; overviewGroupViews = fixture.groups;");
function render() { run("renderAccounts(overviewData.accounts, overviewData.groups)"); }
const selected = () => Array.from(run("accSel")).sort();
const boxes = () => nodes.get("accGroups").querySelectorAll("input[data-gsel]");
function groupBox(scope) {
  const cb = boxes().find(n => n.dataset.gsel === scope);
  assert.ok(cb, "缺少分组全选复选框: " + scope);
  return cb;
}
function rowBoxes(uid) { return queryAll("input[data-sel]").filter(n => n.dataset.sel === uid); }
async function dispatch(type, target) {
  const ev = { target, preventDefault() {} };
  for (let n = target; n; n = n.parentElement) for (const fn of n.listeners[type] || []) await fn(ev);
  for (const fn of listeners[type] || []) await fn(ev);
}
async function change(cb, checked) { cb.checked = checked; await dispatch("change", cb); }
function state(cb, checked, partial, disabled = false) {
  assert.deepEqual([cb.checked, cb.indeterminate, cb.disabled], [checked, partial, disabled]);
}
function batch(scope, count) {
  const buttons = queryAll("button[data-aa]").filter(n => n.dataset.aaScope === scope && ["move", "export", "remove"].includes(n.dataset.aa));
  assert.equal(buttons.length, 3);
  for (const b of buttons) assert.equal(b.disabled, count === 0, scope + "批量按钮状态");
  assert.equal(buttons.find(b => b.dataset.aa === "remove").textContent, count ? "移除所选 (" + count + ")" : "移除所选");
  assert.equal(queryAll("[data-aa-info]").find(n => n.dataset.aaInfo === scope).textContent, count ? "已选 " + count : "");
}
(async () => {
  render();
  assert.deepEqual(boxes().map(n => n.dataset.gsel), ["_public", "ga", "gb", "empty"], "每张号池卡片都应有全选入口");
  assert.match(groupBox("_public").getAttribute("aria-label"), /含共享分组账号/);
  state(groupBox("empty"), false, false, true);
  state(groupBox("ga"), false, false);
  batch("ga", 0);

  // 只全选独立组，禁止误选其他组；禁用/暂停账号也允许勾选后批量管理。
  await change(groupBox("gb"), true);
  assert.deepEqual(selected(), ["u4", "u5"]);
  state(groupBox("gb"), true, false);
  state(groupBox("_public"), false, false);
  state(nodes.get("accSelAll"), false, true);
  assert.ok(rowBoxes("u4").every(n => n.checked));
  batch("gb", 2); batch("ga", 0); batch("all", 2);

  // 单号勾选/取消实时更新半选状态；共享账号在公共池与原组的两个复选框必须一致。
  await change(rowBoxes("u2")[0], true);
  assert.equal(rowBoxes("u2").length, 2);
  assert.ok(rowBoxes("u2").every(n => n.checked));
  state(groupBox("ga"), false, true); state(groupBox("_public"), false, true);
  await change(groupBox("ga"), true);
  assert.deepEqual(selected(), ["u2", "u3", "u4", "u5"]);
  state(groupBox("ga"), true, false); batch("ga", 2); batch("_public", 2);
  await change(rowBoxes("u2")[1], false);
  assert.ok(rowBoxes("u2").every(n => !n.checked));
  state(groupBox("ga"), false, true);
  await change(groupBox("ga"), false);
  assert.deepEqual(selected(), ["u4", "u5"], "取消本组不得清空其他组");

  // 公共池包括未分组、失效分组兜底及共享组账号，全局 Set 去重。
  await change(groupBox("_public"), true);
  assert.deepEqual(selected(), ["orphan", "u1", "u2", "u3", "u4", "u5"]);
  state(nodes.get("accSelAll"), true, false); state(groupBox("ga"), true, false);
  batch("all", 6); batch("_public", 4); batch("gb", 2);
  assert.deepEqual(Array.from(run("accScopeUids('ga')")).sort(), ["u2", "u3"]);
  await change(groupBox("_public"), false);
  assert.deepEqual(selected(), ["u4", "u5"]);
  state(groupBox("ga"), false, false);

  // 点击复选框及 label 文本都不能折叠；点击标题其他部分仍能折叠，折叠后也可全选。
  let cb = groupBox("gb"), header = cb.closest("header"), card = header.parentElement;
  await dispatch("click", cb); await dispatch("click", cb.closest("label"));
  assert.equal(card.classList.contains("fold"), false);
  assert.equal(storage.get("wb2api.accFold"), undefined, "全选点击不得写入折叠状态");
  await dispatch("click", header);
  assert.equal(card.classList.contains("fold"), true);
  await change(cb, false);
  assert.deepEqual(selected(), []);
  assert.equal(card.classList.contains("fold"), true);
  await change(cb, true);

  // 排序、自动刷新重建控件后维持选择与半选，删除账号后剔除过期 UID。
  await change(rowBoxes("u2")[0], true);
  const sort = nodes.get("accGroups").querySelector('th[data-act="sort"]');
  assert.ok(sort);
  await dispatch("click", sort);
  render();
  assert.deepEqual(selected(), ["u2", "u4", "u5"]);
  state(groupBox("gb"), true, false); state(groupBox("ga"), false, true);
  assert.equal(groupBox("gb").closest("header").parentElement.classList.contains("fold"), true);
  assert.ok(rowBoxes("u2").every(n => n.checked));
  accounts.splice(accounts.findIndex(a => a.uid === "u2"), 1); render();
  assert.deepEqual(selected(), ["u4", "u5"]); state(groupBox("ga"), false, false);
  batch("all", 2);
  run("accScopeClear('gb', ['u4'])");
  assert.ok(rowBoxes("u4").every(n => !n.checked)); state(groupBox("gb"), false, true);

  // 原有整池全选/取消仍有效；无分组时的单表也保留该行为。
  await change(nodes.get("accSelAll"), true);
  assert.equal(selected().length, accounts.length);
  for (const scope of ["_public", "ga", "gb"]) state(groupBox(scope), true, false);
  state(groupBox("empty"), false, false, true);
  await change(nodes.get("accSelAll"), false);
  assert.deepEqual(selected(), []);
  // 分组已删除而旧卡片尚未重绘时，不得留下与选择集合不一致的勾选状态。
  const stale = groupBox("gb");
  ctx.fixture.groups = groups.filter(g => g.id !== "gb");
  await change(stale, true);
  assert.deepEqual(selected(), []);
  state(stale, false, false, true);

  ctx.fixture.groups = []; render();
  assert.equal(boxes().length, 0);
  await change(nodes.get("accSelAll"), true);
  assert.equal(selected().length, accounts.length);
  assert.ok(queryAll("input[data-sel]").every(n => n.checked));
  await change(nodes.get("accSelAll"), false);
  assert.ok(queryAll("input[data-sel]").every(n => !n.checked));
  console.log("GROUP SELECTION OK");
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	file := filepath.Join(t.TempDir(), "group-selection.cjs")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("group selection test failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("GROUP SELECTION OK")) {
		t.Fatalf("group selection test did not finish:\n%s", out)
	}
}

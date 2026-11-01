package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 用真实前端函数渲染混合记录，验证处理日志、进度、空状态与 HTML 转义。
func TestAppJSPatrolActionLogs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; patrol action log test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert/strict');
const src = fs.readFileSync(process.argv[2], 'utf8');
const helpersStart = src.indexOf('function esc(');
const helpersEnd = src.indexOf('const TRANGE_PRESETS');
const start = src.indexOf('function patrolActionText(');
const end = src.indexOf('async function loadPatrol(');
assert.ok([helpersStart, helpersEnd, start, end].every(i => i >= 0));
const els = { patrolBody: {}, patrolNote: {} };
const ctx = { $: id => els[id] };
vm.createContext(ctx);
vm.runInContext(src.slice(helpersStart, helpersEnd) + src.slice(start, end), ctx);
const time = new Date(2026, 10, 2, 1, 2, 3).toISOString();
const banned = { uid: 'ban-1', nickname: '封禁账号', verdict: 'banned', moved: true, at: time };
const zero = { uid: 'zero-1', verdict: 'no_credit', moved: true, at: time };
const restored = { uid: 'restored-1', verdict: 'ok', moved: true, restored: true, at: time };
const hidden = [
  { uid: 'healthy-hidden', verdict: 'ok' },
  { uid: 'rate-hidden', verdict: 'rate_limited' },
  { uid: 'probe-failed-hidden', verdict: 'failed', error: 'network' },
  { uid: 'already-grouped-hidden', verdict: 'no_credit', group: 'nc' },
  { uid: 'dry-run-hidden', verdict: 'banned', code: 11140 },
  { uid: 'move-failed-hidden', verdict: 'banned', error: '移动分组失败' },
];
assert.equal(ctx.patrolActionText(banned), '封禁:[移动] 封禁账号（ban-1） 移至封号池');
assert.equal(ctx.patrolActionText(zero), '余额:[移动] zero-1 移至无积分池');
assert.equal(ctx.patrolActionText(restored), '余额:[回流] restored-1 积分已恢复，移回公共池');
for (const row of hidden) assert.equal(ctx.patrolActionText(row), '');
assert.equal(ctx.patrolActionText(null), '');
// 兼容历史 restored 标记；探针附带错误不应掩盖已经成功执行的回流。
assert.equal(ctx.patrolActionText({ ...restored, moved: false, error: '旧探测错误' }), ctx.patrolActionText(restored));
const data = { results: [banned, ...hidden, zero, restored], running: true, done: 8, total: 10,
  skipped_banned: 2, settings: { enabled: true, interval_value: 6, interval_unit: 'hour', check_ban: true, check_credit: true } };
ctx.renderPatrolReport(data, false);
const html = els.patrolBody.innerHTML;
assert.equal((html.match(/<tr>/g) || []).length, 3);
assert.ok(html.includes('2026-11-02 01:02:03'));
for (const row of [banned, zero, restored]) assert.ok(html.includes(ctx.patrolActionText(row)));
for (const row of hidden) assert.ok(!html.includes(row.uid));
assert.ok(els.patrolNote.textContent.includes('巡检中 8/10'));
assert.ok(els.patrolNote.textContent.includes('3 条处理日志'));
assert.ok(els.patrolNote.textContent.includes('跳过封号池 2 个'));
// 昵称和完整 UID 都必须转义；错误明细不混入成功处理日志。
ctx.renderPatrolReport({ results: [{ ...banned, nickname: '<img src=x onerror=alert(1)>', uid: '<script>uid</script>',
  detail: '不应展示的探测详情', at: 'bad-time' }] }, false);
assert.ok(!els.patrolBody.innerHTML.includes('<img'));
assert.ok(!els.patrolBody.innerHTML.includes('<script>'));
assert.ok(els.patrolBody.innerHTML.includes('&lt;img'));
assert.ok(els.patrolBody.innerHTML.includes('&lt;script&gt;uid&lt;/script&gt;'));
assert.ok(!els.patrolBody.innerHTML.includes('不应展示的探测详情'));
assert.ok(!els.patrolBody.innerHTML.includes('NaN'));
for (const results of [hidden, [], null]) {
  ctx.renderPatrolReport({ results, last_run: time }, false);
  assert.ok(els.patrolBody.innerHTML.includes('colspan="2"'));
  assert.ok(els.patrolBody.innerHTML.includes('暂无处理日志'));
  assert.ok(!els.patrolBody.innerHTML.includes('还没有巡检记录'));
  assert.ok(els.patrolNote.textContent.includes('0 条处理日志'));
}
const page = fs.readFileSync(process.argv[3], 'utf8');
assert.ok(page.includes('<th>时间</th><th>处理日志（仅移动 / 回流）</th>'));
console.log('OK');
`
	path := filepath.Join(t.TempDir(), "patrol-logs.cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path, "app.js", "index.html").CombinedOutput()
	if err != nil {
		t.Fatalf("巡检处理日志前端回归失败：%v\n%s", err, out)
	}
}

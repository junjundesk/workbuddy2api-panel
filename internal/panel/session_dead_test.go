// session_dead_test.go 凭证失效实时归组「失效池」：余额 / 签到 401+12153 自动建组并
// 搬移、响应带 dead/moved_to 标记、幂等不重复建组、非失效错误（500）不误伤、
// 测活（alive）对话 401 同样归组。全程不打真网。
package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
)

// dead401Body 上游凭证失效的标准 body（upstream.Classify 据此判 ErrSessionDead）。
const dead401Body = "{\"code\":12153,\"msg\":\"Offline user session not found\"}"

// deadResOK 余额成功体（一条 100/100 的积分包）。
const deadResOK = "{\"code\":0,\"data\":{\"Response\":{\"Data\":{\"Accounts\":[{\"CapacityRemain\":100,\"CapacitySize\":100}]}}}}"

// deadBillingFake 假上游：billing 端点（签到 / 余额）响应可配，其余端点固定失败。
// 非失效场景的 body 刻意不含 "12153"/"Offline user session not found" 字样——
// Classify 靠这些 marker 判 ErrSessionDead，500 场景若带上就测不出误伤。
type deadBillingFake struct {
	checkinStatus int
	checkinBody   string
	resStatus     int
	resBody       string
}

func (f *deadBillingFake) transport(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/billing/meter/daily-checkin"):
		return fakeJSON(f.checkinStatus, f.checkinBody), nil
	case strings.HasSuffix(req.URL.Path, "/billing/meter/get-user-resource"):
		return fakeJSON(f.resStatus, f.resBody), nil
	default:
		// /v3/config（目录主路）：测试里固定失败，走企业端点降级（与 patrol/alive 同口径）。
		return fakeJSON(http.StatusInternalServerError, "{\"code\":500,\"msg\":\"v3 disabled in test\"}"), nil
	}
}

// assertDeadGroup 断言 uid 位于「失效池」：组恰好一个、是隔离分组（shared_pool=false、
// call_public=false），且 groups 与 pool 两侧归属一致。返回组 id。
func assertDeadGroup(t *testing.T, gs *groups.Store, pl *pool.Pool, uid string) string {
	t.Helper()
	count := 0
	gid := ""
	for _, g := range gs.Groups() {
		if g.Name == deadGroupName {
			count++
			gid = g.ID
			if g.SharedPool || g.CallPublic {
				t.Errorf("「%s」必须是隔离分组：shared_pool=%v call_public=%v", deadGroupName, g.SharedPool, g.CallPublic)
			}
		}
	}
	if count != 1 {
		t.Fatalf("「%s」组数量 = %d，want 1（幂等，不得重复建组）", deadGroupName, count)
	}
	if got := gs.GroupOf(uid); got != gid {
		t.Fatalf("账号 %s 分组归属 = %q，want %q（失效池）", uid, got, gid)
	}
	for _, st := range pl.List() {
		if st.UID == uid && st.Group != gid {
			t.Errorf("pool 内 group = %q，want %q", st.Group, gid)
		}
	}
	return gid
}

// TestSessionDeadBalanceMovesToDeadGroup 余额 401+12153：自动建「失效池」并搬移，
// 502 响应带 dead/moved_to。
func TestSessionDeadBalanceMovesToDeadGroup(t *testing.T) {
	fake := &deadBillingFake{
		checkinStatus: http.StatusOK,
		checkinBody:   "{\"code\":0}",
		resStatus:     http.StatusUnauthorized,
		resBody:       dead401Body,
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "tok-dead"))

	code, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/balance", "")
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d，want 502（余额失败仍回 502）", code)
	}
	if resp["dead"] != true {
		t.Errorf("响应缺少 dead=true：%v", resp)
	}
	if resp["moved_to"] != deadGroupName {
		t.Errorf("moved_to = %v，want %q", resp["moved_to"], deadGroupName)
	}
	assertDeadGroup(t, gs, pl, "u-dead")
}

// TestSessionDeadBalanceIdempotent 连续命中同一账号：只允许一个「失效池」、
// 成员不重复，两次响应标记一致（幂等，不重复建组 / 不重复落盘）。
func TestSessionDeadBalanceIdempotent(t *testing.T) {
	fake := &deadBillingFake{
		checkinStatus: http.StatusOK,
		checkinBody:   "{\"code\":0}",
		resStatus:     http.StatusUnauthorized,
		resBody:       dead401Body,
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "tok-dead"))

	for i := 0; i < 2; i++ {
		_, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/balance", "")
		if resp["dead"] != true || resp["moved_to"] != deadGroupName {
			t.Fatalf("第 %d 次响应标记不一致：%v", i+1, resp)
		}
	}
	gid := assertDeadGroup(t, gs, pl, "u-dead")
	for _, g := range gs.Groups() {
		if g.ID == gid && len(g.Members) != 1 {
			t.Fatalf("「%s」成员数 = %d，want 1（不得重复归组）", deadGroupName, len(g.Members))
		}
	}
}

// TestSessionDeadBalance500NotMoved 上游 500 不是凭证失效：不移动、不建组。
// （500 走 retryBillingTransient，真实重试 2 次、间隔 2s+4s，本用例约 6s。）
func TestSessionDeadBalance500NotMoved(t *testing.T) {
	fake := &deadBillingFake{
		checkinStatus: http.StatusOK,
		checkinBody:   "{\"code\":0}",
		resStatus:     http.StatusInternalServerError,
		resBody:       "{\"code\":500,\"msg\":\"internal error\"}",
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "tok-dead"))

	code, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/balance", "")
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d，want 502", code)
	}
	if _, ok := resp["dead"]; ok {
		t.Errorf("500 不该带 dead 标记：%v", resp)
	}
	if got := gs.GroupOf("u-dead"); got != "" {
		t.Errorf("500 不该移动账号，group = %q", got)
	}
	for _, g := range gs.Groups() {
		if g.Name == deadGroupName {
			t.Errorf("500 不该创建「%s」组", deadGroupName)
		}
	}
	for _, st := range pl.List() {
		if st.UID == "u-dead" && st.Group != "" {
			t.Errorf("pool 内 group = %q，want 未分组", st.Group)
		}
	}
}

// TestSessionDeadCheckinMovesToDeadGroup 签到 401 同样归组；余额继续查（既有语义不变），
// 余额成功时 credits 照常返回。
func TestSessionDeadCheckinMovesToDeadGroup(t *testing.T) {
	fake := &deadBillingFake{
		checkinStatus: http.StatusUnauthorized,
		checkinBody:   dead401Body,
		resStatus:     http.StatusOK,
		resBody:       deadResOK,
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "tok-dead"))

	code, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/checkin", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d，want 200（checkin 端点保持 200）", code)
	}
	if resp["dead"] != true || resp["moved_to"] != deadGroupName {
		t.Fatalf("响应缺少 dead/moved_to 标记：%v", resp)
	}
	if msg, _ := resp["checkin_message"].(string); msg == "" {
		t.Errorf("签到失败应带 checkin_message：%v", resp)
	}
	// 「签到出错仍继续查余额」的既有语义：余额查询成功，credits 在、balance_error 不在。
	if _, ok := resp["balance_error"]; ok {
		t.Errorf("余额应继续查询成功，不该有 balance_error：%v", resp)
	}
	if _, ok := resp["credits"]; !ok {
		t.Errorf("余额应继续查询成功，响应应带 credits：%v", resp)
	}
	assertDeadGroup(t, gs, pl, "u-dead")
}

// TestSessionDeadAliveChat401 测活对话 401：aliveResp 带 dead/moved_to，账号归组。
func TestSessionDeadAliveChat401(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("m1"),
		chatStatus: http.StatusUnauthorized,
		chatBody:   dead401Body,
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "at1"))

	code, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/alive", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d，want 200（上游失败仍回 200 + ok:false）", code)
	}
	if resp["ok"] != false {
		t.Errorf("ok 应为 false：%v", resp)
	}
	if resp["dead"] != true || resp["moved_to"] != deadGroupName {
		t.Fatalf("响应缺少 dead/moved_to 标记：%v", resp)
	}
	if resp["stage"] != "chat" {
		t.Errorf("stage = %v，want chat", resp["stage"])
	}
	assertDeadGroup(t, gs, pl, "u-dead")
}

// TestSessionDeadAliveCatalog401 测活模型目录 401：同样归组并回填标记。
func TestSessionDeadAliveCatalog401(t *testing.T) {
	fake := &aliveUpstream{
		modelsStatus: http.StatusUnauthorized,
		modelsBody:   dead401Body,
	}
	p, pl, gs := newPatrolPanelRT(t, rtFunc(fake.transport), acc("u-dead", "at1"))

	code, resp, _ := call(t, p, "POST", "/panel/api/accounts/u-dead/alive", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d，want 200", code)
	}
	if resp["dead"] != true || resp["moved_to"] != deadGroupName {
		t.Fatalf("响应缺少 dead/moved_to 标记：%v", resp)
	}
	if resp["stage"] != "catalog" {
		t.Errorf("stage = %v，want catalog", resp["stage"])
	}
	assertDeadGroup(t, gs, pl, "u-dead")
}

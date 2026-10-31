// patrol_test.go 账号巡检：假上游验证「11140 判封号 / 积分 0 判无积分 / 分组自动创建
// 与移动 / 限流变体不误判 / 试运行不改分组」。全程不打真网。
package panel

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// patrolUpstream 假上游：模型目录（企业端点）+ chat SSE + 余额。
// 按 Authorization 里的 token 区分账号：f.banned 的账号余额接口回 403 业务码（11140），
// f.zero 的账号余额回空 Accounts（= 剩余 0），其余账号一切正常。巡检只打余额接口，
// chat 只记命中次数（默认设置下必须恒为 0）。
type patrolUpstream struct {
	mu sync.Mutex

	banned map[string]string // token -> chat 错误体（"" = 正常）
	zero   map[string]bool   // token -> 余额为 0
	chat   map[string]int    // token -> chat 命中次数
	bill   map[string]int
}

func newPatrolUpstream() *patrolUpstream {
	return &patrolUpstream{
		banned: map[string]string{},
		zero:   map[string]bool{},
		chat:   map[string]int{},
		bill:   map[string]int{},
	}
}

func (f *patrolUpstream) transport(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/console/enterprises/personal/models"):
		return fakeJSON(http.StatusOK, enterprisePayload("m1")), nil
	case strings.HasSuffix(req.URL.Path, "/v2/chat/completions"):
		// 巡检不该走到这里（默认 ProbeChat=false）；保留计数供测试断言"一次都没发"。
		f.chat[token]++
		return fakeJSON(http.StatusOK, aliveSSEBody), nil
	case strings.HasSuffix(req.URL.Path, "/v2/billing/meter/get-user-resource"):
		f.bill[token]++
		if body, ok := f.banned[token]; ok && body != "" {
			// 封号/限流由余额接口的业务信封暴露（巡检只调这个接口）。
			return fakeJSON(http.StatusForbidden, body), nil
		}
		if f.zero[token] {
			// Accounts 为空 = 没有任何积分包 → remain/total 均为 0。
			return fakeJSON(http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
		}
		return fakeJSON(http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":100,"CapacitySize":100}]}}}}`), nil
	default:
		// /v3/config（目录主路）固定失败，走企业端点降级（与 alive_test 同口径）。
		return fakeJSON(http.StatusInternalServerError, `{"code":500,"msg":"v3 disabled in test"}`), nil
	}
}

// newPatrolPanel 组装「假上游 + 账号池 + 空分组注册表」面板。
func newPatrolPanel(t *testing.T, fake *patrolUpstream, accounts ...*auth.Auth) (*Panel, *pool.Pool, *groups.Store) {
	t.Helper()
	return newPatrolPanelRT(t, rtFunc(fake.transport), accounts...)
}

// newPatrolPanelRT 用自定义 RoundTripper 组装面板（取消测试用阻塞上游）。
func newPatrolPanelRT(t *testing.T, rt http.RoundTripper, accounts ...*auth.Auth) (*Panel, *pool.Pool, *groups.Store) {
	t.Helper()
	cl := upstream.New()
	hc := &http.Client{Transport: rt}
	cl.HTTP = hc
	cl.ChatHTTP = hc
	pl := pool.New("")
	for _, a := range accounts {
		pl.Add(a)
	}
	gs, err := groups.Load(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{
		Version:    "test",
		APIKey:     "k",
		Pool:       pl,
		Upstream:   cl,
		Groups:     gs,
		PatrolFile: filepath.Join(t.TempDir(), "patrol.json"),
	})
	p.applyGroupMeta()
	return p, pl, gs
}

// waitPatrol 等当前一轮巡检跑完（测试上限 10s）。
func waitPatrol(t *testing.T, p *Panel) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.patrol.mu.Lock()
		running := p.patrol.running
		p.patrol.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("巡检未在 10s 内结束")
}

// groupIDByName 按名字取分组 id（巡检自动建组后断言用）。
func groupIDByName(t *testing.T, gs *groups.Store, name string) string {
	t.Helper()
	for _, g := range gs.Groups() {
		if g.Name == name {
			return g.ID
		}
	}
	return ""
}

func acc(uid, token string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: token, RefreshToken: "rt-" + uid, ExpiresAt: 9999999999}
}

// TestPatrolMovesBannedAndNoCredit：11140 → 封号池；积分 0 → 无积分；正常号不动。
func TestPatrolMovesBannedAndNoCredit(t *testing.T) {
	fake := newPatrolUpstream()
	fake.banned["tok-ban"] = `{"code":11140,"msg":"request illegal"}`
	fake.zero["tok-zero"] = true

	p, pl, gs := newPatrolPanel(t, fake, acc("u-ban", "tok-ban"), acc("u-zero", "tok-zero"), acc("u-ok", "tok-ok"))
	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	bannedID := groupIDByName(t, gs, patrolGroupBannedName)
	noCreditID := groupIDByName(t, gs, patrolGroupNoCreditName)
	if bannedID == "" {
		t.Fatal("未自动创建「封号池」分组")
	}
	if noCreditID == "" {
		t.Fatal("未自动创建「无积分」分组")
	}
	if got := gs.GroupOf("u-ban"); got != bannedID {
		t.Errorf("u-ban group=%q want 封号池 %q", got, bannedID)
	}
	if got := gs.GroupOf("u-zero"); got != noCreditID {
		t.Errorf("u-zero group=%q want 无积分 %q", got, noCreditID)
	}
	if got := gs.GroupOf("u-ok"); got != "" {
		t.Errorf("u-ok 不该被移动，group=%q", got)
	}

	// 池内视图必须同步（选号看的是 pool 的 group，不是 groups.json）。
	for _, st := range pl.List() {
		switch st.UID {
		case "u-ban":
			if st.Group != bannedID {
				t.Errorf("pool u-ban group=%q want %q", st.Group, bannedID)
			}
		case "u-zero":
			if st.Group != noCreditID {
				t.Errorf("pool u-zero group=%q want %q", st.Group, noCreditID)
			}
		case "u-ok":
			if st.Group != "" {
				t.Errorf("pool u-ok group=%q want 公共池", st.Group)
			}
		}
	}

	// 隔离语义：自动建的分组不进公共池（否则封号号仍会接公共流量）。
	for _, g := range gs.Groups() {
		if g.ID == bannedID || g.ID == noCreditID {
			if g.SharedPool || g.CallPublic {
				t.Errorf("分组 %s 应为隔离策略（shared=false call_public=false），实得 shared=%v call_public=%v",
					g.Name, g.SharedPool, g.CallPublic)
			}
		}
	}
}

// TestPatrolRateLimited11140NotBanned：11140 的模型级限流变体不是封号，不能进封号池。
func TestPatrolRateLimited11140NotBanned(t *testing.T) {
	fake := newPatrolUpstream()
	fake.banned["tok-rate"] = `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`

	p, _, gs := newPatrolPanel(t, fake, acc("u-rate", "tok-rate"))
	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	if id := groupIDByName(t, gs, patrolGroupBannedName); id != "" && gs.GroupOf("u-rate") == id {
		t.Fatal("模型级限流（11140 限流文案）被误判成封号")
	}
	p.patrol.mu.Lock()
	results := append([]patrolResult(nil), p.patrol.results...)
	p.patrol.mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("results=%d want 1", len(results))
	}
	if results[0].Verdict != patrolVerdictRateLimited {
		t.Errorf("verdict=%q want %q（detail=%s）", results[0].Verdict, patrolVerdictRateLimited, results[0].Detail)
	}
}

// TestPatrolDryRunDoesNotMove：试运行只出结论，不改分组、不建组。
func TestPatrolDryRunDoesNotMove(t *testing.T) {
	fake := newPatrolUpstream()
	fake.banned["tok-ban"] = `{"code":11140,"msg":"request illegal"}`
	p, _, gs := newPatrolPanel(t, fake, acc("u-ban", "tok-ban"))

	if err := p.patrol.start(nil, boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	if got := gs.GroupOf("u-ban"); got != "" {
		t.Errorf("试运行不该移动账号，group=%q", got)
	}
	if id := groupIDByName(t, gs, patrolGroupBannedName); id != "" {
		t.Errorf("试运行不该创建分组，实得 %q", id)
	}
	p.patrol.mu.Lock()
	defer p.patrol.mu.Unlock()
	if len(p.patrol.results) != 1 || p.patrol.results[0].Verdict != patrolVerdictBanned {
		t.Fatalf("results=%+v", p.patrol.results)
	}
	if p.patrol.results[0].Moved {
		t.Error("试运行不该标记 moved")
	}
}

// TestPatrolBalanceModeSkipsChat：balance 模式不打对话（零积分消耗）。
func TestPatrolBalanceModeSkipsChat(t *testing.T) {
	fake := newPatrolUpstream()
	fake.zero["tok-zero"] = true
	p, _, gs := newPatrolPanel(t, fake, acc("u-zero", "tok-zero"))
	// 只开积分检测：封号检测关闭时巡检不打对话请求（零对话消耗）。
	p.patrol.updateSettings(patrolSettings{Enabled: true, IntervalValue: 1, IntervalUnit: patrolUnitHour, CheckCredit: true, Concurrency: 2})

	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	fake.mu.Lock()
	chatHits := fake.chat["tok-zero"]
	fake.mu.Unlock()
	if chatHits != 0 {
		t.Errorf("balance 模式不该发对话请求，hits=%d", chatHits)
	}
	if id := groupIDByName(t, gs, patrolGroupNoCreditName); id == "" || gs.GroupOf("u-zero") != id {
		t.Errorf("u-zero 应进无积分分组，group=%q", gs.GroupOf("u-zero"))
	}
}

// TestPatrolAlreadyInGroupNoDoubleMove：已在目标分组时不重复移动（Moved=false）。
func TestPatrolAlreadyInGroupNoDoubleMove(t *testing.T) {
	fake := newPatrolUpstream()
	fake.zero["tok-zero"] = true
	p, _, gs := newPatrolPanel(t, fake, acc("u-zero", "tok-zero"))
	g, _, err := gs.Create(patrolGroupNoCreditName, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.AddMember(g.ID, "u-zero"); err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.SetGroup("u-zero", g.ID)
	p.applyGroupMeta()

	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	p.patrol.mu.Lock()
	defer p.patrol.mu.Unlock()
	if len(p.patrol.results) != 1 {
		t.Fatalf("results=%d", len(p.patrol.results))
	}
	if p.patrol.results[0].Moved {
		t.Error("已在目标分组时不应标记 moved")
	}
	if len(gs.Groups()) != 1 {
		t.Errorf("不该建第二个同名分组，groups=%d", len(gs.Groups()))
	}
}

// TestPatrolConfigPersists：设置落盘并能被再次读出（重启后保持）。
func TestPatrolConfigPersists(t *testing.T) {
	fake := newPatrolUpstream()
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"))
	code, out, raw := call(t, p, "POST", "/panel/api/patrol", `{"enabled":false,"interval_value":45,"interval_unit":"minute","check_ban":false,"check_credit":true,"concurrency":2}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["ok"] != true {
		t.Fatalf("out=%v", out)
	}
	// 重新从磁盘加载（模拟重启）。
	again := newPatrolRunner(p, p.cfg.PatrolFile)
	if again.settings.Enabled {
		t.Error("enabled 应持久化为 false")
	}
	if again.settings.IntervalValue != 45 || again.settings.IntervalUnit != patrolUnitMinute ||
		again.settings.CheckBan || !again.settings.CheckCredit || again.settings.Concurrency != 2 {
		t.Errorf("settings=%+v", again.settings)
	}
	if d := again.settings.interval(); d != 45*time.Minute {
		t.Errorf("interval=%v want 45m", d)
	}
}

// TestPatrolStatusEndpoint：GET 返回设置 / 结果 / 目标分组 id。
func TestPatrolStatusEndpoint(t *testing.T) {
	fake := newPatrolUpstream()
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"))
	code, out, raw := call(t, p, "GET", "/panel/api/patrol", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["available"] != true {
		t.Errorf("available=%v", out["available"])
	}
	if _, ok := out["settings"].(map[string]any); !ok {
		t.Errorf("settings missing: %s", raw)
	}
}

// TestAccountsRemoveBatch：多选删除同时出池、退组、删 auths 文件；不存在的 uid 进 failed。
func TestAccountsRemoveBatch(t *testing.T) {
	fake := newPatrolUpstream()
	dir := t.TempDir()
	a1, a2 := acc("u1", "t1"), acc("u2", "t2")
	f1, f2 := filepath.Join(dir, "workbuddy-u1.json"), filepath.Join(dir, "workbuddy-u2.json")
	for _, f := range []string{f1, f2} {
		if err := writeFile(f, `{"uid":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	a1.FilePath, a2.FilePath = f1, f2
	p, pl, gs := newPatrolPanel(t, fake, a1, a2)
	g, _, err := gs.Create("G", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.AddMember(g.ID, "u1"); err != nil {
		t.Fatal(err)
	}

	code, out, raw := call(t, p, "POST", "/panel/api/accounts/remove_batch", `{"uids":["u1","u2","u1","ghost"]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["ok"] != true {
		t.Fatalf("out=%v", out)
	}
	if got := len(out["removed"].([]any)); got != 2 {
		t.Errorf("removed=%d want 2（重复 uid 去重）", got)
	}
	if got := len(out["failed"].([]any)); got != 1 {
		t.Errorf("failed=%d want 1（ghost）", got)
	}
	if pl.AuthByUID("u1") != nil || pl.AuthByUID("u2") != nil {
		t.Error("账号仍在池内")
	}
	if gs.GroupOf("u1") != "" {
		t.Error("账号未退组")
	}
	for _, f := range []string{f1, f2} {
		if fileExists(f) {
			t.Errorf("凭证文件未删除：%s", f)
		}
	}
}

// TestCombinePatrolVerdictPriority 合并优先级：封号 > 无积分 > 失败 > 限流 > 正常。
func TestCombinePatrolVerdictPriority(t *testing.T) {
	ok := patrolResult{Verdict: patrolVerdictOK}
	noCredit := patrolResult{Verdict: patrolVerdictNoCredit}
	banned := patrolResult{Verdict: patrolVerdictBanned}
	failed := patrolResult{Verdict: patrolVerdictFailed, Detail: "对话失败"}
	rate := patrolResult{Verdict: patrolVerdictRateLimited}

	cases := []struct {
		name          string
		balance, chat *patrolResult
		want          string
	}{
		{"对话封号优先", &ok, &banned, patrolVerdictBanned},
		{"余额封号", &banned, &ok, patrolVerdictBanned},
		{"无积分保留（对话成功）", &noCredit, &ok, patrolVerdictNoCredit},
		{"对话失败覆盖正常", &ok, &failed, patrolVerdictFailed},
		{"余额失败但对话正常 → 正常", &failed, &ok, patrolVerdictOK},
		{"限流不移动", &ok, &rate, patrolVerdictRateLimited},
		{"只跑余额", &ok, nil, patrolVerdictOK},
	}
	for _, c := range cases {
		got := combinePatrol(patrolResult{Verdict: patrolVerdictOK}, c.balance, c.chat).Verdict
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// writeFile / fileExists 测试内的小工具（auth 文件落盘与存在性断言）。
func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// boolPtr 测试用的 *bool 构造（start 的 dryRun 用 nil 表示「沿用设置」）。
func boolPtr(v bool) *bool { return &v }

// TestNotifyAccountBannedRealtime 主链路命中 11140 时实时归组（不等下一轮巡检）。
func TestNotifyAccountBannedRealtime(t *testing.T) {
	fake := newPatrolUpstream()
	p, pl, gs := newPatrolPanel(t, fake, acc("u-ban", "tok-ban"))

	p.NotifyAccountBanned("u-ban")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gs.GroupOf("u-ban") != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	id := groupIDByName(t, gs, patrolGroupBannedName)
	if id == "" || gs.GroupOf("u-ban") != id {
		t.Fatalf("实时归组失败：group=%q want %q", gs.GroupOf("u-ban"), id)
	}
	for _, st := range pl.List() {
		if st.UID == "u-ban" && st.Group != id {
			t.Errorf("pool group=%q want %q", st.Group, id)
		}
	}
}

// TestNotifyAccountBannedRespectsSwitch 封号检测关闭时实时回调不搬分组。
func TestNotifyAccountBannedRespectsSwitch(t *testing.T) {
	fake := newPatrolUpstream()
	p, _, gs := newPatrolPanel(t, fake, acc("u-ban", "tok-ban"))
	p.patrol.updateSettings(patrolSettings{Enabled: true, IntervalValue: 1, IntervalUnit: patrolUnitHour, CheckCredit: true})

	p.NotifyAccountBanned("u-ban")
	time.Sleep(200 * time.Millisecond)
	if got := gs.GroupOf("u-ban"); got != "" {
		t.Errorf("封号检测关闭时不该移动账号，group=%q", got)
	}
}

// TestPatrolSettingsIntervalUnits 周期单位「分 / 时」折算与边界归一。
func TestPatrolSettingsIntervalUnits(t *testing.T) {
	cases := []struct {
		in   patrolSettings
		want time.Duration
	}{
		{patrolSettings{IntervalValue: 6, IntervalUnit: patrolUnitHour}, 6 * time.Hour},
		{patrolSettings{IntervalValue: 30, IntervalUnit: patrolUnitMinute}, 30 * time.Minute},
		{patrolSettings{IntervalValue: 0, IntervalUnit: patrolUnitHour}, 6 * time.Hour},
		{patrolSettings{IntervalValue: 1, IntervalUnit: patrolUnitMinute}, 1 * time.Minute},
		{patrolSettings{IntervalValue: 0, IntervalUnit: patrolUnitMinute}, 60 * time.Minute},
		{patrolSettings{IntervalValue: 99999, IntervalUnit: patrolUnitMinute}, time.Duration(patrolMaxIntervalMinutes) * time.Minute},
		{patrolSettings{IntervalValue: 999, IntervalUnit: patrolUnitHour}, time.Duration(patrolMaxIntervalMinutes/60) * time.Hour},
	}
	for _, c := range cases {
		if got := c.in.normalized().interval(); got != c.want {
			t.Errorf("in=%+v got %v want %v", c.in, got, c.want)
		}
	}
	// 两项检测都关 → 回落积分检测，避免空转巡检。
	s := patrolSettings{IntervalValue: 1, IntervalUnit: patrolUnitHour}.normalized()
	if !s.CheckCredit {
		t.Error("两项检测都关闭时应回落积分检测")
	}
}

// TestOverviewMarksBannedAccounts 封号账号在 overview 里带 banned 标记（前端据此把
// 它的积分排除在统计外）：封号池分组与 11140 禁用原因两条路径都要命中。
func TestOverviewMarksBannedAccounts(t *testing.T) {
	fake := newPatrolUpstream()
	p, pl, gs := newPatrolPanel(t, fake, acc("u-ban", "t1"), acc("u-reason", "t2"), acc("u-ok", "t3"))

	g, _, err := gs.Create(patrolGroupBannedName, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.AddMember(g.ID, "u-ban"); err != nil {
		t.Fatal(err)
	}
	pl.SetGroup("u-ban", g.ID)
	// 实时封号路径：server 侧禁用时写入的原因文案。
	pl.Disable("u-reason", "account banned by upstream (11140 request illegal), re-login required")

	code, out, raw := call(t, p, "GET", "/panel/api/overview", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	banned := map[string]bool{}
	accounts, _ := out["accounts"].([]any)
	for _, it := range accounts {
		m, _ := it.(map[string]any)
		uid, _ := m["uid"].(string)
		banned[uid], _ = m["banned"].(bool)
	}
	if !banned["u-ban"] {
		t.Error("封号池分组内的账号应标 banned")
	}
	if !banned["u-reason"] {
		t.Error("禁用原因含 11140 的账号应标 banned")
	}
	if banned["u-ok"] {
		t.Error("正常账号不该标 banned")
	}
	if got, _ := out["banned_total"].(float64); got != 2 {
		t.Errorf("banned_total=%v want 2", out["banned_total"])
	}
}

// TestPatrolSkipsBannedGroup 已在「封号池」里的账号不再巡检（封号是终态，再探一次
// 只是白烧一次对话），并透出 skipped_banned 供面板提示。
func TestPatrolSkipsBannedGroup(t *testing.T) {
	fake := newPatrolUpstream()
	fake.banned["tok-ban"] = `{"code":11140,"msg":"request illegal"}`
	p, pl, gs := newPatrolPanel(t, fake, acc("u-ban", "tok-ban"), acc("u-ok", "tok-ok"))

	g, _, err := gs.Create(patrolGroupBannedName, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.AddMember(g.ID, "u-ban"); err != nil {
		t.Fatal(err)
	}
	pl.SetGroup("u-ban", g.ID)

	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	fake.mu.Lock()
	banHits := fake.bill["tok-ban"]
	fake.mu.Unlock()
	if banHits != 0 {
		t.Errorf("封号池里的账号不该被巡检（余额接口命中 %d 次）", banHits)
	}

	p.patrol.mu.Lock()
	results := append([]patrolResult(nil), p.patrol.results...)
	skipped := p.patrol.skippedBanned
	p.patrol.mu.Unlock()
	if len(results) != 1 || results[0].UID != "u-ok" {
		t.Fatalf("results=%+v want 只巡检 u-ok", results)
	}
	if skipped != 1 {
		t.Errorf("skippedBanned=%d want 1", skipped)
	}

	_, out, raw := call(t, p, "GET", "/panel/api/patrol", "")
	if got, _ := out["skipped_banned"].(float64); got != 1 {
		t.Errorf("skipped_banned=%v want 1（raw=%s）", out["skipped_banned"], raw)
	}
}

// blockingUpstream 阻塞型上游：billing / chat 都卡住直到 block 关闭或请求被取消。
// 用于验证面板「停止」能真正中断进行中的一轮。
type blockingUpstream struct {
	block chan struct{}
}

func (f *blockingUpstream) transport(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/console/enterprises/personal/models") {
		return fakeJSON(http.StatusOK, enterprisePayload("m1")), nil
	}
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-f.block:
		return fakeJSON(http.StatusOK, aliveSSEBody), nil
	}
}

// TestPatrolStopCancelsRunning：停止能中断进行中的一轮，并关闭自动巡检开关。
func TestPatrolStopCancelsRunning(t *testing.T) {
	bu := &blockingUpstream{block: make(chan struct{})}
	p, _, _ := newPatrolPanelRT(t, rtFunc(bu.transport), acc("u1", "t1"), acc("u2", "t2"))

	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 让 worker 进入上游等待

	cancelled, autoDisabled := p.patrol.stop(true, true)
	close(bu.block) // 放开泄漏的在途请求，避免测试结束时残留 goroutine
	if !cancelled {
		t.Error("应取消到进行中的一轮")
	}
	if !autoDisabled {
		t.Error("应关闭自动巡检")
	}
	waitPatrol(t, p)
	if got := p.patrol.snapshotSettings().Enabled; got {
		t.Error("停止后自动巡检开关应为 false")
	}
}

// TestPatrolStopWithoutRun：没在跑时只关自动巡检，不报错。
func TestPatrolStopWithoutRun(t *testing.T) {
	fake := newPatrolUpstream()
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"))
	cancelled, autoDisabled := p.patrol.stop(true, true)
	if cancelled {
		t.Error("没有进行中的轮次时不该报 cancelled")
	}
	if !autoDisabled {
		t.Error("应关闭自动巡检")
	}
	code, out, raw := call(t, p, "POST", "/panel/api/patrol/stop", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["ok"] != true {
		t.Errorf("out=%v", out)
	}
	// 停止接口幂等：再调一次仍 200。
	if code, _, raw := call(t, p, "POST", "/panel/api/patrol/stop", ""); code != http.StatusOK {
		t.Fatalf("second stop code=%d raw=%s", code, raw)
	}
}

// TestPatrolConcurrencyZeroUnlimited：并发 0 = 不限，全池账号都要有结论。
func TestPatrolConcurrencyZeroUnlimited(t *testing.T) {
	fake := newPatrolUpstream()
	fake.zero["t2"] = true
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"), acc("u2", "t2"), acc("u3", "t3"))
	p.patrol.updateSettings(patrolSettings{
		Enabled: true, IntervalValue: 1, IntervalUnit: patrolUnitHour,
		CheckBan: false, CheckCredit: true, Concurrency: 0,
	})
	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)
	p.patrol.mu.Lock()
	n := len(p.patrol.results)
	p.patrol.mu.Unlock()
	if n != 3 {
		t.Fatalf("results=%d want 3（并发不限时每个账号都要探到）", n)
	}
	if got := p.patrol.snapshotSettings().Concurrency; got != 0 {
		t.Errorf("concurrency=%d want 0（不限）", got)
	}
}

// TestPatrolAutoTickerFixedCadence 自动巡检是**固定节拍定时器**：节拍到点就触发，
// 上一轮没结束就跳过本次（而不是"等上一轮结束再计时"）。用阻塞上游让第一轮卡住，
// 观察后续节拍仍按时到来并被跳过。
func TestPatrolAutoTickerFixedCadence(t *testing.T) {
	bu := &blockingUpstream{block: make(chan struct{})}
	p, _, _ := newPatrolPanelRT(t, rtFunc(bu.transport), acc("u1", "t1"))
	p.patrol.tickOverride = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartPatrol(ctx)
	p.StartPatrol(ctx) // 幂等：不该叠加第二个 ticker
	time.Sleep(280 * time.Millisecond)

	p.patrol.mu.Lock()
	skipped := p.patrol.skippedTicks
	running := p.patrol.running
	p.patrol.mu.Unlock()

	close(bu.block) // 放开阻塞的请求，让本轮收尾
	cancel()
	waitPatrol(t, p)

	if !running {
		t.Error("第一轮应仍在跑（上游被阻塞）")
	}
	if skipped < 2 {
		t.Errorf("skippedTicks=%d want >=2：固定节拍应在上一轮未结束时继续到点并跳过", skipped)
	}
}

// TestPatrolAutoTickerStopsWhenDisabled 关闭自动巡检后不再有新节拍。
func TestPatrolAutoTickerStopsWhenDisabled(t *testing.T) {
	fake := newPatrolUpstream()
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"))
	p.patrol.tickOverride = 30 * time.Millisecond
	p.patrol.updateSettings(patrolSettings{Enabled: false, IntervalValue: 1, IntervalUnit: patrolUnitHour, CheckCredit: true, Concurrency: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartPatrol(ctx)
	time.Sleep(150 * time.Millisecond)

	p.patrol.mu.Lock()
	runs := len(p.patrol.results)
	p.patrol.mu.Unlock()
	if runs != 0 {
		t.Errorf("自动巡检关闭时不该跑轮次，results=%d", runs)
	}
}

// TestPatrolDefaultNeverProbesChat 默认设置（ProbeChat=false）下巡检**绝不**发起
// 对话请求：封号只靠用户请求命中 11140 + 余额接口返回码，避免后台烧积分。
func TestPatrolDefaultNeverProbesChat(t *testing.T) {
	fake := newPatrolUpstream()
	fake.zero["t2"] = true
	p, _, _ := newPatrolPanel(t, fake, acc("u1", "t1"), acc("u2", "t2"))

	if p.patrol.snapshotSettings().ProbeChat {
		t.Fatal("默认设置不该开启对话探测")
	}
	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	waitPatrol(t, p)

	fake.mu.Lock()
	chatHits := fake.chat["t1"] + fake.chat["t2"]
	billHits := fake.bill["t1"] + fake.bill["t2"]
	fake.mu.Unlock()
	if chatHits != 0 {
		t.Errorf("巡检不该发起对话请求（chat 命中 %d 次）", chatHits)
	}
	if billHits == 0 {
		t.Error("积分检测应调用余额接口")
	}
}

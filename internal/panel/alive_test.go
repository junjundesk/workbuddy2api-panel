// alive_test.go 单账号测活（POST /panel/api/accounts/{uid}/alive）：
// 用假上游验证「发的是 hi / 模型选择规则 / 结果与错误分类 / 不改池状态」。全程不打真网。
package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// aliveSSEBody 成功流的形态：role 首帧 → 正文帧 → 收尾 + usage → [DONE]。
const aliveSSEBody = "data: {\"id\":\"c1\",\"model\":\"zzz-default\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好！\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n" +
	"data: [DONE]\n\n"

// aliveUpstream 假上游：CN 模型目录（企业端点）+ chat SSE。
// /v3/config 默认回 500 → FetchModels 降级为「仅企业端点」，测试路径确定。
type aliveUpstream struct {
	mu sync.Mutex

	modelsBody   string
	modelsStatus int // 0 = 200
	chatStatus   int // 0 = 200
	chatBody     string

	chatHits int
	chatReq  string
	chatAuth string
	chatPath string
}

func (f *aliveUpstream) transport(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/console/enterprises/personal/models"):
		status := f.modelsStatus
		if status == 0 {
			status = http.StatusOK
		}
		return fakeJSON(status, f.modelsBody), nil
	case strings.HasSuffix(req.URL.Path, "/v2/chat/completions"):
		raw, _ := io.ReadAll(req.Body)
		f.chatHits++
		f.chatReq = string(raw)
		f.chatAuth = req.Header.Get("Authorization")
		f.chatPath = req.URL.Path
		status := f.chatStatus
		if status == 0 {
			status = http.StatusOK
		}
		return fakeJSON(status, f.chatBody), nil
	default:
		// /v3/config（目录主路）：测试里固定失败，走企业端点降级。
		return fakeJSON(http.StatusInternalServerError, "{\"code\":500,\"msg\":\"v3 disabled in test\"}"), nil
	}
}

// newAlivePanel 组装「假上游 + 单账号池」面板。
// 注意 HTTP 与 ChatHTTP 都要换成假 client：ChatStream 走的是 ChatHTTP。
func newAlivePanel(t *testing.T, fake *aliveUpstream) (*Panel, *pool.Pool) {
	t.Helper()
	cl := upstream.New()
	hc := &http.Client{Transport: rtFunc(fake.transport)}
	cl.HTTP = hc
	cl.ChatHTTP = hc
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	return New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: cl}), pl
}

// TestAccountAliveOK：默认模型优先、请求体是 hi、成功回包带回复/用量/耗时。
func TestAccountAliveOK(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: "{\"code\":0,\"data\":{\"models\":[" +
			"{\"id\":\"aaa-model\",\"maxInputTokens\":1000,\"maxOutputTokens\":8192}," +
			"{\"id\":\"zzz-default\",\"maxInputTokens\":1000,\"maxOutputTokens\":8192,\"isDefault\":true}]," +
			"\"agents\":[{\"name\":\"cli\",\"models\":[\"aaa-model\",\"zzz-default\"]}]}}",
		chatBody: aliveSSEBody,
	}
	p, _ := newAlivePanel(t, fake)
	code, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v raw=%s", out["ok"], raw)
	}
	if out["realm"] != "cn" {
		t.Errorf("realm=%v want cn", out["realm"])
	}
	if out["model"] != "zzz-default" {
		t.Errorf("model=%v want 默认模型 zzz-default", out["model"])
	}
	if reply, _ := out["reply"].(string); !strings.Contains(reply, "你好") {
		t.Errorf("reply=%q", reply)
	}
	if toks, _ := out["total_tokens"].(float64); toks != 8 {
		t.Errorf("total_tokens=%v want 8", out["total_tokens"])
	}
	if ttfb, ok := out["ttfb_ms"].(float64); !ok || ttfb < 1 {
		t.Errorf("ttfb_ms=%v want >=1", out["ttfb_ms"])
	}
	if _, ok := out["latency_ms"]; !ok {
		t.Errorf("latency_ms missing: %s", raw)
	}

	// 出站请求：同一账号、一句 hi、选中模型、流式。
	if fake.chatHits != 1 {
		t.Errorf("chat hits=%d want 1", fake.chatHits)
	}
	if fake.chatPath != "/v2/chat/completions" {
		t.Errorf("chat path=%q", fake.chatPath)
	}
	if fake.chatAuth != "Bearer at1" {
		t.Errorf("Authorization=%q want 该账号 token", fake.chatAuth)
	}
	for _, want := range []string{"\"content\":\"hi\"", "\"model\":\"zzz-default\"", "\"stream\":true"} {
		if !strings.Contains(fake.chatReq, want) {
			t.Errorf("chat body missing %s: %s", want, fake.chatReq)
		}
	}
}

// TestAccountAliveModelFallback：没有 isDefault 时按 id 字典序取第一个（确定性）。
func TestAccountAliveModelFallback(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("mmm-model", "aaa-model"),
		chatBody:   aliveSSEBody,
	}
	p, _ := newAlivePanel(t, fake)
	_, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if out["ok"] != true {
		t.Fatalf("ok=%v raw=%s", out["ok"], raw)
	}
	if out["model"] != "aaa-model" {
		t.Errorf("model=%v want 字典序最小 aaa-model", out["model"])
	}
}

// TestAccountAliveChatUpstreamError：对话阶段 401 + 12153 → 分类透出、仍回 HTTP 200。
func TestAccountAliveChatUpstreamError(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("m1"),
		chatStatus: http.StatusUnauthorized,
		chatBody:   "{\"code\":12153,\"msg\":\"Offline user session not found\"}",
	}
	p, _ := newAlivePanel(t, fake)
	code, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if out["ok"] != false || out["stage"] != "chat" {
		t.Fatalf("ok=%v stage=%v raw=%s", out["ok"], out["stage"], raw)
	}
	if out["kind"] != "session_dead" {
		t.Errorf("kind=%v want session_dead", out["kind"])
	}
	if status, _ := out["upstream_status"].(float64); status != 401 {
		t.Errorf("upstream_status=%v want 401", out["upstream_status"])
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Errorf("error missing: %s", raw)
	}
}

// TestAccountAliveCatalogFailure：模型目录阶段失败 → stage=catalog，且不发起对话。
func TestAccountAliveCatalogFailure(t *testing.T) {
	fake := &aliveUpstream{modelsStatus: http.StatusInternalServerError, modelsBody: "{\"code\":500}"}
	p, _ := newAlivePanel(t, fake)
	_, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if out["ok"] != false || out["stage"] != "catalog" {
		t.Fatalf("ok=%v stage=%v raw=%s", out["ok"], out["stage"], raw)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Errorf("error missing: %s", raw)
	}
	if fake.chatHits != 0 {
		t.Errorf("目录失败后不应发起对话，chat hits=%d", fake.chatHits)
	}
}

// TestAccountAliveEmptyStream：200 但只有 [DONE] → 空流失败，不假报成功。
func TestAccountAliveEmptyStream(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("m1"),
		chatBody:   "data: [DONE]\n\n",
	}
	p, _ := newAlivePanel(t, fake)
	_, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if out["ok"] != false || out["stage"] != "chat" {
		t.Fatalf("ok=%v stage=%v raw=%s", out["ok"], out["stage"], raw)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "空流") {
		t.Errorf("error=%q want 空流说明", msg)
	}
}

// TestAccountAliveErrorFrame：200 + error 帧（限流）→ 走 Aggregate 分类错误。
func TestAccountAliveErrorFrame(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("m1"),
		chatBody:   "data: {\"error\":{\"message\":\"rate limit exceeded\"}}\n\n",
	}
	p, _ := newAlivePanel(t, fake)
	_, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", "")
	if out["ok"] != false || out["stage"] != "chat" {
		t.Fatalf("ok=%v stage=%v raw=%s", out["ok"], out["stage"], raw)
	}
	if out["kind"] != "soft_rate" {
		t.Errorf("kind=%v want soft_rate raw=%s", out["kind"], raw)
	}
}

// TestAccountAliveUnknownAccount：未知 uid 404（硬错误，与上游失败区分）。
func TestAccountAliveUnknownAccount(t *testing.T) {
	p, _ := newAlivePanel(t, &aliveUpstream{})
	if code, _, _ := call(t, p, "POST", "/panel/api/accounts/ghost/alive", ""); code != http.StatusNotFound {
		t.Fatalf("unknown uid code=%d want 404", code)
	}
}

// TestAccountAliveRequiresAuth：面板鉴权同口径，无 Bearer 直接 401。
func TestAccountAliveRequiresAuth(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/alive", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// TestAccountAliveKeepsPoolState：测活是诊断动作，不写成功/失败计数、不碰冷却与余额。
func TestAccountAliveKeepsPoolState(t *testing.T) {
	fake := &aliveUpstream{
		modelsBody: enterprisePayload("m1"),
		chatBody:   aliveSSEBody,
	}
	p, pl := newAlivePanel(t, fake)
	if _, out, raw := call(t, p, "POST", "/panel/api/accounts/u1/alive", ""); out["ok"] != true {
		t.Fatalf("测活应成功: %s", raw)
	}
	var st pool.Status
	for _, s := range pl.List() {
		if s.UID == "u1" {
			st = s
		}
	}
	if st.SuccessCount != 0 || st.ErrTotal != 0 || st.Cooling || st.Disabled || st.Credits != 0 {
		t.Fatalf("测活改动了池状态: %+v", st)
	}
}

// TestAliveProbeModel 模型选择的纯函数边界。
func TestAliveProbeModel(t *testing.T) {
	cases := []struct {
		name  string
		infos []upstream.ModelInfo
		want  string
	}{
		{"默认优先", []upstream.ModelInfo{{ID: "zzz", IsDefault: true}, {ID: "aaa"}}, "zzz"},
		{"多默认取字典序", []upstream.ModelInfo{{ID: "zzz", IsDefault: true}, {ID: "aaa", IsDefault: true}}, "aaa"},
		{"无默认取字典序", []upstream.ModelInfo{{ID: "mmm"}, {ID: "aaa"}}, "aaa"},
		{"空列表", nil, ""},
		{"跳过空 id", []upstream.ModelInfo{{ID: ""}, {ID: "b"}}, "b"},
	}
	for _, c := range cases {
		if got := aliveProbeModel(c.infos); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// account_models_test.go 账号级实时模型列表（GET /panel/api/accounts/{uid}/models）：
// 把 upstream.Client 的 HTTP 传输层换成假上游，验证「每次调用都真实打上游」与
// 「同账号快照差集」，并覆盖未知账号与差集纯函数边界。全程不打真网。
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rtFunc 假 RoundTripper（避免依赖 net 连接）。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeJSON 构造假响应体。
func fakeJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// enterprisePayload 企业端点（/console/enterprises/personal/models）响应：agents[cli]
// 是可见模型名单，models[] 提供元数据（两者按 id 对齐才进结果）。
func enterprisePayload(ids ...string) string {
	m, cli := make([]string, 0, len(ids)), make([]string, 0, len(ids))
	for _, id := range ids {
		m = append(m, `{"id":"`+id+`","maxInputTokens":131072,"maxOutputTokens":8192,"descriptionZh":"测试"}`)
		cli = append(cli, `"`+id+`"`)
	}
	return `{"code":0,"data":{"models":[` + strings.Join(m, ",") +
		`],"agents":[{"name":"cli","models":[` + strings.Join(cli, ",") + `]}]}}`
}

// fakeUpstream 可换内容的假上游：enterprise 端点回当前模型集，
// /v3/config 回 500（走 FetchModels 的降级路径：仅企业端点），其余路径回 404。
type fakeUpstream struct {
	mu   sync.Mutex
	body string
	hits int
}

func (f *fakeUpstream) set(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = enterprisePayload(ids...)
}

func (f *fakeUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// setRaw 直接设置响应体（用于构造"上游不下发字段"的形态）。
func (f *fakeUpstream) setRaw(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *fakeUpstream) transport(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/console/enterprises/personal/models") {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		return fakeJSON(http.StatusOK, f.body), nil
	}
	return fakeJSON(http.StatusInternalServerError, `{"code":500,"msg":"v3 disabled in test"}`), nil
}

// newAccountModelsPanel 组装「假上游 + 单账号池」的面板。
func newAccountModelsPanel(t *testing.T, fake *fakeUpstream) *Panel {
	t.Helper()
	upstream.ResetLookupChainForTest() // 清包级缓存/在途拉取，测试间互不污染
	t.Cleanup(upstream.ResetLookupChainForTest)

	cl := upstream.New()
	cl.HTTP = &http.Client{Transport: rtFunc(fake.transport)}

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	return New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: cl})
}

// getModels 调账号模型接口并解析响应。
func getModels(t *testing.T, p *Panel, uid string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/accounts/"+uid+"/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// modelIDs 从响应 models[] 抽 id 列表。
func modelIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["models"].([]any)
	ids := make([]string, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("model entry not object: %v", e)
		}
		ids = append(ids, m["id"].(string))
	}
	return ids
}

func strList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// TestAccountModelsRealtimeAndDiff 每次调用都打上游（不读缓存），并按同账号上次结果报差集。
func TestAccountModelsRealtimeAndDiff(t *testing.T) {
	fake := &fakeUpstream{}
	fake.set("alpha", "beta")
	p := newAccountModelsPanel(t, fake)

	code, body := getModels(t, p, "u1")
	if code != http.StatusOK {
		t.Fatalf("first call code=%d body=%v", code, body)
	}
	if body["realm"] != "cn" {
		t.Errorf("realm=%v want cn", body["realm"])
	}
	got := modelIDs(t, body)
	if len(got) != 2 || got[0] != "cn:alpha" || got[1] != "cn:beta" {
		t.Fatalf("first call models=%v want [cn:alpha cn:beta]", got)
	}
	if _, ok := body["prev_at"]; ok {
		t.Errorf("首次拉取不应有 prev_at（无前值不报变化）: %v", body["prev_at"])
	}
	if len(strList(body["added"])) != 0 || len(strList(body["removed"])) != 0 {
		t.Errorf("首次拉取不应有差集: added=%v removed=%v", body["added"], body["removed"])
	}
	if n := fake.calls(); n != 1 {
		t.Fatalf("上游调用次数=%d want 1（每次调用都实时拉取）", n)
	}

	// 上游目录变化：gamma 上、beta 下 —— 第二次调用必须重新打上游并报差集。
	fake.set("alpha", "gamma")
	code, body = getModels(t, p, "u1")
	if code != http.StatusOK {
		t.Fatalf("second call code=%d body=%v", code, body)
	}
	if n := fake.calls(); n != 2 {
		t.Fatalf("上游调用次数=%d want 2（不得命中任何缓存）", n)
	}
	if got := modelIDs(t, body); len(got) != 2 || got[0] != "cn:alpha" || got[1] != "cn:gamma" {
		t.Fatalf("second call models=%v want [cn:alpha cn:gamma]", got)
	}
	if added := strList(body["added"]); len(added) != 1 || added[0] != "cn:gamma" {
		t.Errorf("added=%v want [cn:gamma]", body["added"])
	}
	if removed := strList(body["removed"]); len(removed) != 1 || removed[0] != "cn:beta" {
		t.Errorf("removed=%v want [cn:beta]", body["removed"])
	}
	if body["prev_at"] == nil || body["prev_at"] == "" {
		t.Errorf("第二次拉取应带 prev_at: %v", body["prev_at"])
	}
}

// TestAccountModelsUnknownUID 未知账号 404（不触发任何上游探测）。
func TestAccountModelsUnknownUID(t *testing.T) {
	fake := &fakeUpstream{}
	fake.set("alpha")
	p := newAccountModelsPanel(t, fake)

	code, body := getModels(t, p, "nope")
	if code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%v", code, body)
	}
	if n := fake.calls(); n != 0 {
		t.Errorf("未知账号不应打上游，实际调用 %d 次", n)
	}
}

// TestAccountModelsUpstreamFailure 上游全失败 → 502（不返回兜底假名单）。
func TestAccountModelsUpstreamFailure(t *testing.T) {
	fake := &fakeUpstream{} // body 为空 → 企业端点 JSON 解析失败，v3 路 500
	p := newAccountModelsPanel(t, fake)

	code, body := getModels(t, p, "u1")
	if code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%v", code, body)
	}
}

// TestAccountModelsNoSourceFallback 上游没下发的字段必须省略：即便模型 id 命中源码静态表
// （glm-5.2 在 context 种子表里是 1000000，在 CN 档位表里是 high,xhigh），也绝不许回填内置值。
func TestAccountModelsNoSourceFallback(t *testing.T) {
	fake := &fakeUpstream{}
	// 上游只下发 id：无 maxInputTokens / maxOutputTokens / supportedEfforts。
	fake.setRaw(`{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`)
	p := newAccountModelsPanel(t, fake)

	code, body := getModels(t, p, "u1")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, body)
	}
	raw, _ := body["models"].([]any)
	if len(raw) != 1 {
		t.Fatalf("models=%v want 1 条", body["models"])
	}
	m, _ := raw[0].(map[string]any)
	for _, k := range []string{"context_length", "max_output_tokens", "supported_efforts"} {
		if v, ok := m[k]; ok && v != nil {
			t.Errorf("%s 不得回填源码内置兜底值，got %v", k, v)
		}
	}
	if v, _ := m["default_effort"].(string); v != "" {
		t.Errorf("default_effort 不得回填静态档位，got %q", v)
	}
	if m["id"] != "cn:glm-5.2" {
		t.Errorf("id=%v want cn:glm-5.2（名单本身来自上游）", m["id"])
	}
}

// TestDiffModelIDs 差集纯函数：首次全为新增、双向差集、排序稳定、重复 id 去重。
func TestDiffModelIDs(t *testing.T) {
	cases := []struct {
		name           string
		prev, cur      []string
		added, removed []string
	}{
		{name: "首次拉取全部为新增", prev: nil, cur: []string{"b", "a"}, added: []string{"a", "b"}},
		{name: "双向差集", prev: []string{"a", "b"}, cur: []string{"b", "c"}, added: []string{"c"}, removed: []string{"a"}},
		{name: "无变化", prev: []string{"a"}, cur: []string{"a"}},
		{name: "全部消失", prev: []string{"a", "b"}, cur: nil, removed: []string{"a", "b"}},
		{name: "重复 id 只算一次", prev: []string{"a", "a"}, cur: []string{"b", "b"}, added: []string{"b"}, removed: []string{"a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			added, removed := diffModelIDs(c.prev, c.cur)
			if strings.Join(added, ",") != strings.Join(c.added, ",") {
				t.Errorf("added=%v want %v", added, c.added)
			}
			if strings.Join(removed, ",") != strings.Join(c.removed, ",") {
				t.Errorf("removed=%v want %v", removed, c.removed)
			}
		})
	}
}

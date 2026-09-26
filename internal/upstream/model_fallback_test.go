// model_fallback_test.go 模型目录「主域不可达 → 备用域」兜底的行为锁定：
// 主域连接失败/5xx/404 时换域拿到目录；成功、鉴权失败（401/403）、非模型路径、
// 非 GET 请求一律不换域。CN 与 global 两条端到端路径都覆盖（走真实 Fetch* 调用）。
package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// hostFake 按请求主机分派响应并记录访问序列。status=0 表示模拟连接层失败。
type hostFake struct {
	mu    sync.Mutex
	calls []string
	resp  func(host, path string) (status int, body string)
}

func (f *hostFake) rt() rtFunc {
	return func(r *http.Request) (*http.Response, error) {
		host, path := r.URL.Hostname(), r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+host+path)
		f.mu.Unlock()
		status, body := f.resp(host, path)
		if status == 0 {
			return nil, errors.New("dial tcp: connection refused")
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func (f *hostFake) hits(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, host) {
			n++
		}
	}
	return n
}

func (f *hostFake) seq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// cnEnterprisePayload 企业端点响应（agents[cli] 决定可见名单）。
func cnEnterprisePayload(ids ...string) string {
	m, cli := make([]string, 0, len(ids)), make([]string, 0, len(ids))
	for _, id := range ids {
		m = append(m, `{"id":"`+id+`","maxInputTokens":131072,"maxOutputTokens":8192}`)
		cli = append(cli, `"`+id+`"`)
	}
	return `{"code":0,"data":{"models":[` + strings.Join(m, ",") +
		`],"agents":[{"name":"cli","models":[` + strings.Join(cli, ",") + `]}]}}`
}

// globalPayload global 目录响应（对象数组形态）。
func globalPayload(ids ...string) string {
	m := make([]string, 0, len(ids))
	for _, id := range ids {
		m = append(m, `{"id":"`+id+`","maxInputTokens":262144,"maxOutputTokens":8192}`)
	}
	return `{"code":0,"data":{"models":[` + strings.Join(m, ",") + `]}}`
}

// TestModelFallbackCNEndToEnd 国内主域全失败 → 备用域 www.codebuddy.cn 供目录，
// 且返回的模型 id 全部来自备用域响应（仍然只有账号上游数据）。
func TestModelFallbackCNEndToEnd(t *testing.T) {
	fake := &hostFake{resp: func(host, path string) (int, string) {
		switch {
		case host == "copilot.tencent.com":
			return 500, `{"code":500,"msg":"primary down"}`
		case host == "www.codebuddy.cn" && strings.HasSuffix(path, "/console/enterprises/personal/models"):
			return 200, cnEnterprisePayload("alt-cn-a", "alt-cn-b")
		}
		return 500, `{}`
	}}
	cl := New()
	cl.HTTP = &http.Client{Transport: &modelFallbackTransport{base: fake.rt()}}

	infos, err := cl.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if err != nil {
		t.Fatalf("FetchModels err=%v（应经备用域成功）", err)
	}
	got := []string{}
	for _, mi := range infos {
		got = append(got, mi.ID)
	}
	if strings.Join(got, ",") != "alt-cn-a,alt-cn-b" {
		t.Fatalf("ids=%v want [alt-cn-a alt-cn-b]", got)
	}
	if fake.hits("copilot.tencent.com") == 0 {
		t.Error("应先尝试主域")
	}
	if fake.hits("www.codebuddy.cn") == 0 {
		t.Error("主域失败后应改打备用域")
	}
}

// TestModelFallbackGlobalEndToEnd global 主域失败 → 备用域 www.codebuddy.ai 供目录。
func TestModelFallbackGlobalEndToEnd(t *testing.T) {
	fake := &hostFake{resp: func(host, path string) (int, string) {
		switch host {
		case "www.workbuddy.ai":
			return 0, "" // 连接层失败（DNS/连接被挡）
		case "www.codebuddy.ai":
			return 200, globalPayload("alt-global-1", "alt-global-2")
		}
		return 500, `{}`
	}}
	cl := New()
	cl.HTTP = &http.Client{Transport: &modelFallbackTransport{base: fake.rt()}}

	a := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("BackfillRealmFor: %v", err)
	}
	infos := cl.FetchGlobalModelInfos(a)
	got := []string{}
	for _, mi := range infos {
		got = append(got, mi.ID)
	}
	if strings.Join(got, ",") != "alt-global-1,alt-global-2" {
		t.Fatalf("ids=%v want [alt-global-1 alt-global-2]（备用域目录）", got)
	}
	if fake.hits("www.codebuddy.ai") == 0 {
		t.Error("主域不可达后应改打备用域")
	}
}

// TestModelFallbackScope 兜底边界：成功 / 鉴权失败 / 非模型路径 / 非 GET 都不换域。
func TestModelFallbackScope(t *testing.T) {
	cases := []struct {
		name   string
		method string
		url    string
		status int
	}{
		{name: "主域成功不换域", method: "GET", url: "https://www.workbuddy.ai/v2/enterprises/personal/models", status: 200},
		{name: "401/403 是凭证问题不换域", method: "GET", url: "https://www.workbuddy.ai/v2/enterprises/personal/models", status: 403},
		{name: "非模型路径不换域", method: "GET", url: "https://www.workbuddy.ai/v2/billing/meter/balance", status: 500},
		{name: "chat 路径不换域", method: "POST", url: "https://www.workbuddy.ai/v2/chat/completions", status: 500},
		{name: "CN 非模型路径不换域", method: "GET", url: "https://copilot.tencent.com/v2/credit", status: 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &hostFake{resp: func(host, path string) (int, string) {
				return c.status, `{"code":0,"data":{"models":[{"id":"primary-only"}]}}`
			}}
			client := &http.Client{Transport: &modelFallbackTransport{base: fake.rt()}}
			req, err := http.NewRequest(c.method, c.url, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Errorf("status=%d want %d（不应被兜底改变）", resp.StatusCode, c.status)
			}
			for _, alt := range []string{"www.codebuddy.ai", "www.codebuddy.cn"} {
				if fake.hits(alt) != 0 {
					t.Errorf("不应打备用域 %s，实际调用序列=%v", alt, fake.seq())
				}
			}
		})
	}
}

// TestModelFallbackPreservesPathAndQuery 换域时保留 path/query，只换 host。
func TestModelFallbackPreservesPathAndQuery(t *testing.T) {
	var got string
	fake := &hostFake{resp: func(host, path string) (int, string) {
		if host == "www.workbuddy.ai" {
			return 502, `{}`
		}
		got = host + path
		return 200, `{"code":0,"data":{"models":[]}}`
	}}
	client := &http.Client{Transport: &modelFallbackTransport{base: fake.rt()}}
	req, _ := http.NewRequest("GET", "https://www.workbuddy.ai/v3/config?agent=cli&x=1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if got != "www.codebuddy.ai/v3/config" {
		t.Errorf("备用域请求 host+path=%q want www.codebuddy.ai/v3/config", got)
	}
}

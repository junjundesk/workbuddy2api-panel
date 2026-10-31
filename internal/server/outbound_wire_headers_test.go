package server

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// captureWireHeaders 把上游收到的**线上真值**头抓下来（服务端视角，含传输层追加的
// Accept-Encoding/Content-Length/Host——这些在代码里看不见，只能这样验）。
func captureWireHeaders(t *testing.T, path, body string) (http.Header, string) {
	t.Helper()
	var got http.Header
	var upPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		got.Set("Host", r.Host)
		upPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseOK))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(a)
	p.SetCredits(a.UID, 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("上游未收到请求")
	}
	return got, upPath
}

// wireHeaderNames 规范化头名（小写、排序）。
func wireHeaderNames(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return out
}

// TestOutboundWireHeaders 线上真值回归：入站 → handler → 真实 Transport → 上游，
// 断言出站头集合与关键值。与 upstream 包的纯函数 golden 互补：那边锁代码意图，
// 这边锁"代码设了但被传输层改掉/漏掉"的漂移。
func TestOutboundWireHeaders(t *testing.T) {
	h, path := captureWireHeaders(t, "/v1/chat/completions",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if path != "/v2/chat/completions" {
		t.Errorf("上游路径 = %q", path)
	}

	want := []string{
		"accept", "accept-encoding", "accept-language", "authorization", "b3",
		"content-length", "content-type", "host", "origin", "referer", "traceparent",
		"user-agent", "x-agent-intent", "x-agent-purpose", "x-agent-runtime", "x-agent-type",
		"x-b3-sampled", "x-b3-spanid", "x-b3-traceid", "x-codebuddy-request",
		"x-conversation-id", "x-conversation-message-id", "x-conversation-request-id",
		"x-domain", "x-ide-name", "x-ide-type", "x-ide-version",
		"x-no-department-info", "x-no-enterprise-id", "x-product",
		"x-request-id", "x-requested-with", "x-root-request-id", "x-trace-id", "x-user-id",
	}
	got := wireHeaderNames(h)
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("缺头: %s", k)
		}
	}
	allowed := map[string]bool{}
	for _, k := range want {
		allowed[k] = true
	}
	for _, k := range got {
		if !allowed[k] {
			t.Errorf("多头（官方客户端不发）: %s", k)
		}
	}

	for k, v := range map[string]string{
		"X-Product":       "SaaS",
		"X-Agent-Type":    "main",
		"X-Agent-Purpose": "conversation",
		"X-Domain":        "www.workbuddy.cn",
		"X-IDE-Version":   "5.9.3",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// 自造设备头默认必须缺席。
	if h.Get("X-Machine-ID") != "" || h.Get("X-Session-ID") != "" {
		t.Error("默认不应发送 X-Machine-ID / X-Session-ID")
	}
}

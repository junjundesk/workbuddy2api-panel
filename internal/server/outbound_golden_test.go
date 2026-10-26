package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// outboundCase 一组「入站请求 → 出站 body」的固定夹具，用于出站等价性回归。
type outboundCase struct {
	name       string
	path       string // /v1/chat/completions 或 /v1/responses
	promptMode string
	promptText string
	sanitize   bool
	global     bool
	body       string
}

func outboundCases() []outboundCase {
	return []outboundCase{
		{
			name: "chat_plain_tools",
			path: "/v1/chat/completions",
			body: `{"model":"glm-5.2","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_completion_tokens":128,"tools":[{"type":"function","function":{"name":"f1","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"f1"}}}`,
		},
		{
			name: "chat_multimodal_reasoning",
			path: "/v1/chat/completions",
			body: `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"what"},{"type":"image_url","image_url":"https://x/y.png"}]},{"role":"assistant","content":"ok","reasoning_content":"think"}],"reasoning_effort":"max"}`,
		},
		{
			name: "chat_tool_pairing",
			path: "/v1/chat/completions",
			body: `{"model":"glm-5.2","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f2","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"1"},{"role":"user","content":"go on"},{"role":"tool","tool_call_id":"b","content":"2"},{"role":"tool","tool_call_id":"orphan","content":"x"}]}`,
		},
		{
			name:       "chat_custom_prompt",
			path:       "/v1/chat/completions",
			promptMode: "custom",
			promptText: "GATEWAY PROMPT",
			body:       `{"model":"glm-5.2","messages":[{"role":"system","content":"client sys"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:       "chat_append_prompt",
			path:       "/v1/chat/completions",
			promptMode: "append",
			promptText: "APPENDED",
			body:       `{"model":"glm-5.2","messages":[{"role":"system","content":"client sys"},{"role":"developer","content":"dev"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "chat_sanitize",
			path:     "/v1/chat/completions",
			sanitize: true,
			body:     `{"model":"glm-5.2","messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI tool for Claude. Default branch (you will usually use this for PRs)."},{"role":"user","content":"hi 11-128"}]}`,
		},
		{
			name:   "chat_global_realm",
			path:   "/v1/chat/completions",
			global: true,
			body:   `{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`,
		},
		{
			name: "responses_convert",
			path: "/v1/responses",
			body: `{"model":"glm-5.2","instructions":"be brief","store":false,"max_output_tokens":64,"reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object"}}},"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"custom","name":"shell","description":"run"}],"tool_choice":{"type":"custom","name":"shell"},"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"},{"type":"input_image","image_url":"https://x/y.png"}]},{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"sunny"},{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}]}`,
		},
	}
}

type capturedOutbound struct {
	path string
	body []byte
}

func newCaptureClient(t *testing.T, sanitize bool) (*upstream.Client, *[]capturedOutbound) {
	t.Helper()
	cap := &[]capturedOutbound{}
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*cap = append(*cap, capturedOutbound{path: r.URL.Path, body: raw})
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(sanitize)
	return c, cap
}

func newOutboundHandler(t *testing.T, c outboundCase, up *upstream.Client) *Handler {
	t.Helper()
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	if c.global {
		auth.SetGlobalEnabled(true)
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			t.Fatal(err)
		}
	}
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(a)
	p.SetCredits(a.UID, 1000, 0)
	mode := c.promptMode
	if mode == "" {
		mode = "passthrough"
	}
	return NewHandler(Config{Pool: p, Upstream: up, PromptMode: mode, PromptText: c.promptText})
}

// runOutbound 跑一次入站请求，返回出站 body（断言恰好一次上游调用）。
func runOutbound(t *testing.T, c outboundCase) []byte {
	t.Helper()
	up, cap := newCaptureClient(t, c.sanitize)
	h := newOutboundHandler(t, c, up)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
	if rec.Code != 200 {
		t.Fatalf("%s: status=%d body=%s", c.name, rec.Code, rec.Body.String())
	}
	if len(*cap) != 1 {
		t.Fatalf("%s: upstream calls=%d", c.name, len(*cap))
	}
	return (*cap)[0].body
}

// TestOutboundGolden 锁住「入站请求 → 出站 body」的语义：任何改写管线的重构
// （单次解析/单次 encode 等）都必须让本测试保持全绿。
//
// 重新生成：
//
//	DUMP_GOLDEN=1 go test ./internal/server -run TestOutboundGolden -count 1
func TestOutboundGolden(t *testing.T) {
	if os.Getenv("DUMP_GOLDEN") != "" {
		dir := filepath.Join("testdata", "outbound")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, c := range outboundCases() {
			raw := runOutbound(t, c)
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("%s: outbound not json: %v", c.name, err)
			}
			out, _ := json.MarshalIndent(v, "", "  ")
			if err := os.WriteFile(filepath.Join(dir, c.name+".json"), out, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, c := range outboundCases() {
		t.Run(c.name, func(t *testing.T) {
			raw := runOutbound(t, c)
			var got any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("outbound not json: %v (body=%s)", err, raw)
			}
			wantRaw, err := os.ReadFile(filepath.Join("testdata", "outbound", c.name+".json"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			var want any
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("golden not json: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("outbound body changed:\n--- got ---\n%s\n--- want ---\n%s", gotJSON, wantRaw)
			}
		})
	}
}

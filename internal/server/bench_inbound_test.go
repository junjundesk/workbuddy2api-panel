package server

// bench_inbound_test.go 入站路径基准：chat / responses、小包 / 大包 / 流式。
//
// 目的：让「网关侧开销」可度量、可回归。基准不含上游等待（假上游立即返回），
// 测的是解析、改写、选号、响应转换与写出的纯 CPU/分配成本。
//
// 跑法：
//
//	go test ./internal/server -run '^$' -bench 'BenchmarkInbound' -benchmem
//
// 说明：GC 噪声会让绝对值抖动（同一台机器上 ±20% 属正常）。比较优化前后时
// 建议 GOGC=off 取 CPU 净值，或看 -benchmem 的分配量（分配量非常稳定）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

func benchUpstreamClient(sse string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &readCloser{strings.NewReader(sse)},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type readCloser struct{ *strings.Reader }

func (r *readCloser) Close() error { return nil }

// mkChatBody n 条消息，每条 pad 字节正文。
func mkChatBody(n, pad int) []byte {
	text := strings.Repeat("x", pad)
	msgs := make([]any, 0, n+1)
	msgs = append(msgs, map[string]any{"role": "system", "content": "you are a helpful assistant"})
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": fmt.Sprintf("%d:%s", i, text)})
	}
	raw, _ := json.Marshal(map[string]any{"model": "cn:glm-5.2", "messages": msgs})
	return raw
}

// mkResponsesBody 与 mkChatBody 等量的 Responses input items。
func mkResponsesBody(n, pad int) []byte {
	text := strings.Repeat("x", pad)
	items := make([]any, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{
			"type": "message", "role": role,
			"content": []any{map[string]any{"type": "input_text", "text": fmt.Sprintf("%d:%s", i, text)}},
		})
	}
	raw, _ := json.Marshal(map[string]any{"model": "cn:glm-5.2", "instructions": "you are a helpful assistant", "input": items})
	return raw
}

const benchSSEOne = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n" +
	"data: [DONE]\n\n"

func benchSSEMany(frames int) string {
	var b strings.Builder
	for i := 0; i < frames; i++ {
		b.WriteString("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n")
	}
	b.WriteString("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100,\"total_tokens\":110}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func benchHandler(sse string) *Handler {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	p.Add(a)
	p.SetCredits(a.UID, 1000, 0)
	return NewHandler(Config{
		Pool:       p,
		Upstream:   benchUpstreamClient(sse),
		PromptMode: "custom",
		PromptText: "you are a helpful assistant",
	})
}

func benchRun(b *testing.B, path string, body []byte, sse string) {
	b.Helper()
	h := benchHandler(sse)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(string(body))))
		if rec.Code != 200 {
			b.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func withStream(raw []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	m["stream"] = true
	out, _ := json.Marshal(m)
	return out
}

func BenchmarkInboundChatSmall(b *testing.B) {
	benchRun(b, "/v1/chat/completions", mkChatBody(10, 100), benchSSEOne)
}

func BenchmarkInboundResponsesSmall(b *testing.B) {
	benchRun(b, "/v1/responses", mkResponsesBody(10, 100), benchSSEOne)
}

func BenchmarkInboundChatLarge(b *testing.B) {
	benchRun(b, "/v1/chat/completions", mkChatBody(200, 1024), benchSSEOne)
}

func BenchmarkInboundResponsesLarge(b *testing.B) {
	benchRun(b, "/v1/responses", mkResponsesBody(200, 1024), benchSSEOne)
}

func BenchmarkChatStream50(b *testing.B) {
	benchRun(b, "/v1/chat/completions", withStream(mkChatBody(10, 100)), benchSSEMany(50))
}

func BenchmarkResponsesStream50(b *testing.B) {
	benchRun(b, "/v1/responses", withStream(mkResponsesBody(10, 100)), benchSSEMany(50))
}

func BenchmarkResponsesConvertSmall(b *testing.B) {
	raw := mkResponsesBody(10, 100)
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		b.Fatal(err)
	}
	store := newResponsesStore()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chat, _, _, err := responsesChatRequest(req, store, "")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(chat); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResponsesConvertLarge(b *testing.B) {
	raw := mkResponsesBody(200, 1024)
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		b.Fatal(err)
	}
	store := newResponsesStore()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chat, _, _, err := responsesChatRequest(req, store, "")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(chat); err != nil {
			b.Fatal(err)
		}
	}
}

// TestInboundAllocBudget 入站分配量回归闸门（分配量比耗时稳定，跨机器可比）。
// 默认跳过（基准要跑数秒）；CI/本地性能检查：PERF_BUDGET=1 go test ./internal/server -run TestInboundAllocBudget
func TestInboundAllocBudget(t *testing.T) {
	if os.Getenv("PERF_BUDGET") == "" {
		t.Skip("set PERF_BUDGET=1 to enforce inbound allocation budgets")
	}
	cases := []struct {
		name  string
		bench func(*testing.B)
		max   uint64
	}{
		{"chat_small", BenchmarkInboundChatSmall, 140_000},
		{"chat_large", BenchmarkInboundChatLarge, 2_800_000},
		{"responses_small", BenchmarkInboundResponsesSmall, 170_000},
		{"responses_large", BenchmarkInboundResponsesLarge, 3_200_000},
	}
	for _, c := range cases {
		res := testing.Benchmark(c.bench)
		got := uint64(res.AllocedBytesPerOp())
		if got > c.max {
			t.Errorf("%s: alloc=%d B/op > budget %d（入站分配回退）", c.name, got, c.max)
		} else {
			t.Logf("%s: alloc=%d B/op (budget %d)", c.name, got, c.max)
		}
	}
}

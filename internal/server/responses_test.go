package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------- 测试辅助 ----------

type capturedChat struct {
	auth string
	body map[string]any
}

// newCapturingUpstream 返回固定 SSE 响应的假上游，并记录网关真正发出去的 chat 请求体
// （/v1/responses 的转换效果只能从出站请求上验证）。
func newCapturingUpstream(t *testing.T, sse string, status int) (*upstream.Client, *[]capturedChat) {
	t.Helper()
	captured := &[]capturedChat{}
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			*captured = append(*captured, capturedChat{auth: r.Header.Get("Authorization"), body: m})
			st := status
			if st == 0 {
				st = http.StatusOK
			}
			return &http.Response{
				StatusCode: st,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return client, captured
}

func responsesPool() *pool.Pool {
	return testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
}

// sseChunk 构造一帧 chat.completion.chunk。
func sseChunk(delta map[string]any, finish string, usage map[string]any) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish == "" {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = finish
	}
	obj := map[string]any{
		"id": "chatcmpl-t", "object": "chat.completion.chunk", "created": 1753600000,
		"model": "glm-5.2", "choices": []any{choice},
	}
	if usage != nil {
		obj["usage"] = usage
	}
	return obj
}

func sseBody(frames ...map[string]any) string {
	var b strings.Builder
	for _, f := range frames {
		raw, _ := json.Marshal(f)
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func toolCallsDelta(index int, id, name, args string, withFunction bool) []any {
	fn := map[string]any{"arguments": args}
	if withFunction {
		fn["name"] = name
	}
	tc := map[string]any{"index": index, "function": fn}
	if withFunction {
		tc["id"] = id
		tc["type"] = "function"
	}
	return []any{tc}
}

// parseResponsesSSE 解析适配层输出的 Responses SSE 事件序列。
func parseResponsesSSE(body string) []map[string]any {
	out := []map[string]any{}
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		payload := ""
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "data:") {
				payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, jStr(ev["type"]))
	}
	return out
}

func responsesOutputText(resp map[string]any) string {
	var b strings.Builder
	for _, raw := range jArr(resp["output"]) {
		item := jMap(raw)
		if item == nil || jStr(item["type"]) != "message" {
			continue
		}
		for _, p := range jArr(item["content"]) {
			if pm := jMap(p); pm != nil && jStr(pm["type"]) == "output_text" {
				b.WriteString(jStr(pm["text"]))
			}
		}
	}
	return b.String()
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func lastEvent(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no responses SSE events parsed")
	}
	return events[len(events)-1]
}

// ---------- 非流式端到端 ----------

func TestResponsesNonStreamEndToEnd(t *testing.T) {
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	body := `{"model":"cn:glm-5.2","instructions":"be brief","input":"hi","max_output_tokens":64,"store":true}`
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("object=%v status=%v", resp["object"], resp["status"])
	}
	if resp["model"] != "cn:glm-5.2" {
		t.Errorf("model echo=%v", resp["model"])
	}
	if v, ok := resp["max_output_tokens"].(float64); !ok || v != 64 {
		t.Errorf("max_output_tokens echo=%v", resp["max_output_tokens"])
	}
	out := jArr(resp["output"])
	if len(out) != 1 {
		t.Fatalf("output=%v", out)
	}
	item := jMap(out[0])
	if jStr(item["type"]) != "message" || jStr(item["status"]) != "completed" || jStr(item["role"]) != "assistant" {
		t.Errorf("message item=%v", item)
	}
	if got := responsesOutputText(resp); got != "你好" {
		t.Errorf("output text=%q", got)
	}
	usage := jMap(resp["usage"])
	if jStr(usage["input_tokens"]) == "" {
		// 数值比较走 jInt
	}
	if v, _ := jInt(usage["input_tokens"]); v != 1 {
		t.Errorf("usage.input_tokens=%v", usage["input_tokens"])
	}
	if v, _ := jInt(usage["output_tokens"]); v != 1 {
		t.Errorf("usage.output_tokens=%v", usage["output_tokens"])
	}
	if v, _ := jInt(usage["total_tokens"]); v != 2 {
		t.Errorf("usage.total_tokens=%v", usage["total_tokens"])
	}

	if len(*captured) != 1 {
		t.Fatalf("upstream calls=%d", len(*captured))
	}
	cb := (*captured)[0].body
	if cb["model"] != "glm-5.2" {
		t.Errorf("outbound model=%v (realm 前缀必须剥离)", cb["model"])
	}
	if v, _ := jInt(cb["max_tokens"]); v != 64 {
		t.Errorf("outbound max_tokens=%v", cb["max_tokens"])
	}
	msgs := jArr(cb["messages"])
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	if m0 := jMap(msgs[0]); jStr(m0["role"]) != "system" || jStr(m0["content"]) != "be brief" {
		t.Errorf("instructions -> system 失败: %v", msgs[0])
	}
	if m1 := jMap(msgs[1]); jStr(m1["role"]) != "user" || jStr(m1["content"]) != "hi" {
		t.Errorf("input string -> user 失败: %v", msgs[1])
	}
}

// ---------- 流式事件序列 ----------

func TestResponsesStreamEventSequence(t *testing.T) {
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("chat 帧泄漏到 Responses 流: %s", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta") {
		t.Errorf("缺少 response.output_text.delta: %s", body)
	}
	events := parseResponsesSSE(body)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Errorf("event sequence=%v want=%v", got, want)
	}
	for i, ev := range events {
		if v, _ := jInt(ev["sequence_number"]); int(v) != i {
			t.Errorf("event[%d] sequence_number=%v", i, ev["sequence_number"])
		}
	}
	final := jMap(lastEvent(t, events)["response"])
	if jStr(final["status"]) != "completed" {
		t.Errorf("final status=%v", final["status"])
	}
	if got := responsesOutputText(final); got != "你好" {
		t.Errorf("final output text=%q", got)
	}
	if v, _ := jInt(jMap(final["usage"])["total_tokens"]); v != 2 {
		t.Errorf("final usage=%v", final["usage"])
	}
	if len(*captured) != 1 {
		t.Fatalf("upstream calls=%d", len(*captured))
	}
	if (*captured)[0].body["stream"] != true {
		t.Errorf("outbound stream=%v", (*captured)[0].body["stream"])
	}
}

// TestResponsesStreamWithHeartbeatPrefix 上游 SSE 以注释/心跳行（": heartbeat"）开头时，
// 适配层必须仍按流式处理并产出完整事件序列。真机踩到过：只看 "data:" 前缀会把这种流
// 误判成本地聚合 JSON，收尾解析失败 → 502 unexpected upstream response shape。
func TestResponsesStreamWithHeartbeatPrefix(t *testing.T) {
	up, _ := newCapturingUpstream(t, ": heartbeat\n\n"+sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "unexpected upstream response shape") {
		t.Fatalf("心跳前缀被误判为聚合 JSON: %s", body)
	}
	events := parseResponsesSSE(body)
	types := eventTypes(events)
	want := []string{"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("event sequence=%v want=%v", types, want)
	}
	if got := responsesOutputText(jMap(lastEvent(t, events)["response"])); got != "你好" {
		t.Errorf("output text=%q", got)
	}
}

// ---------- 流式工具调用 ----------

func TestResponsesStreamToolCall(t *testing.T) {
	sse := sseBody(
		sseChunk(map[string]any{"role": "assistant", "tool_calls": toolCallsDelta(0, "call_abc", "get_weather", "", true)}, "", nil),
		sseChunk(map[string]any{"tool_calls": toolCallsDelta(0, "", "", `{"city":`, false)}, "", nil),
		sseChunk(map[string]any{"tool_calls": toolCallsDelta(0, "", "", `"sf"}`, false)}, "", nil),
		sseChunk(map[string]any{}, "tool_calls", map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}),
	)
	up, _ := newCapturingUpstream(t, sse, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := parseResponsesSSE(rec.Body.String())
	types := eventTypes(events)
	for _, want := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		if !containsStr(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	var done map[string]any
	for _, ev := range events {
		if jStr(ev["type"]) == "response.function_call_arguments.done" {
			done = ev
		}
	}
	if jStr(done["arguments"]) != `{"city":"sf"}` {
		t.Errorf("assembled arguments=%q", done["arguments"])
	}
	final := jMap(lastEvent(t, events)["response"])
	items := jArr(final["output"])
	if len(items) != 1 {
		t.Fatalf("output items=%v", items)
	}
	item := jMap(items[0])
	if jStr(item["type"]) != "function_call" || jStr(item["call_id"]) != "call_abc" ||
		jStr(item["name"]) != "get_weather" || jStr(item["arguments"]) != `{"city":"sf"}` {
		t.Errorf("function_call item=%v", item)
	}
	if v, _ := jInt(jMap(final["usage"])["input_tokens"]); v != 5 {
		t.Errorf("final usage=%v", final["usage"])
	}
}

// ---------- 流式 reasoning ----------

func TestResponsesStreamReasoning(t *testing.T) {
	sse := sseBody(
		sseChunk(map[string]any{"role": "assistant", "reasoning_content": "想一想"}, "", nil),
		sseChunk(map[string]any{"content": "答案"}, "", nil),
		sseChunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 2, "completion_tokens": 4, "total_tokens": 6}),
	)
	up, _ := newCapturingUpstream(t, sse, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"q","reasoning":{"effort":"high"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := parseResponsesSSE(rec.Body.String())
	types := eventTypes(events)
	for _, want := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done"} {
		if !containsStr(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	final := jMap(lastEvent(t, events)["response"])
	items := jArr(final["output"])
	if len(items) != 2 {
		t.Fatalf("output=%v (reasoning 在前、message 在后)", items)
	}
	if jStr(jMap(items[0])["type"]) != "reasoning" || jStr(jMap(items[1])["type"]) != "message" {
		t.Errorf("output order=%v", items)
	}
	summary := jArr(jMap(items[0])["summary"])
	if len(summary) != 1 || jStr(jMap(summary[0])["text"]) != "想一想" {
		t.Errorf("reasoning summary=%v", summary)
	}
	if got := responsesOutputText(final); got != "答案" {
		t.Errorf("message text=%q", got)
	}
}

// ---------- custom（自由格式）工具往返 ----------

func TestResponsesCustomToolRoundTrip(t *testing.T) {
	sse := sseBody(
		sseChunk(map[string]any{"role": "assistant", "tool_calls": toolCallsDelta(0, "call_c1", "shell", `{"input":"ls -la"}`, true)}, "", nil),
		sseChunk(map[string]any{}, "tool_calls", map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}),
	)
	up, captured := newCapturingUpstream(t, sse, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"ls","tools":[{"type":"custom","name":"shell","description":"run a command"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	items := jArr(resp["output"])
	if len(items) != 1 {
		t.Fatalf("output=%v", items)
	}
	item := jMap(items[0])
	if jStr(item["type"]) != "custom_tool_call" || jStr(item["input"]) != "ls -la" || jStr(item["call_id"]) != "call_c1" {
		t.Errorf("custom_tool_call item=%v", item)
	}
	// custom 工具声明 → chat function（参数固定 {"input": string}）
	tools := jArr((*captured)[0].body["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	fn := jMap(jMap(tools[0])["function"])
	props := jMap(jMap(fn["parameters"])["properties"])
	if jStr(jMap(props["input"])["type"]) != "string" {
		t.Errorf("custom tool parameters=%v", fn["parameters"])
	}

	// 回灌：客户端把 custom_tool_call + custom_tool_call_output 发回来
	replay := `{"model":"glm-5.2","tools":[{"type":"custom","name":"shell"}],"input":[` +
		`{"type":"custom_tool_call","call_id":"call_c1","name":"shell","input":"ls -la"},` +
		`{"type":"custom_tool_call_output","call_id":"call_c1","output":"file1 file2"}]}`
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(replay)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay code=%d body=%s", rec2.Code, rec2.Body)
	}
	msgs := jArr((*captured)[1].body["messages"])
	if len(msgs) != 2 {
		t.Fatalf("replay messages=%v", msgs)
	}
	tcs := jArr(jMap(msgs[0])["tool_calls"])
	if len(tcs) != 1 || jStr(jMap(jMap(tcs[0])["function"])["arguments"]) != `{"input":"ls -la"}` {
		t.Errorf("replay tool_calls=%v", tcs)
	}
	if tm := jMap(msgs[1]); jStr(tm["role"]) != "tool" || jStr(tm["tool_call_id"]) != "call_c1" || jStr(tm["content"]) != "file1 file2" {
		t.Errorf("replay tool message=%v", msgs[1])
	}
}

// ---------- previous_response_id 续接 ----------

func TestResponsesPreviousResponseID(t *testing.T) {
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("first code=%d body=%s", rec.Code, rec.Body)
	}
	var first map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	id := jStr(first["id"])
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("response id=%q", id)
	}

	second := `{"model":"glm-5.2","previous_response_id":"` + id + `","input":"again"}`
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(second)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	msgs := jArr((*captured)[1].body["messages"])
	if len(msgs) != 3 {
		t.Fatalf("续接 messages=%v (want 前一轮 user + assistant + 本轮 user)", msgs)
	}
	if m := jMap(msgs[0]); jStr(m["role"]) != "user" || jStr(m["content"]) != "hi" {
		t.Errorf("msgs[0]=%v", msgs[0])
	}
	if m := jMap(msgs[1]); jStr(m["role"]) != "assistant" || jStr(m["content"]) != "你好" {
		t.Errorf("msgs[1]=%v", msgs[1])
	}
	if m := jMap(msgs[2]); jStr(m["role"]) != "user" || jStr(m["content"]) != "again" {
		t.Errorf("msgs[2]=%v", msgs[2])
	}

	// 未知 id → 400（不静默丢上下文）
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","previous_response_id":"resp_unknown","input":"x"}`)))
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("unknown previous_response_id code=%d body=%s", rec3.Code, rec3.Body)
	}
}

// ---------- GET /v1/responses/{id} ----------

func TestResponsesGetStored(t *testing.T) {
	up, _ := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := jStr(created["id"])

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/responses/"+id, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", rec2.Code, rec2.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if jStr(got["id"]) != id || jStr(got["object"]) != "response" {
		t.Errorf("stored response=%v", got)
	}

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("GET", "/v1/responses/resp_missing", nil))
	if rec3.Code != http.StatusNotFound {
		t.Errorf("missing id code=%d", rec3.Code)
	}
}

// ---------- 错误透传 ----------

func TestResponsesErrorsPassthrough(t *testing.T) {
	up, _ := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (无可用账号应 503)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_healthy_account") || !strings.Contains(body, "error") {
		t.Errorf("错误对象应原样透传: %s", body)
	}
}

// ---------- 请求转换（纯函数） ----------

func TestResponsesRequestConversion(t *testing.T) {
	store := newResponsesStore()
	req := map[string]any{
		"model": "cn:glm-5.2",
		"instructions": []any{
			map[string]any{"type": "input_text", "text": "be terse"},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{
				map[string]any{"type": "input_text", "text": "sys2"},
				map[string]any{"type": "input_image", "image_url": "https://img.example/a.png"},
			}},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "thought"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "hit"},
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "function", "name": "extra", "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "web_search_call"},
		},
		"tools": []any{
			map[string]any{"type": "custom", "name": "shell", "description": "run"},
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
			map[string]any{"type": "web_search_preview"},
		},
		"tool_choice":       map[string]any{"type": "function", "name": "lookup"},
		"text":              map[string]any{"format": map[string]any{"type": "json_schema", "name": "out", "schema": map[string]any{"type": "object"}, "strict": true}},
		"reasoning":         map[string]any{"effort": "none"},
		"max_output_tokens": float64(256),
		"temperature":       0.3,
		"stream":            true,
		"background":        true,
	}
	chat, custom, dropped, err := responsesChatRequest(req, store)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if chat["model"] != "cn:glm-5.2" || chat["stream"] != true {
		t.Errorf("model/stream=%v/%v", chat["model"], chat["stream"])
	}
	if v, _ := jInt(chat["max_tokens"]); v != 256 {
		t.Errorf("max_tokens=%v", chat["max_tokens"])
	}
	if chat["reasoning_effort"] != "off" {
		t.Errorf("reasoning_effort=%v (none → off)", chat["reasoning_effort"])
	}
	if !custom["shell"] {
		t.Errorf("custom 工具未登记: %v", custom)
	}
	msgs := jArr(chat["messages"])
	if len(msgs) != 4 {
		t.Fatalf("messages=%v", msgs)
	}
	if m := jMap(msgs[0]); jStr(m["role"]) != "system" || jStr(m["content"]) != "be terse" {
		t.Errorf("msgs[0]=%v", msgs[0])
	}
	dev := jMap(msgs[1])
	if jStr(dev["role"]) != "system" {
		t.Errorf("developer → system 失败: %v", dev)
	}
	parts := jArr(dev["content"])
	if len(parts) != 2 || jStr(jMap(parts[0])["type"]) != "text" || jStr(jMap(parts[1])["type"]) != "image_url" {
		t.Errorf("content parts=%v", parts)
	}
	if url := jStr(jMap(jMap(parts[1])["image_url"])["url"]); url != "https://img.example/a.png" {
		t.Errorf("image url=%q", url)
	}
	asst := jMap(msgs[2])
	if jStr(asst["role"]) != "assistant" || jStr(asst["reasoning_content"]) != "thought" {
		t.Errorf("assistant=%v", asst)
	}
	tcs := jArr(asst["tool_calls"])
	if len(tcs) != 1 || jStr(jMap(tcs[0])["id"]) != "call_1" || jStr(jMap(jMap(tcs[0])["function"])["arguments"]) != `{"q":"x"}` {
		t.Errorf("tool_calls=%v", tcs)
	}
	if tm := jMap(msgs[3]); jStr(tm["role"]) != "tool" || jStr(tm["tool_call_id"]) != "call_1" || jStr(tm["content"]) != "hit" {
		t.Errorf("tool message=%v", msgs[3])
	}

	tools := jArr(chat["tools"])
	if len(tools) != 3 {
		t.Fatalf("tools=%v (custom + function + additional_tools)", tools)
	}
	if n := jStr(jMap(jMap(tools[0])["function"])["name"]); n != "shell" {
		t.Errorf("tools[0]=%v", tools[0])
	}
	if n := jStr(jMap(jMap(tools[2])["function"])["name"]); n != "extra" {
		t.Errorf("tools[2]=%v (additional_tools 应并入)", tools[2])
	}
	tc := jMap(chat["tool_choice"])
	if jStr(tc["type"]) != "function" || jStr(jMap(tc["function"])["name"]) != "lookup" {
		t.Errorf("tool_choice=%v", tc)
	}
	rf := jMap(chat["response_format"])
	if jStr(rf["type"]) != "json_schema" || jStr(jMap(rf["json_schema"])["name"]) != "out" {
		t.Errorf("response_format=%v", rf)
	}
	for _, want := range []string{"background", "input.web_search_call", "tools.web_search_preview"} {
		if !containsStr(dropped, want) {
			t.Errorf("dropped 缺少 %q: %v", want, dropped)
		}
	}

	// 字符串 input / 简单 tool_choice / text.format json_object
	chat2, _, _, err := responsesChatRequest(map[string]any{
		"model":       "glm-5.2",
		"input":       "plain",
		"tool_choice": "required",
		"text":        map[string]any{"format": map[string]any{"type": "json_object"}},
	}, store)
	if err != nil {
		t.Fatalf("convert2: %v", err)
	}
	if chat2["tool_choice"] != "required" {
		t.Errorf("tool_choice=%v", chat2["tool_choice"])
	}
	if jStr(jMap(chat2["response_format"])["type"]) != "json_object" {
		t.Errorf("response_format=%v", chat2["response_format"])
	}
	if m := jMap(jArr(chat2["messages"])[0]); jStr(m["content"]) != "plain" {
		t.Errorf("messages=%v", chat2["messages"])
	}
}

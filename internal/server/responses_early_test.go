package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
)

// TestResponsesEarlyCreatedNoAccount 提前开局后调度失败：HTTP 已 200，
// 必须发 response.failed 事件（而不是旧的 503 JSON）。
func TestResponsesEarlyCreatedNoAccount(t *testing.T) {
	up, _ := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (提前开局后只能是 200 + response.failed)", rec.Code, rec.Body)
	}
	events := parseResponsesSSE(rec.Body.String())
	got := eventTypes(events)
	want := []string{"response.created", "response.in_progress", "response.failed"}
	if len(got) != len(want) {
		t.Fatalf("events=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events=%v want=%v", got, want)
		}
	}
	final := jMap(lastEvent(t, events)["response"])
	if jStr(final["status"]) != "failed" {
		t.Errorf("status=%v", final["status"])
	}
	if m := jMap(jMap(final["error"])); m == nil || jStr(m["message"]) == "" {
		t.Errorf("error=%v want message", final["error"])
	}
}

// TestResponsesEarlyCreatedKillSwitch 关掉开关恢复旧行为：未开局 → 503 JSON。
func TestResponsesEarlyCreatedKillSwitch(t *testing.T) {
	up, _ := newCapturingUpstream(t, sseOK, 200)
	off := false
	h := NewHandler(Config{Pool: pool.New(""), Upstream: up, ResponsesEarlyCreated: &off})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (关掉提前开局应保持旧 503)", rec.Code, rec.Body)
	}
}

// TestResponsesEarlyCreatedInvalidJSON 开局前失败仍走 400（不提前写头）。
func TestResponsesEarlyCreatedInvalidJSON(t *testing.T) {
	up, _ := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("invalid JSON 不应提前开流: content-type=%q", ct)
	}
}

// TestRequestProtocol 归档协议标记：两条入口区分 chat / responses。
func TestRequestProtocol(t *testing.T) {
	if got := requestProtocol("/v1/chat/completions"); got != "chat" {
		t.Errorf("chat protocol=%q", got)
	}
	if got := requestProtocol("/v1/responses"); got != "responses" {
		t.Errorf("responses protocol=%q", got)
	}
}

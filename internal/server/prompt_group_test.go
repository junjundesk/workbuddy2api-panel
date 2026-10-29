package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/livecfg"
)

// 分组专属系统提示词：取值链（分组覆盖 > 全局）、作用域（跟随密钥而非账号）、
// 以及"分组覆盖走 custom 语义因此不参与内容拦截降级"的行为。

const (
	testGroupPrompt = "GROUP-SYS：本组专属提示词"
	testClientSys   = "CLIENT-SYS：客户端原始系统提示词"
)

// promptTestStore 建一个分组并写入提示词（enabled=enable）。
func promptTestStore(t *testing.T, enable bool, text string) (*groups.Store, string) {
	t.Helper()
	gs, err := groups.Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.SetPrompt(g.ID, enable, text); err != nil {
		t.Fatal(err)
	}
	return gs, g.ID
}

// promptScopedRequest 带作用域发起一次 chat / responses 请求（绕过 withAuth，
// 与 scopedTestRequest 同风格，但允许自定义 body 以携带客户端 system）。
func promptScopedRequest(t *testing.T, h *Handler, path string, scope authScope, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withScope(httptest.NewRequest("POST", path, bytes.NewReader([]byte(body))), scope)
	rec := httptest.NewRecorder()
	if path == "/v1/responses" {
		h.responses(rec, req)
	} else {
		h.chatCompletions(rec, req)
	}
	return rec
}

// capturedSystem 返回捕获到的出站请求里第一条 system 消息的 content（没有则空）。
func capturedSystem(t *testing.T, c capturedChat) string {
	t.Helper()
	msgs, ok := c.body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("captured body has no messages: %v", c.body)
	}
	first, ok := msgs[0].(map[string]any)
	if !ok || first["role"] != "system" {
		t.Fatalf("first outbound message is not system: %v", msgs[0])
	}
	s, _ := first["content"].(string)
	return s
}

func capturedRaw(t *testing.T, c capturedChat) string {
	t.Helper()
	raw, err := json.Marshal(c.body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// promptChatBody / promptResponsesBody 两条入口的请求体（同一个客户端 system）。
const (
	promptChatBody      = `{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT-SYS：客户端原始系统提示词"},{"role":"system","content":"SECOND-SYS"},{"role":"user","content":"hi"}]}`
	promptResponsesBody = `{"model":"glm-5.2","stream":true,"instructions":"CLIENT-SYS：客户端原始系统提示词","input":"hi"}`
)

// TestGroupPromptReplacesSystem 分组开启提示词后，两条入口的出站 system 都被替换为
// 分组文本、客户端原始 system 全部消失；且分组账号仍能正常被选中。
func TestGroupPromptReplacesSystem(t *testing.T) {
	cases := []struct{ path, body string }{
		{"/v1/chat/completions", promptChatBody},
		{"/v1/responses", promptResponsesBody},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			gs, gid := promptTestStore(t, true, testGroupPrompt)
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			p.SetGroups(map[string]string{"u1": gid})
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

			rec := promptScopedRequest(t, h, tc.path, authScope{GroupID: gid}, tc.body)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if len(*captured) != 1 {
				t.Fatalf("want 1 upstream call, got %d", len(*captured))
			}
			if got := capturedSystem(t, (*captured)[0]); got != testGroupPrompt {
				t.Fatalf("outbound system = %q, want group prompt %q", got, testGroupPrompt)
			}
			if raw := capturedRaw(t, (*captured)[0]); strings.Contains(raw, "CLIENT-SYS") || strings.Contains(raw, "SECOND-SYS") {
				t.Fatalf("client system leaked into outbound body: %s", raw)
			}
		})
	}
}

// TestGroupPromptNotAppliedToPublicScope 主密钥（公共池作用域）不受分组提示词影响。
func TestGroupPromptNotAppliedToPublicScope(t *testing.T) {
	gs, _ := promptTestStore(t, true, testGroupPrompt)
	p := testPoolWith(&auth.Auth{UID: "pub", AccessToken: "pub", ExpiresAt: 9999999999})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{Main: true}, promptChatBody)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw := capturedRaw(t, (*captured)[0])
	if !strings.Contains(raw, "CLIENT-SYS") {
		t.Fatalf("passthrough should keep client system for main key: %s", raw)
	}
	if strings.Contains(raw, testGroupPrompt) {
		t.Fatalf("group prompt leaked into public-scope request: %s", raw)
	}
}

// TestGroupPromptDisabledInheritsGlobal 分组存在但未开启提示词 → 沿用全局 custom 文本。
func TestGroupPromptDisabledInheritsGlobal(t *testing.T) {
	gs, gid := promptTestStore(t, false, "")
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetGroups(map[string]string{"u1": gid})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	const globalSys = "GLOBAL-SYS"
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "custom", PromptText: globalSys})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{GroupID: gid}, promptChatBody)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := capturedSystem(t, (*captured)[0]); got != globalSys {
		t.Fatalf("outbound system = %q, want global %q", got, globalSys)
	}
}

// TestGroupPromptSkipsDegrade 分组覆盖按 custom 语义执行：上游内容拦截直接 400
// content_blocked，不触发降级重试、不罚账号（与全局 custom 行为一致）。
func TestGroupPromptSkipsDegrade(t *testing.T) {
	gs, gid := promptTestStore(t, true, testGroupPrompt)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetGroups(map[string]string{"u1": gid})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{GroupID: gid}, promptChatBody)
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked, body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Fatalf("error.code=%q want content_blocked", env.Error.Code)
	}
	if h.degrade.Active() {
		t.Fatal("group override must not trigger degrade")
	}
}

// TestGlobalPromptHotConfigOverridesStatic 面板保存后的热配置优先于启动期静态字段：
// prompt.mode / prompt.file 改动无需重启即时生效。
func TestGlobalPromptHotConfigOverridesStatic(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "pub", AccessToken: "pub", ExpiresAt: 9999999999})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	const hotSys = "HOT-SYS"
	live := livecfg.New(livecfg.Snapshot{APIKey: "k", PromptMode: "custom", PromptText: hotSys})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Live: live})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{Main: true}, promptChatBody)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := capturedSystem(t, (*captured)[0]); got != hotSys {
		t.Fatalf("outbound system = %q, want hot config %q", got, hotSys)
	}
}

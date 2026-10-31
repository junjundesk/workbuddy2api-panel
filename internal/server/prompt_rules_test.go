package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

// 分组提示词规则（多条 system/user + 关键词替换）：作用域、两条入口的一致性、
// 关键词替换的先后顺序、以及与内容拦截降级的互斥关系。

// promptRulesStore 建一个分组并写入规则（enabled=enable）。
func promptRulesStore(t *testing.T, enable bool, rs prompt.Rules) (*groups.Store, string) {
	t.Helper()
	gs, err := groups.Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.SetPromptRules(g.ID, enable, rs); err != nil {
		t.Fatal(err)
	}
	return gs, g.ID
}

// outboundRoleTexts 把出站 messages 解成 "role:text" 序列，便于整体断言顺序与内容。
func outboundRoleTexts(t *testing.T, c capturedChat) []string {
	t.Helper()
	msgs, ok := c.body["messages"].([]any)
	if !ok {
		t.Fatalf("outbound body has no messages: %v", c.body)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message not map: %v", m)
		}
		role, _ := mm["role"].(string)
		text, _ := mm["content"].(string)
		out = append(out, role+":"+text)
	}
	return out
}

// TestGroupPromptRulesBothEntries 两条入口（chat / responses）都按规则改写：
// system 整体替换为多条拼接、user 提示词前置注入、请求里的关键词被替换。
func TestGroupPromptRulesBothEntries(t *testing.T) {
	rules := prompt.Rules{
		Entries: []prompt.Entry{
			{Role: prompt.RoleSystem, Text: "SYS-1"},
			{Role: prompt.RoleSystem, Text: "SYS-2"},
			{Role: prompt.RoleUser, Text: "USR-1"},
		},
		Replaces: []prompt.Replace{{Match: "旧词", With: "新词"}},
	}
	chatBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT-SYS 旧词"},{"role":"user","content":"hi 旧词"}]}`
	respBody := `{"model":"glm-5.2","stream":true,"instructions":"CLIENT-SYS 旧词","input":"hi 旧词"}`
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", chatBody},
		{"/v1/responses", respBody},
	} {
		t.Run(tc.path, func(t *testing.T) {
			gs, gid := promptRulesStore(t, true, rules)
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			p.SetGroups(map[string]string{"u1": gid})
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

			rec := promptScopedRequest(t, h, tc.path, authScope{GroupID: gid}, tc.body)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			got := outboundRoleTexts(t, (*captured)[0])
			want := []string{"system:SYS-1\n\nSYS-2", "user:USR-1", "user:hi 新词"}
			if len(got) != len(want) {
				t.Fatalf("messages = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("messages[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
				}
			}
		})
	}
}

// TestGroupPromptRulesReplaceOnlyKeepsSystem 只配关键词替换时不动 system 结构，
// 客户端 system 原文保留、其中的关键词同样被替换。
func TestGroupPromptRulesReplaceOnlyKeepsSystem(t *testing.T) {
	gs, gid := promptRulesStore(t, true, prompt.Rules{
		Replaces: []prompt.Replace{{Match: "旧词", With: "新词"}},
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetGroups(map[string]string{"u1": gid})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{GroupID: gid},
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"SYS 旧词"},{"role":"user","content":"hi 旧词"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	got := outboundRoleTexts(t, (*captured)[0])
	if len(got) != 2 || got[0] != "system:SYS 新词" || got[1] != "user:hi 新词" {
		t.Fatalf("messages = %v", got)
	}
}

// TestGroupPromptRulesScopeIsKeyScoped 主密钥（公共池作用域）不受分组规则影响。
func TestGroupPromptRulesScopeIsKeyScoped(t *testing.T) {
	gs, _ := promptRulesStore(t, true, prompt.Rules{
		Entries:  []prompt.Entry{{Role: prompt.RoleSystem, Text: "GROUP-SYS"}},
		Replaces: []prompt.Replace{{Match: "旧词", With: "新词"}},
	})
	p := testPoolWith(&auth.Auth{UID: "pub", AccessToken: "pub", ExpiresAt: 9999999999})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "passthrough"})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{Main: true},
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT-SYS 旧词"},{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw := capturedRaw(t, (*captured)[0])
	if !strings.Contains(raw, "CLIENT-SYS 旧词") {
		t.Fatalf("main-scope request should pass through untouched: %s", raw)
	}
	if strings.Contains(raw, "GROUP-SYS") {
		t.Fatalf("group rules leaked into main-scope request: %s", raw)
	}
}

// TestGroupPromptRulesDisabledInheritsGlobal 分组规则关闭时沿用全局 prompt.mode（custom 文本）。
func TestGroupPromptRulesDisabledInheritsGlobal(t *testing.T) {
	gs, gid := promptRulesStore(t, false, prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleSystem, Text: "GROUP-SYS"}}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetGroups(map[string]string{"u1": gid})
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptMode: "custom", PromptText: "GLOBAL-SYS"})

	rec := promptScopedRequest(t, h, "/v1/chat/completions", authScope{GroupID: gid},
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT-SYS"},{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := capturedSystem(t, (*captured)[0]); got != "GLOBAL-SYS" {
		t.Fatalf("outbound system = %q, want global", got)
	}
}

// TestGroupPromptRulesSkipDegrade 规则存在时不进内容拦截降级：上游 400 直接透传
// content_blocked，不换中性提示词重试（规则是运营者显式配置的文本）。
func TestGroupPromptRulesSkipDegrade(t *testing.T) {
	gs, gid := promptRulesStore(t, true, prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleUser, Text: "USR"}}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetGroups(map[string]string{"u1": gid})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11-128,"msg":"blocked by security policy"}`, false
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
		t.Fatal("group rules must not trigger degrade")
	}
}

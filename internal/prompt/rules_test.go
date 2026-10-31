package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeMessages 把改写结果解成 messages 切片，便于按角色断言。
func decodeMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	raw, _ := obj["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message not map: %v", m)
		}
		out = append(out, mm)
	}
	return out
}

func msgText(t *testing.T, m map[string]any) string {
	t.Helper()
	s, _ := m["content"].(string)
	return s
}

// TestApplyRulesSystemAndUser 覆盖主路径：关键词替换先做，系统提示词整体替换，
// 用户提示词按配置顺序插到 system 块之后、客户端首条 user 之前。
func TestApplyRulesSystemAndUser(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"CLIENT-SYS 旧词"},
			{"role":"developer","content":"DEV-SYS"},
			{"role":"user","content":"用户说：旧词"},
			{"role":"assistant","content":"助手回复"}
		]
	}`)
	rs := Rules{
		Entries: []Entry{
			{Role: RoleSystem, Text: "SYS-A"},
			{Role: RoleUser, Text: "USER-1"},
			{Role: RoleSystem, Text: "SYS-B"},
			{Role: RoleUser, Text: "USER-2"},
		},
		Replaces: []Replace{{Match: "旧词", With: "新词"}},
	}
	out := Apply(in, rs)
	msgs := decodeMessages(t, out)
	if len(msgs) != 5 {
		t.Fatalf("len=%d want 5: %s", len(msgs), out)
	}
	wantRoles := []string{"system", "user", "user", "user", "assistant"}
	for i, r := range wantRoles {
		if msgs[i]["role"] != r {
			t.Fatalf("roles[%d]=%v want %s (%s)", i, msgs[i]["role"], r, out)
		}
	}
	if got := msgText(t, msgs[0]); got != "SYS-A\n\nSYS-B" {
		t.Errorf("system = %q, want joined entries", got)
	}
	if got := msgText(t, msgs[1]); got != "USER-1" {
		t.Errorf("user[1] = %q", got)
	}
	if got := msgText(t, msgs[2]); got != "USER-2" {
		t.Errorf("user[2] = %q", got)
	}
	if got := msgText(t, msgs[3]); got != "用户说：新词" {
		t.Errorf("client user not replaced: %q", got)
	}
	raw := string(out)
	if strings.Contains(raw, "CLIENT-SYS") || strings.Contains(raw, "DEV-SYS") {
		t.Errorf("client system leaked: %s", raw)
	}
}

// TestApplyRulesUserOnlyKeepsClientSystem 只配置 user 条目时不动客户端 system，
// 用户提示词插到开头 system 块之后。
func TestApplyRulesUserOnlyKeepsClientSystem(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"CLIENT-SYS"},{"role":"user","content":"hi"}]}`)
	out := Apply(in, Rules{Entries: []Entry{{Role: RoleUser, Text: "PREFIX"}}})
	msgs := decodeMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("len=%d want 3: %s", len(msgs), out)
	}
	if msgText(t, msgs[0]) != "CLIENT-SYS" {
		t.Errorf("client system should be kept: %v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgText(t, msgs[1]) != "PREFIX" {
		t.Errorf("injected user mismatch: %v", msgs[1])
	}
	if msgText(t, msgs[2]) != "hi" {
		t.Errorf("client user moved/changed: %v", msgs[2])
	}
}

// TestApplyRulesReplaceScope 验证作用范围：system 规则只改 system/developer，
// user 规则只改 user，With 为空即删除关键词。
func TestApplyRulesReplaceScope(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"K-SYS"},
		{"role":"developer","content":"K-DEV"},
		{"role":"user","content":"K-USER"},
		{"role":"assistant","content":"K-ASSIST"}
	]}`)
	rs := Rules{Replaces: []Replace{{Match: "K-", With: "X-", Scope: "system"}}}
	out := Apply(in, rs)
	msgs := decodeMessages(t, out)
	if got := msgText(t, msgs[0]); got != "X-SYS" {
		t.Errorf("system = %q", got)
	}
	if got := msgText(t, msgs[1]); got != "X-DEV" {
		t.Errorf("developer = %q", got)
	}
	if got := msgText(t, msgs[2]); got != "K-USER" {
		t.Errorf("user should not be replaced with system scope: %q", got)
	}
	if got := msgText(t, msgs[3]); got != "K-ASSIST" {
		t.Errorf("assistant should not be replaced with system scope: %q", got)
	}

	rs = Rules{Replaces: []Replace{{Match: "K-", With: "", Scope: "user"}}}
	out = Apply(in, rs)
	msgs = decodeMessages(t, out)
	if got := msgText(t, msgs[2]); got != "USER" {
		t.Errorf("empty With should delete keyword: %q", got)
	}
	if got := msgText(t, msgs[0]); got != "K-SYS" {
		t.Errorf("system changed by user scope: %q", got)
	}
}

// TestApplyRulesMultimodalTextParts 只改 text part，image part 原样保留。
func TestApplyRulesMultimodalTextParts(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"看看 K"},
		{"type":"image_url","image_url":{"url":"data:..."}}
	]}]}`)
	out := Apply(in, Rules{Replaces: []Replace{{Match: "K", With: "V"}}})
	msgs := decodeMessages(t, out)
	arr, ok := msgs[0]["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("content changed: %v", msgs[0]["content"])
	}
	part := arr[0].(map[string]any)
	if part["text"] != "看看 V" {
		t.Errorf("text part = %v", part["text"])
	}
	img := arr[1].(map[string]any)
	if img["image_url"] == nil {
		t.Errorf("image part lost: %v", img)
	}
}

// TestApplyRulesNoMessages 无 messages 字段时：有系统条目 → 建一条 system；
// 只有 user 条目 → 只建 user 消息。
func TestApplyRulesNoMessages(t *testing.T) {
	out := Apply([]byte(`{"model":"m"}`), Rules{Entries: []Entry{{Role: RoleSystem, Text: "S"}}})
	msgs := decodeMessages(t, out)
	if len(msgs) != 1 || msgs[0]["role"] != "system" || msgText(t, msgs[0]) != "S" {
		t.Fatalf("system-only rules on empty messages: %s", out)
	}
	out = Apply([]byte(`{"model":"m"}`), Rules{Entries: []Entry{{Role: RoleUser, Text: "U"}}})
	msgs = decodeMessages(t, out)
	if len(msgs) != 1 || msgs[0]["role"] != "user" || msgText(t, msgs[0]) != "U" {
		t.Fatalf("user-only rules on empty messages: %s", out)
	}
}

// TestApplyRulesGuards 空规则 / 空 body / 坏 JSON / 空条目一律原样返回。
func TestApplyRulesGuards(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	if out := Apply(in, Rules{}); string(out) != string(in) {
		t.Errorf("empty rules should not touch body: %s", out)
	}
	if out := Apply(in, Rules{Entries: []Entry{{Role: RoleSystem, Text: "   "}}}); string(out) != string(in) {
		t.Errorf("blank entries should not touch body: %s", out)
	}
	if out := Apply(nil, Rules{Entries: []Entry{{Role: RoleSystem, Text: "S"}}}); len(out) != 0 {
		t.Errorf("nil body: %s", out)
	}
	bad := []byte("{not json")
	if out := Apply(bad, Rules{Entries: []Entry{{Role: RoleSystem, Text: "S"}}}); string(out) != string(bad) {
		t.Errorf("invalid json: %s", out)
	}
	if ApplyObject(nil, Rules{Entries: []Entry{{Role: RoleSystem, Text: "S"}}}) {
		t.Error("nil obj should report no change")
	}
}

// TestRulesNormalize 归一：developer→system、未知角色丢弃、空文本丢弃、scope 缺省 all。
func TestRulesNormalize(t *testing.T) {
	rs := Rules{
		Entries: []Entry{
			{Role: "developer", Text: "D"},
			{Role: "", Text: "EMPTY-ROLE"},
			{Role: "tool", Text: "TOOL"},
			{Role: "user", Text: "  U  "},
			{Role: "system", Text: "   "},
		},
		Replaces: []Replace{
			{Match: "", With: "x"},
			{Match: "K", With: "V"},
			{Match: "K2", With: "V2", Scope: "bogus"},
		},
	}
	got := rs.Normalize()
	if len(got.Entries) != 3 {
		t.Fatalf("entries=%+v", got.Entries)
	}
	if got.Entries[0].Role != RoleSystem || got.Entries[0].Text != "D" {
		t.Errorf("developer not normalized: %+v", got.Entries[0])
	}
	if got.Entries[1].Text != "EMPTY-ROLE" || got.Entries[2].Text != "U" {
		t.Errorf("entry normalization mismatch: %+v", got.Entries)
	}
	if len(got.Replaces) != 2 || got.Replaces[0].Scope != "all" || got.Replaces[1].Scope != "all" {
		t.Errorf("replaces=%+v", got.Replaces)
	}
	if !(Rules{}).Empty() || got.Empty() {
		t.Error("Empty() mismatch")
	}
	if got.SystemText() != "D\n\nEMPTY-ROLE" {
		t.Errorf("SystemText=%q", got.SystemText())
	}
}

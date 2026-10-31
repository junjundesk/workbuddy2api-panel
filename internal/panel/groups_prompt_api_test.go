package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

// promptBody 组装分组提示词保存请求体（避免在测试里手写转义）。
func promptBody(enabled bool, text string) string {
	b, _ := json.Marshal(map[string]any{"enabled": enabled, "text": text})
	return string(b)
}

// TestGroupPromptApiRoundTrip 保存 → 回读 → 列表元数据 → 关闭保留正文。
func TestGroupPromptApiRoundTrip(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	longText := strings.Repeat("长", 80) // 超过预览上限，用于验证列表不回全文

	code, out, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptBody(true, longText))
	if code != 200 {
		t.Fatalf("save code=%d raw=%s", code, raw)
	}
	grp, _ := out["group"].(map[string]any)
	if grp == nil || grp["prompt_enabled"] != true {
		t.Fatalf("saved group view missing prompt_enabled: %s", raw)
	}

	code, out, raw = call(t, p, "GET", "/panel/api/groups/"+g.ID+"/prompt", "")
	if code != 200 {
		t.Fatalf("get code=%d raw=%s", code, raw)
	}
	if out["enabled"] != true || out["text"] != longText {
		t.Fatalf("get mismatch: %s", raw)
	}
	if out["chars"] != float64(80) {
		t.Fatalf("chars = %v, want 80", out["chars"])
	}

	// 列表：只回元数据（开关 / 字数 / 预览），不回全文。
	_, out, raw = call(t, p, "GET", "/panel/api/groups", "")
	if strings.Contains(raw, longText) {
		t.Fatal("group list leaks full prompt text")
	}
	list, _ := out["groups"].([]any)
	if len(list) != 1 {
		t.Fatalf("groups len=%d", len(list))
	}
	item, _ := list[0].(map[string]any)
	if item["prompt_enabled"] != true || item["prompt_chars"] != float64(80) {
		t.Fatalf("list metadata mismatch: %v", item)
	}
	preview, _ := item["prompt_preview"].(string)
	if n := len([]rune(preview)); n == 0 || n > 61 {
		t.Fatalf("preview rune len=%d (%q)", n, preview)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Fatalf("truncated preview should end with ellipsis: %q", preview)
	}

	// 关闭开关：正文保留、不再生效（再次开启无需重填）。
	code, _, raw = call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptBody(false, longText))
	if code != 200 {
		t.Fatalf("disable code=%d raw=%s", code, raw)
	}
	_, out, _ = call(t, p, "GET", "/panel/api/groups/"+g.ID+"/prompt", "")
	if out["enabled"] != false || out["text"] != longText {
		t.Fatalf("disable should keep text but clear enabled: %v", out)
	}
	if _, ok := gs.Prompt(g.ID); ok {
		t.Fatal("disabled prompt should not be effective")
	}
}

// TestGroupPromptApiValidation 空内容 / 超长 / 未知分组 / 关闭清空。
func TestGroupPromptApiValidation(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptBody(true, "   ")); code != 400 {
		t.Fatalf("blank text with enabled should 400: code=%d raw=%s", code, raw)
	}
	huge := strings.Repeat("x", groups.MaxPromptBytes+1)
	if code, _, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptBody(true, huge)); code != 400 {
		t.Fatalf("oversized text should 400: code=%d raw=%s", code, raw)
	}
	if code, _, _ := call(t, p, "PUT", "/panel/api/groups/g_nope/prompt", promptBody(true, "x")); code != 404 {
		t.Fatalf("unknown group code=%d", code)
	}
	if code, _, _ := call(t, p, "GET", "/panel/api/groups/g_nope/prompt", ""); code != 404 {
		t.Fatalf("get unknown group code=%d", code)
	}
	if code, _, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptBody(false, "")); code != 200 {
		t.Fatalf("disable with empty text should 200: code=%d raw=%s", code, raw)
	}
}

// promptRulesBody 组装新版规则保存请求体（entries + replaces）。
func promptRulesBody(enabled bool, entries []prompt.Entry, replaces []prompt.Replace) string {
	b, _ := json.Marshal(map[string]any{"enabled": enabled, "entries": entries, "replaces": replaces})
	return string(b)
}

// TestGroupPromptRulesApiRoundTrip 多条提示词 + 关键词替换：保存 → 回读 → 列表元数据
// → 关闭保留规则。
func TestGroupPromptRulesApiRoundTrip(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	longSys := strings.Repeat("长", 80) // 超过预览上限：列表不得回全文
	entries := []prompt.Entry{
		{Role: prompt.RoleSystem, Text: longSys},
		{Role: prompt.RoleUser, Text: "USR-1"},
		{Role: prompt.RoleSystem, Text: "SYS-2"},
	}
	replaces := []prompt.Replace{{Match: "旧词", With: "新词"}}

	code, out, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptRulesBody(true, entries, replaces))
	if code != 200 {
		t.Fatalf("save code=%d raw=%s", code, raw)
	}
	grp, _ := out["group"].(map[string]any)
	if grp == nil || grp["prompt_enabled"] != true || grp["prompt_entries"] != float64(3) || grp["prompt_replaces"] != float64(1) {
		t.Fatalf("saved group view metadata mismatch: %s", raw)
	}

	code, out, raw = call(t, p, "GET", "/panel/api/groups/"+g.ID+"/prompt", "")
	if code != 200 {
		t.Fatalf("get code=%d raw=%s", code, raw)
	}
	gotEntries, _ := out["entries"].([]any)
	gotReplaces, _ := out["replaces"].([]any)
	if len(gotEntries) != 3 || len(gotReplaces) != 1 {
		t.Fatalf("get rules mismatch: %s", raw)
	}
	first, _ := gotEntries[0].(map[string]any)
	if first["role"] != "system" || first["text"] != longSys {
		t.Fatalf("entry[0] mismatch: %v", first)
	}
	wantJoined := longSys + "\n\nSYS-2"
	if out["text"] != wantJoined {
		t.Fatalf("joined system text = %q", out["text"])
	}

	// 列表只回条数与预览，不回正文。
	_, _, raw = call(t, p, "GET", "/panel/api/groups", "")
	if strings.Contains(raw, longSys) || strings.Contains(raw, "USR-1") {
		t.Fatal("group list leaks rule text")
	}

	// 关闭开关：规则保留、不再生效。
	code, _, raw = call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptRulesBody(false, entries, replaces))
	if code != 200 {
		t.Fatalf("disable code=%d raw=%s", code, raw)
	}
	_, out, _ = call(t, p, "GET", "/panel/api/groups/"+g.ID+"/prompt", "")
	if out["enabled"] != false || len(out["entries"].([]any)) != 3 {
		t.Fatalf("disable should keep rules: %v", out)
	}
	if _, ok := gs.Rules(g.ID); ok {
		t.Fatal("disabled rules should not be effective")
	}
}

// TestGroupPromptRulesApiValidation 开启但空规则必须 400；未知分组 404；
// 只配关键词替换也算有效规则。
func TestGroupPromptRulesApiValidation(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, _, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptRulesBody(true, nil, nil)); code != 400 {
		t.Fatalf("enabled empty rules should 400: code=%d raw=%s", code, raw)
	}
	if code, _, _ := call(t, p, "PUT", "/panel/api/groups/g_nope/prompt", promptRulesBody(true, []prompt.Entry{{Role: "system", Text: "x"}}, nil)); code != 404 {
		t.Fatalf("unknown group code=%d", code)
	}
	if code, _, raw := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/prompt", promptRulesBody(true, nil, []prompt.Replace{{Match: "a", With: "b"}})); code != 200 {
		t.Fatalf("replace-only rules should save: code=%d raw=%s", code, raw)
	}
}

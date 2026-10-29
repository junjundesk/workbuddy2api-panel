package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
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

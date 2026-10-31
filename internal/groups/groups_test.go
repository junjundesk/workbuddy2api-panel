package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

func TestStoreCreateResolveRotateAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, key, err := s.Create("客户A", "#5b7cfa", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || key == "" {
		t.Fatalf("empty group/key: %+v %q", g, key)
	}
	got, ok := s.ResolveKey(key)
	if !ok || got.ID != g.ID {
		t.Fatalf("resolve key failed: %+v %v", got, ok)
	}
	if _, ok := s.ResolveKey("sk-grp-invalid"); ok {
		t.Fatal("invalid key resolved")
	}
	if err := s.AddMember(g.ID, "uid-1"); err != nil {
		t.Fatal(err)
	}
	if s.GroupOf("uid-1") != g.ID {
		t.Fatal("member not mapped")
	}
	newKey, err := s.RotateKey(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ResolveKey(key); ok {
		t.Fatal("old key still valid after rotate")
	}
	if _, ok := s.ResolveKey(newKey); !ok {
		t.Fatal("new key invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("groups.json empty")
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get(g.ID); !ok || got.Name != "客户A" || len(got.Members) != 1 {
		t.Fatalf("reload mismatch: %+v %v", got, ok)
	}
	if _, ok := s2.ResolveKey(newKey); !ok {
		t.Fatal("reloaded key invalid")
	}
}

func TestStoreMemberUniqueAndProxyValidation(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g1, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	g2, _, err := s.Create("B", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(g1.ID, "uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(g2.ID, "uid-1"); err != nil {
		t.Fatal(err)
	}
	if s.GroupOf("uid-1") != g2.ID {
		t.Fatal("uid should move to latest group")
	}
	if len(s.Membership()) != 1 {
		t.Fatal("uid duplicated across groups")
	}
	if err := s.SetProxy(g1.ID, &Proxy{Type: "bad", Host: "127.0.0.1", Port: 1}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if err := s.SetProxy(g1.ID, &Proxy{Type: "socks5h", Host: "127.0.0.1", Port: 1080}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(g1.ID)
	if got.Proxy == nil || got.Proxy.Type != "socks5h" {
		t.Fatal("proxy not stored")
	}
	if _, err := s.Delete(g2.ID); err != nil {
		t.Fatal(err)
	}
	if s.GroupOf("uid-1") != "" {
		t.Fatal("member should be gone with group")
	}
}

// TestSetPromptPersistsAndReloads 分组提示词落盘后重新加载仍在（实时生效的前提：
// 面板保存即写盘，进程重启后配置不丢）。
func TestSetPromptPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	const text = "你是分组 A 的专属助手。\n只回答数据问题。"
	if err := s.SetPrompt(g.ID, true, text); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Prompt(g.ID); !ok || got != text {
		t.Fatalf("prompt after save = %q,%v", got, ok)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Prompt(g.ID); !ok || got != text {
		t.Fatalf("prompt after reload = %q,%v", got, ok)
	}
	got, ok := s2.Get(g.ID)
	if !ok || !got.PromptEnabled || got.PromptText != text {
		t.Fatalf("reloaded group prompt mismatch: %+v %v", got, ok)
	}
}

// TestSetPromptValidation 空内容/超长/未知分组必须报错，且报错不改变已存值。
func TestSetPromptValidation(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrompt(g.ID, true, "   \n\t "); err == nil {
		t.Fatal("enabled with blank text should fail")
	}
	if err := s.SetPrompt(g.ID, true, strings.Repeat("x", MaxPromptBytes+1)); err == nil {
		t.Fatal("oversized prompt should fail")
	}
	if err := s.SetPrompt("g_nope", true, "x"); !os.IsNotExist(err) {
		t.Fatalf("unknown group err = %v, want os.ErrNotExist", err)
	}
	// 失败不改变已存值。
	if err := s.SetPrompt(g.ID, true, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrompt(g.ID, true, ""); err == nil {
		t.Fatal("blank text should fail")
	}
	if got, ok := s.Prompt(g.ID); !ok || got != "ok" {
		t.Fatalf("failed write mutated prompt: %q,%v", got, ok)
	}
}

// TestSetPromptDisableKeepsText 关闭开关后不再生效，但正文保留（再次开启无需重填）。
func TestSetPromptDisableKeepsText(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrompt(g.ID, true, "P"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrompt(g.ID, false, "P"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Prompt(g.ID); ok {
		t.Fatal("disabled prompt should not be effective")
	}
	got, _ := s.Get(g.ID)
	if got.PromptText != "P" {
		t.Fatalf("disabled prompt text lost: %q", got.PromptText)
	}
	if _, ok := s.Prompt(""); ok {
		t.Fatal("empty id should not resolve")
	}
}

// TestSetPromptRulesPersistAndReload 多条提示词 + 关键词替换落盘后重新加载仍在，
// 且旧版 PromptText 不被新写入污染（两套字段不并存，避免口径漂移）。
func TestSetPromptRulesPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	rs := prompt.Rules{
		Entries: []prompt.Entry{
			{Role: prompt.RoleSystem, Text: "SYS-1"},
			{Role: prompt.RoleUser, Text: "USR-1"},
			{Role: prompt.RoleSystem, Text: "SYS-2"},
		},
		Replaces: []prompt.Replace{{Match: "旧词", With: "新词"}, {Match: "删掉", With: ""}},
	}
	if err := s.SetPromptRules(g.ID, true, rs); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Rules(g.ID)
	if !ok || len(got.Entries) != 3 || len(got.Replaces) != 2 {
		t.Fatalf("rules after save = %+v,%v", got, ok)
	}
	if text, ok := s.Prompt(g.ID); !ok || text != "SYS-1\n\nSYS-2" {
		t.Fatalf("system prompt = %q,%v", text, ok)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got2, ok := s2.Rules(g.ID)
	if !ok || len(got2.Entries) != 3 || len(got2.Replaces) != 2 {
		t.Fatalf("rules after reload = %+v,%v", got2, ok)
	}
	g2, _ := s2.Get(g.ID)
	if len(g2.PromptEntries) != 3 || len(g2.PromptReplaces) != 2 {
		t.Fatalf("reloaded fields = %+v", g2)
	}
	if g2.PromptText != "" {
		t.Fatalf("legacy prompt_text should stay empty on rules write, got %q", g2.PromptText)
	}
}

// TestSetPromptRulesValidation 空规则开启 / 超量 / 超长 / 未知分组必须报错，且不改变已存值。
func TestSetPromptRulesValidation(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPromptRules(g.ID, true, prompt.Rules{}); err == nil {
		t.Fatal("enabled with empty rules should fail")
	}
	ok := prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleSystem, Text: "OK"}}}
	if err := s.SetPromptRules(g.ID, true, ok); err != nil {
		t.Fatal(err)
	}
	tooMany := prompt.Rules{}
	for i := 0; i <= MaxPromptEntries; i++ {
		tooMany.Entries = append(tooMany.Entries, prompt.Entry{Role: prompt.RoleSystem, Text: "x"})
	}
	if err := s.SetPromptRules(g.ID, true, tooMany); err == nil {
		t.Fatal("too many entries should fail")
	}
	longMatch := prompt.Rules{Replaces: []prompt.Replace{{Match: strings.Repeat("k", MaxReplaceMatchBytes+1)}}}
	if err := s.SetPromptRules(g.ID, true, longMatch); err == nil {
		t.Fatal("oversized keyword should fail")
	}
	huge := prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleSystem, Text: strings.Repeat("x", MaxPromptBytes+1)}}}
	if err := s.SetPromptRules(g.ID, true, huge); err == nil {
		t.Fatal("oversized rules should fail")
	}
	if err := s.SetPromptRules("g_nope", true, ok); !os.IsNotExist(err) {
		t.Fatalf("unknown group err = %v, want os.ErrNotExist", err)
	}
	if got, okk := s.Rules(g.ID); !okk || len(got.Entries) != 1 || got.Entries[0].Text != "OK" {
		t.Fatalf("failed writes must not mutate stored rules: %+v,%v", got, okk)
	}
}

// TestRulesLegacyPromptTextFallback 旧版只有 PromptText 的分组，Rules 自动补成
// 一条 system 条目（面板与请求侧口径一致）。
func TestRulesLegacyPromptTextFallback(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrompt(g.ID, true, "LEGACY"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Rules(g.ID)
	if !ok || len(got.Entries) != 1 || got.Entries[0].Role != prompt.RoleSystem || got.Entries[0].Text != "LEGACY" {
		t.Fatalf("legacy fallback = %+v,%v", got, ok)
	}
	if text, ok := s.Prompt(g.ID); !ok || text != "LEGACY" {
		t.Fatalf("legacy Prompt = %q,%v", text, ok)
	}
}

// TestSetPromptRulesDisableKeepsRules 关闭开关后不再生效，但规则保留（再次开启无需重填）。
func TestSetPromptRulesDisableKeepsRules(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	rs := prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleUser, Text: "U"}}}
	if err := s.SetPromptRules(g.ID, true, rs); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPromptRules(g.ID, false, rs); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Rules(g.ID); ok {
		t.Fatal("disabled rules should not be effective")
	}
	got, _ := s.Get(g.ID)
	if len(got.PromptEntries) != 1 || got.PromptEnabled {
		t.Fatalf("disabled rules should be kept but inactive: %+v", got)
	}
}

package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/prompt"
)

// configuredGroup 建一个"配置满配"的分组：代理带账号密码 + 一条 system 提示词 +
// 一条关键词替换 + 共享池开启。
func configuredGroup(t *testing.T, p *Panel, gs *groups.Store, name string) groups.Group {
	t.Helper()
	g, _, err := gs.Create(name, "#5b7cfa", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.SetProxy(g.ID, &groups.Proxy{Type: "socks5h", Host: "127.0.0.1", Port: 1080, Username: "ops", Password: "p@ss"}); err != nil {
		t.Fatal(err)
	}
	if err := gs.SetPromptRules(g.ID, true, prompt.Rules{
		Entries:  []prompt.Entry{{Role: prompt.RoleSystem, Text: "你是买买号助手"}},
		Replaces: []prompt.Replace{{Match: "内部代号", With: "公开名"}},
	}); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGroupConfigExport(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g := configuredGroup(t, p, gs, "A")

	// no-store：配置导出带代理密码，不能被任何中间层缓存。
	req := httptest.NewRequest("GET", "/panel/api/groups/"+g.ID+"/config", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control=%q，配置含密码必须 no-store", cc)
	}
	var f groupConfigFile
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if f.Format != groupConfigFormat || f.Version != groupConfigVersion || f.ExportedAt == 0 {
		t.Fatalf("信封=%+v", f)
	}
	if f.Group == nil || f.Group.Proxy == nil || f.Group.Proxy.Password != "p@ss" {
		t.Fatalf("导出应带代理密码（搬配置到别的实例要用）：%+v", f.Group)
	}
	if f.Group.Name != "A" || !f.Group.SharedPool || f.Group.CallPublic {
		t.Fatalf("分组字段=%+v", f.Group)
	}
	if !f.Group.Prompt.Enabled || len(f.Group.Prompt.Entries) != 1 || f.Group.Prompt.Entries[0].Text != "你是买买号助手" || len(f.Group.Prompt.Replaces) != 1 {
		t.Fatalf("提示词=%+v", f.Group.Prompt)
	}
	// 分组列表仍不能泄露密码（导出是唯一显式出口）。
	if _, _, raw := call(t, p, "GET", "/panel/api/groups", ""); strings.Contains(raw, "p@ss") {
		t.Fatal("分组列表泄露代理密码")
	}
	if code, _, _ := call(t, p, "GET", "/panel/api/groups/g_nope/config", ""); code != 404 {
		t.Fatalf("未知分组 code=%d", code)
	}
}

func TestGroupConfigImportRoundTrip(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	src := configuredGroup(t, p, gs, "A")
	_, _, raw := call(t, p, "GET", "/panel/api/groups/"+src.ID+"/config", "")

	dst, dstKey, err := gs.Create("B", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	code, out, body := call(t, p, "PUT", "/panel/api/groups/"+dst.ID+"/config", raw)
	if code != 200 || out["ok"] != true {
		t.Fatalf("导入 code=%d body=%s", code, body)
	}
	got, ok := gs.Get(dst.ID)
	if !ok {
		t.Fatal("分组丢失")
	}
	if got.Proxy == nil || got.Proxy.Password != "p@ss" || got.Proxy.Type != "socks5h" || got.Proxy.Username != "ops" {
		t.Fatalf("代理未导入：%+v", got.Proxy)
	}
	if !got.SharedPool || got.CallPublic {
		t.Fatalf("共享开关未导入：shared=%v call=%v", got.SharedPool, got.CallPublic)
	}
	if !got.PromptEnabled || len(got.PromptEntries) != 1 || len(got.PromptReplaces) != 1 || got.PromptText != "" {
		t.Fatalf("提示词未导入：%+v", got)
	}
	// 名称/密钥/成员属于目标分组的身份，导入不能改。
	if got.Name != "B" || got.Key != dstKey {
		t.Fatalf("导入改了身份字段：name=%q key 变了吗=%v", got.Name, got.Key != dstKey)
	}

	// proxy:null = 直连（清空目标分组现有代理）。
	code, _, body = call(t, p, "PUT", "/panel/api/groups/"+dst.ID+"/config", `{"format":"workbuddy-group-config","version":1,"group":{"proxy":null,"prompt":{"enabled":false,"entries":[{"role":"system","text":"保留但不生效"}],"replaces":[]}}}`)
	if code != 200 {
		t.Fatalf("直连导入 code=%d body=%s", code, body)
	}
	got, _ = gs.Get(dst.ID)
	if got.Proxy != nil {
		t.Fatalf("proxy:null 应清空代理，实际 %+v", got.Proxy)
	}
	if got.PromptEnabled || len(got.PromptEntries) != 1 || got.PromptText != "" {
		t.Fatalf("关闭提示词应保留规则且不写旧字段：%+v", got)
	}
}

func TestGroupConfigImportRejects(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, _, _ := gs.Create("B", "", false, false)
	if err := gs.SetProxy(g.ID, &groups.Proxy{Type: "http", Host: "10.0.0.1", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := gs.SetPromptRules(g.ID, true, prompt.Rules{Entries: []prompt.Entry{{Role: prompt.RoleSystem, Text: "原配置"}}}); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, body string }{
		{"format 不符", `{"format":"workbuddy-accounts","version":1,"group":{}}`},
		{"版本过高", `{"format":"workbuddy-group-config","version":2,"group":{}}`},
		{"缺 group", `{"format":"workbuddy-group-config","version":1}`},
		{"代理类型非法", `{"group":{"proxy":{"type":"ftp","host":"h","port":21}}}`},
		{"代理端口非法", `{"group":{"proxy":{"type":"http","host":"h","port":70000}}}`},
		{"开启但规则为空", `{"group":{"shared_pool":true,"prompt":{"enabled":true,"entries":[]}}}`},
	}
	for _, c := range cases {
		code, _, body := call(t, p, "PUT", "/panel/api/groups/"+g.ID+"/config", c.body)
		if code != 400 {
			t.Errorf("%s：code=%d body=%s（应 400）", c.name, code, body)
		}
		got, _ := gs.Get(g.ID)
		// 校验失败必须整份不落地：代理、开关、提示词都还是导入前那份。
		if got.Proxy == nil || got.Proxy.Host != "10.0.0.1" || got.SharedPool || !got.PromptEnabled || len(got.PromptEntries) != 1 || got.PromptEntries[0].Text != "原配置" {
			t.Errorf("%s：失败后状态被改动 %+v", c.name, got)
		}
	}
	if code, _, _ := call(t, p, "PUT", "/panel/api/groups/g_nope/config", `{"group":{}}`); code != 404 {
		t.Fatalf("未知分组 code=%d", code)
	}
}

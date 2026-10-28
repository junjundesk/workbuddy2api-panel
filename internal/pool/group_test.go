package pool

import (
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
)

func addTestAccount(p *Pool, uid string) {
	p.Add(&auth.Auth{UID: uid, AccessToken: "t", RefreshToken: "r", Nickname: uid})
}

func TestScopedPickPublicAndGroup(t *testing.T) {
	p := New("")
	addTestAccount(p, "public")
	addTestAccount(p, "g1")
	addTestAccount(p, "g2")
	p.SetGroups(map[string]string{"g1": "g_one", "g2": "g_two"})
	p.SetGroupMeta(map[string]GroupMeta{
		"g_one": {Shared: false},
		"g_two": {Shared: true},
	})
	// 公共池口径：未分组 + shared 分组，不含独立分组 g1。
	if a := p.PickExcludingForRealmScoped(nil, "", "", "", false); a == nil || a.UID == "g1" {
		t.Fatalf("public pick=%v, must not include independent group g1", a)
	}
	// 本组专属（call_public=false）：40 次抽签只可能拿到 g1。
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", false)
		if a == nil {
			t.Fatal("group pick nil")
		}
		seen[a.UID] = true
	}
	if len(seen) != 1 || !seen["g1"] {
		t.Fatalf("isolated group candidates=%v, want only g1", seen)
	}
	// 共用候选集（call_public=true）：候选集层面直接断言并集——本组 g1 与公共池
	// （未分组 public + shared 分组 g2）同时在列，不是"本组优先、无号再回源"。
	union := p.AvailableUIDsForRealmScoped("", "g_one", true)
	want := map[string]bool{"g1": true, "public": true, "g2": true}
	if len(union) != len(want) {
		t.Fatalf("call_public 候选集=%v，want 本组∪公共池（%d 个）", union, len(want))
	}
	for _, u := range union {
		if !want[u] {
			t.Fatalf("call_public 候选集出现非预期账号 %q（%v）", u, union)
		}
	}
	// 抽签层面同样共用：本组号健康时，公共池号也会被选中（旧语义下只会是 g1）。
	p.SetCredits("public", 100000, 0)
	p.SetCredits("g1", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", true); a == nil || a.UID == "g1" {
		t.Fatalf("call_public 应让公共池号参与同一权重池，got %v", a)
	}
	// 关掉开关 → 候选集只剩本组。
	if got := p.AvailableUIDsForRealmScoped("", "g_one", false); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("call_public=false 候选集=%v，want [g1]", got)
	}
	// 本组号不可用（禁用）后，共用候选集由公共池顶上。
	p.Disable("g1", "test")
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", true); a == nil || a.UID == "g1" {
		t.Fatalf("call_public 应能由公共池顶上，got %v", a)
	}
}

func TestGroupStatus(t *testing.T) {
	p := New("")
	addTestAccount(p, "g1")
	p.SetGroups(map[string]string{"g1": "g_one"})
	st, ok := p.Status("g1")
	if !ok || st.Group != "g_one" {
		t.Fatalf("status group=%q ok=%v", st.Group, ok)
	}
	if p.GroupOf("g1") != "g_one" {
		t.Fatal("GroupOf mismatch")
	}
}

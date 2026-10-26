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
	if a := p.PickExcludingForRealmScoped(nil, "", "", "", false); a == nil || a.UID == "g1" {
		t.Fatalf("public pick=%v, must not include independent group g1", a)
	}
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", false); a == nil || a.UID != "g1" {
		t.Fatalf("group pick=%v, want g1", a)
	}
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", true); a == nil || a.UID != "g1" {
		t.Fatalf("group call_public should prefer own account, got %v", a)
	}
	p.Disable("g1", "test")
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", true); a == nil || a.UID == "g1" {
		t.Fatalf("call_public fallback got=%v, want public/shared account", a)
	}
	if a := p.PickExcludingForRealmScoped(nil, "", "", "g_one", false); a != nil && a.UID == "g1" {
		t.Fatal("isolated group must not pick another group account")
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

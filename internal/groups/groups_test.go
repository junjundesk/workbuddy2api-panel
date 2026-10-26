package groups

import (
	"os"
	"path/filepath"
	"testing"
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

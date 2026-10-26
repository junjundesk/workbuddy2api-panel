package groups

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRevealKeyPersistAndRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, key, err := s.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.RevealKey(g.ID); !ok || got != key {
		t.Fatalf("reveal=%q,%v want %q", got, ok, key)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.RevealKey(g.ID); !ok || got != key {
		t.Fatalf("reloaded reveal=%q,%v", got, ok)
	}
	nk, err := s2.RotateKey(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.RevealKey(g.ID); got != nk {
		t.Fatalf("after rotate reveal=%q want %q", got, nk)
	}
	if _, ok := s2.ResolveKey(nk); !ok {
		t.Fatal("rotated key should resolve")
	}
}

func TestRevealKeyLegacyHashOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	raw := `{"version":1,"groups":[{"id":"g_old","name":"old","key_hash":"abc","members":[]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RevealKey("g_old"); ok {
		t.Fatal("legacy group should not reveal")
	}
}

func TestPublicProxyPersistAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	s, _ := Load(path)
	if s.PublicProxy() != nil {
		t.Fatal("default should be direct")
	}
	if err := s.SetPublicProxy(&Proxy{Type: "bad", Host: "h", Port: 1}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if err := s.SetPublicProxy(&Proxy{Type: "SOCKS5H", Host: " 127.0.0.1 ", Port: 1080, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := Load(path)
	px := s2.PublicProxy()
	if px == nil || px.Type != "socks5h" || px.Host != "127.0.0.1" || px.Password != "pw" {
		t.Fatalf("reloaded public proxy=%+v", px)
	}
	if err := s2.SetPublicProxy(nil); err != nil {
		t.Fatal(err)
	}
	s3, _ := Load(path)
	if s3.PublicProxy() != nil {
		t.Fatal("public proxy should be cleared")
	}
}

package upstream

import "testing"

func TestClientForPublicPoolProxy(t *testing.T) {
	c := New()
	var px *ProxyConfig
	c.SetGroupProxyFunc(func(group string) *ProxyConfig {
		if group == "" {
			return px
		}
		return nil
	})
	if c.clientFor("", false) != c.HTTP {
		t.Fatal("public pool without proxy should use default client")
	}
	px = &ProxyConfig{Type: "http", Host: "127.0.0.1", Port: 8080}
	c.InvalidateGroup("")
	got := c.clientFor("", false)
	if got == c.HTTP {
		t.Fatal("public pool proxy not applied")
	}
	if c.clientFor("", false) != got {
		t.Fatal("public proxy client should be cached")
	}
	px = nil
	c.InvalidateGroup("")
	if c.clientFor("", false) != c.HTTP {
		t.Fatal("cleared public proxy should fall back to direct")
	}
}

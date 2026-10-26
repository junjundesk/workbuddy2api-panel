package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSocks5DialerHandshakeAndRemoteDNS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotHost := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var greet [2]byte
		if _, err := io.ReadFull(conn, greet[:]); err != nil {
			return
		}
		methods := make([]byte, int(greet[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			return
		}
		var head [4]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		var host string
		switch head[3] {
		case 0x03:
			var l [1]byte
			if _, err := io.ReadFull(conn, l[:]); err != nil {
				return
			}
			b := make([]byte, int(l[0]))
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = string(b)
		case 0x01:
			b := make([]byte, 4)
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			host = net.IP(b).String()
		default:
			return
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		gotHost <- host
		// success reply with 0.0.0.0:0
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		buf := make([]byte, 1)
		_, _ = io.ReadFull(conn, buf)
		_, _ = conn.Write(buf)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dial := socks5DialContext(ProxyConfig{Type: "socks5h", Host: "127.0.0.1", Port: port}, false)
	conn, err := dial(context.Background(), "tcp", "example.com:80")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if host := <-gotHost; host != "example.com" {
		t.Fatalf("remote DNS host=%q, want example.com", host)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("echo=%q err=%v", b[:], err)
	}
}

func TestNewTransportWithProxyRejectsInvalid(t *testing.T) {
	if _, err := newTransportWithProxy(newTransport(), ProxyConfig{Type: "ftp", Host: "127.0.0.1", Port: 1}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	tr, err := newTransportWithProxy(newTransport(), ProxyConfig{Type: "http", Host: "127.0.0.1", Port: 8080})
	if err != nil || tr.Proxy == nil {
		t.Fatalf("http proxy transport: tr=%v err=%v", tr, err)
	}
}

func TestHTTPProxyTransport(t *testing.T) {
	var seen bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	port := u.Port()
	var n int
	_, _ = fmt.Sscanf(port, "%d", &n)
	tr, err := newTransportWithProxy(newTransport(), ProxyConfig{Type: "http", Host: u.Hostname(), Port: n})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get("http://example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !seen || resp.StatusCode != 200 {
		t.Fatalf("proxy not used: seen=%v status=%d", seen, resp.StatusCode)
	}
}

// proxy.go 分组出站代理：HTTP / SOCKS5（本地 DNS）/ SOCKS5H（远程 DNS）。
//
// 每个分组独立 Transport + HTTP/ChatHTTP client，互不复用连接池；配置变更时由
// Client.InvalidateGroup 关闭空闲连接并重建。代理不可用时 fail-closed：返回错误，
// 绝不回落到服务器直连。
package upstream

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxyConfig 分组代理配置。Type: http / socks5 / socks5h。
type ProxyConfig struct {
	Type     string
	Host     string
	Port     int
	Username string
	Password string
}

func (p ProxyConfig) addr() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.Port)) }

func (p ProxyConfig) validate() error {
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "http", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy type must be http/socks5/socks5h")
	}
	if strings.TrimSpace(p.Host) == "" {
		return fmt.Errorf("proxy host is empty")
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("proxy port invalid")
	}
	return nil
}

// newTransportWithProxy 在现有加固 Transport 基础上叠加代理。
// 返回的 Transport 仍保留禁 h2 / TLS 握手超时 / keepalive / ResponseHeaderTimeout。
func newTransportWithProxy(base *http.Transport, p ProxyConfig) (*http.Transport, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	tr := base.Clone()
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "http":
		u := &url.URL{Scheme: "http", Host: p.addr()}
		if p.Username != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		}
		tr.Proxy = http.ProxyURL(u)
		tr.DialContext = newDialer().DialContext
	case "socks5", "socks5h":
		tr.Proxy = nil
		localDNS := strings.EqualFold(p.Type, "socks5")
		tr.DialContext = socks5DialContext(p, localDNS)
	default:
		return nil, fmt.Errorf("unsupported proxy type %q", p.Type)
	}
	return tr, nil
}

// socks5DialContext 返回一个 DialContext：先连代理，再做 RFC1928 握手。
// localDNS=true 时先在本地解析目标域名，把 IP 交给代理（socks5 语义）；
// false 时把域名交给代理（socks5h 语义）。
func socks5DialContext(p ProxyConfig, localDNS bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("socks5: unsupported network %s", network)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("socks5: target addr %q: %w", addr, err)
		}
		targetHost := host
		if localDNS && net.ParseIP(host) == nil {
			ips, err := net.DefaultResolver.LookupHost(ctx, host)
			if err != nil || len(ips) == 0 {
				if err == nil {
					err = fmt.Errorf("no address")
				}
				return nil, fmt.Errorf("socks5 local dns %s: %w", host, err)
			}
			targetHost = ips[0]
		}
		dialer := newDialer()
		conn, err := dialer.DialContext(ctx, "tcp", p.addr())
		if err != nil {
			return nil, fmt.Errorf("socks5 connect proxy %s: %w", p.addr(), err)
		}
		deadline := time.Now().Add(10 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetDeadline(deadline)
		if err := socks5Handshake(conn, targetHost, port, p.Username, p.Password); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

func socks5Handshake(conn net.Conn, host, port, username, password string) error {
	methods := []byte{0x00}
	if username != "" || password != "" {
		methods = append(methods, 0x02)
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return err
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return err
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5: bad version %d", resp[0])
	}
	switch resp[1] {
	case 0x00:
	case 0x02:
		if len(username) > 255 || len(password) > 255 {
			return fmt.Errorf("socks5: credentials too long")
		}
		auth := []byte{0x01, byte(len(username))}
		auth = append(auth, username...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		var ar [2]byte
		if _, err := io.ReadFull(conn, ar[:]); err != nil {
			return err
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("socks5: auth rejected")
		}
	case 0xFF:
		return fmt.Errorf("socks5: no acceptable auth method")
	default:
		return fmt.Errorf("socks5: proxy chose unsupported method %d", resp[1])
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("socks5: bad target port %q", port)
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: target host too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(portNum))
	req = append(req, pb[:]...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 0x05 || head[1] != 0x00 {
		return fmt.Errorf("socks5: connect rejected (rep=%d)", head[1])
	}
	switch head[3] {
	case 0x01:
		var b [4]byte
		_, err = io.ReadFull(conn, b[:])
	case 0x04:
		var b [16]byte
		_, err = io.ReadFull(conn, b[:])
	case 0x03:
		var l [1]byte
		if _, err = io.ReadFull(conn, l[:]); err == nil {
			var b = make([]byte, int(l[0]))
			_, err = io.ReadFull(conn, b)
		}
	default:
		err = fmt.Errorf("socks5: unknown address type %d", head[3])
	}
	if err != nil {
		return err
	}
	var bindPort [2]byte
	_, err = io.ReadFull(conn, bindPort[:])
	return err
}

// NewProxyHTTPClient 构造一个单次测试用 client（代理 + 超时）。
func NewProxyHTTPClient(p ProxyConfig, timeout time.Duration) (*http.Client, error) {
	tr, err := newTransportWithProxy(newTransport(), p)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: timeout, Transport: http.RoundTripper(tr)}, nil
}

// TestProxy 用分组代理访问指定 URL，返回耗时与响应状态。仅供面板“测试连接”使用，
// target 由调用方固定为上游白名单地址，禁止用户自由填写造成 SSRF。
func TestProxy(ctx context.Context, p ProxyConfig, target string, timeout time.Duration) (time.Duration, int, error) {
	client, err := NewProxyHTTPClient(p, timeout)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return time.Since(start), resp.StatusCode, nil
}

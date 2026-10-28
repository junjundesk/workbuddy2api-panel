package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ipip.net 的真实响应形态都必须解析成 IP + 归属地；网页、空响应、纯报错页必须报错，
// 不能把页面里的文字（例如认证页里的 10.0.0.1）当成出口 IP 显示到面板上。
func TestParseExitInfo(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		ip      string
		region  string
		wantErr bool
	}{
		{"ipip 标准响应", "当前 IP：111.196.65.234  来自于：中国 北京 北京  联通\n", "111.196.65.234", "中国 北京 北京 联通", false},
		{"归属地为运营商域名", "当前 IP：111.196.65.234  来自于：中国 北京 北京  chinaunicom.com\n", "111.196.65.234", "中国 北京 北京 chinaunicom.com", false},
		{"纯 IP 响应", "111.196.65.234\n", "111.196.65.234", "", false},
		{"IPv6 出口", "当前 IP：2001:db8::1  来自于：日本 东京\n", "2001:db8::1", "日本 东京", false},
		{"代理认证页", "<html><body>请先登录代理 10.0.0.1</body></html>", "", "", true},
		{"网页片段", "<!DOCTYPE html><p>502 Bad Gateway</p>", "", "", true},
		{"空响应", "  \n\t ", "", "", true},
		{"无 IP 的拦截页", "Blocked by firewall", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := parseExitInfo(c.body)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.IP != c.ip || info.Region != c.region {
				t.Fatalf("got ip=%q region=%q, want ip=%q region=%q", info.IP, info.Region, c.ip, c.region)
			}
		})
	}
}

// 响应体被截断/夹带控制字符时，提示文本必须是单行短串（不把多行网页塞进面板与日志）。
func TestParseExitInfoErrorTextIsOneLine(t *testing.T) {
	_, err := parseExitInfo("line1\r\nline2\x00" + strings.Repeat("x", 200))
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\x00") {
		t.Fatalf("error text not single line: %q", msg)
	}
	if len([]rune(msg)) > 100 {
		t.Fatalf("error text too long (%d runes): %q", len([]rune(msg)), msg)
	}
}

func TestFetchExitInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("探测请求缺少 User-Agent（部分出口会直接 403）")
		}
		_, _ = w.Write([]byte("当前 IP：203.0.113.9  来自于：中国 上海 上海  电信\n"))
	}))
	defer srv.Close()

	info, err := fetchExitInfo(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if info.IP != "203.0.113.9" || info.Region != "中国 上海 上海 电信" {
		t.Fatalf("got %+v", info)
	}
}

func TestFetchExitInfoRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()

	if _, err := fetchExitInfo(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("502 必须报错")
	}
}

// 探测走的是与聊天同一个代理出口：用本地 HTTP 代理验证请求确实经过代理链路，
// 同时验证超时/取消不会把 handler 卡住。
func TestProbeExitIPViaHTTPProxy(t *testing.T) {
	// 探测目标固定为 https://myip.ipip.net/，这里只验证「代理不可达时快速报错」。
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer ln.Close()
	u, err := url.Parse(ln.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := ProbeExitIP(context.Background(), ProxyConfig{Type: "http", Host: u.Hostname(), Port: port}, 5*time.Second); err == nil {
		t.Fatal("被代理拒绝时必须报错")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("探测耗时 %s，超时未生效", d)
	}
}

func TestProbeExitIPRejectsInvalidConfig(t *testing.T) {
	if _, err := ProbeExitIP(context.Background(), ProxyConfig{Type: "ftp", Host: "127.0.0.1", Port: 1}, time.Second); err == nil {
		t.Fatal("非法代理类型必须报错")
	}
}

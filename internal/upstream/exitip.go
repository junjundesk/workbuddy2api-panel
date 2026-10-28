// exitip.go 代理出口 IP 探测。
//
// 面板「测试连接」除了回答"代理通不通"，还要回答"这条代理实际从哪个 IP 出去"：
// 代理只对部分域名生效、链式代理、或规则写错仍然直连时，光看 HTTP 200 是看不出来的。
// 探测地址固定为 myip.ipip.net（响应形如「当前 IP：1.2.3.4  来自于：中国 北京 北京  联通」），
// 不接受调用方传参，避免面板测试接口变成 SSRF 跳板。
package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// ExitIPURL 出口 IP 探测地址。固定值，不做成可配置项：一旦可配，面板接口就成了 SSRF。
const ExitIPURL = "https://myip.ipip.net/"

// exitIPMaxBody 单次读取上限。正常响应不足 100 字节，读到这个量必然是网页或错误页。
const exitIPMaxBody = 4096

// ExitInfo 出口探测结果。Region 为空表示上游没给归属地（境外出口常见）。
type ExitInfo struct {
	IP     string
	Region string
}

// ProbeExitIP 经代理访问 ExitIPURL，返回出口 IP 与归属地。
func ProbeExitIP(ctx context.Context, p ProxyConfig, timeout time.Duration) (ExitInfo, error) {
	client, err := NewProxyHTTPClient(p, timeout)
	if err != nil {
		return ExitInfo{}, err
	}
	return fetchExitInfo(ctx, client, ExitIPURL)
}

// fetchExitInfo 用给定 client 取一次探测页并解析。单独抽出来是为了让 httptest
// 能覆盖状态码与异常响应，不必真的连外网。
func fetchExitInfo(ctx context.Context, client *http.Client, target string) (ExitInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ExitInfo{}, err
	}
	// 少数出口对空 UA 直接 403，带一个常见 UA 最稳。
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := client.Do(req)
	if err != nil {
		return ExitInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, exitIPMaxBody))
	if err != nil {
		return ExitInfo{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ExitInfo{}, fmt.Errorf("出口 IP 接口返回 HTTP %d", resp.StatusCode)
	}
	return parseExitInfo(string(body))
}

// parseExitInfo 解析 myip.ipip.net 的响应，兼容两种形态：
//
//	当前 IP：1.2.3.4  来自于：中国 北京 北京  联通
//	1.2.3.4
//
// 认不出的响应（代理认证页、网关错误页、CDN 拦截页）一律报错：把网页文字当成 IP
// 显示到面板上，比什么都不显示更误导人。
func parseExitInfo(body string) (ExitInfo, error) {
	text := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if text == "" {
		return ExitInfo{}, fmt.Errorf("出口 IP 接口返回空响应")
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<body") {
		return ExitInfo{}, fmt.Errorf("代理返回的是网页不是 IP：可能是认证页、拦截页或错误页")
	}
	if ip := net.ParseIP(text); ip != nil {
		return ExitInfo{IP: ip.String()}, nil
	}
	var region string
	if i := strings.Index(text, "来自于"); i >= 0 {
		region = oneLine(strings.TrimLeft(text[i+len("来自于"):], " \t:："), 64)
	}
	rest := text
	if i := strings.Index(strings.ToUpper(rest), "IP"); i >= 0 {
		rest = rest[i+len("IP"):]
	}
	fields := strings.Fields(strings.TrimLeft(rest, " \t\n:："))
	if len(fields) > 0 {
		if ip := net.ParseIP(strings.Trim(fields[0], ",，;；。")); ip != nil {
			return ExitInfo{IP: ip.String(), Region: region}, nil
		}
	}
	return ExitInfo{}, fmt.Errorf("出口 IP 接口响应无法识别：%s", oneLine(text, 60))
}

// oneLine 把响应片段压成单行短串：控制字符/连续空白归一为单个空格，限长，
// 避免把多行网页原样塞进日志或面板提示。
func oneLine(s string, maxRunes int) string {
	var b strings.Builder
	gap := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			gap = b.Len() > 0
			continue
		}
		if gap {
			b.WriteRune(' ')
			gap = false
		}
		b.WriteRune(r)
	}
	out := strings.TrimRight(b.String(), "。.，,；;！! ")
	if rs := []rune(out); len(rs) > maxRunes {
		out = string(rs[:maxRunes]) + "…"
	}
	return out
}

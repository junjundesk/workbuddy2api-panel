// model_fallback.go 模型目录端点「主域不可达 → 备用域」兜底。
//
// 背景：模型目录只有三条家族路径（/v3/config、/console/enterprises/personal/models、
// /v2/enterprises/personal/models）。整域不可达（DNS/连接被挡、域名被墙、上游 5xx）时
// 就没有任何模型——这是"上游不可达"最常见的真实形态，而非端点被下线。
//
// 依据（2026-09-27 用真实账号实测：同一套 token 在备用域返回**逐 ID 一致**的目录）：
//   - 国际：www.workbuddy.ai ≡ www.codebuddy.ai（并集 26 个 ID，两边互无差异；网关侧
//     输出 27 条含 default-model）；
//   - 国内：copilot.tencent.com ≡ www.codebuddy.cn（models 30 / agents.cli 16，集合全等）。
//
// 备用域仍属于同一账号的上游服务，数据由上游下发——与「严格上游口径」不冲突
// （本项目不引入任何本地/内置数据）。
//
// 作用域**仅限模型目录 GET**：chat / billing / report / 任务等路径一律不动（未实测的
// 跨主机语义不冒险）。仅当主域「连接层失败 / 5xx / 404（该域无此路径）」才切换；
// 401/403 不切换——那是凭证或权限问题，换域不会变好，只会掩盖真实错误。
//
// Origin/Referer 无需改写：headers.go 的 originRefererFor 按 realm 固定
// （CN→https://www.codebuddy.cn，global→https://www.workbuddy.ai），与请求主机无关；
// 实例：global base 覆盖为 codebuddy.ai 的实测中，Origin 仍是 workbuddy.ai，上游照常返回
// 完整目录（200）。
package upstream

import (
	"io"
	"log"
	"net/http"
	"strings"
)

// modelHostFallback 模型目录主域 → 备用域（同族服务入口，hostname 精确匹配）。
var modelHostFallback = map[string]string{
	"www.workbuddy.ai":    "www.codebuddy.ai", // 国际版
	"copilot.tencent.com": "www.codebuddy.cn", // 国内
}

// isModelCatalogPath 是否模型目录路径（三条家族路径；企业变体 /console/enterprises/<id>/models 同属）。
func isModelCatalogPath(path string) bool {
	if strings.HasPrefix(path, "/v3/config") {
		return true
	}
	return strings.Contains(path, "/enterprises/") && strings.HasSuffix(path, "/models")
}

// fallbackWorthRetry 主域结果是否值得换域重试：连接层错误 / 5xx / 404（该域无此端点）。
// 401/403 与其余 4xx 不重试（凭证/请求本身的问题，换域无益且会掩盖错误）。
func fallbackWorthRetry(resp *http.Response, err error) bool {
	if err != nil || resp == nil {
		return true
	}
	return resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound
}

// modelFallbackTransport 只拦模型目录 GET 的兜底传输层；其余请求零改写直通。
type modelFallbackTransport struct {
	base http.RoundTripper
}

func (t *modelFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	alt, ok := modelHostFallback[req.URL.Hostname()]
	if !ok || req.Method != http.MethodGet || !isModelCatalogPath(req.URL.Path) {
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(req)
	if !fallbackWorthRetry(resp, err) {
		return resp, err
	}
	if resp != nil && resp.Body != nil {
		// 丢弃主域失败响应体（复用连接），失败也不影响兜底流程。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}
	if err != nil {
		log.Printf("WARN: [upstream] 模型目录主域不可达，改打备用域 %s → %s: %v", req.URL.Host, alt, err)
	} else {
		log.Printf("WARN: [upstream] 模型目录主域返回 %d，改打备用域 %s → %s", resp.StatusCode, req.URL.Host, alt)
	}

	retry := req.Clone(req.Context())
	u := *req.URL
	u.Host = alt // 保留 path/query；hostname 与 port 一并替换
	retry.URL = &u
	retry.Host = alt
	return t.base.RoundTrip(retry)
}

// CloseIdleConnections 透传给底层传输（roundTripCloseIdle 走的就是这个可选接口）。
func (t *modelFallbackTransport) CloseIdleConnections() {
	if ci, ok := t.base.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

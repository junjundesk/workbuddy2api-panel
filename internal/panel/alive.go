// alive.go 单账号测活：用指定账号直连上游发一句 hi，验证账号当下能否正常对话。
//
// 与主链路的边界：
//   - 完全绕开选号 / 粘性 / 会话计数，直接把指定账号交给 upstream.ChatStreamContext；
//     client 由 upstream 按账号所属分组选择，因此分组代理照常生效；
//   - **不改写池状态**（不记成功/失败计数、不改冷却/禁用/余额、不写用量台账）——
//     测活是人工诊断动作，不能污染「成功 / 失败」统计与冷却状态机；
//   - 模型不写死：从该账号自己的模型目录里选（isDefault 优先，否则 id 字典序最小），
//     避免「账号没有某个内置模型」被误判成断线（账号域 / 套餐 / 灰度可见模型不同）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

const (
	// alivePrompt 测活发送的唯一一条用户消息（需求：点击测活发一句 hi）。
	alivePrompt = "hi"
	// aliveTimeout 对话阶段总预算：chat client 本身无总超时（流式），人工按钮必须封顶。
	aliveTimeout = 30 * time.Second
	// aliveStreamLimit 读取的流上限：足够覆盖 hi 的完整回复，异常上游也不会无限读。
	aliveStreamLimit = 1 << 20
	// aliveReplyRunes 回复预览的最大字符数（按 rune，中文/emoji 不切坏）。
	aliveReplyRunes = 80
)

// aliveResp 测活响应：成功与失败共用一份结构（失败时带 stage/error 与可选分类字段）。
// 与「代理测试」同口径：上游失败仍回 HTTP 200 + ok:false，前端按 ok 决定提示样式。
type aliveResp struct {
	OK             bool   `json:"ok"`
	UID            string `json:"uid"`
	Realm          string `json:"realm,omitempty"`
	Model          string `json:"model,omitempty"`
	LatencyMS      int64  `json:"latency_ms"`
	TTFBMS         int64  `json:"ttfb_ms,omitempty"`
	Reply          string `json:"reply,omitempty"`
	TotalTokens    int64  `json:"total_tokens,omitempty"`
	Stage          string `json:"stage,omitempty"`           // catalog / chat（成功时省略）
	Kind           string `json:"kind,omitempty"`            // 上游错误分类（*upstream.Error）
	UpstreamStatus int    `json:"upstream_status,omitempty"` // 上游 HTTP 状态（有则带）
	Error          string `json:"error,omitempty"`
}

// accountAlive 面板「测活」：POST /panel/api/accounts/{uid}/alive。
//
// 流程：账号自己的模型目录 → 选模型 → 真实 chat（一句 hi）→ 聚合 SSE；
// 只回诊断结果，不触碰池状态。返回码：账号不存在 404、面板未装配 Pool/Upstream 501，
// 其余（含上游失败）一律 200 + ok:false。
func (p *Panel) accountAlive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "account pool or upstream not available")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	resp := aliveResp{UID: uid}
	started := time.Now()
	realm, infos, err := p.accountModelCatalog(a)
	resp.Realm = realm
	if err != nil {
		resp.Stage = "catalog"
		resp.LatencyMS = time.Since(started).Milliseconds()
		resp.Error = err.Error()
		log.Printf("panel: 测活 uid=%s realm=%s 模型目录失败: %v", uid, realm, err)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	model := aliveProbeModel(infos)
	if model == "" {
		resp.Stage = "catalog"
		resp.LatencyMS = time.Since(started).Milliseconds()
		resp.Error = "模型目录里没有可用的对话模型"
		log.Printf("panel: 测活 uid=%s realm=%s 模型目录为空", uid, realm)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Model = model

	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": alivePrompt},
		},
		"stream": true,
	})
	if err != nil { // 全常量结构，理论不可达；保留防御，避免把 500 抛给前端。
		resp.Stage = "chat"
		resp.LatencyMS = time.Since(started).Milliseconds()
		resp.Error = "构造请求失败：" + err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), aliveTimeout)
	defer cancel()
	chatStart := time.Now()
	rc, status, respBody, chatErr := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{})
	if rc != nil {
		// 流读取失败 / 提前返回时也要释放上游连接（Close 会 cancel 在途请求）。
		defer rc.Close()
	}
	if chatErr == nil && status >= 400 {
		// ChatStream 对 ≥400 通常已返回分类错误；ErrNone 防御分支可能 err=nil，这里兜底。
		chatErr = fmt.Errorf("上游返回 HTTP %d：%s", status, truncateRunes(string(respBody), 200))
	}
	if chatErr != nil {
		kind, upStatus := aliveErrorFields(chatErr)
		if upStatus == 0 {
			upStatus = status
		}
		resp.Stage = "chat"
		resp.LatencyMS = time.Since(chatStart).Milliseconds()
		resp.Error = aliveErrorText(chatErr)
		resp.Kind = kind
		resp.UpstreamStatus = upStatus
		log.Printf("panel: 测活 uid=%s realm=%s model=%s 对话失败: %v", uid, realm, model, chatErr)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if rc == nil {
		resp.Stage = "chat"
		resp.LatencyMS = time.Since(chatStart).Milliseconds()
		resp.Error = "上游未返回响应体"
		log.Printf("panel: 测活 uid=%s realm=%s model=%s 上游未返回响应体", uid, realm, model)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	fb := &firstByteReader{r: io.LimitReader(rc, aliveStreamLimit)}
	agg, aggErr := upstream.Aggregate(fb)
	latency := time.Since(chatStart)
	if aggErr != nil {
		kind, upStatus := aliveErrorFields(aggErr)
		resp.Stage = "chat"
		resp.LatencyMS = latency.Milliseconds()
		resp.Error = aliveErrorText(aggErr)
		resp.Kind = kind
		resp.UpstreamStatus = upStatus
		log.Printf("panel: 测活 uid=%s realm=%s model=%s 流读取失败: %v", uid, realm, model, aggErr)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.OK = true
	resp.LatencyMS = latency.Milliseconds()
	if !fb.at.IsZero() {
		// 亚毫秒（或同一时钟刻度）的极快响应按 1ms 记：字段非零即「确实收到了首字节」，
		// 也避免 omitempty 把成功响应里的首字节证据抹掉。
		resp.TTFBMS = fb.at.Sub(chatStart).Milliseconds()
		if resp.TTFBMS < 1 {
			resp.TTFBMS = 1
		}
	}
	resp.Reply = aliveReplyText(agg)
	resp.TotalTokens = aliveTotalTokens(agg)
	log.Printf("panel: 测活 uid=%s realm=%s model=%s ok=true http=%d latency=%dms ttfb=%dms tokens=%d",
		uid, realm, model, status, resp.LatencyMS, resp.TTFBMS, resp.TotalTokens)
	writeJSON(w, http.StatusOK, resp)
}

// aliveProbeModel 从账号自己可见的模型目录里选一个测活模型：
// isDefault 优先（多个默认取 id 字典序最小），没有默认标记时取 id 字典序最小。
// 字典序是刻意选择——CN 目录来自 /v3/config 与端点并集（map 顺序不稳定），
// 固定规则保证同一账号重复测活命中同一模型，结果可比。
func aliveProbeModel(infos []upstream.ModelInfo) string {
	var defaults, ids []string
	for _, mi := range infos {
		id := strings.TrimSpace(mi.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		if mi.IsDefault {
			defaults = append(defaults, id)
		}
	}
	if len(defaults) > 0 {
		sort.Strings(defaults)
		return defaults[0]
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return ids[0]
}

// aliveReplyText 取聚合响应的正文预览：压平全部空白（换行/连续空格），按 rune 截断。
// 纯思考模型可能没有 content（仍算测活成功），此时返回空串。
func aliveReplyText(agg map[string]any) string {
	choices, _ := agg["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return truncateRunes(strings.Join(strings.Fields(content), " "), aliveReplyRunes)
}

// aliveTotalTokens 取聚合响应 usage.total_tokens（上游末帧未带 usage 时返回 0）。
func aliveTotalTokens(agg map[string]any) int64 {
	usage, _ := agg["usage"].(map[string]any)
	switch v := usage["total_tokens"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// aliveErrorFields 从上游错误里取分类与 HTTP 状态（非 *upstream.Error 返回空/0）。
func aliveErrorFields(err error) (kind string, status int) {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue.Kind.String(), ue.Status
	}
	return "", 0
}

// aliveErrorText 把测活失败转成前端可读文案：空流 / 超时 / 取消单独说明，其余透传。
func aliveErrorText(err error) string {
	switch {
	case err == nil:
		return ""
	case upstream.IsEmptyStreamError(err):
		return "上游返回 200 但没有任何有效数据帧（空流）"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("测活超时（%s 内未完成）", aliveTimeout)
	case errors.Is(err, context.Canceled):
		return "测活已取消（客户端断开）"
	}
	return err.Error()
}

// truncateRunes 按字符截断，多字节 / emoji 不会被切坏。
// 与 autotask.go 的 truncateStr（按字节，用于 ASCII 错误文本）区分，勿混用。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// firstByteReader 记录第一次读到非空字节的时刻，供 TTFB 统计。
type firstByteReader struct {
	r  io.Reader
	at time.Time
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.at.IsZero() {
		f.at = time.Now()
	}
	return n, err
}

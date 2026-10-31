// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// X-IDE-Version）。对齐 2026-10 线上 WorkBuddy IDE 实测抓包
	// `WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0`；旧值 5.5.4 已落后一整代，
	// 上游按客户端版本做灰度/实验分流时会把网关判成老客户端。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.9.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本（同上抓包实测 2.165.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.165.0"

	// defaultProductName 出站 X-Product 取值：官方 IDE 实测发 "SaaS"。
	// X-Product 是**渠道位**，产品身份由 X-IDE-Name/X-IDE-Type 承载——两者不可混用
	// （早期实现把二者绑在 client_name 上，导致要么 X-Product 错、要么 X-IDE-* 错）。
	// config upstream.product_name 可覆盖。
	defaultProductName = "SaaS"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"

	// domainCN/domainGlobal 个人账号的 X-Domain 回落值（官方 IDE 恒定发送该头）；
	// 企业账号仍优先用 auth.Domain 的自有域，绝不覆盖。
	domainCN     = "www.workbuddy.cn"
	domainGlobal = "www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 applicationName 同值 `WorkBuddy`，
// global 用官方国际版 productName `WorkBuddy AI`（intl 项目逆向证据：
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global 账号送错平台段（`WorkBuddy` 非 `WorkBuddy AI`）可能触发上游 403 code 11140
// "request illegal" 风控。官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	// 官方桌面端在 chat 面**不发**这两个头（2026-10 IDE 抓包实证），它们既是自造
	// 指纹又是每账号恒定高熵值——上游一旦做头部白名单/未知头统计，这就是最干净的
	// 网关特征。默认关闭，config upstream.send_device_headers 可开。
	if c == nil || !c.SendDeviceHeaders || a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值；空则按 (uid,sessKey) 稳定派生
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
	// SessionKey 会话级粘性键（handler 的 in.sessKey）：X-Conversation-ID 的派生种子，
	// 保证同一对话跨轮恒定、账号间互异；空则回落本轮 requestID。
	SessionKey string
	// AgentType 请求类型："main"（默认）或 "auxiliary"（标题/摘要类辅助请求）。
	// 决定 X-Agent-Type / X-Agent-Purpose / 是否发 X-Root-Request-ID。
	AgentType string
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d) // 企业自有域原样透传
		} else {
			// 个人账号：官方 IDE 恒发 X-Domain（官网域），缺失会被上游当作非官方客户端。
			req.Header.Set("X-Domain", domainCN)
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：X-Product（渠道位）+ X-IDE-*（产品身份），默认伪造官方桌面端指纹。
	c.injectAttribution(req)
	// 请求意图头族：X-Agent-Type / X-Agent-Purpose / X-Agent-Intent / X-Agent-Runtime。
	c.injectAgentHeaders(req, meta.AgentType)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3+W3C 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, a, meta)
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定。入站有则透传；没有则按 (uid, sessKey)
//     派生与官方同形 UUID（8-4-4-4-12）——官方 IDE 恒定发送该头，缺失本身即特征。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	uid := ""
	if a != nil {
		uid = a.UID
	}
	convID := meta.ConversationID
	if convID == "" {
		convID = deriveConversationID(uid, meta.SessionKey, convReqID)
	}
	req.Header.Set("X-Conversation-ID", convID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	// 官方仅 main 请求发根请求 ID（auxiliary 抓包里没有这一项）。
	if meta.AgentType != "auxiliary" {
		req.Header.Set("X-Root-Request-ID", convReqID)
	}
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	// 链路族一次生成、B3 与 W3C 共用同一 trace/span，避免两套 ID 各说各话。
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	spanID := messageID[:16]
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
	// b3 单头紧凑格式：trace-span-sampled[-parent]。没有父 span 就不发第四段
	// （规范允许），绝不为了"像"而编一个 parent。
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1")
	// W3C traceparent 要求 trace-id 恒 32 hex；16 位的合法 B3 值在 W3C 里非法，
	// 此时改用消息级 ID（恒 32 hex），保证头本身合法。
	w3cTrace := b3Trace
	if len(w3cTrace) != 32 {
		w3cTrace = messageID
	}
	req.Header.Set("traceparent", "00-"+w3cTrace+"-"+spanID+"-01")
}

// deriveConversationID 派生与官方同形的会话 ID（UUID 8-4-4-4-12，version 7）。
//
// 派生种子取 (uid, sessKey)：同一账号的同一对话跨轮恒定（后台按此聚合不会碎片化），
// 账号之间互异（不同号不会串会话），跨进程重启稳定。
// sessKey 为空（无 user 消息/无可签名内容）时退回本轮 requestID——至少保证单轮内
// 自洽，不至于发出空值或非法形状。
func deriveConversationID(uid, sessKey, fallback string) string {
	if sessKey == "" {
		if len(fallback) == 32 {
			return uuidFromHex(fallback)
		}
		return fallback
	}
	sum := sha256.Sum256([]byte("wb2a:conv:" + uid + ":" + sessKey))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x70 // version 7（时间有序 UUID，官方 IDE 同代）
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return uuidFromHex(hex.EncodeToString(u[:]))
}

// uuidFromHex 把 32 位 hex 串格式化成 8-4-4-4-12 的 UUID 文本。
func uuidFromHex(h string) string {
	if len(h) != 32 {
		return h
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// productName 生效的 X-Product 取值：Client.ProductName 非空取之，否则 "SaaS"。
// 与 attributionClientName（X-IDE-Name/Type）**解耦**：官方 IDE 是
// `X-Product: SaaS` + `X-IDE-Name/Type: WorkBuddy` 的组合，绑在同一字段上必然错一头。
func (c *Client) productName() string {
	if c != nil && c.ProductName != "" {
		return c.ProductName
	}
	return defaultProductName
}

// injectAttribution 注入用量归属头（X-Product / X-IDE-*）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认即对齐官方 IDE 指纹：X-Product="SaaS"（渠道位）+ X-IDE-Name/Type="WorkBuddy"
// + X-IDE-Version=client_version。显式 ClientName="SaaS" 还原旧行为（只发 X-Product，
// 不设 X-IDE-*）；配其他值则 X-IDE-* 跟随该值。
// X-Agent-Purpose 不在此注入——它要随 main/auxiliary 变化，见 injectAgentHeaders。
func (c *Client) injectAttribution(req *http.Request) {
	req.Header.Set("X-Product", c.productName())
	name := c.attributionClientName()
	if name == "SaaS" {
		return
	}
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion())
}

// injectAgentHeaders 注入官方 IDE 的请求意图头族（2026-10 抓包实证，四项必发）：
//   - X-Agent-Type：main（用户发起的主对话）/ auxiliary（标题、摘要类辅助请求）；
//   - X-Agent-Purpose：随类型取 conversation / conversation_topic；
//   - X-Agent-Intent："craft"（IDE 的 agent 意图标识，常量）；
//   - X-Agent-Runtime："local-agent"（本地 agent 运行时，常量）。
//
// 缺这组头时上游后台按 agentType/agentPurpose 聚合的维度全为空白——这是网关流量
// 与官方流量在归因面上最大的一处差异（比 x-stainless-* 这类 SDK 指纹重要得多）。
func (c *Client) injectAgentHeaders(req *http.Request, agentType string) {
	if agentType == "auxiliary" {
		req.Header.Set("X-Agent-Type", "auxiliary")
		req.Header.Set("X-Agent-Purpose", "conversation_topic")
	} else {
		req.Header.Set("X-Agent-Type", "main")
		req.Header.Set("X-Agent-Purpose", "conversation")
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "local-agent")
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

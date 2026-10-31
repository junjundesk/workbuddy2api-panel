package upstream

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
)

// headerNames 返回规范化（小写）后的头名集合，排序便于比较与打印差值。
func headerNames(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return out
}

// diffHeaderNames 打印 missing / extra，比整块 diff 好读——上游换版本时一眼看出差哪几个。
func diffHeaderNames(got, want []string) (missing, extra []string) {
	g, w := map[string]bool{}, map[string]bool{}
	for _, k := range got {
		g[k] = true
	}
	for _, k := range want {
		w[k] = true
		if !g[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range got {
		if !w[k] {
			extra = append(extra, k)
		}
	}
	return missing, extra
}

// cnChatHeaderNames CN 主对话（main）出站头全集。
//
// 与 2026-10 线上 WorkBuddy IDE 5.9.3 抓包对齐后，仍刻意**不发**的：
//   - x-stainless-*（7 个）：OpenAI Node SDK 运行时痕迹，Go 侧伪造等于连运行时一起伪造；
//   - x-api-key / acp-connection-id：relay/ACP 进程语义，网关无对应物；
//   - x-machine-id / x-session-id：自造指纹，官方 chat 面不发（见 SendDeviceHeaders）。
var cnChatHeaderNames = []string{
	"accept",
	"accept-encoding",
	"accept-language",
	"authorization",
	"b3",
	"content-length",
	"content-type",
	"host",
	"origin",
	"referer",
	"traceparent",
	"user-agent",
	"x-agent-intent",
	"x-agent-purpose",
	"x-agent-runtime",
	"x-agent-type",
	"x-b3-sampled",
	"x-b3-spanid",
	"x-b3-traceid",
	"x-codebuddy-request",
	"x-conversation-id",
	"x-conversation-message-id",
	"x-conversation-request-id",
	"x-domain",
	"x-ide-name",
	"x-ide-type",
	"x-ide-version",
	"x-no-department-info",
	"x-no-enterprise-id",
	"x-product",
	"x-request-id",
	"x-requested-with",
	"x-root-request-id",
	"x-trace-id",
	"x-user-id",
}

// TestChatHeadersCNSet 锁住 CN 主对话出站头**集合**：任何新增/丢失/改名都会被拦下。
func TestChatHeadersCNSet(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example"}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: "0123456789abcdef0123456789abcdef"})

	got := headerNames(req.Header)
	// httptest 不产生 Host/Content-Length/Accept-Encoding（由传输层补），集合断言扣除这三项。
	want := []string{}
	for _, k := range cnChatHeaderNames {
		if k == "host" || k == "content-length" || k == "accept-encoding" {
			continue
		}
		want = append(want, k)
	}
	missing, extra := diffHeaderNames(got, want)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("header set drift\n missing=%v\n extra=%v\n got=%v", missing, extra, got)
	}

	// 关键值断言（集合一致但值错同样没用）。
	for k, v := range map[string]string{
		"X-Product":       "SaaS",
		"X-Agent-Type":    "main",
		"X-Agent-Purpose": "conversation",
		"X-Agent-Intent":  "craft",
		"X-Agent-Runtime": "local-agent",
		"X-Domain":        "www.workbuddy.cn",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.9.3",
	} {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if ua := req.Header.Get("User-Agent"); ua != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0" {
		t.Errorf("User-Agent = %q", ua)
	}
	// 聚合主键三兄弟必须同值；消息级 ID 独立。
	if a1, a2 := req.Header.Get("X-Conversation-Request-ID"), req.Header.Get("X-Root-Request-ID"); a1 != a2 {
		t.Errorf("X-Root-Request-ID=%q want %q", a2, a1)
	}
	if req.Header.Get("X-Request-ID") != req.Header.Get("X-Conversation-Message-ID") {
		t.Error("X-Request-ID 必须等于 X-Conversation-Message-ID")
	}
}

var (
	uuidRe        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
)

// TestChatHeadersAuxiliary auxiliary（辅助）请求：purpose 换档、不发根请求 ID。
func TestChatHeadersAuxiliary(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example"}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{AgentType: "auxiliary"})

	if got := req.Header.Get("X-Agent-Type"); got != "auxiliary" {
		t.Errorf("X-Agent-Type=%q want auxiliary", got)
	}
	if got := req.Header.Get("X-Agent-Purpose"); got != "conversation_topic" {
		t.Errorf("X-Agent-Purpose=%q want conversation_topic", got)
	}
	if req.Header.Get("X-Root-Request-ID") != "" {
		t.Error("auxiliary 不应发 X-Root-Request-ID")
	}
}

// TestChatHeadersGlobalRealm global 账号：域头与语言随 realm 切。
func TestChatHeadersGlobalRealm(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example", GlobalEnabled: true}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("X-Domain=%q want www.workbuddy.ai", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("Accept-Language=%q want en-US", got)
	}
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("X-No-Enterprise-Id=%q want 1", got)
	}
}

// TestConversationIDDerived X-Conversation-ID 恒发、跨轮稳定、账号间互异、形状合法。
func TestConversationIDDerived(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example"}
	call := func(uid, sessKey string) string {
		req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
		c.ChatHeaders(req, &auth.Auth{UID: uid, AccessToken: "at"}, "", ChatMeta{SessionKey: sessKey})
		id := req.Header.Get("X-Conversation-ID")
		if !uuidRe.MatchString(id) {
			t.Fatalf("conversation id %q 不是合法 v7 UUID", id)
		}
		return id
	}
	if a, b := call("u1", "sess-a"), call("u1", "sess-a"); a != b {
		t.Errorf("同账号同会话应恒定: %q vs %q", a, b)
	}
	if a, b := call("u1", "sess-a"), call("u1", "sess-b"); a == b {
		t.Error("不同会话应互异")
	}
	if a, b := call("u1", "sess-a"), call("u2", "sess-a"); a == b {
		t.Error("不同账号应互异")
	}
}

// TestDeviceHeadersOptIn 自造设备头默认不发，显式开启才注入。
func TestDeviceHeadersOptIn(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	(&Client{}).ChatHeaders(req, a, "", ChatMeta{})
	if req.Header.Get("X-Machine-ID") != "" || req.Header.Get("X-Session-ID") != "" {
		t.Fatal("默认不应注入 X-Machine-ID / X-Session-ID")
	}
	req2 := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	(&Client{SendDeviceHeaders: true}).ChatHeaders(req2, a, "", ChatMeta{})
	if req2.Header.Get("X-Machine-ID") == "" || req2.Header.Get("X-Session-ID") == "" {
		t.Fatal("开启后应注入设备头")
	}
}

// TestTraceFamilyConsistent B3 与 W3C 同源，且两个头本身格式合法。
func TestTraceFamilyConsistent(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example"}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	// 入站 trace id 是 16 位合法 B3（W3C 里非法）→ traceparent 必须回落 32 位消息级 ID。
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{ConversationRequestID: "0123456789abcdef"})

	if got := req.Header.Get("X-B3-TraceId"); got != "0123456789abcdef" {
		t.Errorf("X-B3-TraceId=%q", got)
	}
	span := req.Header.Get("X-B3-SpanId")
	if want := req.Header.Get("b3"); want != "0123456789abcdef-"+span+"-1" {
		t.Errorf("b3=%q", want)
	}
	if tp := req.Header.Get("traceparent"); !traceparentRe.MatchString(tp) {
		t.Errorf("traceparent=%q 不合法", tp)
	}
}

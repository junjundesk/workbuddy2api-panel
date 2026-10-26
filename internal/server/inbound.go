package server

import (
	"bytes"
	"encoding/json"

	"github.com/junjundesk/workbuddy2api-panel/internal/session"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
	"github.com/junjundesk/workbuddy2api-panel/internal/wire"
)

// chatInbound 一次入站 chat 请求的解析结果。
//
// 目的：同一个 body 只做一次顶层解析（wire.ParseChat），后续 peek/工具名单/
// 会话键/轮级键/conversationId 全部复用；出站改写（prompt/模型名/prepareBody）
// 仍按字节进行，保证 passthrough 语义与既有出站字节契约不变。
//
// raw 保留原始字节：坏 JSON、出站归一化失败等场景按原样透传（与既有语义一致）。
type chatInbound struct {
	raw  []byte
	wire *wire.Chat
	// obj 已解析对象：/v1/responses 直接复用转换后的 chat 对象（零额外解析）；
	// chat 入口在需要改写时惰性填充。
	obj map[string]any

	stream bool
	model  string

	tools    map[string]bool
	sessKey  string
	turnKey  string
	convID   string
	hasImage bool
}

// parseChatInboundWithObject 在已解析对象的基础上构造入站视图（responses 复用）。
func parseChatInboundWithObject(body []byte, obj map[string]any) *chatInbound {
	in := parseChatInbound(body)
	in.obj = obj
	return in
}

// parseChatInbound 解析入站 chat body（单次顶层解析 + 少量惰性字段解码）。
func parseChatInbound(body []byte) *chatInbound {
	in := &chatInbound{raw: body}
	w, err := wire.ParseChat(body)
	if err != nil {
		return in
	}
	in.wire = w
	// 与旧 peek 同口径：stream+model 一次解码，任一字段类型不符 → 两者都按零值。
	streamOK := true
	if raw := w.Raw("stream"); raw != nil {
		if b, ok := w.Bool("stream"); ok {
			in.stream = b
		} else {
			streamOK = false
		}
	}
	model := ""
	if raw := w.Raw("model"); raw != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			model = s
		} else {
			streamOK = false
		}
	}
	if streamOK {
		in.model = model
	}
	in.tools = upstream.ToolNameAllowlistWire(w)
	in.sessKey = session.ExtractKeyWire(w)
	in.turnKey = session.TurnKeyWire(w)
	in.convID = session.ConversationIDWire(w)
	in.hasImage = hasImagePart(w)
	return in
}

// hasImagePart 报告请求是否携带 image_url part。
//
// 与旧实现同语义（畸形即 false，宁缺勿滥）：任何一条消息的 content 不是数组
// （字符串/null 除外）就整体判 false；content 为字符串是常态，会让旧实现整包
// Unmarshal 失败 → 同样返回 false。逐消息解码避免再整包解析一遍。
func hasImagePart(c *wire.Chat) bool {
	if c == nil {
		return false
	}
	// 整包预检：正文里没有字面量 image_url 就一定没有图片 part（图 URL/part 类型
	// 都必然包含该串），避免为纯文本长上下文遍历每条消息。
	if !bytes.Contains(c.Raw("messages"), []byte("image_url")) {
		return false
	}
	saw := false
	for _, raw := range c.Messages() {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
			return false
		}
		if len(m.Content) == 0 || string(m.Content) == "null" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return false
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				saw = true
			}
		}
	}
	return saw
}

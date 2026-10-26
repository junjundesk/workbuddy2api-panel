// wire.go 会话键/轮级键的「一次解析」入口。
//
// 与 session.go / ids.go 里的字节入口逐条同义：字节入口先 wire.ParseChat 再调用
// 这里的 *Wire 版本。Web 侧（server）在入站请求上只解析一次，随后复用同一视图，
// 避免同一 body 被完整 Unmarshal 多次。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/junjundesk/workbuddy2api-panel/internal/wire"
)

// ExtractKeyWire 等价于 ExtractKey（见 session.go 的优先级与闸门说明）。
func ExtractKeyWire(c *wire.Chat) string {
	if c == nil {
		return ""
	}
	meta := c.Meta()
	if meta != nil {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := c.Str("conversation_id"); v != "" {
		return v
	}
	if v := c.Str("conversationId"); v != "" {
		return v
	}
	if v := c.Str("prompt_cache_key"); v != "" {
		return v
	}
	// 派生回退闸：带 user 维度标识的请求不派生（契约与 ExtractKey 一致）。
	if meta != nil && strOrEmpty(meta["user_id"]) != "" {
		return ""
	}
	if c.Str("user_id") != "" {
		return ""
	}
	return deriveKeyWire(c)
}

// deriveKeyWire 等价于 deriveKey：system 文本 + 首条 user 内容签名 → sha256 前 16 字节。
func deriveKeyWire(c *wire.Chat) string {
	msgs := c.Messages()
	if len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, raw := range msgs {
		msg, ok := wire.DecodeMessage(raw)
		if !ok {
			continue
		}
		text := messageTextAny(msg.Content)
		switch msg.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				firstUserText = userContentSignatureAny(msg.Content)
			}
		}
		if firstUserText != "" && systemText != "" {
			break
		}
	}
	if firstUserText == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// messageTextAny 把 content 原始字节按 map 形态解码后取文本，与 messageText(any) 同口径。
func messageTextAny(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return messageText(v)
}

// userContentSignatureAny 把 content 原始字节按 map 形态解码、再序列化后取签名，
// 与 deriveKey 里 userContentSignature(content any) 的字节口径完全一致（键序/空白
// 由 encoding/json 规范化），保证存量粘性键零漂移。
func userContentSignatureAny(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return userContentSignature(v)
}

// TurnKeyWire 等价于 TurnKey（最后一条 user 消息的「序号 + 内容签名」）。
func TurnKeyWire(c *wire.Chat) string {
	if c == nil {
		return ""
	}
	msgs := c.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := wire.DecodeMessage(msgs[i])
		if !ok || msg.Role != "user" {
			continue
		}
		sig := contentSignature(msg.Content)
		if sig == "" {
			return ""
		}
		return fmt.Sprintf("u%d:%s", i, sig)
	}
	return ""
}

// ConversationIDWire 等价于 ResolveConversationID（只认 conversationId，不回落 user_id）。
func ConversationIDWire(c *wire.Chat) string {
	if c == nil {
		return ""
	}
	if meta := c.Meta(); meta != nil {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := c.Str("conversation_id"); v != "" {
		return v
	}
	return c.Str("conversationId")
}

// Package wire 提供「只解析一次、按需取字段」的入站 JSON 视图。
//
// 背景：chat 入站管线此前对同一个 body 做了约 10 次完整 json.Unmarshal（peek、
// 工具名单、会话键、轮级键、conversationId、model、提示词改写、模型名改写、
// 出站归一化、缓存键注入…）。本包把**顶层字段**解成 map[string]json.RawMessage
// （一次解析、按字段惰性解码），供各消费方复用，避免重复整包解析。
//
// 关键契约：messages 元素保留原始字节（json.RawMessage）。会话轮级键的
// contentSignature 对非文本 part 取「原始字节 sha256 前 8 hex」，走 map 往返会
// 改变键值（键序/空白被规范化）→ 存量会话键漂移。保留 RawMessage 是零漂移前提。
package wire

import (
	"encoding/json"
	"sync"
)

// Chat 是 chat/completions 请求体的惰性视图。
type Chat struct {
	raw map[string]json.RawMessage

	metaOnce sync.Once
	meta     map[string]any

	msgsOnce sync.Once
	msgs     []json.RawMessage
}

// ParseChat 解析顶层对象；非对象/坏 JSON 返回 error（调用方按「全部字段缺失」处理）。
func ParseChat(body []byte) (*Chat, error) {
	if len(body) == 0 {
		return nil, errEmpty
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return &Chat{raw: raw}, nil
}

type wireError string

func (e wireError) Error() string { return string(e) }

const errEmpty = wireError("wire: empty body")

// Meta 返回 metadata 对象（非对象/缺失 → nil）。惰性解码一次并缓存：
// ExtractKey / ResolveConversationID 都要读它，避免同一字段被解码多次。
func (c *Chat) Meta() map[string]any {
	if c == nil {
		return nil
	}
	c.metaOnce.Do(func() {
		r := c.Raw("metadata")
		if r == nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(r, &m) == nil {
			c.meta = m
		}
	})
	return c.meta
}

// Raw 返回顶层字段原始字节（不存在或为 null 时 nil）。
func (c *Chat) Raw(key string) json.RawMessage {
	if c == nil {
		return nil
	}
	r, ok := c.raw[key]
	if !ok || len(r) == 0 || string(r) == "null" {
		return nil
	}
	return r
}

// Str 取顶层字符串字段（非字符串/缺失 → ""）。
func (c *Chat) Str(key string) string {
	r := c.Raw(key)
	if r == nil {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return s
}

// Bool 取顶层布尔字段。
func (c *Chat) Bool(key string) (bool, bool) {
	r := c.Raw(key)
	if r == nil {
		return false, false
	}
	var b bool
	if json.Unmarshal(r, &b) != nil {
		return false, false
	}
	return b, true
}

// Messages 返回 messages 数组的原始元素；messages 不是数组时返回 nil。
// 解码一次并缓存：工具名单/会话键/轮级键/图片判定都要遍历消息，重复解码
// 200KB 的 messages 数组是入站路径最大的隐藏成本。
func (c *Chat) Messages() []json.RawMessage {
	if c == nil {
		return nil
	}
	c.msgsOnce.Do(func() {
		r := c.Raw("messages")
		if r == nil {
			return
		}
		var msgs []json.RawMessage
		if json.Unmarshal(r, &msgs) == nil {
			c.msgs = msgs
		}
	})
	return c.msgs
}

// Message 解码单条消息的 role/content（content 保留原始字节）。
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// DecodeMessage 解码第 i 条消息；非对象消息返回 false（与 map 形态下「跳过非 map」一致）。
func DecodeMessage(raw json.RawMessage) (Message, bool) {
	var m Message
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return Message{}, false
	}
	return m, true
}

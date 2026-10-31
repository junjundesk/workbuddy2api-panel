// rules.go 多条提示词 + 关键词替换（面板"提示词规则"的后端语义）。
//
// 与 Rewrite / Append 的关系：那两个函数是"一条文本"的固定策略（整体替换 / 块后
// 插一条），适合 config 里的单文件提示词；本文件把提示词拆成**多条、带角色**的
// 规则，并支持按关键词改写请求消息文本，供分组级配置（groups.json）使用。
//
// 语义（按执行顺序）：
//  1. 关键词替换：对 messages 的文本 content 做字面替换（strings.ReplaceAll），
//     可按角色限定范围。先做替换再做注入——替换针对"请求上来的"内容，不能改动
//     网关自己注入的提示词（否则一次误配就会把注入文本也改掉，排查成本极高）。
//  2. 系统提示词：所有 role=system 的条目按顺序用空行拼接，整体替换客户端的
//     system / developer 消息（与 Rewrite 的 custom 语义一致）。
//  3. 用户提示词：所有 role=user 的条目作为 user 消息插入到开头连续
//     system/developer 块之后、客户端第一条 user 消息之前。
//
// 边界：入参坏 JSON / 空规则 / 空文本一律不改写（绝不因规则配置阻塞转发）。
package prompt

import (
	"encoding/json"
	"strings"
)

// 提示词角色（落盘值，前端下拉框直接使用这两个字符串）。
const (
	RoleSystem = "system"
	RoleUser   = "user"
)

// Entry 一条注入提示词。
type Entry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Replace 一条关键词替换规则：请求消息文本里的 Match 全部替换为 With（With 空 =
// 删除该关键词）。Scope 取 all / system / user，缺省 all。
type Replace struct {
	Match string `json:"match"`
	With  string `json:"with"`
	Scope string `json:"scope,omitempty"`
}

// Rules 一组提示词规则。零值 = 不生效。
type Rules struct {
	Entries  []Entry   `json:"entries,omitempty"`
	Replaces []Replace `json:"replaces,omitempty"`
}

// Normalize 清洗规则：去空白、丢弃空文本条目、归一角色与作用范围。
//
// 角色认 developer（部分客户端用它代替 system），统一归一为 system——上游 role
// 白名单不含 developer，注入 developer 等于制造一次无谓的兼容风险。
func (r Rules) Normalize() Rules {
	out := Rules{}
	for _, e := range r.Entries {
		role := strings.ToLower(strings.TrimSpace(e.Role))
		switch role {
		case "", RoleSystem, "developer":
			role = RoleSystem
		case RoleUser:
		default:
			continue // 未知角色直接丢弃，不做"猜一个"的宽容
		}
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		out.Entries = append(out.Entries, Entry{Role: role, Text: text})
	}
	for _, rp := range r.Replaces {
		if rp.Match == "" {
			continue
		}
		scope := strings.ToLower(strings.TrimSpace(rp.Scope))
		switch scope {
		case "", "all":
			scope = "all"
		case "system", "user":
		default:
			scope = "all"
		}
		out.Replaces = append(out.Replaces, Replace{Match: rp.Match, With: rp.With, Scope: scope})
	}
	return out
}

// Empty 归一后是否没有任何有效规则。
func (r Rules) Empty() bool {
	for _, e := range r.Entries {
		if strings.TrimSpace(e.Text) != "" {
			return false
		}
	}
	for _, rp := range r.Replaces {
		if rp.Match != "" {
			return false
		}
	}
	return true
}

// SystemText 拼接所有系统提示词条目（无条目 → 空串）。条目之间用空行分隔，
// 多条提示词按配置顺序合并成一条 system 消息。
func (r Rules) SystemText() string {
	parts := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.Role == RoleSystem {
			parts = append(parts, e.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// userTexts 返回用户提示词条目正文（保持配置顺序）。
func (r Rules) userTexts() []string {
	out := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.Role == RoleUser {
			out = append(out, e.Text)
		}
	}
	return out
}

// Apply 解析 OpenAI 请求体并应用规则（坏 JSON / 空规则 → 原样返回）。
func Apply(body []byte, r Rules) []byte {
	if len(body) == 0 || r.Empty() {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if !ApplyObject(obj, r) {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ApplyObject 是 Apply 的对象形态（原地改，返回是否改动）。供入站管线在**同一次
// 解析**上完成规则改写，避免 decode→encode→再 decode 的重复往返。
func ApplyObject(obj map[string]any, r Rules) bool {
	if obj == nil {
		return false
	}
	r = r.Normalize()
	if len(r.Entries) == 0 && len(r.Replaces) == 0 {
		return false
	}
	msgs, _ := obj["messages"].([]any)
	changed := false
	if len(r.Replaces) > 0 && len(msgs) > 0 {
		if applyReplaces(msgs, r.Replaces) {
			changed = true
		}
	}
	if sys := r.SystemText(); sys != "" {
		msgs = replaceSystem(msgs, sys)
		changed = true
	}
	if users := r.userTexts(); len(users) > 0 {
		msgs = insertUsers(msgs, users)
		changed = true
	}
	if !changed {
		return false
	}
	obj["messages"] = msgs
	return true
}

// replaceSystem 删除全部 system/developer 消息，头部插入一条 system。
func replaceSystem(msgs []any, text string) []any {
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := mm["role"].(string); role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	return append([]any{map[string]any{"role": "system", "content": text}}, kept...)
}

// insertUsers 在开头连续 system/developer 块之后插入 user 消息（顺序保持配置顺序）。
func insertUsers(msgs []any, texts []string) []any {
	at := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		at++
	}
	out := make([]any, 0, len(msgs)+len(texts))
	out = append(out, msgs[:at]...)
	for _, t := range texts {
		out = append(out, map[string]any{"role": "user", "content": t})
	}
	out = append(out, msgs[at:]...)
	return out
}

// applyReplaces 按作用范围替换 messages 文本 content（字符串 / 多模态 text part）。
func applyReplaces(msgs []any, rules []Replace) bool {
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		for _, rp := range rules {
			if !scopeHit(rp.Scope, role) {
				continue
			}
			switch c := mm["content"].(type) {
			case string:
				if s := strings.ReplaceAll(c, rp.Match, rp.With); s != c {
					mm["content"] = s
					changed = true
				}
			case []any:
				for _, p := range c {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					text, ok := pm["text"].(string)
					if !ok {
						continue
					}
					if s := strings.ReplaceAll(text, rp.Match, rp.With); s != text {
						pm["text"] = s
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// scopeHit 角色是否落在替换规则的作用范围内。
func scopeHit(scope, role string) bool {
	switch scope {
	case "system":
		return role == "system" || role == "developer"
	case "user":
		return role == "user"
	default:
		return true
	}
}

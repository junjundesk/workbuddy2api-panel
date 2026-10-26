// wire.go 工具名名单的「一次解析」入口（供入站管线复用已解析视图）。
package upstream

import (
	"bytes"
	"encoding/json"

	"github.com/junjundesk/workbuddy2api-panel/internal/wire"
)

// ToolNameAllowlistWire 与 ToolNameAllowlist 同义：从已解析视图收集工具名。
func ToolNameAllowlistWire(c *wire.Chat) map[string]bool {
	if c == nil {
		return nil
	}
	names := map[string]bool{}
	// 整包预检：绝大多数请求没有任何工具调用/声明，直接返回 nil，避免遍历消息。
	hasToolBytes := func(r []byte) bool {
		return bytes.Contains(r, []byte("tool_calls")) || bytes.Contains(r, []byte("function_call"))
	}
	bodyHasTools := c.Raw("tools") != nil || c.Raw("functions") != nil
	if raw := c.Raw("tools"); raw != nil {
		var tools []struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tools) == nil {
			for _, t := range tools {
				if t.Function != nil && t.Function.Name != "" {
					names[t.Function.Name] = true
					continue
				}
				if t.Name != "" {
					names[t.Name] = true
				}
			}
		}
	}
	if raw := c.Raw("functions"); raw != nil {
		var fns []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &fns) == nil {
			for _, f := range fns {
				if f.Name != "" {
					names[f.Name] = true
				}
			}
		}
	}
	if !bodyHasTools && !hasToolBytes(c.Raw("messages")) {
		return nil
	}
	for _, raw := range c.Messages() {
		// 单条消息再预检一次：只对可能含工具调用的消息做结构解码。
		if !hasToolBytes(raw) {
			continue
		}
		var m struct {
			ToolCalls []struct {
				Function *struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"function_call"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function != nil && tc.Function.Name != "" {
				names[tc.Function.Name] = true
			}
		}
		if m.FunctionCall != nil && m.FunctionCall.Name != "" {
			names[m.FunctionCall.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

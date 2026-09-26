// responses_convert.go —— Responses API 与 chat/completions 之间的纯转换层。
//
// 分层参考 router-for-me/CLIProxyAPI 的 internal/translator
// （responses→chat 请求转换 + chat→responses 响应转换两段式），只覆盖本网关实际
// 需要映射的子集，且不引入 gjson/sjson 依赖，全程 encoding/json。
//
// 请求方向覆盖：
//
//	instructions            → 首条 system 消息
//	input (string|items[])  → messages：message / function_call / function_call_output
//	                          / custom_tool_call / custom_tool_call_output / reasoning
//	                          / item_reference / additional_tools
//	tools                   → chat tools（function 直译；custom 包成 {"input": string}）
//	tool_choice / text.format / reasoning.effort / max_output_tokens 等参数映射
//	previous_response_id    → 从内存响应存储续接上下文
//
// 响应方向（chat 响应 → responses 响应体 / SSE 事件序列）见 responses.go。
package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------- JSON 取值助手（map 形态，与 upstream/sse.go 同风格）----------

func jMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func jStr(v any) string {
	s, _ := v.(string)
	return s
}

func jArr(v any) []any {
	a, _ := v.([]any)
	return a
}

func jBool(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func jInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true
		}
	}
	return 0, false
}

func jsonTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// dedupStrings 排序 + 去重（日志与测试断言需要稳定顺序）。
func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sorted := append([]string{}, in...)
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted))
	for _, s := range sorted {
		if s == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ---------- responses 请求 → chat 请求 ----------

// responsesChatRequest 把 Responses 请求体转换成 chat/completions 请求体。
// 返回值：chat body / custom 工具名集合（响应方向据此还原 custom_tool_call）/ 被忽略字段清单。
func responsesChatRequest(req map[string]any, store *responsesStore) (map[string]any, map[string]bool, []string, error) {
	dropped := []string{}
	inputItems := responsesInputItems(req["input"])

	// 工具声明：顶层 tools + Codex Desktop（Responses Lite）放在 input 里的
	// additional_tools 项（两处合并，否则 Lite 客户端工具全丢）。
	rawTools := append([]any{}, jArr(req["tools"])...)
	for _, it := range inputItems {
		if m := jMap(it); m != nil && jStr(m["type"]) == "additional_tools" {
			rawTools = append(rawTools, jArr(m["tools"])...)
		}
	}
	tools, custom, td := responsesTools(rawTools)
	dropped = append(dropped, td...)

	messages := []any{}
	if inst := responsesInstructionsText(req["instructions"]); inst != "" {
		messages = append(messages, map[string]any{"role": "system", "content": inst})
	}
	if prev := strings.TrimSpace(jStr(req["previous_response_id"])); prev != "" {
		stored := store.get(prev)
		if stored == nil {
			return nil, custom, dropped, fmt.Errorf("previous_response_id %q not found (unknown or expired)", prev)
		}
		messages = append(messages, responsesItemsToMessages(
			responsesExpandReferences(stored.items, store, &dropped), custom, &dropped)...)
	}
	messages = append(messages, responsesItemsToMessages(
		responsesExpandReferences(inputItems, store, &dropped), custom, &dropped)...)

	chat := map[string]any{"model": jStr(req["model"]), "messages": messages}
	if jBool(req["stream"], false) {
		chat["stream"] = true
	}
	if v, ok := req["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p", "user", "parallel_tool_calls"} {
		if v, ok := req[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if eff := responsesReasoningEffort(req["reasoning"]); eff != "" {
		chat["reasoning_effort"] = eff
	}
	if len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc := responsesToolChoice(req["tool_choice"]); tc != nil {
		chat["tool_choice"] = tc
	}
	if rf := responsesTextFormat(req["text"]); rf != nil {
		chat["response_format"] = rf
	}
	// 本网关 chat 通道没有对应语义的请求字段：记账后由调用方日志透出，
	// 不静默假装已支持（客户端据此可判断是否需要降级）。
	for _, k := range []string{"background", "conversation", "include", "max_tool_calls",
		"metadata", "prompt_cache_key", "safety_identifier", "service_tier", "top_logprobs", "truncation"} {
		if v, ok := req[k]; ok && jsonTruthy(v) {
			dropped = append(dropped, k)
		}
	}
	return chat, custom, dedupStrings(dropped), nil
}

// responsesInputItems 归一化 input：字符串 → 单条 user 文本项；数组 → 原样。
func responsesInputItems(raw any) []any {
	switch t := raw.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []any{t}
	case []any:
		return t
	}
	return nil
}

// responsesExpandReferences 展开 input 里的 item_reference（指向先前响应存储中的条目）。
// 找不到（未存/过期）时丢弃并记账，不编造内容。
func responsesExpandReferences(items []any, store *responsesStore, dropped *[]string) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		if m := jMap(it); m != nil && jStr(m["type"]) == "item_reference" {
			if found := store.findItem(strings.TrimSpace(jStr(m["id"]))); found != nil {
				out = append(out, found)
				continue
			}
			*dropped = append(*dropped, "input.item_reference")
			continue
		}
		out = append(out, it)
	}
	return out
}

// responsesInstructionsText 提取 instructions（字符串或 input_text 部件数组）为纯文本。
func responsesInstructionsText(raw any) string {
	switch t := raw.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		var b strings.Builder
		for _, p := range t {
			switch part := p.(type) {
			case string:
				b.WriteString(part)
			case map[string]any:
				switch jStr(part["type"]) {
				case "input_text", "output_text", "text":
					b.WriteString(jStr(part["text"]))
				}
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// responsesReasoningEffort 映射 reasoning.effort → chat reasoning_effort。
// none 语义（关思考）在本网关上游取值为 off（upstream effortRank 的零档）。
func responsesReasoningEffort(raw any) string {
	m := jMap(raw)
	if m == nil {
		return ""
	}
	eff := strings.ToLower(strings.TrimSpace(jStr(m["effort"])))
	if eff == "none" {
		return "off"
	}
	return eff
}

// responsesTools 转换工具声明。
//
//	function（含旧式无 type）→ chat function 原样直译
//	custom（Codex 自由格式工具）→ chat function，参数固定为 {"input": string}
//	namespace（Codex Desktop 分组）→ 展开子工具（本地名），分组名记账
//
// 其余类型（web_search_preview/file_search/computer_use/local_shell 等）本网关
// 没有对应上游能力 → 丢弃并记账。
func responsesTools(raw []any) ([]any, map[string]bool, []string) {
	custom := map[string]bool{}
	dropped := []string{}
	out := make([]any, 0, len(raw))
	seen := map[string]bool{}

	add := func(tool map[string]any, ns string) {
		typ := strings.TrimSpace(jStr(tool["type"]))
		// 不支持的工具类型先记账退出：这类声明没有本网关可用的对应能力，
		// 名称校验/命名冲突记账对它没有意义（也更便于客户端定位被丢的是什么）。
		if typ != "" && typ != "function" && typ != "custom" {
			dropped = append(dropped, "tools."+typ)
			return
		}
		name := strings.TrimSpace(jStr(tool["name"]))
		if name == "" {
			name = strings.TrimSpace(jStr(jMap(tool["function"])["name"]))
		}
		if name == "" {
			dropped = append(dropped, "tools.unnamed")
			return
		}
		if ns != "" {
			dropped = append(dropped, "tools.namespace:"+ns)
		}
		fn := map[string]any{"name": name}
		if d := tool["description"]; d != nil {
			fn["description"] = d
		} else if f := jMap(tool["function"]); f != nil && f["description"] != nil {
			fn["description"] = f["description"]
		}
		switch typ {
		case "function":
			params := tool["parameters"]
			if params == nil {
				params = tool["parametersJsonSchema"]
			}
			if params == nil {
				params = tool["input_schema"]
			}
			if params == nil {
				if f := jMap(tool["function"]); f != nil {
					params = f["parameters"]
				}
			}
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			fn["parameters"] = params
			if s, ok := tool["strict"]; ok && s != nil {
				fn["strict"] = s
			}
		case "custom":
			fn["parameters"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{"input": map[string]any{"type": "string"}},
				"required":   []any{"input"},
			}
			custom[name] = true
		}
		if seen[name] {
			dropped = append(dropped, "tools.duplicate:"+name)
			return
		}
		seen[name] = true
		out = append(out, map[string]any{"type": "function", "function": fn})
	}

	for _, t := range raw {
		tool := jMap(t)
		if tool == nil {
			dropped = append(dropped, "tools.item")
			continue
		}
		if strings.TrimSpace(jStr(tool["type"])) == "namespace" {
			ns := strings.TrimSpace(jStr(tool["name"]))
			for _, child := range jArr(tool["tools"]) {
				if cm := jMap(child); cm != nil {
					add(cm, ns)
				}
			}
			continue
		}
		add(tool, "")
	}
	return out, custom, dedupStrings(dropped)
}

// responsesToolChoice 映射 tool_choice：字符串直译；{type:function|custom|tool,name}
// → chat 的 function 形态；{type:allowed_tools} → auto。
func responsesToolChoice(raw any) any {
	switch t := raw.(type) {
	case string:
		switch t {
		case "auto", "none", "required":
			return t
		}
		return nil
	case map[string]any:
		name := strings.TrimSpace(jStr(t["name"]))
		if name == "" {
			name = strings.TrimSpace(jStr(jMap(t["function"])["name"]))
		}
		switch jStr(t["type"]) {
		case "function", "custom", "tool":
			if name == "" {
				return nil
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		case "allowed_tools", "auto", "none", "required":
			return "auto"
		}
	}
	return nil
}

// responsesTextFormat 映射 text.format → chat response_format。
func responsesTextFormat(raw any) any {
	m := jMap(raw)
	if m == nil {
		return nil
	}
	f := jMap(m["format"])
	if f == nil {
		return nil
	}
	switch jStr(f["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{"name": "responses"}
		if n := strings.TrimSpace(jStr(f["name"])); n != "" {
			js["name"] = n
		}
		if s, ok := f["schema"]; ok && s != nil {
			js["schema"] = s
		}
		if st, ok := f["strict"]; ok && st != nil {
			js["strict"] = st
		}
		if d := strings.TrimSpace(jStr(f["description"])); d != "" {
			js["description"] = d
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}

// responsesContent 转换消息内容：字符串原样；部件数组 → chat 部件数组。
// 图片直译；文件类部件（input_file/file_url/file_data）本网关无对应能力 → 记账丢弃。
func responsesContent(raw any, dropped *[]string) any {
	switch t := raw.(type) {
	case string:
		return t
	case []any:
		parts := make([]any, 0, len(t))
		textOnly := true
		var lastText string
		for _, p := range t {
			part := jMap(p)
			if part == nil {
				*dropped = append(*dropped, "input.content.item")
				continue
			}
			switch jStr(part["type"]) {
			case "input_text", "output_text", "text", "summary_text":
				txt := jStr(part["text"])
				lastText = txt
				parts = append(parts, map[string]any{"type": "text", "text": txt})
			case "refusal":
				txt := jStr(part["refusal"])
				lastText = txt
				parts = append(parts, map[string]any{"type": "text", "text": txt})
			case "input_image", "image_url":
				textOnly = false
				url := strings.TrimSpace(jStr(part["image_url"]))
				detail := ""
				if im := jMap(part["image_url"]); im != nil {
					url = strings.TrimSpace(jStr(im["url"]))
					detail = strings.TrimSpace(jStr(im["detail"]))
				}
				if url == "" {
					*dropped = append(*dropped, "input.image_url_empty")
					continue
				}
				img := map[string]any{"url": url}
				if detail != "" {
					img["detail"] = detail
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
			case "input_file", "file_url", "file_data", "file_id":
				*dropped = append(*dropped, "input.file")
			default:
				*dropped = append(*dropped, "input.content."+jStr(part["type"]))
			}
		}
		if len(parts) == 0 {
			return ""
		}
		if textOnly && len(parts) == 1 {
			return lastText // 纯文本单部件：回落字符串形态，兼容最严格的上游
		}
		return parts
	}
	return ""
}

// responsesReasoningText 汇总 reasoning item 的 summary/content 文本（回灌为
// assistant 的 reasoning_content，供推理模型多轮续接）。
func responsesReasoningText(item map[string]any) string {
	var b strings.Builder
	for _, group := range []string{"summary", "content"} {
		for _, s := range jArr(item[group]) {
			if m := jMap(s); m != nil {
				b.WriteString(jStr(m["text"]))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// responsesToolOutput 工具输出取文本：字符串原样；部件数组取 text 汇总；
// 其他形态序列化回 JSON（不丢信息）。
func responsesToolOutput(raw any) string {
	switch t := raw.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m := jMap(p); m != nil {
				b.WriteString(jStr(m["text"]))
			}
		}
		return b.String()
	}
	if raw2, err := json.Marshal(raw); err == nil {
		return string(raw2)
	}
	return ""
}

// responsesItemsToMessages 把 Responses input items 转换为 chat messages。
// 连续 function_call/custom_tool_call 合并进同一条 assistant 消息（chat 工具调用约定）；
// tool 输出成为 role=tool 消息；reasoning 文本挂到下一条 assistant 消息的
// reasoning_content（网关自身也会补该字段，形态与其它客户端一致）。
func responsesItemsToMessages(items []any, custom map[string]bool, dropped *[]string) []any {
	out := []any{}
	var pendingCalls []any
	pendingReason := ""

	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": pendingCalls}
		if pendingReason != "" {
			msg["reasoning_content"] = pendingReason
			pendingReason = ""
		}
		out = append(out, msg)
		pendingCalls = nil
	}

	for _, raw := range items {
		switch it := raw.(type) {
		case string:
			flushCalls()
			out = append(out, map[string]any{"role": "user", "content": it})
		case map[string]any:
			typ := strings.TrimSpace(jStr(it["type"]))
			switch typ {
			case "", "message":
				role := strings.TrimSpace(jStr(it["role"]))
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system" // chat 通道统一用 system
				}
				flushCalls()
				msg := map[string]any{"role": role}
				if c, ok := it["content"]; ok {
					msg["content"] = responsesContent(c, dropped)
				} else {
					msg["content"] = ""
				}
				if role == "assistant" && pendingReason != "" {
					msg["reasoning_content"] = pendingReason
					pendingReason = ""
				}
				out = append(out, msg)
			case "function_call", "custom_tool_call":
				callID := strings.TrimSpace(jStr(it["call_id"]))
				if callID == "" {
					callID = strings.TrimSpace(jStr(it["id"]))
				}
				name := strings.TrimSpace(jStr(it["name"]))
				args := jStr(it["arguments"])
				if typ == "custom_tool_call" {
					// Codex 自由格式调用回灌：包成 {"input": <原文>}，与工具声明
					// 转换后的 {"input": string} 参数形态对齐。
					wrapped, err := json.Marshal(map[string]any{"input": jStr(it["input"])})
					if err != nil {
						wrapped = []byte(`{"input":""}`)
					}
					args = string(wrapped)
				}
				pendingCalls = append(pendingCalls, map[string]any{
					"id":       callID,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "function_call_output", "custom_tool_call_output":
				flushCalls()
				callID := strings.TrimSpace(jStr(it["call_id"]))
				if callID == "" {
					callID = strings.TrimSpace(jStr(it["id"]))
				}
				if callID == "" {
					// 无 call_id 的孤儿输出：转成 user 文本，不伪造 tool_call_id
					text := responsesToolOutput(it["output"])
					if text != "" {
						out = append(out, map[string]any{"role": "user", "content": text})
					}
					*dropped = append(*dropped, "input.tool_output_without_call_id")
					continue
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      responsesToolOutput(it["output"]),
				})
			case "reasoning":
				if txt := responsesReasoningText(it); txt != "" {
					pendingReason = txt
				}
			case "additional_tools":
				// 工具声明已合并进 chat tools，不产生消息
			default:
				*dropped = append(*dropped, "input."+typ)
			}
		default:
			*dropped = append(*dropped, "input.item")
		}
	}
	flushCalls()
	return out
}

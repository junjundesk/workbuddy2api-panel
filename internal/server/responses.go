// responses.go —— POST /v1/responses（OpenAI Responses API）兼容层。
//
// 设计（对齐 router-for-me/CLIProxyAPI 的 translator 分层，本仓库落成适配器形态）：
//
//	客户端 Responses 请求
//	  → responsesChatRequest()                     转成 chat/completions 请求体
//	  → h.chatCompletions(适配 writer, 合成请求)     复用现有 chat 全链路
//	  → responsesWriter 把 chat 输出实时改写成 Responses 形态（JSON / SSE 事件）
//
// 复用 chat 管线的好处：选号轮换、会话粘性、自有提示词改写、逐请求用量与成本台账、
// 错误分类与软冷却、WAF IP 门、gateway_hint 全部与 /v1/chat/completions 同源，
// 不存在第二套实现漂移；chat 主链路零改动（只在 NewHandler 里多挂两条路由）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// responsesStoreTTL 响应存储存活时长（previous_response_id / item_reference /
	// GET /v1/responses/{id} 的后端）。进程内内存存储：多副本部署时粘性会话才有意义，
	// 与池内粘性路由同口径（单副本或共享 Redis 之外的场景不做跨副本共享）。
	responsesStoreTTL = time.Hour
	// responsesStoreCap 存储条目上限（LRU 淘汰）。
	responsesStoreCap = 512
)

// ---------- 响应存储 ----------

type storedResponses struct {
	items   []any
	full    map[string]any
	created time.Time
}

type responsesStore struct {
	mu    sync.Mutex
	byID  map[string]*storedResponses
	order []string
}

func newResponsesStore() *responsesStore {
	return &responsesStore{byID: map[string]*storedResponses{}}
}

func (s *responsesStore) put(id string, items []any, full map[string]any) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	s.byID[id] = &storedResponses{items: items, full: full, created: time.Now()}
	s.order = append(s.order, id)
}

// evictLocked 过期清理 + 容量淘汰（调用方持锁）。
func (s *responsesStore) evictLocked() {
	now := time.Now()
	kept := s.order[:0]
	for _, id := range s.order {
		st := s.byID[id]
		if st == nil {
			continue
		}
		if now.Sub(st.created) > responsesStoreTTL {
			delete(s.byID, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	for len(s.order) > responsesStoreCap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest)
	}
}

func (s *responsesStore) get(id string) *storedResponses {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.byID[id]
	if st == nil {
		return nil
	}
	if time.Since(st.created) > responsesStoreTTL {
		delete(s.byID, id)
		return nil
	}
	return st
}

// findItem 按 item id 反查（item_reference 展开）。存储规模有界（<=512 响应），
// 直接线性扫描，不额外维护反向索引。
func (s *responsesStore) findItem(id string) any {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.byID {
		for _, it := range st.items {
			if m := jMap(it); m != nil && jStr(m["id"]) == id {
				return it
			}
		}
	}
	return nil
}

// ---------- 单次请求的转换状态 ----------

type responsesUsage struct {
	input     int64
	output    int64
	total     int64
	cached    int64
	reasoning int64
	seen      bool
}

type responsesToolState struct {
	itemID string
	callID string
	name   string
	outIdx int
	custom bool
	done   bool
	args   strings.Builder
}

type responsesRun struct {
	req       map[string]any
	id        string
	model     string
	createdAt int64
	custom    map[string]bool
	stream    bool
	persist   bool
	dropped   []string

	seq     int
	nextOut int

	msgItemID string
	msgOutIdx int
	msgAdded  bool
	msgDone   bool
	msgText   strings.Builder

	reasonItemID string
	reasonOutIdx int
	reasonAdded  bool
	reasonDone   bool
	reasonText   strings.Builder

	tools     map[int]*responsesToolState
	toolOrder []int

	items map[int]map[string]any

	usage        responsesUsage
	finishReason string
	started      bool
	completed    bool
	failed       bool
	final        map[string]any
}

var responsesIDCounter uint64

func newResponsesID() string {
	return fmt.Sprintf("resp_%x%03x", time.Now().UnixNano(), atomic.AddUint64(&responsesIDCounter, 1)&0xfff)
}

func newResponsesRun(req map[string]any, custom map[string]bool, dropped []string) *responsesRun {
	return &responsesRun{
		req:       req,
		id:        newResponsesID(),
		model:     jStr(req["model"]),
		createdAt: time.Now().Unix(),
		custom:    custom,
		stream:    jBool(req["stream"], false),
		persist:   jBool(req["store"], true),
		dropped:   dropped,
		tools:     map[int]*responsesToolState{},
		items:     map[int]map[string]any{},
	}
}

// ---------- 事件发射 ----------

// writeEvent 写一帧 Responses SSE（event: <type> + data: <json>，含 sequence_number）。
func (t *responsesRun) writeEvent(out io.Writer, event string, payload map[string]any) error {
	payload["type"] = event
	payload["sequence_number"] = t.seq
	t.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return nil
}

// scaffold 构造 response 对象骨架（created/in_progress/completed 共用），
// 请求侧字段原样回显（OpenAI 语义），未传入的字段用规范默认值。
func (t *responsesRun) scaffold(status string) map[string]any {
	resp := map[string]any{
		"id":                   t.id,
		"object":               "response",
		"created_at":           t.createdAt,
		"status":               status,
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                t.model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            nil,
		"store":                t.persist,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
	for _, k := range []string{"instructions", "max_output_tokens", "previous_response_id",
		"reasoning", "temperature", "top_p", "text", "tool_choice", "tools",
		"parallel_tool_calls", "store", "metadata", "user", "truncation", "max_tool_calls",
		"prompt_cache_key", "safety_identifier", "service_tier", "top_logprobs"} {
		if v, ok := t.req[k]; ok && v != nil {
			resp[k] = v
		}
	}
	return resp
}

func (t *responsesRun) emitStarted(out io.Writer) error {
	if t.started {
		return nil
	}
	t.started = true
	if err := t.writeEvent(out, "response.created", map[string]any{
		"response": t.scaffold("in_progress"),
	}); err != nil {
		return err
	}
	return t.writeEvent(out, "response.in_progress", map[string]any{
		"response": t.scaffold("in_progress"),
	})
}

// ---------- 输出条目 ----------

func (t *responsesRun) addItem(idx int, item map[string]any) {
	if item == nil {
		return
	}
	t.items[idx] = item
}

// outputList 按 output_index 升序输出条目（事件到达顺序与索引可能不同，
// 例如先出 tool_call 再出正文）。
func (t *responsesRun) outputList() []any {
	idxs := make([]int, 0, len(t.items))
	for i := range t.items {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]any, 0, len(idxs))
	for _, i := range idxs {
		if it := t.items[i]; it != nil {
			out = append(out, it)
		}
	}
	return out
}

func messageItem(id, status, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"status": status,
		"role":   "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		},
	}
}

func reasoningItem(id, text string) map[string]any {
	summary := []any{}
	if text != "" {
		summary = append(summary, map[string]any{"type": "summary_text", "text": text})
	}
	return map[string]any{"id": id, "type": "reasoning", "summary": summary}
}

func functionCallItem(id, status, callID, name, args string) map[string]any {
	return map[string]any{
		"id": id, "type": "function_call", "status": status,
		"call_id": callID, "name": name, "arguments": args,
	}
}

func customCallItem(id, status, callID, name, input string) map[string]any {
	return map[string]any{
		"id": id, "type": "custom_tool_call", "status": status,
		"call_id": callID, "name": name, "input": input,
	}
}

// unwrapCustomInput 反解自由格式工具的 {"input": "..."} 包装（非包装形态原样返回）。
func unwrapCustomInput(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) == nil {
		if v, ok := m["input"].(string); ok {
			return v
		}
	}
	return args
}

// ---------- 流式：增量与收尾 ----------

func (t *responsesRun) ensureMessage(out io.Writer) error {
	if t.msgAdded {
		return nil
	}
	t.msgAdded = true
	t.msgItemID = "msg_" + strings.TrimPrefix(t.id, "resp_")
	t.msgOutIdx = t.nextOut
	t.nextOut++
	item := map[string]any{
		"id": t.msgItemID, "type": "message", "status": "in_progress",
		"role": "assistant", "content": []any{},
	}
	if err := t.writeEvent(out, "response.output_item.added", map[string]any{
		"output_index": t.msgOutIdx, "item": item,
	}); err != nil {
		return err
	}
	return t.writeEvent(out, "response.content_part.added", map[string]any{
		"item_id": t.msgItemID, "output_index": t.msgOutIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (t *responsesRun) textDelta(out io.Writer, delta string) error {
	if delta == "" {
		return nil
	}
	if err := t.ensureMessage(out); err != nil {
		return err
	}
	t.msgText.WriteString(delta)
	return t.writeEvent(out, "response.output_text.delta", map[string]any{
		"item_id": t.msgItemID, "output_index": t.msgOutIdx, "content_index": 0, "delta": delta,
	})
}

func (t *responsesRun) ensureReasoning(out io.Writer) error {
	if t.reasonAdded {
		return nil
	}
	t.reasonAdded = true
	t.reasonItemID = "rs_" + strings.TrimPrefix(t.id, "resp_")
	t.reasonOutIdx = t.nextOut
	t.nextOut++
	item := map[string]any{"id": t.reasonItemID, "type": "reasoning", "summary": []any{}}
	if err := t.writeEvent(out, "response.output_item.added", map[string]any{
		"output_index": t.reasonOutIdx, "item": item,
	}); err != nil {
		return err
	}
	return t.writeEvent(out, "response.reasoning_summary_part.added", map[string]any{
		"item_id": t.reasonItemID, "output_index": t.reasonOutIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (t *responsesRun) reasoningDelta(out io.Writer, delta string) error {
	if delta == "" {
		return nil
	}
	if err := t.ensureReasoning(out); err != nil {
		return err
	}
	t.reasonText.WriteString(delta)
	return t.writeEvent(out, "response.reasoning_summary_text.delta", map[string]any{
		"item_id": t.reasonItemID, "output_index": t.reasonOutIdx, "summary_index": 0, "delta": delta,
	})
}

// toolDelta 累积并转发工具调用增量。custom 工具的参数需要反解包装，无法增量
// 发确定性片段 → 只累积，收尾时一次性发 delta + done（协议允许）。
func (t *responsesRun) toolDelta(out io.Writer, idx int, callID, name, args string) error {
	st := t.tools[idx]
	if st == nil {
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", strings.TrimPrefix(t.id, "resp_"), idx)
		}
		st = &responsesToolState{
			callID: callID,
			name:   name,
			outIdx: t.nextOut,
			custom: t.custom[name],
		}
		t.nextOut++
		if st.custom {
			st.itemID = "ctc_" + st.callID
		} else {
			st.itemID = "fc_" + st.callID
		}
		t.tools[idx] = st
		t.toolOrder = append(t.toolOrder, idx)
		item := map[string]any{
			"id": st.itemID, "type": "function_call", "status": "in_progress",
			"call_id": st.callID, "name": name, "arguments": "",
		}
		if st.custom {
			item["type"] = "custom_tool_call"
			item["input"] = ""
			delete(item, "arguments")
		}
		if err := t.writeEvent(out, "response.output_item.added", map[string]any{
			"output_index": st.outIdx, "item": item,
		}); err != nil {
			return err
		}
	}
	if name != "" && st.name == "" {
		st.name = name
	}
	if args == "" {
		return nil
	}
	st.args.WriteString(args)
	if st.custom {
		return nil
	}
	return t.writeEvent(out, "response.function_call_arguments.delta", map[string]any{
		"item_id": st.itemID, "output_index": st.outIdx, "delta": args,
	})
}

// finalizeItems 收尾所有打开的输出条目（done 事件 + 完成态 item）。
func (t *responsesRun) finalizeItems(out io.Writer, status string) error {
	if t.reasonAdded && !t.reasonDone {
		text := t.reasonText.String()
		if err := t.writeEvent(out, "response.reasoning_summary_text.done", map[string]any{
			"item_id": t.reasonItemID, "output_index": t.reasonOutIdx, "summary_index": 0, "text": text,
		}); err != nil {
			return err
		}
		if err := t.writeEvent(out, "response.reasoning_summary_part.done", map[string]any{
			"item_id": t.reasonItemID, "output_index": t.reasonOutIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		}); err != nil {
			return err
		}
		item := reasoningItem(t.reasonItemID, text)
		if err := t.writeEvent(out, "response.output_item.done", map[string]any{
			"output_index": t.reasonOutIdx, "item": item,
		}); err != nil {
			return err
		}
		t.reasonDone = true
		t.addItem(t.reasonOutIdx, item)
	}
	if t.msgAdded && !t.msgDone {
		text := t.msgText.String()
		if err := t.writeEvent(out, "response.output_text.done", map[string]any{
			"item_id": t.msgItemID, "output_index": t.msgOutIdx, "content_index": 0, "text": text,
		}); err != nil {
			return err
		}
		if err := t.writeEvent(out, "response.content_part.done", map[string]any{
			"item_id": t.msgItemID, "output_index": t.msgOutIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := messageItem(t.msgItemID, status, text)
		if err := t.writeEvent(out, "response.output_item.done", map[string]any{
			"output_index": t.msgOutIdx, "item": item,
		}); err != nil {
			return err
		}
		t.msgDone = true
		t.addItem(t.msgOutIdx, item)
	}
	for _, idx := range t.toolOrder {
		st := t.tools[idx]
		if st == nil || st.done {
			continue
		}
		st.done = true
		name := st.name
		if st.custom {
			input := unwrapCustomInput(st.args.String())
			if err := t.writeEvent(out, "response.custom_tool_call_input.delta", map[string]any{
				"item_id": st.itemID, "output_index": st.outIdx, "delta": input,
			}); err != nil {
				return err
			}
			if err := t.writeEvent(out, "response.custom_tool_call_input.done", map[string]any{
				"item_id": st.itemID, "output_index": st.outIdx, "input": input,
			}); err != nil {
				return err
			}
			item := customCallItem(st.itemID, status, st.callID, name, input)
			if err := t.writeEvent(out, "response.output_item.done", map[string]any{
				"output_index": st.outIdx, "item": item,
			}); err != nil {
				return err
			}
			t.addItem(st.outIdx, item)
			continue
		}
		args := st.args.String()
		if err := t.writeEvent(out, "response.function_call_arguments.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outIdx, "arguments": args,
		}); err != nil {
			return err
		}
		item := functionCallItem(st.itemID, status, st.callID, name, args)
		if err := t.writeEvent(out, "response.output_item.done", map[string]any{
			"output_index": st.outIdx, "item": item,
		}); err != nil {
			return err
		}
		t.addItem(st.outIdx, item)
	}
	return nil
}

// usageObject 映射 usage（未观测到 → nil，不伪造零值）。
func (t *responsesRun) usageObject() map[string]any {
	if !t.usage.seen {
		return nil
	}
	return map[string]any{
		"input_tokens":          t.usage.input,
		"output_tokens":         t.usage.output,
		"total_tokens":          t.usage.total,
		"input_tokens_details":  map[string]any{"cached_tokens": t.usage.cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": t.usage.reasoning},
	}
}

func (t *responsesRun) absorbUsage(u map[string]any) {
	if u == nil {
		return
	}
	seen := false
	hasTotal := false
	if v, ok := jInt(u["prompt_tokens"]); ok {
		t.usage.input, seen = v, true
	} else if v, ok := jInt(u["input_tokens"]); ok {
		t.usage.input, seen = v, true
	}
	if v, ok := jInt(u["completion_tokens"]); ok {
		t.usage.output, seen = v, true
	} else if v, ok := jInt(u["output_tokens"]); ok {
		t.usage.output, seen = v, true
	}
	if v, ok := jInt(u["total_tokens"]); ok {
		t.usage.total, seen, hasTotal = v, true, true
	}
	if d := jMap(u["prompt_tokens_details"]); d != nil {
		if v, ok := jInt(d["cached_tokens"]); ok {
			t.usage.cached, seen = v, true
		}
	}
	if d := jMap(u["completion_tokens_details"]); d != nil {
		if v, ok := jInt(d["reasoning_tokens"]); ok {
			t.usage.reasoning, seen = v, true
		}
	}
	if !seen {
		return
	}
	t.usage.seen = true
	if !hasTotal {
		t.usage.total = t.usage.input + t.usage.output
	}
}

// terminalStatus 由 finish_reason 推导响应/条目状态。
func (t *responsesRun) terminalStatus() (status string, event string, reason string) {
	switch t.finishReason {
	case "length", "max_tokens":
		return "incomplete", "response.incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "response.incomplete", "content_filter"
	}
	return "completed", "response.completed", ""
}

func (t *responsesRun) buildFinal(status, event, reason string, errObj map[string]any) map[string]any {
	resp := t.scaffold(status)
	resp["output"] = t.outputList()
	if reason != "" {
		resp["incomplete_details"] = map[string]any{"reason": reason}
	}
	if errObj != nil {
		resp["error"] = errObj
	}
	if u := t.usageObject(); u != nil {
		resp["usage"] = u
	}
	t.final = resp
	_ = event
	return resp
}

// emitCompleted 收尾流：done 事件 + response.completed / response.incomplete。
func (t *responsesRun) emitCompleted(out io.Writer) error {
	if t.completed {
		return nil
	}
	t.completed = true
	status, event, reason := t.terminalStatus()
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	if t.started {
		if err := t.finalizeItems(out, itemStatus); err != nil {
			return err
		}
	}
	resp := t.buildFinal(status, event, reason, nil)
	return t.writeEvent(out, event, map[string]any{"response": resp})
}

// emitFailed 上游 error 帧 / 中途失败 → response.failed（error 原文透传）。
func (t *responsesRun) emitFailed(out io.Writer, errObj map[string]any) error {
	if t.completed {
		return nil
	}
	t.completed = true
	t.failed = true
	if t.started {
		_ = t.finalizeItems(out, "incomplete")
	}
	resp := t.buildFinal("failed", "response.failed", "", errObj)
	return t.writeEvent(out, "response.failed", map[string]any{"response": resp})
}

// absorbChatMessage 非流式：把 chat 完成态的 message 吸收成 responses 输出条目。
func (t *responsesRun) absorbChatMessage(msg map[string]any) {
	if msg == nil {
		return
	}
	if rc := jStr(msg["reasoning_content"]); rc != "" {
		t.reasonAdded = true
		t.reasonItemID = "rs_" + strings.TrimPrefix(t.id, "resp_")
		t.reasonOutIdx = t.nextOut
		t.nextOut++
		t.reasonText.WriteString(rc)
	}
	if txt := jStr(msg["content"]); txt != "" {
		t.msgAdded = true
		t.msgItemID = "msg_" + strings.TrimPrefix(t.id, "resp_")
		t.msgOutIdx = t.nextOut
		t.nextOut++
		t.msgText.WriteString(txt)
	}
	for i, raw := range jArr(msg["tool_calls"]) {
		tc := jMap(raw)
		if tc == nil {
			continue
		}
		fn := jMap(tc["function"])
		name := ""
		args := ""
		if fn != nil {
			name = jStr(fn["name"])
			args = jStr(fn["arguments"])
		}
		callID := jStr(tc["id"])
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", strings.TrimPrefix(t.id, "resp_"), i)
		}
		st := &responsesToolState{
			callID: callID,
			name:   name,
			outIdx: t.nextOut,
			custom: t.custom[name],
			done:   true,
		}
		t.nextOut++
		if st.custom {
			st.itemID = "ctc_" + callID
		} else {
			st.itemID = "fc_" + callID
		}
		st.args.WriteString(args)
		t.tools[i] = st
		t.toolOrder = append(t.toolOrder, i)
	}
}

// absorbChatCompletion 非流式：chat 完成态 → responses 响应对象。
func (t *responsesRun) absorbChatCompletion(chat map[string]any) map[string]any {
	t.absorbUsage(jMap(chat["usage"]))
	if choices := jArr(chat["choices"]); len(choices) > 0 {
		if ch := jMap(choices[0]); ch != nil {
			t.finishReason = jStr(ch["finish_reason"])
			t.absorbChatMessage(jMap(ch["message"]))
		}
	}
	status, event, reason := t.terminalStatus()
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	if t.reasonAdded {
		t.addItem(t.reasonOutIdx, reasoningItem(t.reasonItemID, t.reasonText.String()))
	}
	if t.msgAdded {
		t.addItem(t.msgOutIdx, messageItem(t.msgItemID, itemStatus, t.msgText.String()))
	}
	for _, idx := range t.toolOrder {
		st := t.tools[idx]
		if st == nil {
			continue
		}
		if st.custom {
			t.addItem(st.outIdx, customCallItem(st.itemID, itemStatus, st.callID, st.name, unwrapCustomInput(st.args.String())))
			continue
		}
		t.addItem(st.outIdx, functionCallItem(st.itemID, itemStatus, st.callID, st.name, st.args.String()))
	}
	return t.buildFinal(status, event, reason, nil)
}

// feedFrame 处理 chat SSE 的一帧 payload（已去掉 data: 前缀）。
func (t *responsesRun) feedFrame(out io.Writer, payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return t.emitCompleted(out)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil // 非 JSON 帧：忽略（上游噪声）
	}
	if errObj, hasErr := obj["error"]; hasErr && errObj != nil {
		m := jMap(errObj)
		if m == nil {
			m = map[string]any{"message": jStr(errObj), "type": "api_error"}
		}
		return t.emitFailed(out, m)
	}
	if t.completed {
		t.absorbUsage(jMap(obj["usage"])) // 收尾后的帧只补 usage
		return nil
	}
	if err := t.emitStarted(out); err != nil {
		return err
	}
	t.absorbUsage(jMap(obj["usage"]))
	choices := jArr(obj["choices"])
	if len(choices) == 0 {
		return nil
	}
	ch := jMap(choices[0])
	if ch == nil {
		return nil
	}
	if delta := jMap(ch["delta"]); delta != nil {
		if err := t.reasoningDelta(out, jStr(delta["reasoning_content"])); err != nil {
			return err
		}
		if err := t.textDelta(out, jStr(delta["content"])); err != nil {
			return err
		}
		if err := t.textDelta(out, jStr(delta["refusal"])); err != nil {
			return err
		}
		for _, raw := range jArr(delta["tool_calls"]) {
			tc := jMap(raw)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := jInt(tc["index"]); ok {
				idx = int(v)
			}
			fn := jMap(tc["function"])
			name := ""
			args := ""
			if fn != nil {
				name = jStr(fn["name"])
				args = jStr(fn["arguments"])
			}
			if err := t.toolDelta(out, idx, jStr(tc["id"]), name, args); err != nil {
				return err
			}
		}
	}
	if fr := jStr(ch["finish_reason"]); fr != "" {
		t.finishReason = fr
		return t.emitCompleted(out)
	}
	return nil
}

// ---------- 适配 writer ----------

const (
	respModeUnknown = iota
	respModeJSON
	respModeStream
	respModePass
)

// responsesWriter 接在 chat handler 与真实 ResponseWriter 之间：
// 上游是 SSE → 逐帧改写成 Responses 事件序列；聚合 JSON → 收尾时整体转换；
// 已判定为错误响应（>=400）→ 原样透传（OpenAI 错误对象两端同构）。
type responsesWriter struct {
	real       http.ResponseWriter
	flusher    http.Flusher
	run        *responsesRun
	mode       int
	status     int
	headerSent bool
	finalized  bool
	sseBuf     bytes.Buffer
	jsonBuf    bytes.Buffer
}

func newResponsesWriter(real http.ResponseWriter, run *responsesRun) *responsesWriter {
	fl, _ := real.(http.Flusher)
	return &responsesWriter{real: real, flusher: fl, run: run}
}

func (w *responsesWriter) Header() http.Header { return w.real.Header() }

func (w *responsesWriter) WriteHeader(code int) {
	if w.headerSent {
		return
	}
	w.headerSent = true
	w.status = code
	if code >= 400 {
		// 错误响应（含 no_healthy_account / upstream 透传）走原样透传：
		// Responses 与 chat 的错误对象同构（{"error":{message,type,code}}）。
		w.mode = respModePass
		w.real.WriteHeader(code)
	}
}

func (w *responsesWriter) Write(p []byte) (int, error) {
	if !w.headerSent {
		w.headerSent = true
		w.status = http.StatusOK
	}
	switch w.mode {
	case respModePass:
		return w.real.Write(p)
	case respModeJSON:
		w.jsonBuf.Write(p)
		return len(p), nil
	case respModeStream:
		if err := w.appendStream(p); err != nil {
			return 0, err
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
		return len(p), nil
	default:
		// 判定口径：本地聚合的 chat JSON 恒以 { / [ 开头；其余（data: 帧、event: 帧、
		// 以及上游真实存在的注释/心跳行 ": heartbeat"）一律按 SSE 处理——只看
		// "data:" 前缀会被前导心跳行骗到 JSON 分支（真机实测踩到过）。
		trimmed := bytes.TrimLeft(p, " \t\r\n")
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			w.mode = respModeJSON
			w.jsonBuf.Write(p)
			return len(p), nil
		}
		w.mode = respModeStream
		if err := w.appendStream(p); err != nil {
			return 0, err
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
		return len(p), nil
	}
}

// appendStream 按 SSE 空行切帧，逐帧交给 run 转换。
func (w *responsesWriter) appendStream(p []byte) error {
	w.sseBuf.Write(p)
	for {
		raw := w.sseBuf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := string(raw[:idx])
		rest := append([]byte{}, raw[idx+2:]...)
		w.sseBuf.Reset()
		w.sseBuf.Write(rest)
		payload := sseFramePayload(frame)
		if payload == "" {
			continue
		}
		if err := w.run.feedFrame(w.real, payload); err != nil {
			return err
		}
	}
	return nil
}

// sseFramePayload 从一帧 SSE 中取 data 负载（多行 data 按 SSE 规范以换行拼接）。
func sseFramePayload(frame string) string {
	var lines []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// finalize 幂等收尾：补发缺失的 completed / 转换聚合 JSON / 兜底错误。
func (w *responsesWriter) finalize() {
	if w.finalized {
		return
	}
	w.finalized = true
	switch w.mode {
	case respModeStream:
		if err := w.run.emitCompleted(w.real); err != nil {
			log.Printf("[responses] stream finalize: %v", err)
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
	case respModeJSON:
		var chat map[string]any
		if json.Unmarshal(w.jsonBuf.Bytes(), &chat) != nil {
			writeOpenAIError(w.real, http.StatusBadGateway, "upstream_parse", "unexpected upstream response shape")
			return
		}
		writeJSON(w.real, http.StatusOK, w.run.absorbChatCompletion(chat))
	case respModePass:
		// 已透传，无需处理
	default:
		writeOpenAIError(w.real, http.StatusBadGateway, "upstream_parse", "empty upstream response")
	}
}

// ---------- HTTP 入口 ----------

// responses POST /v1/responses：Responses 请求 → chat 管线 → Responses 响应。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	chat, custom, dropped, err := responsesChatRequest(req, h.respStore)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(dropped) > 0 {
		log.Printf("[responses] ignored request fields: %s", strings.Join(dropped, ","))
	}
	run := newResponsesRun(req, custom, dropped)
	chatBody, err := json.Marshal(chat)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "encode chat body: "+err.Error())
		return
	}
	// 合成 chat 请求：转发原始 ctx / 头族（会话与 trace 头、客户端 IP），只换 body。
	synth := r.Clone(r.Context())
	synth.Body = io.NopCloser(bytes.NewReader(chatBody))
	synth.ContentLength = int64(len(chatBody))
	synth.Header.Set("Content-Type", "application/json")

	sw := newResponsesWriter(w, run)
	defer sw.finalize()
	h.chatCompletions(sw, synth)
	sw.finalize()

	if run.persist && run.final != nil {
		items := append(append([]any{}, run.inputItems()...), run.outputList()...)
		h.respStore.put(run.id, items, run.final)
	}
}

// getResponse GET /v1/responses/{id}：取回收存在内存里的响应（TTL 1h，最多 512 条）。
func (h *Handler) getResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st := h.respStore.get(id)
	if st == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"response "+id+" not found (in-memory store, TTL 1h)")
		return
	}
	writeJSON(w, http.StatusOK, st.full)
}

// inputItems 本次请求的原始 input items（含 additional_tools 之外的全部条目），
// 供响应存储续接 previous_response_id。
func (t *responsesRun) inputItems() []any {
	return responsesInputItems(t.req["input"])
}

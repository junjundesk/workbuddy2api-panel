package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归：同一入站对象在换号重试中复用时，每次出站改写必须互不污染。
// 2599eb5 之后对象被原地改写，第一次注入的 prompt_cache_key（含首号 uid）
// 会残留到第二个账号的请求里 → 换号后缓存键错号。
func TestPrepareBodyFromObject_RetryDoesNotLeakCacheKey(t *testing.T) {
	c := &Client{}
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), &obj); err != nil {
		t.Fatal(err)
	}
	keyOf := func(b []byte) string {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		s, _ := m["prompt_cache_key"].(string)
		return s
	}
	a := keyOf(c.prepareBodyFromObject(deepCopyJSON(obj).(map[string]any), "cn", "AAAAAAAA1111", "conv", false))
	b := keyOf(c.prepareBodyFromObject(deepCopyJSON(obj).(map[string]any), "cn", "BBBBBBBB2222", "conv", false))
	if !strings.HasPrefix(a, "wb2a-AAAAAAAA-") || !strings.HasPrefix(b, "wb2a-BBBBBBBB-") {
		t.Fatalf("cache key leaked across accounts: a=%q b=%q", a, b)
	}
	if _, ok := obj["prompt_cache_key"]; ok {
		t.Fatalf("caller object mutated: %v", obj)
	}
	if _, ok := obj["stream"]; ok {
		t.Fatalf("caller object mutated (stream): %v", obj)
	}
}

func TestDeepCopyJSON_Independent(t *testing.T) {
	src := map[string]any{"messages": []any{map[string]any{"role": "user", "tool_calls": []map[string]any{{"id": "x"}}}}}
	cp := deepCopyJSON(src).(map[string]any)
	cp["messages"].([]any)[0].(map[string]any)["role"] = "system"
	cp["messages"].([]any)[0].(map[string]any)["tool_calls"].([]map[string]any)[0]["id"] = "y"
	m := src["messages"].([]any)[0].(map[string]any)
	if m["role"] != "user" || m["tool_calls"].([]map[string]any)[0]["id"] != "x" {
		t.Fatalf("source mutated: %v", src)
	}
}

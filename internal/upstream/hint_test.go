package upstream

import (
	"strings"
	"testing"
)

// 14003 是模型级请求速率限流，hint 必须指向「换模型 / 稍后重试」，
// 不能用账号级 429 的「等 reset」——后者误导客户端整号不可用。
func TestGatewayHint14003ModelBusy(t *testing.T) {
	body := `{"code":14003,"msg":"too many requests","displayMsg":"Model busy. Please switch models or retry later","retry_policy":{"retry":true,"retry_after":30}}`
	hint := GatewayHint(ErrSoftRate, body, HintContext{})
	if !strings.Contains(hint, "switch models") {
		t.Fatalf("14003 hint should point at switching models, got %q", hint)
	}
	// 普通账号级 429（无 14003 业务码）保持原口径。
	plain := GatewayHint(ErrSoftRate, `{"code":42900,"msg":"rate limited"}`, HintContext{})
	if plain != "rate limited by upstream; retry after reset" {
		t.Fatalf("plain 429 hint changed: %q", plain)
	}
	// 文本里出现 14003 但不是 code 字段 → 不得误判（避免 traceId 撞号）。
	if got := GatewayHint(ErrSoftRate, `{"msg":"rate limited","requestId":"req-14003-abc"}`, HintContext{}); got != plain {
		t.Fatalf("substring 14003 must not switch hint, got %q", got)
	}
}

// 14003 的 SSE error 帧与 6004 一样直接判为 ErrSoftRate，即便帧内 message
// 不含 Classify 词表里的 "too many requests"。
func TestFrameKind14003(t *testing.T) {
	frame := `{"error":{"code":14003,"message":"模型繁忙，请稍后重试"}}`
	if got := FrameKind(frame); got != ErrSoftRate {
		t.Fatalf("14003 frame kind = %v, want ErrSoftRate", got)
	}
	if got := FrameKind(`{"error":{"code":6004,"message":"quota"}}`); got != ErrSoftRate {
		t.Fatalf("6004 frame kind = %v, want ErrSoftRate", got)
	}
	if got := FrameKind(`{"error":{"message":"invalid api key"}}`); got == ErrSoftRate {
		t.Fatalf("unrelated frame must not be ErrSoftRate")
	}
}

// 帧 → hint 全链：14003 帧经 FrameHintFunc 应产出模型繁忙文案。
func TestFrameHintFunc14003(t *testing.T) {
	fn := FrameHintFunc(func() HintContext { return HintContext{} })
	got := fn(`{"error":{"code":14003,"message":"model busy"}}`)
	if !strings.Contains(got, "switch models") {
		t.Fatalf("frame hint = %q, want model-busy hint", got)
	}
}

// request_rate_test.go 上游 14003「模型级请求速率限流」的处置口径：
// 只冷 (账号, 模型)，绝不写账号级 until。
//
// 背景：14003 的语义是「这个模型此刻太忙」（displayMsg: Model busy. Please switch
// models or retry later，body 带 retry_policy.retry_after 30s），同号换模型立刻可用。
// 旧实现把它当普通 429 走账号级软冷却，一次模型抖动就把整号所有模型摘掉
// 30s~600s（2026-09-23 上游风暴里 16 个号在 76 秒内全部阵亡的根因形态）。
package server

import (
	"testing"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// newRateHandler 构造只带一个健康号的最小 handler。
func newRateHandler(t *testing.T) *Handler {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: pl, Upstream: upstream.New()})
}

func rateStatus(t *testing.T, h *Handler) pool.Status {
	t.Helper()
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account not found in pool")
	}
	return st
}

// TestApplyErrorPolicy14003LocksOnlyModel 核心断言：账号级冷却零写入，
// 只有 (u1, 触发模型) 被冷冻约 30 秒。
func TestApplyErrorPolicy14003LocksOnlyModel(t *testing.T) {
	h := newRateHandler(t)
	body := `{"code":14003,"msg":"too many requests","displayMsg":"Model busy. Please switch models or retry later","retry_policy":{"retry":true,"retry_after":30}}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3", &upstream.Error{
		Kind: upstream.ErrSoftRate, Status: 429, Code: 14003, Msg: body,
	})

	st := rateStatus(t, h)
	if st.Cooling {
		t.Errorf("账号级 Cooling=true：14003 不该冷整号（until=%v kind=%s）", st.Until, st.CoolKind)
	}
	if !st.Until.IsZero() {
		t.Errorf("账号级 Until=%v 应保持零值", st.Until)
	}
	if st.SoftStreak != 0 {
		t.Errorf("SoftStreak=%d 应保持 0（账号级退避计数不得被模型级限流推动）", st.SoftStreak)
	}
	if st.Disabled {
		t.Error("14003 不是账号失效，不该禁用")
	}
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("台账行=%+v want 恰好一行", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	if row.Model != "glm-5.3" || row.Kind != "rate_limit" {
		t.Errorf("台账行=%+v want glm-5.3/rate_limit", row)
	}
	if rem := time.Until(row.Until); rem < 25*time.Second || rem > 31*time.Second {
		t.Errorf("冷却剩余=%v want ~30s（body retry_after）", rem)
	}
	if !row.ResetAt.IsZero() {
		t.Errorf("ResetAt=%v 应为零值（14003 只给相对时长）", row.ResetAt)
	}
}

// TestApplyErrorPolicy14003Clamp 时长夹取：上游脏值/极端值不得穿透。
func TestApplyErrorPolicy14003Clamp(t *testing.T) {
	cases := []struct {
		name string
		body string
		ue   *upstream.Error
		want time.Duration
	}{
		{"body 30s", `{"code":14003,"retry_policy":{"retry_after":30}}`, nil, 30 * time.Second},
		{"body 1s 抬到下限", `{"code":14003,"retry_policy":{"retry_after":1}}`, nil, 5 * time.Second},
		{"body 600s 压到上限", `{"code":14003,"retry_policy":{"retry_after":600}}`, nil, 2 * time.Minute},
		{"无 body 时长用响应头", `{"code":14003,"msg":"too many requests"}`, &upstream.Error{RetryAfter: 45 * time.Second}, 45 * time.Second},
		{"都没有用 30s 兜底", `{"code":14003,"msg":"too many requests"}`, &upstream.Error{}, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRateHandler(t)
			ue := c.ue
			if ue == nil {
				ue = &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Code: 14003, Msg: c.body}
			}
			h.applyErrorPolicy("u1", upstream.ErrSoftRate, c.body, "m1", ue)
			st := rateStatus(t, h)
			if len(st.RateLimitedModels) != 1 {
				t.Fatalf("台账行=%+v want 一行", st.RateLimitedModels)
			}
			rem := time.Until(st.RateLimitedModels[0].Until)
			if rem < c.want-3*time.Second || rem > c.want+2*time.Second {
				t.Errorf("冷却剩余=%v want ~%v", rem, c.want)
			}
			if st.Cooling || !st.Until.IsZero() {
				t.Errorf("账号级被写脏: cooling=%v until=%v", st.Cooling, st.Until)
			}
		})
	}
}

// TestApplyErrorPolicy14003DoesNotShorten6004 已有更长的 6004 墙钟冷却不被 14003 截短。
func TestApplyErrorPolicy14003DoesNotShorten6004(t *testing.T) {
	h := newRateHandler(t)
	long := time.Now().Add(90 * time.Minute)
	h.cfg.Pool.CooldownSoftForModel("u1", time.Minute, long, "glm-5.3", "6004 model rate limit")

	body := `{"code":14003,"retry_policy":{"retry_after":30}}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3", &upstream.Error{
		Kind: upstream.ErrSoftRate, Status: 429, Code: 14003, Msg: body,
	})
	st := rateStatus(t, h)
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("台账行=%+v", st.RateLimitedModels)
	}
	if rem := time.Until(st.RateLimitedModels[0].Until); rem < 85*time.Minute {
		t.Errorf("6004 的 90 分钟被截短到 %v", rem)
	}
	if st.RateLimitedModels[0].Reason != "6004 model rate limit" {
		t.Errorf("Reason=%q 不应被 14003 覆盖", st.RateLimitedModels[0].Reason)
	}
}

// TestApplyErrorPolicyPlain429StillAccountLevel 普通 429（无 6004/14003）仍走账号级冷却：
// 14003 的分支不能顺带把真正的账号级限流也降级掉。
func TestApplyErrorPolicyPlain429StillAccountLevel(t *testing.T) {
	h := newRateHandler(t)
	body := `{"code":42900,"msg":"slow down"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "m1", &upstream.Error{
		Kind: upstream.ErrSoftRate, Status: 429, Code: 42900, Msg: body,
	})
	st := rateStatus(t, h)
	if !st.Cooling {
		t.Error("普通 429 应触账号级软冷却")
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("普通 429 不该写模型级台账: %+v", st.RateLimitedModels)
	}
}

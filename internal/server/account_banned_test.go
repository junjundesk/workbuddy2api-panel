// account_banned_test.go 主链路封号判据：命中 11140 request illegal 时既禁用账号，
// 也回调面板把它实时移入「封号池」（面板侧的归组逻辑见 internal/panel/patrol_test.go）。
package server

import (
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// newBanHandler 构造只带封禁回调的最小 handler。
func newBanHandler(t *testing.T, notified *[]string) *Handler {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return NewHandler(Config{
		Pool:     pl,
		Upstream: upstream.New(),
		OnAccountBanned: func(uid string) {
			*notified = append(*notified, uid)
		},
	})
}

func accountDisabled(t *testing.T, h *Handler) bool {
	t.Helper()
	for _, st := range h.cfg.Pool.List() {
		if st.UID == "u1" {
			return st.Disabled
		}
	}
	t.Fatal("account not found in pool")
	return false
}

// TestApplyErrorPolicy11140BansAndNotifies：code 11140 + request illegal → 禁用 + 回调。
func TestApplyErrorPolicy11140BansAndNotifies(t *testing.T) {
	var notified []string
	h := newBanHandler(t, &notified)
	body := `{"code":11140,"msg":"request illegal"}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "m1", &upstream.Error{
		Kind: upstream.ErrAccountFault, Status: 403, Code: 11140, Msg: body,
	})
	if !accountDisabled(t, h) {
		t.Error("11140 封号应禁用账号")
	}
	if len(notified) != 1 || notified[0] != "u1" {
		t.Errorf("notified=%v want [u1]", notified)
	}
}

// TestApplyErrorPolicy11140CodeOnly：body 文本被截断/本地化，只有业务码也能判封号。
func TestApplyErrorPolicy11140CodeOnly(t *testing.T) {
	var notified []string
	h := newBanHandler(t, &notified)
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, "forbidden", "m1", &upstream.Error{
		Kind: upstream.ErrAccountFault, Status: 403, Code: 11140, Msg: "forbidden",
	})
	if !accountDisabled(t, h) {
		t.Error("仅 code 11140 也应判封号")
	}
	if len(notified) != 1 {
		t.Errorf("notified=%v want 1 次", notified)
	}
}

// TestApplyErrorPolicyTrialNotActivatedNoBan：14017（trial not activated）不是封号，
// 只软冷却，不触发封号回调。
func TestApplyErrorPolicyTrialNotActivatedNoBan(t *testing.T) {
	var notified []string
	h := newBanHandler(t, &notified)
	body := `{"code":14017,"msg":"trial not activated"}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "m1", &upstream.Error{
		Kind: upstream.ErrAccountFault, Status: 429, Code: 14017, Msg: body,
	})
	if accountDisabled(t, h) {
		t.Error("14017 不该禁用账号")
	}
	if len(notified) != 0 {
		t.Errorf("notified=%v want 空", notified)
	}
}

// TestApplyErrorPolicyRateLimitVariantNoBan：11140 的模型级限流变体走 ErrSoftRate，
// 不得判封号（同一 code 两种语义）。
func TestApplyErrorPolicyRateLimitVariantNoBan(t *testing.T) {
	var notified []string
	h := newBanHandler(t, &notified)
	body := `{"code":11140,"msg":"The model provider is rate-limiting requests."}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "m1", &upstream.Error{
		Kind: upstream.ErrSoftRate, Status: 403, Code: 11140, Msg: body,
	})
	if accountDisabled(t, h) {
		t.Error("模型级限流不该禁用账号")
	}
	if len(notified) != 0 {
		t.Errorf("notified=%v want 空", notified)
	}
}

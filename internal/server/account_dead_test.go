// account_dead_test.go 主链路凭证失效判据：命中 12153 session dead 时既禁用账号，
// 也回调面板把它实时移入「失效池」（面板侧归组逻辑见 internal/panel/session_dead_test.go）。
package server

import (
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// TestApplyErrorPolicySessionDeadDisablesAndNotifies：ErrSessionDead → 禁用 + 失效回调。
func TestApplyErrorPolicySessionDeadDisablesAndNotifies(t *testing.T) {
	var notified []string
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     pl,
		Upstream: upstream.New(),
		OnAccountDead: func(uid string) {
			notified = append(notified, uid)
		},
	})
	body := `{"code":12153,"msg":"Offline user session not found"}`
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "m1", &upstream.Error{
		Kind: upstream.ErrSessionDead, Status: 401, Code: 12153, Msg: body,
	})
	for _, st := range pl.List() {
		if st.UID == "u1" && !st.Disabled {
			t.Error("12153 session dead 应禁用账号")
		}
	}
	if len(notified) != 1 || notified[0] != "u1" {
		t.Errorf("notified=%v want [u1]", notified)
	}
}

// TestApplyErrorPolicySessionDeadNilCallback：未挂回调时不得 panic。
func TestApplyErrorPolicySessionDeadNilCallback(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pl, Upstream: upstream.New()})
	body := `{"code":12153,"msg":"Offline user session not found"}`
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "m1", &upstream.Error{
		Kind: upstream.ErrSessionDead, Status: 401, Code: 12153, Msg: body,
	})
}

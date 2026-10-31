// business_code_test.go 业务信封 code 的结构化解析：Error.Code 必须拿到上游真实
// code（11140 / 14018 / 6004），而不能只靠被截断的 Msg 文本——面板巡检按 Code
// 断言封号，文本截断会丢 code。
package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
)

func TestEnvelopeCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"顶层 code", `{"code":11140,"msg":"request illegal"}`, 11140},
		{"嵌套 error.data.code", `{"error":{"data":{"code":11140},"msg":"x"}}`, 11140},
		{"error 直接带 code", `{"error":{"code":14018,"msg":"credits exhausted"}}`, 14018},
		{"字符串 code", `{"code":"11140"}`, 11140},
		{"code 0 不误报", `{"code":0,"data":{}}`, 0},
		{"非 JSON", `<html>403</html>`, 0},
		{"空体", ``, 0},
		{"无 code", `{"msg":"boom"}`, 0},
	}
	for _, c := range cases {
		if got := envelopeCode([]byte(c.body)); got != c.want {
			t.Errorf("%s: envelopeCode=%d want %d", c.name, got, c.want)
		}
	}
}

// TestErrorCarriesBusinessCode HTTP 非 2xx 时 Error.Code 取自信封（11140 封号场景）。
func TestErrorCarriesBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`))
	}))
	defer srv.Close()

	c := New()
	c.BillingBaseCN = srv.URL
	a := &auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: 9999999999}

	_, err := c.billingJSON(a, http.MethodPost, "/x", map[string]any{})
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if ue.Code != 11140 {
		t.Errorf("Code=%d want 11140", ue.Code)
	}
	if ue.Kind != ErrAccountFault {
		t.Errorf("Kind=%v want account_fault", ue.Kind)
	}
	if ue.Status != http.StatusForbidden {
		t.Errorf("Status=%d want 403", ue.Status)
	}
}

// TestSuccessEnvelopeCodeIsZero 成功响应不产生错误（code=0 不当故障）。
func TestSuccessEnvelopeCodeIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer srv.Close()

	c := New()
	c.BillingBaseCN = srv.URL
	a := &auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: 9999999999}
	if _, err := c.billingJSON(a, http.MethodPost, "/x", map[string]any{}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

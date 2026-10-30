package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// importStubTransport 让导入端点顺带触发的上游签到/余额调用不出网：固定回 200 +
// 业务错误 code（*upstream.Error 的 business 错误不是 ErrServer，不会触发瞬时重试）。
type importStubTransport struct{}

func (importStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	const body = `{"code":10001,"message":"enterprise account does not support this operation"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestImportCockpitEndpointAcceptsNestedAuthFile 端到端（进程内）回归：
// 把插件 OAuth auths 文件原样 POST 给 /panel/api/import/cockpit，必须 imported=1 skipped=0，
// 且落盘 auths 文件保留 uid/token/enterpriseId/device_token/realm。
//
// 这是用户现场 bug 的最小复现路径：旧解码把嵌套形所有字段解析为空 → 整份文件被 skipped。
func TestImportCockpitEndpointAcceptsNestedAuthFile(t *testing.T) {
	dir := t.TempDir()
	stub := &http.Client{Transport: importStubTransport{}}
	up := &upstream.Client{
		HTTP:          stub,
		ChatHTTP:      stub,
		ChatBaseCN:    "https://upstream.invalid",
		BillingBaseCN: "https://upstream.invalid",
		WebBaseCN:     "https://upstream.invalid",
	}
	p := New(Config{
		Version:  "test",
		APIKey:   "test-key",
		AuthDir:  dir,
		Pool:     pool.New(filepath.Join(dir, "state.json")),
		Upstream: up,
	})

	const uid = "0a2565ad-1111-2222-3333-444455556666"
	nested := `{
  "account": {"enterpriseId": "ent-1", "nickname": "195****0914", "uid": "` + uid + `"},
  "auth": {"accessToken": "at-value", "refreshToken": "rt-value", "expiresAt": 1795958224, "domain": "www.codebuddy.cn", "realm": "cn"},
  "device_token": "dev-token-1"
}`

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "workbuddy-"+uid+".json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, nested); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Total    int      `json:"total"`
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if !resp.OK || resp.Total != 1 || resp.Imported != 1 || resp.Skipped != 0 {
		t.Fatalf("unexpected result: %+v", resp)
	}

	accs, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(accs) != 1 {
		t.Fatalf("auths loaded=%d want 1", len(accs))
	}
	a := accs[0]
	if a.UID != uid {
		t.Fatalf("uid=%q want %q", a.UID, uid)
	}
	if a.AccessTokenValue() != "at-value" || a.RefreshTokenValue() != "rt-value" {
		t.Fatalf("tokens=%q/%q", a.AccessTokenValue(), a.RefreshTokenValue())
	}
	if a.ExpiresAt != 1795958224 || a.DomainValue() != "www.codebuddy.cn" {
		t.Fatalf("expires/domain=%d/%q", a.ExpiresAt, a.DomainValue())
	}
	if a.Realm() != "cn" {
		t.Fatalf("realm=%q want cn", a.Realm())
	}
	if a.EnterpriseID != "ent-1" {
		t.Fatalf("enterpriseId=%q want ent-1", a.EnterpriseID)
	}
	if a.DeviceToken != "dev-token-1" {
		t.Fatalf("device_token=%q want dev-token-1", a.DeviceToken)
	}
	if a.IsEnterprise() != true {
		t.Fatal("IsEnterprise() should be true with enterpriseId set")
	}
}

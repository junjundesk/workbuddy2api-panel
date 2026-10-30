package panel

import (
	"encoding/json"
	"testing"
)

// TestDecodeImportAccountsFormats 覆盖三种输入容器 × 扁平/嵌套两种账号形态：
//   - cockpit 导出裸数组（snake_case，expires_at 毫秒）；
//   - 面板导出 {"format":"workbuddy-accounts","accounts":[...]}（camelCase，秒）；
//   - 插件 OAuth auths 文件原样（嵌套形 {"auth":{...},"account":{...}}，秒）；
//   - 单个扁平 / 单个嵌套对象。
//
// 回归背景：旧解码只认扁平形，用户把 auths/workbuddy-*.json 原样批量导入时
// uid/token 全解析为空 → 面板显示「成功 0 个，跳过 N 个」（issue 现场）。
func TestDecodeImportAccountsFormats(t *testing.T) {
	const nestedOne = `{"account":{"enterpriseId":"","nickname":"195****0914","uid":"u1"},"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1795958224,"domain":"www.codebuddy.cn","realm":"cn"}}`
	cases := map[string]struct {
		in        string
		wantUID   string
		wantAT    string
		wantRT    string
		wantRealm string
		wantExp   int64
	}{
		"cockpit-array": {
			in:      `[{"uid":"u1","access_token":"a","refresh_token":"r","expires_at":1795409352000}]`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantExp: 1795409352,
		},
		"panel-export": {
			in:      `{"format":"workbuddy-accounts","version":1,"accounts":[{"uid":"u1","realm":"cn","accessToken":"a","refreshToken":"r","expiresAt":1795409352}]}`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantRealm: "cn", wantExp: 1795409352,
		},
		"single-flat": {
			in:      `{"uid":"u1","accessToken":"a","refreshToken":"r","expiresAt":1795409352}`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantExp: 1795409352,
		},
		"bom": {
			in:      "\ufeff" + `[{"uid":"u1","access_token":"a","refresh_token":"r","expires_at":1795409352}]`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantExp: 1795409352,
		},
		// 插件 OAuth 落盘的 auths 文件原文：单个嵌套对象（现场样例形态）。
		"nested-single": {
			in:      nestedOne,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantRealm: "cn", wantExp: 1795958224,
		},
		// 嵌套数组（把多个 auths 文件拼成一个数组的常见批量方式）。
		"nested-array": {
			in:      `[{"account":{"uid":"u1"},"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1795958224,"realm":"cn"}}]`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantRealm: "cn", wantExp: 1795958224,
		},
		// {"accounts":[...]} 容器里的嵌套项。
		"nested-in-accounts": {
			in:      `{"format":"workbuddy-accounts","accounts":[{"account":{"uid":"u1"},"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1795958224}}]}`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantExp: 1795958224,
		},
		// 嵌套形里的 expiresAt 也按毫秒容错。
		"nested-ms": {
			in:      `{"account":{"uid":"u1"},"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1795958224000}}`,
			wantUID: "u1", wantAT: "a", wantRT: "r", wantExp: 1795958224,
		},
	}
	for name, tc := range cases {
		got, err := decodeImportAccounts([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: len=%d want 1", name, len(got))
		}
		a := got[0]
		if a.UID != tc.wantUID || a.AccessToken != tc.wantAT || a.RefreshToken != tc.wantRT {
			t.Fatalf("%s: bad decode %+v", name, a)
		}
		if a.Realm != tc.wantRealm {
			t.Fatalf("%s: realm=%q want %q", name, a.Realm, tc.wantRealm)
		}
		if a.ExpiresAt != tc.wantExp {
			t.Fatalf("%s: expires_at=%d want %d", name, a.ExpiresAt, tc.wantExp)
		}
	}
}

// TestDecodeImportAccountsNestedAuthFileFields 校验嵌套形字段完整保真：
// account.enterpriseId / account.nickname / auth.domain / 顶层 device_token。
func TestDecodeImportAccountsNestedAuthFileFields(t *testing.T) {
	const raw = `{
  "account": {
    "enterpriseId": "ent-42",
    "nickname": "195****0914",
    "uid": "0a2565ad-9118-4298-ae13-4edf2a091e58"
  },
  "auth": {
    "accessToken": "at-value",
    "domain": "www.codebuddy.cn",
    "expiresAt": 1795958224,
    "realm": "cn",
    "refreshToken": "rt-value"
  },
  "device_token": "dev-token-value"
}`
	got, err := decodeImportAccounts([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	a := got[0]
	if a.UID != "0a2565ad-9118-4298-ae13-4edf2a091e58" {
		t.Fatalf("uid=%q", a.UID)
	}
	if a.Nickname != "195****0914" {
		t.Fatalf("nickname=%q", a.Nickname)
	}
	if a.EnterpriseIDCC != "ent-42" {
		t.Fatalf("enterpriseId=%q", a.EnterpriseIDCC)
	}
	if a.AccessToken != "at-value" || a.RefreshToken != "rt-value" {
		t.Fatalf("tokens=%q/%q", a.AccessToken, a.RefreshToken)
	}
	if a.Domain != "www.codebuddy.cn" || a.Realm != "cn" || a.ExpiresAt != 1795958224 {
		t.Fatalf("domain/realm/expires=%q/%q/%d", a.Domain, a.Realm, a.ExpiresAt)
	}
	if a.DeviceToken != "dev-token-value" {
		t.Fatalf("device_token=%q", a.DeviceToken)
	}
}

// TestDecodeImportAccountsMixedArray 同一数组里扁平项与嵌套项可混用（逐个项判形态）。
func TestDecodeImportAccountsMixedArray(t *testing.T) {
	in := `[
		{"uid":"flat-1","access_token":"a1","refresh_token":"r1","expires_at":1795409352000},
		{"account":{"uid":"nested-1"},"auth":{"accessToken":"a2","refreshToken":"r2","expiresAt":1795958224}},
		{"uid":"flat-2","accessToken":"a3","refreshToken":"r3","expiresAt":1795409352}
	]`
	got, err := decodeImportAccounts([]byte(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	want := []string{"flat-1", "nested-1", "flat-2"}
	for i, uid := range want {
		if got[i].UID != uid || got[i].AccessToken == "" || got[i].RefreshToken == "" {
			t.Fatalf("item %d: %+v", i, got[i])
		}
	}
}

// TestDecodeImportAccountsInvalidAndEmpty 非 JSON 报错；空数组/空 accounts 得 0 条（由 handler 报错）。
func TestDecodeImportAccountsInvalidAndEmpty(t *testing.T) {
	if _, err := decodeImportAccounts([]byte("hello")); err == nil {
		t.Fatal("want error for non-JSON")
	}
	for _, in := range []string{"[]", `{"accounts":[]}`} {
		got, err := decodeImportAccounts([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s: len=%d want 0", in, len(got))
		}
	}
}

// TestDecodeImportAccountsNestedMissingToken tolerates 坏嵌套项：接入 handler 后逐条 skipped，
// 但不报错、不携带其它项。这里只验证解码层不抛错且 uid 仍可回读（便于报错定位）。
func TestDecodeImportAccountsNestedMissingToken(t *testing.T) {
	in := `{"account":{"uid":"no-token"},"auth":{"refreshToken":"r"}}`
	got, err := decodeImportAccounts([]byte(in))
	if err != nil {
		t.Fatalf("decode should tolerate missing accessToken: %v", err)
	}
	if len(got) != 1 || got[0].UID != "no-token" || got[0].AccessToken != "" {
		b, _ := json.Marshal(got)
		t.Fatalf("unexpected: %s", b)
	}
}

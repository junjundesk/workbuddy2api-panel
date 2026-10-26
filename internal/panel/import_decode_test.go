package panel

import "testing"

func TestDecodeImportAccountsFormats(t *testing.T) {
	cases := map[string]string{
		"cockpit-array": `[{"uid":"u1","access_token":"a","refresh_token":"r","expires_at":1795409352000}]`,
		"panel-export":  `{"format":"workbuddy-accounts","version":1,"accounts":[{"uid":"u1","realm":"cn","accessToken":"a","refreshToken":"r","expiresAt":1795409352}]}`,
		"single":        `{"uid":"u1","accessToken":"a","refreshToken":"r","expiresAt":1795409352}`,
		"bom":           "\ufeff" + `[{"uid":"u1","access_token":"a","refresh_token":"r","expires_at":1795409352}]`,
	}
	for name, in := range cases {
		got, err := decodeImportAccounts([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0].UID != "u1" || got[0].AccessToken != "a" || got[0].RefreshToken != "r" {
			t.Fatalf("%s: bad decode %+v", name, got)
		}
		if got[0].ExpiresAt != 1795409352 {
			t.Fatalf("%s: expires_at=%d want seconds", name, got[0].ExpiresAt)
		}
	}
	if _, err := decodeImportAccounts([]byte("hello")); err == nil {
		t.Fatal("want error for non-JSON")
	}
}

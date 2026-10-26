package panel

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/groups"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

func newGroupsPanel(t *testing.T) (*Panel, *groups.Store, *pool.Pool) {
	t.Helper()
	gs, err := groups.Load(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})
	return New(Config{Version: "test", APIKey: "k", Pool: pl, Groups: gs, Upstream: upstream.New()}), gs, pl
}

func call(t *testing.T, p *Panel, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	var req = httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func TestGroupRevealKeyNotInList(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	g, key, err := gs.Create("A", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, raw := call(t, p, "GET", "/panel/api/groups", ""); strings.Contains(raw, key) {
		t.Fatal("group list leaks plaintext key")
	}
	code, out, _ := call(t, p, "GET", "/panel/api/groups/"+g.ID+"/key", "")
	if code != 200 || out["key"] != key {
		t.Fatalf("reveal code=%d out=%v", code, out)
	}
	if code, _, _ := call(t, p, "GET", "/panel/api/groups/g_nope/key", ""); code != 404 {
		t.Fatalf("unknown group code=%d", code)
	}
}

func TestPublicProxyAPI(t *testing.T) {
	p, gs, _ := newGroupsPanel(t)
	code, _, raw := call(t, p, "PUT", "/panel/api/groups/public/proxy", `{"type":"http","host":"127.0.0.1","port":8080,"password":"secret"}`)
	if code != 200 {
		t.Fatalf("set public proxy code=%d %s", code, raw)
	}
	if px := gs.PublicProxy(); px == nil || px.Password != "secret" {
		t.Fatalf("stored=%+v", px)
	}
	_, out, raw := call(t, p, "GET", "/panel/api/groups", "")
	if out["public_proxy"] == nil || strings.Contains(raw, "secret") {
		t.Fatalf("list public_proxy missing or leaks password: %s", raw)
	}
	// 空密码保留旧密码
	call(t, p, "PUT", "/panel/api/groups/public/proxy", `{"type":"socks5h","host":"127.0.0.1","port":1080}`)
	if px := gs.PublicProxy(); px == nil || px.Password != "secret" || px.Type != "socks5h" {
		t.Fatalf("edit kept=%+v", px)
	}
	if code, _, _ := call(t, p, "DELETE", "/panel/api/groups/public/proxy", ""); code != 200 || gs.PublicProxy() != nil {
		t.Fatalf("clear code=%d proxy=%+v", code, gs.PublicProxy())
	}
}

func TestAccountMoveBetweenGroups(t *testing.T) {
	p, gs, pl := newGroupsPanel(t)
	g1, _, _ := gs.Create("A", "", false, false)
	g2, _, _ := gs.Create("B", "", false, false)
	for _, step := range []struct{ target, want string }{{g1.ID, g1.ID}, {g2.ID, g2.ID}, {"public", ""}} {
		code, _, raw := call(t, p, "POST", "/panel/api/accounts/u1/group", `{"group":"`+step.target+`"}`)
		if code != 200 {
			t.Fatalf("move to %s code=%d %s", step.target, code, raw)
		}
		if gs.GroupOf("u1") != step.want || pl.GroupOf("u1") != step.want {
			t.Fatalf("after move to %s: store=%q pool=%q", step.target, gs.GroupOf("u1"), pl.GroupOf("u1"))
		}
	}
	if gs.CountMembers(g1.ID) != 0 || gs.CountMembers(g2.ID) != 0 {
		t.Fatal("member left behind")
	}
	if code, _, _ := call(t, p, "POST", "/panel/api/accounts/ghost/group", `{"group":"`+g1.ID+`"}`); code != 404 {
		t.Fatalf("unknown uid code=%d", code)
	}
	if code, _, _ := call(t, p, "POST", "/panel/api/accounts/u1/group", `{"group":"g_nope"}`); code != 400 {
		t.Fatalf("unknown group code=%d", code)
	}
}

func TestAccountsExportReimportable(t *testing.T) {
	p, _, _ := newGroupsPanel(t)
	code, out, raw := call(t, p, "POST", "/panel/api/accounts/export", `{"uids":["u1","u2","u1","ghost"]}`)
	if code != 200 || out["format"] != "workbuddy-accounts" {
		t.Fatalf("export code=%d %s", code, raw)
	}
	accs, err := decodeImportAccounts([]byte(raw))
	if err != nil || len(accs) != 2 {
		t.Fatalf("reimport decode n=%d err=%v", len(accs), err)
	}
	if accs[0].UID != "u1" || accs[0].AccessToken != "at1" || accs[0].RefreshToken != "rt1" || accs[0].ExpiresAt != 9999999999 {
		t.Fatalf("decoded=%+v", accs[0])
	}
	if m, _ := out["missing"].([]any); len(m) != 1 || m[0] != "ghost" {
		t.Fatalf("missing=%v", out["missing"])
	}
	if code, _, _ := call(t, p, "POST", "/panel/api/accounts/export", `{"uids":[]}`); code != 400 {
		t.Fatalf("empty uids code=%d", code)
	}
}

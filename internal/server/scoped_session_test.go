package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/session"
)

func scopedTestRouter(p *pool.Pool, store *bindStore) *session.Router {
	return session.New(session.Config{Store: store, Available: p.AvailableUIDs, AvailableForModel: func(model string) []string {
		realm, bare := resolveModel(model)
		return p.WeightedAvailableUIDsForModelRealmScoped(bare, realm, "", false) // 公共池测试路由器
	}})
}

func scopedTestRequest(t *testing.T, h *Handler, path string, scope authScope, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"model": "glm-5.2", "stream": stream, "metadata": map[string]any{"conversation_id": "same-session"}}
	if path == "/v1/responses" {
		body["input"] = "hello"
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := withScope(httptest.NewRequest("POST", path, bytes.NewReader(raw)), scope)
	rec := httptest.NewRecorder()
	if path == "/v1/responses" {
		h.responses(rec, req)
	} else {
		h.chatCompletions(rec, req)
	}
	return rec
}

func TestScopedSessionsKeepAccountsAndCacheKeys(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				var accounts []*auth.Auth
				for _, uid := range []string{"public-a", "public-b", "g1-a", "g1-b", "g2-a", "g2-b"} {
					accounts = append(accounts, &auth.Auth{UID: uid, AccessToken: uid, ExpiresAt: 9999999999})
				}
				p := testPoolWith(accounts...)
				p.SetGroups(map[string]string{"g1-a": "g1", "g1-b": "g1", "g2-a": "g2", "g2-b": "g2"})
				// A non-sticky request can now choose a different healthy account.
				roll := 0
				p.SetRandomSource(func(n int64) int64 {
					roll++
					if roll%2 == 1 {
						return 0
					}
					return n - 1
				})
				store := newBindStore()
				router := scopedTestRouter(p, store)
				up, captured := newCapturingUpstream(t, sseOK, 200)
				h := NewHandler(Config{Pool: p, Upstream: up, Session: router})
				for _, group := range []string{"", "g1", "g2", "", "g1", "g2"} {
					rec := scopedTestRequest(t, h, path, authScope{GroupID: group}, stream)
					if rec.Code != 200 {
						t.Fatalf("group=%q status=%d body=%s", group, rec.Code, rec.Body)
					}
				}
				if len(*captured) != 6 {
					t.Fatalf("attempts=%d", len(*captured))
				}
				for i, group := range []string{"", "g1", "g2"} {
					a, b := (*captured)[i], (*captured)[i+3]
					if a.auth != b.auth || a.body["prompt_cache_key"] != b.body["prompt_cache_key"] {
						t.Errorf("group=%q account/cache key changed", group)
					}
					uid, ok := store.lastUID(session.ScopedKey(group, "same-session"))
					if !ok || p.GroupOf(uid) != group {
						t.Errorf("group=%q bound to %q", group, uid)
					}
					if !strings.Contains(a.auth, uid) {
						t.Errorf("group=%q binding did not follow successful account", group)
					}
					if jStr(jMap(a.body["metadata"])["conversation_id"]) != "same-session" {
						t.Fatal("internal scope key leaked into upstream identity")
					}
				}
				if router.Count() != 3 {
					t.Fatalf("bindings=%d want=3", router.Count())
				}
			})
		}
	}
}

func TestScopedStickyPublicFallbackAndRecovery(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "own", AccessToken: "own", ExpiresAt: 9999999999}, &auth.Auth{UID: "public-a", AccessToken: "public-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "public-b", AccessToken: "public-b", ExpiresAt: 9999999999})
			p.SetGroups(map[string]string{"own": "g"})
			p.Pause("own")
			store := newBindStore()
			router := scopedTestRouter(p, store)
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: p, Upstream: up, Session: router})
			for i := 0; i < 2; i++ {
				rec := scopedTestRequest(t, h, path, authScope{GroupID: "g", CallPublic: true}, false)
				if rec.Code != 200 {
					t.Fatalf("fallback status=%d", rec.Code)
				}
			}
			if len(*captured) != 2 || (*captured)[0].auth != (*captured)[1].auth {
				t.Fatal("public fallback did not stay sticky")
			}
			if _, ok := store.lastUID("same-session"); ok {
				t.Fatal("group fallback overwrote public binding")
			}
			rec := scopedTestRequest(t, h, path, authScope{GroupID: "g"}, false)
			if rec.Code != http.StatusServiceUnavailable || len(*captured) != 2 {
				t.Fatal("revoked call_public still used public account")
			}
			// 共用候选集语义：本组号恢复后，公共池号仍在候选集里（不是"本组优先"）。
			p.Resume("own")
			rec = scopedTestRequest(t, h, path, authScope{GroupID: "g", CallPublic: true}, false)
			if rec.Code != 200 || len(*captured) != 3 {
				t.Fatalf("resume+call_public status=%d", rec.Code)
			}
			uids := p.WeightedAvailableUIDsForModelRealmScoped("glm-5.2", "", "g", true)
			hasOwn, hasPublic := false, false
			for _, u := range uids {
				if u == "own" {
					hasOwn = true
				}
				if strings.HasPrefix(u, "public") {
					hasPublic = true
				}
			}
			if !hasOwn || !hasPublic {
				t.Fatalf("call_public 候选集应为本组 ∪ 公共池，got %v", uids)
			}
			// Moving the only own account must invalidate the binding, not follow it into another group.
			p.SetGroup("own", "other")
			rec = scopedTestRequest(t, h, path, authScope{GroupID: "g"}, false)
			if rec.Code != http.StatusServiceUnavailable || len(*captured) != 3 {
				t.Fatal("sticky account leaked across group move")
			}
		})
	}
}

func TestScopedStickyRetryKeepsOtherBindings(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "bad", AccessToken: "bad", ExpiresAt: 9999999999}, &auth.Auth{UID: "good", AccessToken: "good", ExpiresAt: 9999999999}, &auth.Auth{UID: "public", AccessToken: "public", ExpiresAt: 9999999999})
			p.SetGroups(map[string]string{"bad": "g", "good": "g"})
			store := newBindStore()
			router := scopedTestRouter(p, store)
			router.Bind("same-session", "public")
			router.Bind(session.ScopedKey("g", "same-session"), "bad")
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				if strings.Contains(authz, "bad") {
					return 500, "temporary failure", false
				}
				return 200, sseOK, true
			})
			h := NewHandler(Config{Pool: p, Upstream: up, Session: router, MaxRotate: 2})
			rec := scopedTestRequest(t, h, path, authScope{GroupID: "g"}, false)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if uid, _ := store.lastUID(session.ScopedKey("g", "same-session")); uid != "good" {
				t.Fatalf("retry binding=%q", uid)
			}
			if uid, _ := store.lastUID("same-session"); uid != "public" {
				t.Fatal("retry unbound or replaced public session")
			}
		})
	}
}

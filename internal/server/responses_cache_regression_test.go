package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/session"
)

func postResponsesRegression(t *testing.T, h *Handler, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if jBool(body["stream"], false) {
		return jMap(lastEvent(t, parseResponsesSSE(rec.Body.String()))["response"])
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestResponsesPreviousResponseIDKeepsFullPrefix(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999}, &auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999})
			router := scopedTestRouter(p, newBindStore())
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: p, Upstream: up, Session: router, PromptMode: "passthrough"})
			prev := ""
			var priorMessages []any
			for i, input := range []string{"first-user", "second-user", "third-user", "fourth-user"} {
				req := map[string]any{"model": "glm-5.2", "input": input, "stream": stream}
				if prev != "" {
					req["previous_response_id"] = prev
				}
				resp := postResponsesRegression(t, h, req)
				prev = jStr(resp["id"])
				if prev == "" {
					t.Fatal("missing response id")
				}
				msgs := jArr((*captured)[i].body["messages"])
				if len(msgs) != 2*i+1 {
					t.Fatalf("turn %d messages=%d want=%d", i+1, len(msgs), 2*i+1)
				}
				if i > 0 && !reflect.DeepEqual(msgs[:len(priorMessages)], priorMessages) {
					t.Fatalf("turn %d changed previous prefix", i+1)
				}
				if jMap(msgs[0])["content"] != "first-user" || jMap(msgs[len(msgs)-1])["content"] != input {
					t.Fatal("user history changed")
				}
				if (*captured)[i].auth != (*captured)[0].auth || (*captured)[i].body["prompt_cache_key"] != (*captured)[0].body["prompt_cache_key"] {
					t.Fatal("continuation changed account/cache identity")
				}
				if i > 0 && jMap(msgs[len(msgs)-2])["content"] != "你好" {
					t.Fatal("assistant history lost")
				}
				priorMessages = msgs
			}
		})
	}
}

func TestResponsesHistorySnapshotsExpandedReferences(t *testing.T) {
	up, captured := newCapturingUpstream(t, sseOK, 200)
	h := NewHandler(Config{Pool: responsesPool(), Upstream: up, PromptMode: "passthrough"})
	item := map[string]any{"id": "seed-user", "type": "message", "role": "user", "content": "original-prefix"}
	h.respStore.put("seed-response", []any{item}, nil)
	baseTransport := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Simulate TTL/eviction after acceptance but before response persistence.
		h.respStore.mu.Lock()
		delete(h.respStore.byID, "seed-response")
		h.respStore.mu.Unlock()
		return baseTransport.RoundTrip(r)
	})
	first := postResponsesRegression(t, h, map[string]any{"model": "glm-5.2", "instructions": "only-first-turn", "input": []any{map[string]any{"type": "item_reference", "id": "seed-user"}, "first-followup"}})
	prev := jStr(first["id"])
	stored := h.respStore.get(prev)
	if stored == nil || len(stored.items) != 3 || jMap(stored.items[0])["content"] != "original-prefix" {
		t.Fatal("expanded reference not stored")
	}
	for _, input := range []string{"second-followup", "third-followup"} {
		resp := postResponsesRegression(t, h, map[string]any{"model": "glm-5.2", "previous_response_id": prev, "input": input})
		prev = jStr(resp["id"])
		msgs := jArr((*captured)[len(*captured)-1].body["messages"])
		if jMap(msgs[0])["content"] != "original-prefix" {
			t.Fatal("evicted reference source broke stored prefix")
		}
		for _, m := range msgs {
			if jMap(m)["role"] == "system" {
				t.Fatal("previous instructions carried into next turn")
			}
		}
	}
	if len(jArr((*captured)[2].body["messages"])) != 6 {
		t.Fatal("reference continuation lost cumulative history")
	}
}

func TestResponsesHistoryRespectsOwnerAndUserIDGate(t *testing.T) {
	store := newResponsesStore()
	store.put("private", []any{map[string]any{"id": "private-item", "type": "message", "role": "user", "content": "secret"}}, nil, "g1")
	if _, _, _, _, err := responsesChatRequestWithHistory(map[string]any{"previous_response_id": "private", "input": "x"}, store, "g2"); err == nil {
		t.Fatal("other group could resume private response")
	}
	chat, _, dropped, history, err := responsesChatRequestWithHistory(map[string]any{"input": []any{map[string]any{"type": "item_reference", "id": "private-item"}}}, store, "g2")
	if err != nil || len(history) != 0 || len(jArr(chat["messages"])) != 0 || len(dropped) == 0 {
		t.Fatal("private reference crossed owner boundary")
	}
	for _, fields := range []map[string]any{
		{"metadata": map[string]any{"user_id": "user-only"}},
		{"user_id": "user-only"},
		{"conversation_id": "explicit-conversation"},
		{"conversationId": "explicit-conversation"},
	} {
		req := map[string]any{"input": "hello"}
		for k, v := range fields {
			req[k] = v
		}
		got, _, _, err := responsesChatRequest(req, store)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		want := ""
		if req["conversation_id"] != nil || req["conversationId"] != nil {
			want = "explicit-conversation"
		}
		if key := session.ExtractKey(raw); key != want {
			t.Errorf("identity=%v session=%q want=%q", fields, key, want)
		}
	}
}

func TestCacheIdentityPreservedByBothEndpoints(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			body := map[string]any{"model": "glm-5.2", "prompt_cache_key": "stable-client-cache", "metadata": map[string]any{"conversation_id": "stable-client-session"}}
			if path == "/v1/responses" {
				body["input"] = "hello"
			} else {
				body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
			}
			raw, _ := json.Marshal(body)
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: responsesPool(), Upstream: up, PromptMode: "passthrough"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(string(raw))))
			if rec.Code != 200 || len(*captured) != 1 {
				t.Fatalf("status=%d attempts=%d", rec.Code, len(*captured))
			}
			got := (*captured)[0].body
			t.Logf("path=%s outbound cache key=%v metadata=%v", path, got["prompt_cache_key"], got["metadata"])
			if got["prompt_cache_key"] != "stable-client-cache" {
				t.Error("client prompt_cache_key was dropped")
			}
			if jStr(jMap(got["metadata"])["conversation_id"]) != "stable-client-session" {
				t.Error("client conversation metadata was dropped")
			}
		})
	}
}

func TestOutboundRetryCacheKeyMatchesAccount(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			up, captured := newCapturingUpstream(t, sseOK, 200)
			transport := up.HTTP.Transport
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := transport.RoundTrip(r)
				if len(*captured) == 1 && err == nil {
					resp.Body.Close()
					resp.StatusCode = 500
					resp.Body = io.NopCloser(strings.NewReader(`{"error":"temporary server error"}`))
				}
				return resp, err
			})
			p := testPoolWith(&auth.Auth{UID: "audit-A", AccessToken: "fake-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "audit-B", AccessToken: "fake-b", ExpiresAt: 9999999999})
			p.SetCredits("audit-A", 2000, 0)
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2, PromptMode: "passthrough"})
			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"retry"}]}`
			if path == "/v1/responses" {
				body = `{"model":"glm-5.2","input":"retry"}`
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if rec.Code != 200 || len(*captured) != 2 {
				t.Fatalf("status=%d attempts=%d", rec.Code, len(*captured))
			}
			a, b := (*captured)[0], (*captured)[1]
			t.Logf("retry cache keys=%v -> %v", a.body["prompt_cache_key"], b.body["prompt_cache_key"])
			if a.auth == b.auth {
				t.Fatal("test did not rotate accounts")
			}
			if a.body["prompt_cache_key"] == b.body["prompt_cache_key"] {
				t.Fatal("retry retained first account's injected cache key")
			}
		})
	}
}

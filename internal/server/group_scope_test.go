package server

import (
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
)

// TestChatGroupCallPublicSharesCandidateSet 分组开启「调用公共池」后，本组号与公共池号
// 进**同一候选集**按权重抽签：本组号完全健康时，权重更高的公共池号同样会被选中
// （旧"本组优先、无号才回源"语义下这里必然命中 own）。
func TestChatGroupCallPublicSharesCandidateSet(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			p := testPoolWith(
				&auth.Auth{UID: "own", AccessToken: "own", ExpiresAt: 9999999999},
				&auth.Auth{UID: "pub", AccessToken: "pub", ExpiresAt: 9999999999},
			)
			p.SetGroups(map[string]string{"own": "g"})
			p.SetCredits("own", 0, 0)
			p.SetCredits("pub", 100000, 0) // 公共池号权重压倒本组号
			p.SetRandomSource(func(n int64) int64 { return 0 })
			up, captured := newCapturingUpstream(t, sseOK, 200)
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := scopedTestRequest(t, h, path, authScope{GroupID: "g", CallPublic: true}, false)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if got := (*captured)[0].auth; !strings.Contains(got, "pub") {
				t.Fatalf("共用候选集：本组健康时公共池号也应能被选中，got %q", got)
			}
		})
	}
}

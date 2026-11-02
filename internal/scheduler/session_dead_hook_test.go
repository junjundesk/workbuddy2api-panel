// session_dead_hook_test.go 凭证失效回调：批量任务（签到 / 余额刷新 / token 保活）命中
// 401+12153 时逐条调用 SetSessionDeadHook 挂载的回调（面板据此移入「失效池」）。
// 全程不打真网。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// dead401Server 所有端点统一回 401 + 12153（凭证失效的标准 body）。
func dead401Server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
}

// newDeadScheduler 装一个「u1 必然凭证失效」的调度器，并收集 hook 回调。
func newDeadScheduler(t *testing.T) (*Scheduler, *[]string, *sync.Mutex) {
	t.Helper()
	srv := dead401Server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	var mu sync.Mutex
	got := &[]string{}
	s.SetSessionDeadHook(func(uid, detail string) {
		mu.Lock()
		*got = append(*got, uid+":"+detail)
		mu.Unlock()
	})
	return s, got, &mu
}

// TestSessionDeadHookFiredInBatchTasks 批量三路命中失效都回调（签到 + 余额各一次、
// 余额刷新一次、保活一次），uid 逐条带上。
func TestSessionDeadHookFiredInBatchTasks(t *testing.T) {
	s, got, mu := newDeadScheduler(t)

	s.RunCheckinNow()        // 签到 401 + 余额 401：各回调一次（签到失败仍继续查余额）
	s.RunBalanceRefreshNow() // 余额 401：回调一次
	s.RunKeepaliveNow()      // token 刷新 12153：回调一次

	mu.Lock()
	defer mu.Unlock()
	if len(*got) < 3 {
		t.Fatalf("回调次数 = %d（%v），want ≥ 3（签到/余额/保活）", len(*got), *got)
	}
	for _, e := range *got {
		if !strings.HasPrefix(e, "u1:") {
			t.Errorf("回调 uid 不对：%q", e)
		}
	}
}

// TestSessionDeadHookNilNoPanic 未挂回调时命中失效只照常记日志，不 panic。
func TestSessionDeadHookNilNoPanic(t *testing.T) {
	srv := dead401Server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	New(Config{Pool: p, Upstream: up}).RunBalanceRefreshNow()
}

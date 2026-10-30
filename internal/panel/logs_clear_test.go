// logs_clear_test.go 日志页「清空」两个端点：
//   - POST /panel/api/logs/clear         清空运行日志内存环形缓冲
//   - POST /panel/api/request_logs/clear 删除请求记录归档 + 清空 recent（累计指标保留）
package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/pool"
	"github.com/junjundesk/workbuddy2api-panel/internal/reqlog"
	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

func TestLogsClearEndpoint(t *testing.T) {
	p, _, _ := newGroupsPanel(t)
	if _, err := p.Logs().Write([]byte("panel: line-a\npanel: line-b\n")); err != nil {
		t.Fatal(err)
	}
	code, out, raw := call(t, p, "POST", "/panel/api/logs/clear", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if n, _ := out["cleared"].(float64); n != 2 {
		t.Fatalf("cleared=%v want 2", out["cleared"])
	}
	for _, e := range p.Logs().Snapshot() {
		if strings.Contains(e.Text, "line-a") || strings.Contains(e.Text, "line-b") {
			t.Fatalf("旧日志未被清空: %q", e.Text)
		}
	}
}

func TestRequestLogsClearEndpoint(t *testing.T) {
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	defer rec.Close()
	p := New(Config{
		Version: "test", APIKey: "k",
		Pool: pool.New(""), Upstream: upstream.New(), RequestLog: rec,
	})
	rec.Record(reqlog.Event{RequestID: "a", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess})

	code, out, raw := call(t, p, "POST", "/panel/api/request_logs/clear", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d raw=%s", code, raw)
	}
	if files, _ := out["files"].(float64); files != 1 {
		t.Fatalf("files=%v want 1 raw=%s", out["files"], raw)
	}
	rows, err := rec.ReadArchive(10, reqlog.Filter{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("archive rows=%+v err=%v want empty", rows, err)
	}
	s := rec.Snapshot()
	if len(s.Recent) != 0 {
		t.Fatalf("recent=%d want 0", len(s.Recent))
	}
	if s.Completed != 1 {
		t.Fatalf("completed=%d want 1（累计指标应保留）", s.Completed)
	}
}

func TestRequestLogsClearUnavailable(t *testing.T) {
	p, _, _ := newGroupsPanel(t) // RequestLog 未装配
	if code, _, _ := call(t, p, "POST", "/panel/api/request_logs/clear", ""); code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", code)
	}
}

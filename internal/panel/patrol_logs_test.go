package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func patrolReport(t *testing.T, p *Panel) patrolView {
	t.Helper()
	code, _, raw := call(t, p, "GET", "/panel/api/patrol", "")
	if code != http.StatusOK {
		t.Fatalf("巡检状态 code=%d body=%s", code, raw)
	}
	var view patrolView
	if err := json.Unmarshal([]byte(raw), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// 多轮健康检查、重复归组和试运行不能把已有处理日志挤掉。
func TestPatrolLogsOnlyHandledResults(t *testing.T) {
	fake := newPatrolUpstream()
	fake.banned["t-ban"] = `{"code":11140,"msg":"request illegal"}`
	fake.zero["t-zero"] = true
	p, _, _ := newPatrolPanel(t, fake, acc("ban", "t-ban"), acc("zero", "t-zero"), acc("ok", "t-ok"))
	run := func(dry bool) patrolView {
		t.Helper()
		if err := p.patrol.start(nil, boolPtr(dry)); err != nil {
			t.Fatal(err)
		}
		waitPatrol(t, p)
		return patrolReport(t, p)
	}
	first := run(false)
	if first.Done != 3 || first.Total != 3 || len(first.Results) != 2 {
		t.Fatalf("应检查 3 个账号、只记录 2 次移动：%+v", first)
	}
	for _, res := range first.Results {
		if !res.Moved || (res.UID != "ban" && res.UID != "zero") {
			t.Errorf("处理日志包含未移动账号：%+v", res)
		}
	}
	for _, dry := range []bool{false, true} {
		view := run(dry)
		if len(view.Results) != 2 || view.Done != 2 || view.Total != 2 || view.SkippedBanned != 1 {
			t.Fatalf("重复巡检 / 试运行不应新增日志：dry=%v view=%+v", dry, view)
		}
		for i, res := range view.Results {
			if res.UID != first.Results[i].UID || !res.At.Equal(first.Results[i].At) {
				t.Errorf("未移动不应重写历史记录：%+v", res)
			}
		}
	}
	again := newPatrolRunner(p, p.cfg.PatrolFile)
	if len(again.results) != 2 {
		t.Fatalf("重启后应保留两条处理日志：%+v", again.results)
	}
}

// 老版本混合检查记录加载时先过滤，再截断；普通记录不占保存额度。
func TestPatrolLogLoadFiltersLegacyResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "patrol.json")
	f := patrolFile{Version: 1, Settings: defaultPatrolSettings(), LastRun: time.Now()}
	f.Results = append(f.Results,
		patrolResult{UID: "ban", Verdict: patrolVerdictBanned, Moved: true},
		patrolResult{UID: "restored", Verdict: patrolVerdictOK, Restored: true})
	for i := 0; i < patrolKeepResults+20; i++ {
		f.Results = append(f.Results, patrolResult{UID: "healthy", Verdict: patrolVerdictOK})
	}
	f.Results = append(f.Results,
		patrolResult{UID: "already-grouped", Verdict: patrolVerdictNoCredit},
		patrolResult{UID: "move-failed", Verdict: patrolVerdictBanned, Error: "移动分组失败"},
		patrolResult{UID: "dry-run", Verdict: patrolVerdictBanned},
		patrolResult{UID: "rate", Verdict: patrolVerdictRateLimited},
		patrolResult{UID: "cancelled", Verdict: patrolVerdictFailed})
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newPatrolRunner(nil, path)
	if len(r.results) != 2 || r.results[0].UID != "ban" || r.results[1].UID != "restored" {
		t.Fatalf("旧记录过滤不正确：%+v", r.results)
	}
	if !r.lastRun.Equal(f.LastRun) {
		t.Error("过滤日志不能改变上次运行时间")
	}
	r.mu.Lock()
	r.saveLocked()
	r.mu.Unlock()
	again := newPatrolRunner(nil, path)
	if len(again.results) != 2 {
		t.Fatalf("落盘后不应再出现普通检查记录：%+v", again.results)
	}
}

func TestPatrolLogRetentionCountsOnlyActions(t *testing.T) {
	r := newPatrolRunner(nil, filepath.Join(t.TempDir(), "patrol.json"))
	r.mu.Lock()
	for i := 0; i < patrolKeepResults+25; i++ {
		r.appendHandledLocked(patrolResult{UID: strconv.Itoa(i), Verdict: patrolVerdictNoCredit, Moved: true})
	}
	for i := 0; i < patrolKeepResults+25; i++ {
		r.appendHandledLocked(patrolResult{UID: "ok", Verdict: patrolVerdictOK})
	}
	r.saveLocked()
	r.mu.Unlock()
	if len(r.results) != patrolKeepResults || r.results[0].UID != "25" {
		t.Fatalf("内存应保留最近 %d 条处理日志，实得 %+v", patrolKeepResults, r.results)
	}
	again := newPatrolRunner(nil, r.path)
	if len(again.results) != patrolSavedResults || again.results[0].UID != strconv.Itoa(patrolKeepResults+25-patrolSavedResults) {
		t.Fatalf("落盘应保留最近 %d 条处理日志，实得 %+v", patrolSavedResults, again.results)
	}
}

// 查询失败或取消都没有执行移动，不能伪装为处理成功。
func TestPatrolFailuresProduceNoActionLogs(t *testing.T) {
	for _, probeErr := range []error{errors.New("test upstream failure"), context.Canceled} {
		t.Run(probeErr.Error(), func(t *testing.T) {
			p, _, _ := newPatrolPanelRT(t, rtFunc(func(*http.Request) (*http.Response, error) {
				return nil, probeErr
			}), acc("failed", "t-failed"))
			if err := p.patrol.start(nil, boolPtr(false)); err != nil {
				t.Fatal(err)
			}
			waitPatrol(t, p)
			view := patrolReport(t, p)
			if len(view.Results) != 0 || view.Done != 1 {
				t.Fatalf("失败只能计入检查进度，不应产生处理日志：%+v", view)
			}
		})
	}
}

// 快账号处理成功后立即可见，不必等待其他慢账号结束。
func TestPatrolLogsVisibleWhileRunning(t *testing.T) {
	fake := newPatrolUpstream()
	fake.zero["t-fast"] = true
	block := make(chan struct{})
	p, _, _ := newPatrolPanelRT(t, rtFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "Bearer t-slow" {
			select {
			case <-block:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return fake.transport(req)
	}), acc("fast", "t-fast"), acc("slow", "t-slow"))
	t.Cleanup(func() { close(block); waitPatrol(t, p) })
	if err := p.patrol.start(nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view := patrolReport(t, p)
		if view.Done == 1 {
			if !view.Running || view.Total != 2 || len(view.Results) != 1 || view.Results[0].UID != "fast" {
				t.Fatalf("运行中应已显示 fast 的处理日志：%+v", view)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("未及时发布处理日志")
}

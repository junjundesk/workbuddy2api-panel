package panel

import (
	"testing"

	"github.com/junjundesk/workbuddy2api-panel/internal/upstream"
)

// TestExpertIDFromJumpURL 任务 jump_url 的 expertId 藏在两层 URL 编码里：
// 2026-10 活动任务（expert_use_*）实测形状，反解失败会导致召唤到错的专家
// （服务端只认任务指定的那一位）。
func TestExpertIDFromJumpURL(t *testing.T) {
	cases := []struct {
		name, url, want string
	}{
		{
			"活动任务真实样本",
			"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_WZY136wV5aMt&buddy_type=workbuddy",
			"ex_WZY136wV5aMt",
		},
		{
			"未编码形态",
			"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy://experts?expertId=ex_GDl31WahyIH0&buddy_type=workbuddy",
			"ex_GDl31WahyIH0",
		},
		{"无 expertId", "workbuddy://expert", ""},
		{"空串", "", ""},
	}
	for _, c := range cases {
		if got := expertIDFromJumpURL(c.url); got != c.want {
			t.Errorf("%s: expertIDFromJumpURL(%q)=%q want %q", c.name, c.url, got, c.want)
		}
	}
}

// TestTitleQuotedName 任务标题里「」包住的就是目标专家名（兜底解析用）。
func TestTitleQuotedName(t *testing.T) {
	cases := map[string]string{
		"体验「直播电商教练」":   "直播电商教练",
		"体验「无畏契约拼豆专家」": "无畏契约拼豆专家",
		"和 AI 聊天 5 次":  "",
		"":             "",
	}
	for title, want := range cases {
		if got := titleQuotedName(title); got != want {
			t.Errorf("titleQuotedName(%q)=%q want %q", title, got, want)
		}
	}
}

// TestExpertUseTasksRegistered 「体验指定专家」类任务必须在 autoActions 里登记，
// 否则面板只显示操作指引（一键完成按钮不出现），且队列扫描不会把它当待办。
func TestExpertUseTasksRegistered(t *testing.T) {
	for code := range expertUseTaskTitles {
		if autoActionFor(code) == nil {
			t.Errorf("任务 %s 有专家名映射但 autoActions 未登记（无法一键完成）", code)
		}
	}
	// 已知活动码逐个钉住，防重构时被删。
	for _, code := range []string{
		"WorkBuddy_Valorant",
		"expert_use_maidehao",
		"expert_use_douyinyin",
		"expert_use_shangjia_xuanchuan",
		"expert_use_bowangwang",
	} {
		if autoActionFor(code) == nil {
			t.Errorf("活动任务 %s 未登记到 autoActions", code)
		}
	}
}

// TestResolveTaskExpertPrefersJumpURLID jump_url 给出 expertId 时优先用它——
// 即便市场 keyword 搜索返回同名不同 id 的多个专家（「抖音策略师」实测 10 条）。
// 这里只验证纯函数层面的选择顺序：expertUseTaskTitles 覆盖标题解析。
// TestAutoActionForExpertUseFamily 码族兜底：上游新下发一个没登记过的 `expert_use_*`
// （活动任务按专家名起码，随时可能加新的），也必须能一键完成——否则每来一个新活动
// 就要改一次代码 + 重发一版镜像。
func TestAutoActionForExpertUseFamily(t *testing.T) {
	for _, code := range []string{"expert_use_bowangwang", "expert_use_某个未来新专家"} {
		act := autoActionFor(code)
		if act == nil {
			t.Fatalf("autoActionFor(%q)=nil，码族兜底失效", code)
		}
		if act.TaskCode != code {
			t.Errorf("autoActionFor(%q).TaskCode=%q", code, act.TaskCode)
		}
		if act.run == nil {
			t.Errorf("autoActionFor(%q).run=nil", code)
		}
	}
	// 非该族的未知码仍返回 nil（前端只展示操作指引，不出「一键完成」按钮）。
	if act := autoActionFor("some_unknown_task"); act != nil {
		t.Fatalf("未知任务码不应被兜底：%+v", act)
	}
}

func TestExpertUseTaskTitlesCoverTitles(t *testing.T) {
	task := upstream.Task{TaskCode: "expert_use_bowangwang", Title: "体验「直播电商教练」"}
	if got := expertUseTaskTitles[task.TaskCode]; got != "直播电商教练" {
		t.Fatalf("expertUseTaskTitles[%s]=%q want 直播电商教练", task.TaskCode, got)
	}
	if got := titleQuotedName(task.Title); got != "直播电商教练" {
		t.Fatalf("标题解析=%q want 直播电商教练", got)
	}
}

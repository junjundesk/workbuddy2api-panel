// strict_listing_test.go 列表严格口径回归：/v1/models 与面板只透出账号上游实际下发的
// 字段，禁止任何源码内置兜底（静态种子表 / model.json / models.dev / 1M 默认 / 静态档位表）。
//
// 用「静态表里确有条目」的模型（glm-5.2 的 context 1000000、cn/global 档位表都有条目）
// 断言：上游零值时必须返回"未知"（0 / ok=false / nil），而不是静态值——静态值一旦漏出，
// 本测试立刻失败。CN 与 global 两域都覆盖。
package upstream

import "testing"

func TestStrictListingNoSourceFallback(t *testing.T) {
	if !strictListing {
		t.Fatal("本测试针对严格口径；strictListing=false 时应删除本测试或改写期望值")
	}

	// 静态种子表 glm-5.2 = context 1000000 / maxOutput 131072：上游零值 → 必须"未知"。
	if got := ContextWindowListingV4("glm-5.2", 0, nil); got != 0 {
		t.Errorf("ContextWindowListingV4: got %d want 0（不得回落静态表/1M 默认）", got)
	}
	if got := ContextWindowListing("glm-5.2", 0); got != 0 {
		t.Errorf("ContextWindowListing(遗留链): got %d want 0", got)
	}
	if mo, ok := MaxOutputTokensListingV4("glm-5.2", 0, nil); ok || mo != 0 {
		t.Errorf("MaxOutputTokensListingV4: got (%d,%v) want (0,false)", mo, ok)
	}
	if mo, ok := MaxOutputTokensListing("glm-5.2", 0); ok || mo != 0 {
		t.Errorf("MaxOutputTokensListing(遗留链): got (%d,%v) want (0,false)", mo, ok)
	}

	// 静态档位表：CN 的 glm-5.2（high,xhigh）、global 的 fast-model（medium）都不得漏出。
	if e, d := EffortListing("cn", "glm-5.2", nil, ""); e != nil || d != "" {
		t.Errorf("EffortListing cn: got %v/%q want nil/\"\"（不得回落静态档位表）", e, d)
	}
	if e, d := EffortListing("global", "fast-model", nil, ""); e != nil || d != "" {
		t.Errorf("EffortListing global: got %v/%q want nil/\"\"（不得回落静态档位表）", e, d)
	}
}

func TestStrictListingKeepsUpstreamValues(t *testing.T) {
	if !strictListing {
		t.Skip("严格口径未开启")
	}
	// 上游真值原样透出（严格口径只砍兜底，不砍上游数据）。
	if got := ContextWindowListingV4("glm-5.2", 12345, nil); got != 12345 {
		t.Errorf("ContextWindowListingV4: got %d want 12345", got)
	}
	if mo, ok := MaxOutputTokensListingV4("glm-5.2", 678, nil); !ok || mo != 678 {
		t.Errorf("MaxOutputTokensListingV4: got (%d,%v) want (678,true)", mo, ok)
	}
	if e, d := EffortListing("cn", "glm-5.2", []string{"low", "high"}, "high"); len(e) != 2 || d != "high" {
		t.Errorf("EffortListing: got %v/%q want [low high]/high", e, d)
	}
	// 既有防御保留：默认档不在档位集合内 → 不宣称默认档。
	if _, d := EffortListing("cn", "glm-5.2", []string{"low"}, "max"); d != "" {
		t.Errorf("默认档不在集合内应省略，got %q", d)
	}
	// 上游只给默认档、没给档位集合 → 严格口径下仍省略（不宣称档位）。
	if e, d := EffortListing("cn", "glm-5.2", nil, "high"); e != nil || d != "" {
		t.Errorf("无远端档位集合时应省略，got %v/%q", e, d)
	}
}

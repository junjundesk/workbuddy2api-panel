package panel

import "testing"

// 任务中心并发上限随账号数自适应：不再写死 4，≤0 取满，超出夹到账号数。
func TestClampQueueConcurrency(t *testing.T) {
	cases := []struct{ c, n, want int }{
		{1, 10, 1},
		{3, 10, 3},
		{8, 10, 8},   // 旧上限 4 已移除
		{20, 10, 10}, // 超出夹到账号数
		{0, 6, 6},    // 0 = 取满
		{-1, 6, 6},
		{5, 0, 1}, // 无账号兜底 1
	}
	for _, tc := range cases {
		if got := clampQueueConcurrency(tc.c, tc.n); got != tc.want {
			t.Errorf("clampQueueConcurrency(%d, %d) = %d, want %d", tc.c, tc.n, got, tc.want)
		}
	}
}

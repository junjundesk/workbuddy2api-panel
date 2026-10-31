package panel

import (
	"os"
	"regexp"
	"testing"
)

// TestGroupsJSTopLevelElementIDs groups.js 顶层（脚本加载即执行）用 $('id') 绑定
// 事件的元素，必须真实存在于 groups.html。
//
// 起因：提示词弹窗从单 textarea 改成动态规则编辑器时，旧的
// $('promptText').addEventListener(...) 留在文件末尾——元素已删，脚本加载即抛
// TypeError，整页交互全部失效。Go 只查 JS 语法、vm 切片测试只跑局部函数，都发现
// 不了这类"静态 id 失配"，所以单独做一次全文件静态核对。
// 动态生成的 id（例如列表里的 selInfo）不在此列：它们不是顶层绑定。
func TestGroupsJSTopLevelElementIDs(t *testing.T) {
	js, err := os.ReadFile("groups.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("groups.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		ids[m[1]] = true
	}
	top := regexp.MustCompile(`(?m)^\$\(\s*'([^']+)'\s*\)`)
	for _, m := range top.FindAllStringSubmatch(string(js), -1) {
		if !ids[m[1]] {
			t.Errorf("groups.js 顶层引用了 groups.html 中不存在的元素 id: %q", m[1])
		}
	}
}

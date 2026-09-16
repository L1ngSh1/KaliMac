package session

import (
	"strings"
	"testing"
)

// F2 回归（试用记录 prerelease-c3）：容器脚本里 /proc 读写的重定向顺序必须是
// `2>/dev/null < "$p/stat"`。POSIX 重定向自左向右生效，旧写法 `< … 2>/dev/null`
// 会让输入重定向的 "cannot open" 错误消息在 stderr 重定向生效前逃逸到用户终端
// （功能不受影响，纯观感缺陷）。`cut "/proc/$TPID/stat"` 以参数方式打开文件，
// 2>/dev/null 本就有效，不在本守卫范围。
func TestContainerScriptsRedirectOrder(t *testing.T) {
	for name, content := range scripts() {
		if bad := strings.Count(content, `< "$p/stat" 2>/dev/null`); bad != 0 {
			t.Fatalf("%s 含 %d 处旧重定向顺序（< 在 2>/dev/null 之前）", name, bad)
		}
	}
	// 修后的守卫形态必须真实存在（防守卫本身失效成恒真）
	if n := strings.Count(kmCtlSh, `2>/dev/null < "$p/stat"`); n != 3 {
		t.Fatalf("km-ctl 应含 3 处修后重定向顺序, got %d", n)
	}
	if n := strings.Count(kmRunSh, `2>/dev/null < "$p/stat"`); n != 1 {
		t.Fatalf("km-run 应含 1 处修后重定向顺序, got %d", n)
	}
}

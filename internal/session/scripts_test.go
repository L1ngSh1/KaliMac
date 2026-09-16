package session

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
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

// 重定向机制行为测试（审查定稿）：在相同 shell 下，让输入重定向指向确定不存在的
// 文件，对比两种顺序的 stderr 与退出状态。稳定验证重定向机制本身——旧顺序的
// 错误消息在 stderr 重定向生效前逃逸，新顺序被抑制；`|| 兜底`语义在两种顺序下
// 都必须保留（这是清理逻辑一直正确的原因）。翻涌对照与真实取消回归验证应用场景。
func TestRedirectionOrderBehavior(t *testing.T) {
	missing := "/tmp/km-f2-certainly-missing-" + fmt.Sprint(os.Getpid()) + "/stat"
	run := func(stmt string) (stderr string, handled bool) {
		cmd := exec.Command("/bin/sh", "-c", stmt+` || { echo HANDLED; }`)
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("|| 兜底下语句不应失败: %v", err)
		}
		return errb.String(), strings.Contains(out.String(), "HANDLED")
	}
	oldErr, oldHandled := run(fmt.Sprintf("read -r _ < %q 2>/dev/null", missing))
	newErr, newHandled := run(fmt.Sprintf("read -r _ 2>/dev/null < %q", missing))
	if oldErr == "" {
		t.Fatalf("机制前提不成立：旧顺序应向 stderr 泄漏错误消息（环境差异？）")
	}
	if !oldHandled || !newHandled {
		t.Fatalf("两种顺序的 || 兜底语义都应保留: old=%v new=%v", oldHandled, newHandled)
	}
	if newErr != "" {
		t.Fatalf("新顺序 stderr 应为空（这正是 F2 修复点）: %q", newErr)
	}
}

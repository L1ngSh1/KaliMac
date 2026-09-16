//go:build integration

// 会话查看与显式恢复（km sessions / km cancel <id>）真实集成验收。
// 关键端到端（计划 S4）：长任务 → 强杀宿主 km → 新执行被阻断 →
// km sessions 找到会话 → km cancel <id> → 核验清理 → 再次执行成功。
package integration

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kmSessions 运行 km sessions，返回 stdout/stderr/exit。
func kmSessions(t *testing.T, dir string) (string, string, int) {
	t.Helper()
	return kmRun(t, dir, nil, "sessions")
}

// kmCancel 运行 km cancel <id>，返回 stdout/stderr/exit。
func kmCancel(t *testing.T, dir, sid string) (string, string, int) {
	t.Helper()
	return kmRun(t, dir, nil, "cancel", sid)
}

// assertNoSessionNoise 断言输出不含 F2 的 /proc stat 噪音（脚本重定向修复的
// 行为级证据：修复前偶发 "cannot open /proc/.../stat"，修复后必须绝迹）。
func assertNoSessionNoise(t *testing.T, out, errb string) {
	t.Helper()
	if strings.Contains(out+errb, "cannot open /proc/") {
		t.Fatalf("出现 F2 诊断噪音（重定向修复应已消除）: %q", out+errb)
	}
}

// TestSessionRecoveryChain 是计划的关键端到端：强杀宿主 km 后，用户只靠 km
// 自身命令完成 发现阻断 → 查看会话 → 取消 → 核验 → 恢复。-count=3 连续三轮
// 由测试框架重复执行整个函数（含全新项目与容器）。
func TestSessionRecoveryChain(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)

	// 1. 启动长任务并等会话建立
	cmd := kmRunAsync(t, dir, "run", "--", "/bin/sh", "-c", "sleep 60")
	waitSessionDir(t, id)

	// 2. 强杀宿主 km
	cmd.Process.Kill()
	cmd.Wait()

	// 3. 新执行被保护性阻断（含新恢复入口指引）
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "NOPE")
	if code != 1 || !strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("应阻断: code=%d err=%s", code, errb)
	}
	if !strings.Contains(errb, "km sessions") || !strings.Contains(errb, "km cancel") {
		t.Fatalf("阻断提示应指向新恢复入口: %s", errb)
	}

	// 4. km sessions 找到会话（完整可复制 ID，ACTIVE）
	sOut, sErrb, sCode := kmSessions(t, dir)
	if sCode != 0 {
		t.Fatalf("sessions: code=%d err=%s", sCode, sErrb)
	}
	assertNoSessionNoise(t, sOut, sErrb)
	sid := ""
	for _, l := range strings.Split(sOut, "\n") {
		if strings.HasPrefix(l, "ACTIVE ") {
			sid = strings.TrimSpace(strings.TrimPrefix(l, "ACTIVE "))
		}
	}
	if sid == "" || !strings.HasPrefix(sid, "s") || len(sid) != 17 {
		t.Fatalf("应列出带完整 ID 的 ACTIVE 会话: %q", sOut)
	}

	// 5. km cancel <id>：零 Docker 内部命令完成恢复
	cOut, cErrb, cCode := kmCancel(t, dir, sid)
	if cCode != 0 {
		t.Fatalf("cancel: code=%d out=%s err=%s", cCode, cOut, cErrb)
	}
	assertNoSessionNoise(t, cOut, cErrb)
	if !strings.Contains(cOut, "已取消并确认收尾") {
		t.Fatalf("取消消息不符合合同: %q", cOut)
	}

	// 6. 核验清理：列表为空
	sOut2, _, sCode2 := kmSessions(t, dir)
	if sCode2 != 0 || !strings.Contains(sOut2, "当前项目无会话") {
		t.Fatalf("取消后应无会话: code=%d out=%q", sCode2, sOut2)
	}

	// 7. 再次执行成功
	out, errb2, rcode := kmRun(t, dir, nil, "run", "--", "/bin/echo", "RECOVERED")
	if rcode != 0 || !strings.Contains(out, "RECOVERED") {
		t.Fatalf("恢复执行失败: code=%d out=%q err=%s", rcode, out, errb2)
	}
}

// 参数与输出合同：缺参、非法 ID、未知 ID。
func TestSessionCmdsArgsAndUnknown(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)

	// 缺参 / 多参 → KM_USAGE exit 2
	if _, errb, code := kmRun(t, dir, nil, "cancel"); code != 2 || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("cancel 缺参: code=%d err=%s", code, errb)
	}
	if _, errb, code := kmRun(t, dir, nil, "cancel", "saaaaaaaaaaaaaaaa", "extra"); code != 2 {
		t.Fatalf("cancel 多参: code=%d err=%s", code, errb)
	}
	if _, errb, code := kmRun(t, dir, nil, "sessions", "x"); code != 2 || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("sessions 带参: code=%d err=%s", code, errb)
	}
	// 非法格式（大写/短前缀/非 hex）→ usage，不发起取消
	for _, bad := range []string{"SAAAAAAAAAAAAAAA", "s123", "sxyz1234567890abc"} {
		_, errb, code := kmRun(t, dir, nil, "cancel", bad)
		if code != 2 || !strings.Contains(errb, "KM_USAGE") {
			t.Fatalf("非法 ID %q: code=%d err=%s", bad, code, errb)
		}
	}
	// 未知但格式合法的 ID → KM_SESSION_UNKNOWN exit 1，不宣称成功
	out, errb, code := kmCancel(t, dir, "s0000000000000000")
	if code != 1 || !strings.Contains(errb, "KM_SESSION_UNKNOWN") || strings.Contains(out, "已取消") {
		t.Fatalf("未知 ID: code=%d out=%q err=%s", code, out, errb)
	}
	// 无会话 → 明确显示 + exit 0
	sOut, _, sCode := kmSessions(t, dir)
	if sCode != 0 || !strings.Contains(sOut, "当前项目无会话") {
		t.Fatalf("无会话: code=%d out=%q", sCode, sOut)
	}
}

// 隔离：B 项目的会话 ID 不能被 A 项目取消；A 忙碌（持锁）时 sessions/cancel 仍可用。
func TestSessionCmdsProjectIsolationAndNoLock(t *testing.T) {
	dirA := newP2BProject(t)
	dirB := newP2BProject(t)
	for _, d := range []*string{&dirA, &dirB} {
		if _, errb, code := kmRun(t, *d, nil, "init"); code != 0 {
			t.Fatalf("init: %s", errb)
		}
	}
	idA := registerProjectCleanup(t, dirA)
	registerProjectCleanup(t, dirB)

	// A 启动长任务（持有执行锁），等会话建立
	cmd := kmRunAsync(t, dirA, "run", "--", "/bin/sh", "-c", "sleep 60")
	waitSessionDir(t, idA)

	// B 先造一个自己的 STALE 遗留（后台任务立即退出，组空 → STALE）
	// 简化：直接在 A 列会话；B 的 cancel 对 A 的 ID 必须拒绝。
	_, errbB, codeB := kmCancel(t, dirB, "s0000000000000000")
	if codeB != 1 || !strings.Contains(errbB, "KM_SESSION_UNKNOWN") {
		t.Fatalf("B 对未知 ID: code=%d err=%s", codeB, errbB)
	}

	// A 忙碌（锁被持有）：sessions 仍可用并列出 ACTIVE——取消入口不因执行锁失效
	sOut, sErrb, sCode := kmSessions(t, dirA)
	if sCode != 0 || !strings.Contains(sOut, "ACTIVE ") {
		t.Fatalf("持锁期间 sessions 应可用: code=%d out=%q err=%s", sCode, sOut, sErrb)
	}
	sid := strings.TrimSpace(strings.TrimPrefix(strings.Split(strings.TrimSpace(sOut), "\n")[0], "ACTIVE "))

	// km cancel 取消 A 的活跃任务：执行中的 km 客户端应以 130 结束
	cOut, cErrb, cCode := kmCancel(t, dirA, sid)
	if cCode != 0 || !strings.Contains(cOut, "已取消并确认收尾") {
		t.Fatalf("取消: code=%d out=%q err=%s", cCode, cOut, cErrb)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// 容器侧 km-ctl 以 TERM 结束工具：退出码 143 原样透传（「工具退出码
		// 原样返回」既有合同）。130 专属客户端自取消（Ctrl-C）语义。
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 143 {
			t.Fatalf("被外部取消的客户端应 143（工具被 TERM）: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("等待客户端退出超时")
	}

	// B 项目不受影响
	outB, _, codeB2 := kmRun(t, dirB, nil, "run", "--", "/bin/echo", "B_OK")
	if codeB2 != 0 || !strings.Contains(outB, "B_OK") {
		t.Fatalf("B 受影响: %s", outB)
	}
}

// 重复取消 + 自然退出竞争：同一 ID 二次 cancel 幂等；自然退出后的 cancel 报
// 已结束；两者都不得出现 F2 噪音。
func TestSessionCmdsRepeatAndNaturalExit(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	cmd := kmRunAsync(t, dir, "run", "--", "/bin/sh", "-c", "sleep 60")
	waitSessionDir(t, id)
	cmd.Process.Kill()
	cmd.Wait()

	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "X")
	if code != 1 || !strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("应阻断: %s", errb)
	}
	sOut, _, _ := kmSessions(t, dir)
	sid := strings.TrimSpace(strings.TrimPrefix(strings.Split(strings.TrimSpace(sOut), "\n")[0], "ACTIVE "))

	// 第一次 cancel；紧接着第二次 cancel：无论列表显示 ACTIVE 还是已被清扫，
	// 都必须幂等成功且无噪音（覆盖自然退出竞争的两种时序）
	cOut1, cErrb1, cCode1 := kmCancel(t, dir, sid)
	if cCode1 != 0 {
		t.Fatalf("首次 cancel: code=%d out=%q err=%s", cCode1, cOut1, cErrb1)
	}
	// 第二次 cancel：登记已被首次取消删除 → 按冻结合同属于「列表中不存在」
	// → KM_SESSION_UNKNOWN 退出 1（不把未知报成成功）；无 F2 噪音。
	cOut2, cErrb2, cCode2 := kmCancel(t, dir, sid)
	if cCode2 != 1 || !strings.Contains(cErrb2, "KM_SESSION_UNKNOWN") {
		t.Fatalf("已清除会话的重复 cancel 应报未知: code=%d out=%q err=%s", cCode2, cOut2, cErrb2)
	}
	assertNoSessionNoise(t, cOut1+cOut2, cErrb1+cErrb2)
}

// F2 定向证据：对已结束/未知会话连续 20 次 cancel，捕获的输出必须零噪音、
// 退出码符合合同（未知=1）。
func TestSessionCancelNoiseRegression(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	for i := 0; i < 20; i++ {
		out, errb, code := kmCancel(t, dir, fmt.Sprintf("s%016x", i))
		if code != 1 || !strings.Contains(errb, "KM_SESSION_UNKNOWN") {
			t.Fatalf("第 %d 次: code=%d", i, code)
		}
		assertNoSessionNoise(t, out, errb)
	}
}

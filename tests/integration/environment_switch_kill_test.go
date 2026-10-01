//go:build integration

// 真实宿主 kill -9 阶段边界实验（计划缺口 2）：在 switch/rollback 的不同
// 阶段强杀 km 进程组（含 docker 子进程，模拟宿主崩溃的"操作成功但响应丢失"），
// 然后断言恢复不变量：
//   - recover 幂等收敛，事务清除；
//   - 文件（.km.json/.km/state.json）与运行时实际执行一致；
//   - 资源账本恒等式成立（实际容器 = 当前代 + 槽位 + retained），doctor 无账本外告警。
//
// 轮询观测与 SIGKILL 落点之间存在竞态（km 可能已推进到下一阶段甚至完成），
// 因此不断言"杀在哪个阶段"，只断言"无论杀在哪，最终状态一致且可恢复"。
package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envTxnInfo 读取未完成事务的 kind/stage；文件不存在返回 ("", "")。
func envTxnInfo(dir string) (kind, stage string) {
	raw, err := os.ReadFile(filepath.Join(dir, ".km", "env", "transaction.json"))
	if err != nil {
		return "", ""
	}
	var m struct {
		Kind  string `json:"kind"`
		Stage string `json:"stage"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "", ""
	}
	return m.Kind, m.Stage
}

// envRunAndKill9 启动 km 子命令（独立进程组），轮询 cond 命中后对进程组发
// SIGKILL；返回是否命中（false = 进程在命中前自行结束）。
func envRunAndKill9(t *testing.T, dir string, cond func() bool, args ...string) (killed, completedOK bool, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(kmBin, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 km 失败: %v", err)
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case werr := <-done: // 命中前自行结束
			return false, werr == nil, outBuf.String(), errBuf.String()
		default:
		}
		if cond() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatalf("SIGKILL 后进程组未退出（pid=%d）", pid)
			}
			return true, false, outBuf.String(), errBuf.String()
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	t.Fatalf("60s 内未命中 kill 条件（kill 实验前提失败）")
	return false, false, "", ""
}

// envAssertRecoveredInvariant 断言恢复后的最终不变量：
// 文件↔运行时一致、账本恒等式、doctor 无账本外告警、事务清除、recover 幂等。
func envAssertRecoveredInvariant(t *testing.T, dir string) {
	t.Helper()
	// recover 幂等：dry-run 与两次真实调用都必须干净收敛
	out, errb, code := kmRun(t, dir, nil, "env", "recover", "--dry-run")
	if code != 0 {
		t.Fatalf("recover --dry-run: code=%d out=%s err=%s", code, out, errb)
	}
	recOut, recErr, recCode := kmRun(t, dir, nil, "env", "recover", "--yes")
	if recCode != 0 {
		t.Fatalf("recover: code=%d out=%s err=%s", recCode, recOut, recErr)
	}
	out, _, code = kmRun(t, dir, nil, "env", "recover")
	if code != 0 || !strings.Contains(out, "无需恢复") {
		t.Fatalf("二次 recover 应无需恢复: code=%d out=%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".km", "env", "transaction.json")); !os.IsNotExist(err) {
		t.Fatal("事务必须清除")
	}

	// 文件 ↔ 运行时一致：当前配置引用决定实际执行环境
	rawCfg, err := os.ReadFile(filepath.Join(dir, ".km.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "env-a"
	if strings.Contains(string(rawCfg), envImgBTag) {
		want = "env-b"
	}
	out, errb, code = kmRun(t, dir, nil, "run", "--", "cat", envMarkerPath)
	if code != 0 {
		t.Fatalf("恢复后执行: code=%d out=%s err=%s", code, out, errb)
	}
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("文件与运行时不一致：config 指向 %s，实际 marker=%s", want, got)
	}

	// 账本恒等式：实际容器 = 当前代(1) + 槽位(若有) + retained 条数
	n := 1
	if _, err := os.Stat(filepath.Join(dir, ".km", "env", "previous.json")); err == nil {
		n++
	}
	if raw, err := os.ReadFile(filepath.Join(dir, ".km", "env", "retained.json")); err == nil {
		var r struct {
			Containers []json.RawMessage `json:"containers"`
		}
		if json.Unmarshal(raw, &r) == nil {
			n += len(r.Containers)
		}
	}
	PID := envProjectID(t, dir)
	lout, lcode := runCapture(t, nil, "ps", "-aq", "--filter", "label=km.project="+PID)
	if lcode != 0 {
		t.Fatalf("docker ps 失败: %s", lout)
	}
	got := len(strings.Fields(lout))
	if got != n {
		t.Fatalf("账本恒等式失败：实际容器 %d，期望 %d（当前代+槽位+retained）", got, n)
	}

	// doctor 不得出现账本外告警；status 不得是 unknown/pending
	dout, _, dcode := kmRun(t, dir, nil, "doctor")
	if dcode != 0 {
		t.Fatalf("doctor 退出码 %d", dcode)
	}
	if strings.Contains(dout, "发现账本外的项目容器") {
		t.Fatalf("doctor 报告账本外容器:\n%s", dout)
	}
	sout, _, scode := kmRun(t, dir, nil, "status", "--json")
	if scode != 0 {
		t.Fatalf("status 退出码 %d: %s", scode, sout)
	}
	var st struct {
		State string `json:"state"`
	}
	if json.Unmarshal([]byte(sout), &st) != nil {
		t.Fatalf("status json 解析: %s", sout)
	}
	switch st.State {
	case "running_idle", "running_session_active", "container_stopped":
	default:
		t.Fatalf("恢复后 status 应为正常态，得到 %q", st.State)
	}
}

// 各阶段边界强杀 switch；无论落点在哪，恢复后必须一致可用。
func TestEnvSwitchRealKill9StageBoundaries(t *testing.T) {
	scenarios := []struct {
		name string
		cond func(dir string) bool
	}{
		{"事务建立窗口", func(dir string) bool {
			_, stage := envTxnInfo(dir)
			return stage != ""
		}},
		{"旧容器停止后", func(dir string) bool {
			_, stage := envTxnInfo(dir)
			return stage == "OLD_STOPPED"
		}},
		{"提交点", func(dir string) bool {
			_, stage := envTxnInfo(dir)
			return stage == "COMMIT_INTENT"
		}},
		{"提交完成", func(dir string) bool {
			_, stage := envTxnInfo(dir)
			return stage == "CURRENT_COMMITTED"
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			dir := envInitProject(t)
			killed, completedOK, out, errb := envRunAndKill9(t, dir, func() bool { return sc.cond(dir) },
				"env", "switch", "--image", envImgBTag, "--yes")
			t.Logf("kill 命中=%v 完整成功=%v；switch 输出=%q err=%q", killed, completedOK, strings.TrimSpace(out), strings.TrimSpace(errb))
			if !killed && !completedOK {
				t.Fatalf("switch 未命中 kill 条件且未成功完成（实验无效）: %s", errb)
			}
			envAssertRecoveredInvariant(t, dir)
		})
	}
}

// 回退阶段强杀：同样只断言恢复不变量。
func TestEnvRollbackRealKill9StageBoundary(t *testing.T) {
	dir := envInitProject(t)
	if out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", envImgBTag, "--yes"); code != 0 {
		t.Fatalf("前置 switch: %s %s", out, errb)
	}
	killed, completedOK, out, errb := envRunAndKill9(t, dir, func() bool {
		kind, stage := envTxnInfo(dir)
		return kind == "rollback" && stage == "OLD_STOPPED"
	}, "env", "rollback", "--yes")
	t.Logf("kill 命中=%v 完整成功=%v；rollback 输出=%q err=%q", killed, completedOK, strings.TrimSpace(out), strings.TrimSpace(errb))
	if !killed && !completedOK {
		t.Fatalf("rollback 未命中 kill 条件且未成功完成（实验无效）: %s", errb)
	}
	envAssertRecoveredInvariant(t, dir)
}

// remove 的真实强杀：无论死在 rm 前还是 rm 后，recover 都能收尾
// （容器删除或重试删除 + 账本收尾），且不可逆语义不重建容器。
func TestEnvRemoveRealKill9(t *testing.T) {
	dir := envInitProject(t)
	if out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", envImgBTag, "--yes"); code != 0 {
		t.Fatalf("前置 switch: %s %s", out, errb)
	}
	if out, errb, code := kmRun(t, dir, nil, "env", "rollback", "--yes"); code != 0 {
		t.Fatalf("前置 rollback: %s %s", out, errb)
	}
	// 现在：当前 gen0（A），retained 含 gen1（B，exited）
	target := containerFullIDByName(t, "km-"+envProjectID(t, dir)+"-g1")
	killed, completedOK, out, errb := envRunAndKill9(t, dir, func() bool {
		k, _ := envTxnInfo(dir)
		return k == "remove"
	}, "env", "remove", target, "--yes")
	t.Logf("kill 命中=%v 完整成功=%v；remove 输出=%q err=%q", killed, completedOK, strings.TrimSpace(out), strings.TrimSpace(errb))
	if !killed && !completedOK {
		t.Fatalf("remove 未命中 kill 条件且未成功完成（实验无效）: %s", errb)
	}
	// 不变量：recover 收敛后，要么容器已删且账本条目移除（不可逆），
	// 要么……remove 没有第二种终态：容器必然被删除（恢复语义永不重建）。
	recOut, recErr, recCode := kmRun(t, dir, nil, "env", "recover", "--yes")
	if recCode != 0 {
		t.Fatalf("recover: code=%d out=%s err=%s", recCode, recOut, recErr)
	}
	if _, missing := envContainerState(t, "km-"+envProjectID(t, dir)+"-g1"); !missing {
		t.Fatal("remove 收敛后目标容器应已删除（不重建）")
	}
	ret, err := os.ReadFile(filepath.Join(dir, ".km", "env", "retained.json"))
	if err == nil && strings.Contains(string(ret), target) {
		t.Fatalf("账本应收尾: %s", ret)
	}
	envAssertRecoveredInvariant(t, dir)
}

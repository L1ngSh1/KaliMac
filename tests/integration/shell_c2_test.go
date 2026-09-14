//go:build integration

// C2：km shell 产品入口验收（真实 PTY 驱动 `km shell`）。
// 覆盖：交互可用/会话登记可见/Ctrl-C/退出码透传/SIGKILL 后阻断后续任务/
// termios 恢复/多项目隔离/非 tty 拒绝。原始日志与结构化结果入 evidence。
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runKmShellScenario 用 PTY 驱动真实 `km shell`（cwd=项目目录）。
func runKmShellScenario(t *testing.T, dir string, steps []step) map[string]any {
	t.Helper()
	scen := filepath.Join(t.TempDir(), "steps.json")
	raw, _ := json.Marshal(steps)
	if err := os.WriteFile(scen, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(c1EvidenceDir, fmt.Sprintf("%s.log", t.Name()))
	resPath := filepath.Join(c1EvidenceDir, fmt.Sprintf("%s.json", t.Name()))
	cmd := exec.Command("python3", filepath.Join("..", "integration", "shellproto_driver.py"),
		"-mode", "km", "-proto", kmBin, "-cwd", dir,
		"-container", "unused", "-scenario", scen, "-log", logPath, "-json", resPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("驱动器执行失败: %v", err)
	}
	rawJSON, err := os.ReadFile(resPath)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(rawJSON, &res); err != nil {
		t.Fatal(err)
	}
	rawLog, _ := os.ReadFile(logPath)
	res["raw"] = string(rawLog)
	res["log_path"] = logPath
	// 场景失败（步骤超时/EOF/异常退出/未退出）统一传播为测试失败（review A）
	assertScenarioOK(t, res)
	if _, ok := res["exit_code"]; !ok {
		t.Fatalf("驱动器未报告退出码: %v", res)
	}
	return res
}

func kmShellSteps() []step {
	return expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 25},
	)
}

// T1：km shell 可用、会话登记可见（kind=shell）、命令可执行；正常退出后
// 遗留目录被自动清扫、后续 run 正常。
func TestC2ShellInteractiveAndRegistered(t *testing.T) {
	if os.Getenv("C2_PROBE") == "1" {
		// 探针模式：在会话内打印登记目录与环境变量
		dir := newP2BProject(t)
		if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
			t.Fatalf("init: %s", errb)
		}
		registerProjectCleanup(t, dir)
		res := runKmShellScenario(t, dir, append(kmShellSteps(),
			step{"op": "send", "text": "ls /tmp/km-sessions; echo SID=$KM_SESSION_ID; ls /tmp/km-sessions/$KM_SESSION_ID\r"},
			step{"op": "expect", "pattern": "SID=s", "timeout": 10},
			step{"op": "send", "text": "exit\r"},
		))
		fmt.Println("PROBE RAW:\n" + res["raw"].(string))
		return
	}
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)

	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "send", "text": "ps -o pid,pgid,sid,tpgid,comm\r"},
		step{"op": "expect", "pattern": "bash", "timeout": 10},
		step{"op": "send", "text": "echo SHELL_WORKS=$HOME\r"},
		step{"op": "expect", "pattern": "SHELL_WORKS=", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "SHELL_WORKS=") {
		t.Fatalf("shell 内命令未执行:\n%s", raw)
	}
	assertNoJobControlErrors(t, raw)
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("exit=%v", res["exit_code"])
	}
	if res["termios_equal"] != true {
		t.Fatal("退出后 termios 未恢复")
	}

	// 正常退出后 km 主动清扫自己的会话目录 → sessions 应为空
	out, _ := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "sessions").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("正常退出后会话目录应已清扫: %q", string(out))
	}

	// 后续 run 正常
	outRun, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "AFTER_SHELL")
	if code != 0 || !strings.Contains(outRun, "AFTER_SHELL") {
		t.Fatalf("shell 后 run: code=%d out=%q err=%s", code, outRun, errb)
	}
}

// T2：sleep 中 Ctrl-C → 命令 130、shell 存活；随后可继续执行。
func TestC2ShellCtrlC(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "send", "text": "sleep 5\r"},
		step{"op": "settle", "secs": 0.6},
		step{"op": "sendb", "b64": "Aw=="},
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "echo C=$?\r"},
		step{"op": "expect", "pattern": "C=130", "timeout": 10},
		step{"op": "send", "text": "echo ALIVE\r"},
		step{"op": "expect", "pattern": "ALIVE", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "C=130") || !strings.Contains(raw, "ALIVE") {
		t.Fatalf("Ctrl-C 语义验证失败:\n%s", raw)
	}
}

// T3：shell 内 exit 7 原样透传为 km 退出码。
func TestC2ShellExitCodePassthrough(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "send", "text": "exit 7\r"},
	))
	if res["exit_code"].(float64) != 7 {
		t.Fatalf("exit 7 应透传: %v", res["exit_code"])
	}
	if res["termios_equal"] != true {
		t.Fatal("termios 未恢复")
	}
}

// T4（核心）：SIGKILL km 后，登记的 shell 会话阻断后续 run；显式 cancel 后恢复。
func TestC2ShellSessionBlocksAfterHostKill(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "signal", "name": "SIGKILL"},
	))
	if sig, ok := res["exit_sig"].(float64); !ok || sig != 9 {
		t.Fatalf("SIGKILL 应终止 km: sig=%v exit=%v", res["exit_sig"], res["exit_code"])
	}
	// km 已死，登记的 shell 会话目录仍在（bash 存活）
	_, bashPid := waitSessionDir(t, id)
	_ = bashPid

	// 后续 run 必须被阻断（shell 会话登记 + M0 语义）
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "MUST_NOT_RUN")
	if code != 1 || !strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("shell 会话应阻断 run: code=%d err=%s", code, errb)
	}
	// 显式恢复
	sessOut, _ := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "sessions").Output()
	sid := ""
	for _, line := range strings.Split(string(sessOut), "\n") {
		if strings.HasPrefix(line, "ACTIVE ") {
			sid = strings.TrimSpace(strings.TrimPrefix(line, "ACTIVE "))
		}
	}
	if sid == "" {
		t.Fatalf("应列出活跃 shell 会话: %q", string(sessOut))
	}
	exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "cancel", sid).Run()
	out, _, code2 := kmRun(t, dir, nil, "run", "--", "/bin/echo", "RECOVERED")
	if code2 != 0 || !strings.Contains(out, "RECOVERED") {
		t.Fatalf("恢复后应可执行: code=%d out=%q", code2, out)
	}
}

// T5：非 tty 环境下 km shell 拒绝（KM_NOT_TTY，exit 1），不创建任何容器副作用。
func TestC2ShellRequiresTTY(t *testing.T) {
	dir := newP2BProject(t)
	cmd := exec.Command(kmBin, "shell")
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err == nil {
		t.Fatal("非 tty 应失败")
	}
	if !strings.Contains(errb.String(), "KM_NOT_TTY") {
		t.Fatalf("应报 KM_NOT_TTY: %q", errb.String())
	}
}

// T6：SIGKILL 宿主 shell 后容器内 bash 存活（风险观察，与 C1 一致）。
func TestC2ShellSigkillBashSurvives(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "signal", "name": "SIGKILL"},
	))
	if sig, ok := res["exit_sig"].(float64); !ok || sig != 9 {
		t.Fatalf("SIGKILL 应终止 km: sig=%v exit=%v", res["exit_sig"], res["exit_code"])
	}
	_, bashPid := waitSessionDir(t, id)
	time.Sleep(400 * time.Millisecond)
	out, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"ps -e -o pid,comm | grep -w bash").Output()
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("风险观察失败：SIGKILL 后 bash 竟已退出（语义变化？）")
	}
	exec.Command("docker", "exec", id, "kill", "-9", bashPid).Run()
}

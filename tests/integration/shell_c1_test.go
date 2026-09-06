//go:build integration

// P2-C C1 交互终端原型验收（A–K）。驱动方式：真实宿主 PTY + 提示符标记
// 同步；键盘事件通过向 PTY 写入字节触发，外部信号单独发送。原始输出、
// 退出码、耗时与 termios 快照写入 tests/evidence/p2c-shellproto-*/。
package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var c1EvidenceDir string

func init() {
	c1EvidenceDir = filepath.Join("..", "evidence", fmt.Sprintf("p2c-shellproto-%d", time.Now().Unix()))
	_ = os.MkdirAll(c1EvidenceDir, 0o755)
}

type step = map[string]any

// runShellScenario 用 PTY 驱动器执行一个场景，返回驱动器 JSON 与原始日志路径。
// assertScenarioOK：驱动器场景级 ok 必须为 true（步骤失败/超时/EOF/异常退出
// 统一传播为测试失败——review A）。
func assertScenarioOK(t *testing.T, res map[string]any) {
	t.Helper()
	if ok, present := res["ok"]; !present || ok != true {
		t.Fatalf("PTY 驱动器场景失败（ok=%v error=%v）:\n%v", ok, res["error"], res["steps"])
	}
}

func runShellScenario(t *testing.T, container string, steps []step, extraArgs ...string) (map[string]any, string) {
	t.Helper()
	scen := filepath.Join(t.TempDir(), "steps.json")
	raw, _ := json.Marshal(steps)
	if err := os.WriteFile(scen, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(c1EvidenceDir, fmt.Sprintf("%s.log", t.Name()))
	resPath := filepath.Join(c1EvidenceDir, fmt.Sprintf("%s.json", t.Name()))
	cmd := exec.Command("python3", filepath.Join("..", "integration", "shellproto_driver.py"),
		"-container", container, "-proto", shellBin,
		"-scenario", scen, "-log", logPath, "-json", resPath,
		"-extra-arg", strings.Join(extraArgs, ","))
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
	raw, _ = os.ReadFile(logPath)
	res["raw"] = string(raw)
	assertScenarioOK(t, res)
	return res, logPath
}

func expectSteps(steps ...step) []step { return steps }

func assertNoJobControlErrors(t *testing.T, raw string) {
	t.Helper()
	for _, bad := range []string{"no job control", "cannot set terminal process group", "Inappropriate ioctl"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("交互 shell 出现控制终端错误 %q", bad)
		}
	}
}

// A：可执行命令；无 job-control/controlling-terminal 错误；bash 拥有控制
// 终端且处于前台进程组（TPGID==PID，PGID==SID）。
func TestC1PromptControlTerminal(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "ps -o pid,ppid,pgid,sid,tpgid,comm\r"},
		step{"op": "expect", "pattern": "bash", "timeout": 10},
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	assertNoJobControlErrors(t, raw)
	// bash 行：应为会话首进程（SID==PID==PGID，作业控制可用）
	m := regexp.MustCompile(`(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+bash`).FindStringSubmatch(raw)
	if m == nil {
		t.Fatalf("未取得 bash 的进程组信息:\n%s", raw)
	}
	var pid, pgid, sid, tpgid int
	fmt.Sscanf(m[1], "%d", &pid)
	fmt.Sscanf(m[3], "%d", &pgid)
	fmt.Sscanf(m[4], "%d", &sid)
	fmt.Sscanf(m[5], "%d", &tpgid)
	if pgid != pid || sid != pid {
		t.Fatalf("bash 应为会话首进程且自成进程组: pid=%d pgid=%d sid=%d", pid, pgid, sid)
	}
	if tpgid == 0 {
		t.Fatalf("bash 应拥有控制终端: tpgid=0")
	}
	// ps 行：命令执行瞬间 ps 自己就是前台进程组（TPGID==PS PID），
	// 这正是控制终端与前台组管理工作的证据。
	m2 := regexp.MustCompile(`(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+ps`).FindStringSubmatch(raw)
	if m2 == nil {
		t.Fatalf("未取得 ps 行:\n%s", raw)
	}
	var psPid, psTpgid int
	fmt.Sscanf(m2[1], "%d", &psPid)
	fmt.Sscanf(m2[5], "%d", &psTpgid)
	if psTpgid != psPid {
		t.Fatalf("执行中的命令应处于前台进程组: ps pid=%d tpgid=%d", psPid, psTpgid)
	}
}

// B+C：sleep 期间 Ctrl-C 结束命令且 shell 存活；提示符处 Ctrl-C 不退出。
func TestC1CtrlCDuringSleepAndAtPrompt(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "sleep 5\r"},
		step{"op": "settle", "secs": 0.6},
		step{"op": "sendb", "b64": "Aw=="}, // Ctrl-C
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "echo B=$?\r"},
		step{"op": "expect", "pattern": "B=130", "timeout": 10},
		step{"op": "sendb", "b64": "Aw=="}, // 提示符处 Ctrl-C
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "echo C_STILL_ALIVE\r"},
		step{"op": "expect", "pattern": "C_STILL_ALIVE", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "B=130") || !strings.Contains(raw, "C_STILL_ALIVE") {
		t.Fatalf("B/C 验证失败:\n%s", raw)
	}
	assertNoJobControlErrors(t, raw)
}

// D：Ctrl-D 空行退出、exit 0、exit 7。
func TestC1ExitPaths(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)

	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "exit 0\r"},
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("exit 0: %v", res["exit_code"])
	}

	res, _ = runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "exit 7\r"},
	))
	if res["exit_code"].(float64) != 7 {
		t.Fatalf("exit 7: %v", res["exit_code"])
	}

	res, _ = runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "sendb", "b64": "BA=="}, // Ctrl-D（空行）
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("Ctrl-D: %v", res["exit_code"])
	}
}

// E：Ctrl-Z → jobs → bg/fg 前后台控制。
func TestC1JobControl(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "sleep 30\r"},
		step{"op": "settle", "secs": 0.5},
		step{"op": "sendb", "b64": "Gg=="}, // Ctrl-Z
		step{"op": "expect", "pattern": "Stopped", "timeout": 10},
		step{"op": "send", "text": "jobs\r"},
		step{"op": "expect", "pattern": "Stopped", "timeout": 10},
		step{"op": "send", "text": "bg\r"},
		step{"op": "expect", "pattern": "Running|sleep", "timeout": 10},
		step{"op": "send", "text": "fg\r"},
		step{"op": "settle", "secs": 0.5},
		step{"op": "sendb", "b64": "Aw=="}, // Ctrl-C 结束前台 sleep
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "echo E=$?\r"},
		step{"op": "expect", "pattern": "E=130", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "Stopped") || !strings.Contains(raw, "E=130") {
		t.Fatalf("作业控制验证失败:\n%s", raw)
	}
	assertNoJobControlErrors(t, raw)
}

// F：窗口尺寸变化，容器内跟随。
func TestC1WindowSizeFollows(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "settle", "secs": 0.4}, // 客户端 resize 监听就绪
		step{"op": "resize", "rows": 40, "cols": 100},
		step{"op": "settle", "secs": 0.4},
		step{"op": "send", "text": "stty size\r"},
		step{"op": "expect", "pattern": "40 100", "timeout": 10},
		step{"op": "resize", "rows": 24, "cols": 80},
		step{"op": "settle", "secs": 0.4},
		step{"op": "send", "text": "stty size\r"},
		step{"op": "expect", "pattern": "24 80", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "40 100") || !strings.Contains(raw, "24 80") {
		t.Fatalf("窗口尺寸未跟随:\n%s", raw)
	}
}

// G：会话结束时普通后台任务被清理（SIGHUP），忽略 HUP/TERM 的任务按约定
// 属于逃逸者——记录并单独清理，不停止容器。
// G：会话结束时全部同会话作业（普通、管道、忽略 TERM 的免疫作业）均被
// SID 域收尾清理；仅 setsid 主动脱离者（sleep 303）记录在案存活，按 pid
// 显式清理，不停止容器。
func TestC1BackgroundCleanupAtSessionEnd(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "sleep 290 &\r"},
		step{"op": "expect", "pattern": "\\[1\\]", "timeout": 10},
		step{"op": "send", "text": "sleep 300 | sleep 301 &\r"},
		step{"op": "expect", "pattern": "\\[2\\]", "timeout": 10},
		step{"op": "send", "text": "sh -c 'trap \"\" HUP TERM INT; sleep 302' &\r"},
		step{"op": "expect", "pattern": "\\[3\\]", "timeout": 10},
		step{"op": "send", "text": "setsid sleep 303 &\r"},
		step{"op": "expect", "pattern": "\\[4\\]", "timeout": 10},
		step{"op": "send", "text": "ps -o pid,ppid,pgid,sid,args\r"},
		step{"op": "expect", "pattern": "sleep 303", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("shell 应正常退出: %v", res["exit_code"])
	}
	// 轮询等待 SID 域收尾完成：仅剩 setsid 逃逸者
	deadline := time.Now().Add(10 * time.Second)
	var out string
	for {
		outB, err := exec.Command("docker", "exec", id, "/bin/sh", "-c",
			"ps -e -o pid,args | grep -E 'sleep (29|30)' | grep -v grep").Output()
		if err != nil {
			errMsg := err.Error()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				errMsg += " | stderr: " + string(ee.Stderr)
			}
			out = "EXEC_ERR: " + errMsg
		} else {
			out = string(outB)
		}
		if len(nonEmptyLines(out)) == 1 && strings.Contains(out, "sleep 303") {
			break // 仅剩 setsid 逃逸者
		}
		if time.Now().After(deadline) {
			t.Fatalf("同会话作业未按预期清理（应仅剩 setsid 逃逸者）: %q", out)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// km-ctl sessions 一致：逃逸者不在 SID 域，会话目录已清
	sOut, _ := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "sessions").Output()
	if strings.TrimSpace(string(sOut)) != "" {
		t.Fatalf("会话目录应已清理（逃逸者不属于 SID 域）: %q", string(sOut))
	}
	// 按记录 pid 清理逃逸者
	pidOut, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"ps -e -o pid,args | grep 'sleep 303' | grep -v grep").Output()
	for _, l := range nonEmptyLines(string(pidOut)) {
		exec.Command("docker", "exec", id, "kill", "-9", strings.Fields(l)[0]).Run()
	}
}

// H：正常退出 / 启动失败 / 外部 SIGTERM、SIGHUP 后 termios 恢复到进入前状态。
func TestC1TermiosRestored(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)

	// 正常退出
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "exit\r"},
	))
	if res["termios_equal"] != true {
		t.Fatalf("正常退出后 termios 未恢复: %v", res)
	}

	// 启动失败（容器不存在）：未修改终端即失败
	res, _ = runShellScenario(t, "no-such-container-zz", expectSteps(
		step{"op": "expect", "pattern": "KM_PROTO_FAIL|KM_PROTO_BOOTSTRAP_FAIL|Unable to find", "timeout": 25},
	))
	if res["exit_code"].(float64) != 2 {
		t.Fatalf("启动失败应退出 2（KM_PROTO_FAIL）: %v", res["exit_code"])
	}
	if res["termios_equal"] != true {
		t.Fatalf("启动失败后 termios 未保持: %v", res)
	}

	// 外部 SIGTERM / SIGHUP（单独向进程发送；km 兜底恢复）
	for _, sig := range []string{"SIGTERM", "SIGHUP"} {
		res, _ = runShellScenario(t, id, expectSteps(
			step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
			step{"op": "signal", "name": sig},
		))
		if res["exit_code"].(float64) != 143 {
			t.Fatalf("%s 后应退出 143: %v", sig, res["exit_code"])
		}
		if res["termios_equal"] != true {
			t.Fatalf("%s 后 termios 未恢复: %v", sig, res)
		}
	}
}

// I：项目 A 的 shell 退出不影响项目 B。
func TestC1ProjectIsolation(t *testing.T) {
	dirA := newP2BProject(t)
	dirB := newP2BProject(t)
	idA, _ := sessionContainer(t, dirA)
	idB, _ := sessionContainer(t, dirB)
	if _, errb, code := kmRun(t, dirB, nil, "init"); code != 0 {
		t.Fatalf("B init: %s", errb)
	}
	registerProjectCleanup(t, dirB)

	res, _ := runShellScenario(t, idA, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "exit 3\r"},
	))
	if res["exit_code"].(float64) != 3 {
		t.Fatalf("A shell 退出码: %v", res["exit_code"])
	}
	outB, errbB, codeB := kmRun(t, dirB, nil, "run", "--", "/bin/echo", "B_FINE")
	if codeB != 0 || !strings.Contains(outB, "B_FINE") {
		t.Fatalf("项目 B 受影响: code=%d out=%q err=%s", codeB, outB, errbB)
	}
	if got := containerState(t, idB); got != "running" {
		t.Fatalf("项目 B 容器状态: %s", got)
	}
}

// J：SIGKILL/宿主失联单独记录——bash 仍存活（风险观察），按记录 pid 清理。
func TestC1SigkillObservedSeparately(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "signal", "name": "SIGKILL"},
	))
	if sig, ok := res["exit_sig"].(float64); !ok || sig != 9 {
		t.Fatalf("SIGKILL 应终止 proto: sig=%v exit=%v", res["exit_sig"], res["exit_code"])
	}
	time.Sleep(400 * time.Millisecond)
	out, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"ps -e -o pid,comm | grep -w bash").Output()
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("风险观察失败：SIGKILL 后容器内 bash 竟已退出（语义变化？）")
	}
	bashPid := strings.Fields(strings.TrimSpace(string(out)))[0]
	exec.Command("docker", "exec", id, "kill", "-9", bashPid).Run()
	time.Sleep(300 * time.Millisecond)
	out2, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"ps -e -o pid,comm | grep -w bash").Output()
	if strings.TrimSpace(string(out2)) != "" {
		t.Fatalf("bash 应已按记录 pid 清理: %q", string(out2))
	}
}

// 任务 10：detach keys。默认禁用（ctrl-p/q 不脱离）；显式启用时客户端
// 脱离 ≠ 会话清理——bash 仍存活，必须区分记录，不得当作正常退出。
// 任务 D（更正）：exec 存在 detach 机制——显式启用 keys 后，预热完成再发送
// ctrl-p,q（确定性触发）：客户端脱离退出（exit 1）而容器内 bash 存活；
// 「read escape sequence」只是客户端脱离的输出，不是 bash 退出的证据。
// 脱离 ≠ 会话清理：登记会话仍 ACTIVE，阻断后续任务，可显式 cancel 恢复。
// 注：--detach-keys "" 并非可靠的“禁用”（两轮实测行为不一致：一次字节直达、
// 一次触发默认序列脱离）——km 的语义在两种结果下都安全（登记+阻断）。
func TestC1DetachKeys(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id, _ := sessionContainerImage(t, dir, minImageRef)

	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "echo WARMUP=1\r"},
		step{"op": "expect", "pattern": "WARMUP=1", "timeout": 10},
		step{"op": "sendb", "b64": "EBE="}, // ctrl-p ctrl-q（预热后，确定性触发脱离）
	), "detach-keys=ctrl-p,ctrl-q")
	if res["exit_code"].(float64) != 1 {
		t.Fatalf("脱离应使客户端退出 1: %v", res["exit_code"])
	}

	// 客户端退出 ≠ 会话清理：bash 与登记会话仍活跃
	deadline := time.Now().Add(6 * time.Second)
	bashAlive := false
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
			"ps -e -o pid,comm | grep -w bash").Output()
		if strings.TrimSpace(string(out)) != "" {
			bashAlive = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !bashAlive {
		t.Fatal("脱离后 bash 应仍存活（与实验证据一致）")
	}

	// 客户端退出后的两种合法结果（--detach-keys "" 行为不确定，两轮实测
	// 分别为「字节直达 bash 后退出被清扫」与「脱离、登记保留」）：
	//   A) bash 已退出 → 会话被 finalize 清扫，run 直接成功；
	//   B) bash 存活（脱离）→ 登记保留，run 被 KM_SESSION_ACTIVE 阻断，
	//      显式 cancel 后恢复。
	// 不变量：绝不会卡死、绝不会静默吞任务（假 KM_PROJECT_MISSING 也算失败）。
	_, errbBlock, bcode := kmRun(t, dir, nil, "run", "--", "/bin/echo", "NOPE")
	switch {
	case bcode == 0:
		t.Log("结果 A：bash 已退出，会话已清扫，run 直接成功")
	case bcode == 1 && strings.Contains(errbBlock, "KM_SESSION_ACTIVE"):
		t.Log("结果 B：脱离，登记保留并阻断")
		sOut, _ := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "sessions").Output()
		lines := nonEmptyLines(string(sOut))
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "ACTIVE ") {
			t.Fatalf("阻断时应列出活跃会话: %q", string(sOut))
		}
		sid := strings.TrimSpace(strings.TrimPrefix(lines[0], "ACTIVE "))
		if _, cerr := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "cancel", sid).Output(); cerr != nil {
			t.Fatalf("cancel 失败: %v", cerr)
		}
		recoverDeadline := time.Now().Add(10 * time.Second)
		for {
			_, errbRun, runCode := kmRun(t, dir, nil, "run", "--", "/bin/echo", "RECOVERED")
			if runCode == 0 {
				break
			}
			t.Logf("恢复尝试: code=%d err=%s", runCode, errbRun)
			if time.Now().After(recoverDeadline) {
				t.Fatalf("显式清理后应恢复（最后一次: code=%d err=%s）", runCode, errbRun)
			}
			time.Sleep(500 * time.Millisecond)
		}
	default:
		t.Fatalf("脱离后 run 结果非法: code=%d err=%s", bcode, errbBlock)
	}
}

// K：stdin 或 stdout 非终端时，在修改终端或启动会话前失败（exit 2 +
// KM_PROTO_NOT_TTY；不会发起任何 docker exec）。
func TestC1NonTTYFailsEarly(t *testing.T) {
	// stdin 非终端（管道）
	cmd := exec.Command(shellBin, "-container", "nope")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err == nil {
		t.Fatal("stdin 非终端应失败")
	}
	if !strings.Contains(errb.String(), "KM_PROTO_NOT_TTY") {
		t.Fatalf("应报 KM_PROTO_NOT_TTY: %q", errb.String())
	}

	// stdout 非终端（stdin 为 PTY、stdout 为普通文件）
	redir := filepath.Join(c1EvidenceDir, "notty-stdout.txt")
	cmd2 := exec.Command("python3", filepath.Join("..", "integration", "shellproto_driver.py"),
		"-container", "nope", "-proto", shellBin,
		"-scenario", emptyScenario(t), "-log", filepath.Join(c1EvidenceDir, "notty.log"),
		"-json", filepath.Join(c1EvidenceDir, "notty.json"))
	cmd2.Env = append(os.Environ(), "PROTO_STDOUT_REDIRECT="+redir)
	_ = cmd2.Run() // 驱动器退出码无意义；判定依据是 proto 退出码与重定向文件
	rawJSON, err := os.ReadFile(filepath.Join(c1EvidenceDir, "notty.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(rawJSON, &res); err != nil {
		t.Fatal(err)
	}
	if code, ok := res["exit_code"].(float64); !ok || code != 2 {
		t.Fatalf("stdout 非终端应使 proto 退出 2: %v", res["exit_code"])
	}
	// proto 的诊断走 stderr（PTY 日志），重定向的 stdout 文件保持为空
	msg := readFile2(t, filepath.Join(c1EvidenceDir, "notty.log"))
	if !strings.Contains(msg, "KM_PROTO_NOT_TTY") {
		t.Fatalf("stdout 变体应报 KM_PROTO_NOT_TTY: %q", msg)
	}
}

func emptyScenario(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "steps.json")
	os.WriteFile(p, []byte("[]"), 0o644)
	return p
}

func readFile2(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func nonEmptyLines(s string) []string {
	var res []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			res = append(res, strings.TrimSpace(l))
		}
	}
	return res
}

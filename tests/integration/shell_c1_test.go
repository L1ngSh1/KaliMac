//go:build integration

// P2-C C1 交互终端原型验收（A–K）。驱动方式：真实宿主 PTY + 提示符标记
// 同步；键盘事件通过向 PTY 写入字节触发，外部信号单独发送。原始输出、
// 退出码、耗时与 termios 快照写入 tests/evidence/p2c-shellproto-*/。
package integration

import (
	"encoding/json"
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
func TestC1BackgroundCleanupAtSessionEnd(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "send", "text": "sleep 290 &\r"},
		step{"op": "expect", "pattern": "\\[1\\]", "timeout": 10},
		step{"op": "send", "text": "sh -c 'trap \"\" HUP TERM INT; sleep 300' &\r"},
		step{"op": "expect", "pattern": "\\[2\\]", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("shell 应正常退出: %v", res["exit_code"])
	}
	// 普通 bg sleep 290 应被 EXIT trap TERM（轮询等待，清理与观察存在时序差）；
	// 稳定后剩余 = 免疫 sh + 其子 sleep 300
	var pids []string
	deadline := time.Now().Add(6 * time.Second)
	for {
		out, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
			"ps -e -o pid,args | grep -E 'sleep (290|300)' | grep -v grep").Output()
		lines := nonEmptyLines(string(out))
		cleaned := 0
		pids = nil
		for _, l := range lines {
			if strings.Contains(l, "sleep 290") {
				cleaned++
			}
			pids = append(pids, strings.Fields(l)[0])
		}
		if len(lines) == 2 && cleaned == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("应仅剩免疫任务（sh+子 sleep 300），普通任务未清理: %q", string(out))
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, pid := range pids {
		exec.Command("docker", "exec", id, "kill", "-9", pid).Run()
	}
	time.Sleep(300 * time.Millisecond)
	out2, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"ps -e -o pid,args | grep 'sleep 300' | grep -v grep").Output()
	if strings.TrimSpace(string(out2)) != "" {
		t.Fatalf("逃逸任务应已按记录 pid 清理: %q", string(out2))
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
		step{"op": "expect", "pattern": "KM_PROTO_START_FAIL|Unable to find", "timeout": 20},
	))
	if res["exit_code"].(float64) != 1 {
		t.Fatalf("启动失败应退出 1: %v", res["exit_code"])
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
	if res["exit_code"].(float64) != -9 {
		t.Fatalf("SIGKILL 应终止 proto: %v", res["exit_code"])
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
func TestC1DetachKeys(t *testing.T) {
	id, _ := sessionContainerImage(t, newP2BProject(t), minImageRef)

	// 默认：--detach-keys 传空串 → 禁用脱离，字节直达 shell
	res, _ := runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "sendb", "b64": "EBE="}, // ctrl-p ctrl-q
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 10},
		step{"op": "send", "text": "echo NO_DETACH=ok\r"},
		step{"op": "expect", "pattern": "NO_DETACH=ok", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	raw := res["raw"].(string)
	if !strings.Contains(raw, "NO_DETACH=ok") {
		t.Fatalf("默认应禁用脱离（ctrl-p/q 直达 shell）:\n%s", raw)
	}

	// 显式启用：实验证实 docker exec 不拦截 detach keys——字节直达 bash，
	// bash 读到转义序列后退出（exit 1）。即 exec 路径不存在「客户端脱离」，
	// 客户端退出 == 会话结束，不存在被误判为已清理的脱离态。
	res, _ = runShellScenario(t, id, expectSteps(
		step{"op": "expect", "pattern": "KM_SHELL> ", "timeout": 20},
		step{"op": "sendb", "b64": "EBE="},
	), "detach-keys=ctrl-p,ctrl-q")
	if res["exit_code"].(float64) != 1 {
		t.Fatalf("detach 字节直达 bash 后应致其退出 1: %v", res["exit_code"])
	}
	if !strings.Contains(res["raw"].(string), "read escape sequence") {
		t.Fatalf("应观察到 bash 读取转义序列:\n%s", res["raw"])
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

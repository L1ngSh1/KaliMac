//go:build integration

// C2 加固回归（review 修复轮）：
//   - C：管道后台作业、停止态作业随会话退出清理（SID 域判定 + trap 组杀升级）
//   - B：km shell 外部 SIGTERM/SIGHUP → 143 + termios 恢复；启动失败路径
package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 管道后台作业（review 复现用例）：kill 正 PID 只杀组长，管道成员曾泄漏；
// 修复后 EXIT trap 按作业组整组 TERM+KILL，km-ctl 以 SID 域判定。
func TestC2PipelineJobCleanup(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "send", "text": "sleep 271 | sleep 272 &\r"},
		step{"op": "expect", "pattern": "\\[1\\]", "timeout": 10},
		step{"op": "send", "text": "exit\r"},
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("shell 应正常退出: %v", res["exit_code"])
	}
	deadline := time.Now().Add(8 * time.Second)
	var out string
	for {
		out, _ = execOutput(id, "ps -e -o pid,args | grep -E 'sleep 27' | grep -v grep")
		if strings.TrimSpace(out) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("管道后台作业未清理（review C 复现回归）: %q", out)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// 会话目录已清；后续 run 正常
	sOut, _ := execOutput(id, "/tmp/km-bin/km-ctl sessions")
	if strings.TrimSpace(sOut) != "" {
		t.Fatalf("会话目录应已清扫: %q", sOut)
	}
	if _, _, c := kmRun(t, dir, nil, "run", "--", "/bin/echo", "OK_AFTER_PIPE"); c != 0 {
		t.Fatal("清理后 run 应正常")
	}
}

// 停止态作业：Ctrl-Z 后退出——TERM 无法送达停止进程，KILL 升级必须覆盖。
func TestC2StoppedJobCleanup(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	res := runKmShellScenario(t, dir, append(kmShellSteps(),
		step{"op": "send", "text": "sleep 273\r"},
		step{"op": "settle", "secs": 0.5},
		step{"op": "sendb", "b64": "Gg=="}, // Ctrl-Z → 停止态
		step{"op": "expect", "pattern": "Stopped", "timeout": 10},
		step{"op": "send", "text": "exit\r"}, // 第一次 exit：bash 提示 There are stopped jobs 并拒绝
		step{"op": "settle", "secs": 0.4},
		step{"op": "send", "text": "exit 0\r"}, // 第二次 exit：丢弃停止作业退出
	))
	if res["exit_code"].(float64) != 0 {
		t.Fatalf("shell 应正常退出: %v", res["exit_code"])
	}
	deadline := time.Now().Add(8 * time.Second)
	var out string
	for {
		out, _ = execOutput(id, "ps -e -o pid,stat,args | grep 'sleep 273' | grep -v grep")
		if strings.TrimSpace(out) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("停止态作业未清理: %q", out)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// km shell 外部 SIGTERM / SIGHUP（单独向进程发送）：km 退出 143 且 termios
// 恢复进入前状态；容器内 bash 被终止（km 主动收尾路径）。
func TestC2ShellExternalSignals(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	for _, sig := range []string{"SIGTERM", "SIGHUP"} {
		res := runKmShellScenario(t, dir, append(kmShellSteps(),
			step{"op": "signal", "name": sig},
		))
		if res["exit_code"].(float64) != 143 {
			t.Fatalf("%s 后应退出 143: %v", sig, res["exit_code"])
		}
		if res["termios_equal"] != true {
			t.Fatalf("%s 后 termios 未恢复: %v", sig, res)
		}
		// bash 已被 km 收尾终止（非失联语义）
		deadline := time.Now().Add(6 * time.Second)
		alive := true
		for time.Now().Before(deadline) {
			out, _ := execOutput(id, "ps -e -o pid,comm | grep -w bash")
			if strings.TrimSpace(out) == "" {
				alive = false
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if alive {
			t.Fatalf("%s 后容器内 bash 应已终止（km 收尾路径）", sig)
		}
	}
}

// 启动失败：state 指向不存在的容器 → 明确错误退出（exit 1），termios 未动。
func TestC2ShellStartupFailure(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	// 破坏 state：容器 ID 指向不存在的容器
	patchState(t, dir, map[string]string{"id": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"})
	res := runKmShellScenario(t, dir, []step{
		{"op": "expect", "pattern": "KM_NOT_FOUND|启动 shell 失败|记录的容器", "timeout": 20},
	})
	if code := res["exit_code"].(float64); code != 1 {
		t.Fatalf("启动失败应退出 1: %v", code)
	}
	if res["termios_equal"] != true {
		t.Fatal("启动失败后 termios 未保持进入前状态")
	}
}

// execOutput 在容器内执行 sh -c 并返回 stdout（观察用）。
func execOutput(id, shellCmd string) (string, error) {
	out, err := exec.Command("docker", "exec", id, "/bin/sh", "-c", shellCmd).Output()
	return string(out), err
}

// patchState 修改项目 state.json 中的字符串字段（启动失败注入用）。
func patchState(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	path := dir + "/.km/state.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	container := m["container"].(map[string]any)
	for k, v := range kv {
		container[k] = v
	}
	m["container"] = container
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

//go:build integration

// P2-A 真实 Docker 会话实验的公共工具：唯一 run-id 资源、按完整 ID 清理、
// 独立观察（/proc 进程组、会话目录）。fake 单测与真实实验分开报告。
package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ownerLabel = "km.owner=km-p2a-test"

// toolImage 可经 KM_TEST_TOOL_IMAGE 覆盖：CI（美区 runner）用官方引用
// kalilinux/kali-rolling:latest，本地默认国内镜像站，行为不变。
var toolImage = envOrDefault("KM_TEST_TOOL_IMAGE", "docker.1ms.run/kalilinux/kali-rolling:latest")

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	runID      = fmt.Sprintf("p2a-%d-%s", time.Now().Unix(), uniqCounter0())
	projectLbl = "km.project=" + runID
)

func uniqCounter0() string {
	uniqCounter++
	return fmt.Sprint(uniqCounter)
}

func dockerBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("docker CLI 不存在: %v", err)
	}
	return bin
}

// dockerUp 报告引擎是否可达。
func dockerUp(t *testing.T) bool {
	t.Helper()
	dockerBin(t)
	cmd := exec.Command(dockerBin(t), "info", "--format", "{{.ServerVersion}}")
	return cmd.Run() == nil
}

// runCapture 执行 docker 命令并捕获输出；返回 stdout 与退出码。
func runCapture(t *testing.T, stdin []byte, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(dockerBin(t), args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("docker %v 执行失败: %v (stderr=%s)", args, err, errb.String())
		}
	}
	return out.String() + errB(errb), code
}

func errB(b bytes.Buffer) string {
	if b.Len() == 0 {
		return ""
	}
	return " STDERR[" + b.String() + "]"
}

// dexec 在容器内执行命令（捕获式，仅用于观察与控制）。
func dexec(t *testing.T, container string, args ...string) (string, int) {
	t.Helper()
	return runCapture(t, nil, append([]string{"exec", container}, args...)...)
}

var residueGuarded sync.Map // *testing.T → 已注册

// guardResidue 每个测试只注册一次残留核对；作为最早的 cleanup，它最后执行
// （所有容器移除之后），保证核对时机正确。
func guardResidue(t *testing.T) {
	t.Helper()
	if _, loaded := residueGuarded.LoadOrStore(t, true); !loaded {
		t.Cleanup(func() { assertNoResidue(t) })
	}
}

// sessionContainer 创建一个挂载项目目录的 --init 容器并记录完整 ID。
func sessionContainer(t *testing.T, projectDir string) (id, name string) {
	t.Helper()
	return sessionContainerImage(t, projectDir, toolImage)
}

// sessionContainerImage 同上，但指定镜像（C1 用精选镜像：含 bash/ps/stty）。
func sessionContainerImage(t *testing.T, projectDir, image string) (id, name string) {
	t.Helper()
	guardResidue(t)
	if !dockerUp(t) {
		t.Skip("Docker 引擎不可达")
	}
	bin := dockerBin(t)
	// 预检实际使用的镜像（而非固定工具镜像），缺失则显式跳过
	if _, code := runCapture(t, nil, "image", "inspect", image); code != 0 {
		t.Skipf("镜像不在本地: %s", image)
	}
	name = "km-" + runID + "-" + uniqName()
	out, code := runCapture(t, nil, "run", "-d", "--init",
		"--name", name, "--label", ownerLabel, "--label", projectLbl,
		"-v", projectDir+":/workspace", image, "sleep", "900")
	if code != 0 {
		t.Fatalf("容器创建失败: %s", out)
	}
	id = strings.TrimSpace(out)
	t.Cleanup(func() {
		if err := exec.Command(bin, "rm", "-f", id).Run(); err != nil {
			// 清理失败必须可观察；残留核对（guardResidue）会按标签复核
			t.Logf("P2A-CLEANUP-WARN: 移除容器 %s 失败: %v", id, err)
		}
	})
	return id, name
}

var uniqCounter int64

func uniqName() string {
	n := atomic.AddInt64(&uniqCounter, 1)
	return fmt.Sprintf("c%d-%d", n, time.Now().UnixNano()%1_000_000)
}

// assertNoResidue 断言本 run-id 无容器残留（在每个测试结束时核对）。
// docker ps 查询失败必须报「无法核实」，不得等价为零残留。
func assertNoResidue(t *testing.T) {
	t.Helper()
	out, code := runCapture(t, nil, "ps", "-a", "--filter", "label="+projectLbl, "-q")
	if code != 0 {
		t.Errorf("run-id %s 残留核对无法核实（docker ps 退出码 %d）: %s", runID, code, strings.TrimSpace(out))
		return
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("run-id %s 存在残留容器: %s", runID, strings.TrimSpace(out))
	}
}

// waitSessionDir 等待容器内出现会话目录，返回 (sid, pid)。
func waitSessionDir(t *testing.T, container string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		out, code := dexec(t, container, "/bin/sh", "-c", "ls /tmp/km-sessions 2>/dev/null | head -1")
		if code == 0 && strings.TrimSpace(out) != "" {
			sid := strings.TrimSpace(out)
			pid, pcode := dexec(t, container, "/bin/sh", "-c", "cat /tmp/km-sessions/"+sid+"/pid 2>/dev/null")
			if pcode == 0 && strings.TrimSpace(pid) != "" {
				return sid, strings.TrimSpace(pid)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待会话目录超时")
	return "", ""
}

// observeGroup 独立观察：按 pgrp 列出进程（pid state comm），不使用进程名匹配。
func observeGroup(t *testing.T, container, pgid string) []string {
	t.Helper()
	out, _ := dexec(t, container, "/tmp/km-bin/km-observe", "pgid:"+pgid)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// procExists 独立观察：/proc/<pid> 是否仍存在（僵尸也计入存在）。
func procExists(t *testing.T, container, pid string) bool {
	_, code := dexec(t, container, "/bin/sh", "-c", "test -d /proc/"+pid)
	return code == 0
}

// waitReaped 等待 /proc/<tpid> 消失（由 --init 的 PID1 回收）。
func waitReaped(t *testing.T, container, tpid string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !procExists(t, container, tpid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("tpid %s 未被回收（僵尸或仍存活）", tpid)
}

// writeFixture 写入测试 fixture 并赋予执行权限。
func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func tempProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, "noclean.sh", "#!/bin/sh\nsleep 300 &\nsleep 300 &\nwait\n")
	writeFixture(t, dir, "selfclean.sh", "#!/bin/sh\ntrap 'kill \"$C1\" 2>/dev/null; exit 130' INT TERM\nsleep 300 &\nC1=$!\nwait \"$C1\"\necho normal-exit\n")
	writeFixture(t, dir, "grandchild.sh", "#!/bin/sh\nsh -c 'sleep 300' &\nsleep 300 &\nwait\n")
	writeFixture(t, dir, "errcat.sh", "#!/bin/sh\ncat\necho ERR_LINE >&2\n")
	writeFixture(t, dir, "exit7.sh", "#!/bin/sh\nexit 7\n")
	writeFixture(t, dir, "exit42.sh", "#!/bin/sh\nexit 42\n")
	return dir
}

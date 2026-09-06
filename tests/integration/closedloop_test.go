//go:build integration

// P2-B 非交互最小闭环的黑盒集成验收矩阵：
// init → run -- fixture → stop → 再次执行恢复。
// 通过真实 km 二进制（TestMain 构建）驱动，fake 单测与真实集成分开报告。
package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const minImageRef = "kali-mac-min:0.2"

var kmBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "km-p2b-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(2)
	}
	kmBin = filepath.Join(tmp, "km")
	repo, _ := filepath.Abs("../..")
	build := exec.Command("go", "build", "-o", kmBin, "./cmd/km")
	build.Dir = repo
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "构建 km 失败:", err)
		os.Exit(2)
	}
	// 最小镜像：本地已存在则复用；缺失则构建（有据可查）
	if code := exitCodeOf("image", "inspect", minImageRef); code != 0 {
		fmt.Fprintln(os.Stderr, "镜像不在本地，开始构建", minImageRef)
		b := exec.Command("docker", "build", "-t", minImageRef, "images/kali")
		b.Dir = repo
		b.Stdout, b.Stderr = os.Stdout, os.Stderr
		if err := b.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "镜像构建失败:", err)
			os.Exit(2)
		}
	}
	code := m.Run()

	// 清理终检：本套件注册的每个容器（完整 ID）都必须已消失
	bad := 0
	regMu.Lock()
	defer regMu.Unlock()
	for _, id := range registered {
		if exitCodeOf("container", "inspect", id) == 0 {
			fmt.Fprintln(os.Stderr, "P2B-CLEANUP-FAIL: 残留容器", id)
			bad++
		}
	}
	if bad > 0 {
		code = 1
	}
	os.RemoveAll(tmp)
	os.Exit(code)
}

var (
	regMu      sync.Mutex
	registered []string
)

// registerProjectCleanup 把项目容器（完整 ID）登记进套件级清理与终检清单。
func registerProjectCleanup(t *testing.T, dir string) string {
	t.Helper()
	id := projectContainerID(t, dir)
	regMu.Lock()
	registered = append(registered, id)
	regMu.Unlock()
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	return id
}

func exitCodeOf(args ...string) int {
	err := exec.Command("docker", args...).Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// kmRun 在 dir 中运行 km 子命令，返回 stdout/stderr 与退出码。
func kmRun(t *testing.T, dir string, stdin []byte, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(kmBin, args...)
	cmd.Dir = dir
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
			t.Fatalf("km %v 执行失败: %v", args, err)
		}
	}
	return out.String(), errb.String(), code
}

// kmRunAsync 启动 km 子命令（不等待），用于取消与忙碌场景。
func kmRunAsync(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(kmBin, args...)
	cmd.Dir = dir
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	// SIGKILL 宿主后，孤儿的 docker exec 子进程仍持有管道；
	// WaitDelay 保证 Wait() 不等待它们退出。
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// newP2BProject 创建挂载 fixture 的临时项目目录。
func newP2BProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// 指向本地已构建的最小精选镜像（拉取路径由 TestInitFailureRecovery 单独覆盖）
	if err := os.WriteFile(filepath.Join(dir, ".km.json"),
		[]byte(fmt.Sprintf(`{"schema_version":1,"image":%q}`, minImageRef)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "argv.sh", "#!/bin/sh\ni=0\nfor a in \"$@\"; do i=$((i+1)); printf '%d[%s]\\n' \"$i\" \"$a\"; done\n")
	writeFixture(t, dir, "noclean.sh", "#!/bin/sh\nsleep 300 &\nsleep 300 &\nwait\n")
	writeFixture(t, dir, "ignore.sh", "#!/bin/sh\nsh -c 'trap \"\" TERM INT; sleep 300' &\nsleep 300\n")
	writeFixture(t, dir, "linger.sh", "#!/bin/sh\nsleep 300 &\necho done\n")
	writeFixture(t, dir, "errcat.sh", "#!/bin/sh\ncat\necho ERR_LINE >&2\n")
	writeFixture(t, dir, "exit7.sh", "#!/bin/sh\nexit 7\n")
	writeFixture(t, dir, "exit42.sh", "#!/bin/sh\nexit 42\n")
	writeFixture(t, dir, "hello.txt", "km 数据行\n")
	writeFixture(t, dir, "fixture.py", "print('py-ok')\n")
	writeFixture(t, dir, "data.json", `{"name":"km","n":42}`)
	writeFixture(t, dir, "sample.txt", "plain text sample\n")
	return dir
}

// projectContainerID 从本机状态读容器完整 ID。
func projectContainerID(t *testing.T, dir string) string {
	t.Helper()
	out, _, code := kmRun(t, dir, nil, "doctor")
	_ = out
	if code != 0 {
		t.Fatalf("doctor 失败")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".km", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := extractJSONString(string(raw), "id")
	if id == "" {
		t.Fatalf("state 缺容器 ID: %s", raw)
	}
	return id
}

func extractJSONString(s, key string) string {
	i := strings.Index(s, `"`+key+`"`)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key)+2:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	k := strings.Index(rest[j+1:], `"`)
	if k < 0 {
		return ""
	}
	return rest[j+1 : j+1+k]
}

// containerState 查询容器状态（running/exited/...）。
func containerState(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "container", "inspect", "--format", "{{.State.Status}}", id).Output()
	if err != nil {
		return "MISSING"
	}
	return strings.TrimSpace(string(out))
}

// p2bResidueCount 统计套件登记之外、仍存活的登记容器数（应恒为 0）。
func p2bResidueCount(t *testing.T) int {
	t.Helper()
	regMu.Lock()
	defer regMu.Unlock()
	n := 0
	for _, id := range registered {
		if exitCodeOf("container", "inspect", id) == 0 {
			n++
		}
	}
	return n
}

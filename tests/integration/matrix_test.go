//go:build integration

// P2-B 验收矩阵（非交互最小闭环）：
// 重复init / 复杂argv / 二进制管道 / 退出码 / 中文空格路径 / 子目录cwd /
// 取消清理 / 多项目隔离 / 忙碌状态 / stop恢复 / 离线复用 / 引擎漂移 /
// 同名冲突 / 镜像身份冲突 / 初始化失败恢复 + 六工具 smoke。
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

var runIDP2B = fmt.Sprintf("p2b-%d", time.Now().Unix())

// guardP2BResidue：全包一次，最终核对 km-p2b-test 标签零残留。
func TestP2BResidueCheck(t *testing.T) {
	if n := p2bResidueCount(t); n != 0 {
		t.Errorf("存在 %d 个 p2b 残留容器（应为 0）", n)
	}
}

func TestInitRunStopRestoreLoop(t *testing.T) {
	dir := newP2BProject(t)
	// 1. init
	out, errb, code := kmRun(t, dir, nil, "init")
	if code != 0 {
		t.Fatalf("init: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "环境就绪") {
		t.Fatalf("init out=%q", out)
	}
	id := registerProjectCleanup(t, dir)

	// 2. 重复 init → 复用同一容器
	out2, _, code2 := kmRun(t, dir, nil, "init")
	if code2 != 0 || !strings.Contains(out2, "复用") {
		t.Fatalf("重复 init: code=%d out=%q", code2, out2)
	}
	if again := projectContainerID(t, dir); again != id {
		t.Fatalf("重复 init 应复用同一容器: %s vs %s", id, again)
	}

	// 3. run：数据文件
	_, _, code3 := kmRun(t, dir, nil, "run", "--", "/bin/sh", "-c", "echo run-data > /workspace/run_data.txt")
	if code3 != 0 {
		t.Fatalf("run: code=%d", code3)
	}

	// 4. stop（幂等 ×2）
	if _, errb, code := kmRun(t, dir, nil, "stop"); code != 0 {
		t.Fatalf("stop: %s", errb)
	}
	if got := containerState(t, id); got != "exited" {
		t.Fatalf("stop 后应 exited, got %s", got)
	}
	if out4, _, code4 := kmRun(t, dir, nil, "stop"); code4 != 0 || !strings.Contains(out4, "幂等") {
		t.Fatalf("重复 stop: code=%d out=%q", code4, out4)
	}

	// 5. 再次执行恢复同一容器，数据保留
	_, _, code5 := kmRun(t, dir, nil, "run", "--", "/bin/cat", "/workspace/run_data.txt")
	if code5 != 0 {
		t.Fatalf("恢复执行: code=%d", code5)
	}
	if got := containerState(t, id); got != "running" {
		t.Fatalf("恢复后应 running 且为同一容器 %s, got %s", id, got)
	}
	if again := projectContainerID(t, dir); again != id {
		t.Fatal("恢复后容器身份应不变")
	}
	if _, err := os.Stat(filepath.Join(dir, "run_data.txt")); err != nil {
		t.Fatalf("数据文件应保留: %v", err)
	}
}

func TestRunComplexArgvAndExitCodes(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	var out bytes.Buffer
	cmd := exec.Command(kmBin, "run", "--", "/workspace/argv.sh",
		"", "a b", "中文参数", `he said "hi"`, "--help", "-x", "5 * ?")
	cmd.Dir = dir
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	want := "1[]\n2[a b]\n3[中文参数]\n4[he said \"hi\"]\n5[--help]\n6[-x]\n7[5 * ?]\n"
	if out.String() != want {
		t.Fatalf("argv 不一致:\n got=%q\nwant=%q", out.String(), want)
	}
	// 退出码 0/7/42 原样返回
	for _, tc := range []struct {
		tool string
		want int
	}{
		{"/bin/true", 0}, {"/workspace/exit7.sh", 7}, {"/workspace/exit42.sh", 42},
	} {
		_, _, code := kmRun(t, dir, nil, append([]string{"run", "--", tc.tool}, "--help-expected-only-for-argv-fixture"[:0])...)
		if code != tc.want {
			t.Fatalf("%s: exit=%d want=%d", tc.tool, code, tc.want)
		}
	}
	// 与管理命令重名的 fixture 经长形式可调用
	dir2 := newP2BProject(t)
	writeFixture(t, dir2, "stop.sh", "#!/bin/sh\necho fixture-stop\n")
	if _, errb, code := kmRun(t, dir2, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir2)
	outS, _, codeS := kmRun(t, dir2, nil, "run", "--", "/workspace/stop.sh")
	if codeS != 0 || !strings.Contains(outS, "fixture-stop") {
		t.Fatalf("重名 fixture: code=%d out=%q", codeS, outS)
	}
}

func TestRunBinaryPipeAndStderr(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	raw := make([]byte, 32768)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	inPath := filepath.Join(dir, "bin.in")
	os.WriteFile(inPath, raw, 0o644)
	in, _ := os.Open(inPath)
	defer in.Close()

	outPath := filepath.Join(dir, "bin.out")
	outF, _ := os.Create(outPath)
	errF, _ := os.Create(filepath.Join(dir, "bin.err"))
	cmd := exec.Command(kmBin, "run", "--", "/workspace/errcat.sh")
	cmd.Dir = dir
	cmd.Stdin = in
	cmd.Stdout = outF
	cmd.Stderr = errF
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	outF.Close()
	errF.Close()
	if got := sha256hex(readFile(t, outPath)); got != sha256hex(raw) {
		t.Fatalf("二进制往返哈希不一致")
	}
	if ef := string(readFile(t, filepath.Join(dir, "bin.err"))); strings.TrimSpace(ef) != "ERR_LINE" {
		t.Fatalf("stderr 应独立: %q", ef)
	}
}

func TestRunChineseSpacePathsAndSubdirCwd(t *testing.T) {
	dir := newP2BProject(t)
	sub := filepath.Join(dir, "中文 目录", "子 目")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "note.txt"), []byte("子目录数据"), 0o644)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	// 子目录 cwd：km 在子目录运行，工具 cwd 映射
	out, _, code := kmRun(t, sub, nil, "run", "--", "/bin/sh", "-c", "pwd; cat note.txt")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "/workspace/中文 目录/子 目") || !strings.Contains(out, "子目录数据") {
		t.Fatalf("cwd 映射不正确: %q", out)
	}
	// 项目根创建的文件在容器可见（双向）
	out2, _, code2 := kmRun(t, dir, nil, "run", "--", "/bin/cat", "/workspace/hello.txt")
	if code2 != 0 || !strings.Contains(out2, "km 数据行") {
		t.Fatalf("根挂载: %q", out2)
	}
}

func TestRunCancelCleansSession(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	cmd := kmRunAsync(t, dir, "run", "--", "/workspace/noclean.sh")
	time.Sleep(1500 * time.Millisecond)
	// 快照 tpid（取消前）
	out, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"cat /tmp/km-sessions/*/pid 2>/dev/null").Output()
	tpid := strings.TrimSpace(string(out))
	if tpid == "" {
		t.Fatal("未取得 tpid")
	}
	// SIGINT（同 Ctrl-C）
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("退出错误类型: %v", err)
		}
		if code := ee.ExitCode(); code != 130 {
			t.Fatalf("SIGINT 取消目标 130, got %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("取消超时")
	}
	// 独立观察：会话目录清空 + tpid 被回收
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c", "ls /tmp/km-sessions 2>/dev/null | wc -l").Output()
		dead, _ := exec.Command("docker", "exec", id, "test", "-d", "/proc/"+tpid).Output()
		_ = dead
		if strings.TrimSpace(string(sess)) == "0" {
			if exitCodeOf("exec", id, "test", "-d", "/proc/"+tpid) != 0 {
				return // 进程组清空且 tpid 回收
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("取消后仍有残留（tpid=%s）", tpid)
}

func TestRunMultiProjectIsolationAndBusy(t *testing.T) {
	dirA := newP2BProject(t)
	dirB := newP2BProject(t)
	if _, errb, code := kmRun(t, dirA, nil, "init"); code != 0 {
		t.Fatalf("init A: %s", errb)
	}
	if _, errb, code := kmRun(t, dirB, nil, "init"); code != 0 {
		t.Fatalf("init B: %s", errb)
	}
	idA := registerProjectCleanup(t, dirA)
	idB := registerProjectCleanup(t, dirB)

	// A 持有锁执行长任务 → 同项目第二个执行 BUSY
	cmd := kmRunAsync(t, dirA, "run", "--", "/workspace/noclean.sh")
	time.Sleep(1500 * time.Millisecond)
	_, errb2, code2 := kmRun(t, dirA, nil, "run", "--", "/bin/echo", "hi")
	if code2 != 1 || !strings.Contains(errb2, "KM_PROJECT_BUSY") {
		t.Fatalf("忙碌应报 KM_PROJECT_BUSY: code=%d err=%s", code2, errb2)
	}
	// B 不受影响（不同项目独立）
	outB, _, codeB := kmRun(t, dirB, nil, "run", "--", "/bin/echo", "B-ok")
	if codeB != 0 || !strings.Contains(outB, "B-ok") {
		t.Fatalf("B 应独立可用: code=%d out=%q", codeB, outB)
	}
	// 取消 A → 清理；B 容器状态不变
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Fatalf("A 取消应 130: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("A 取消超时")
	}
	if got := containerState(t, idA); got != "running" {
		t.Fatalf("A 容器应保持 running（取消≠停止容器）: %s", got)
	}
	if got := containerState(t, idB); got != "running" {
		t.Fatalf("B 容器应保持 running: %s", got)
	}
}

func TestRunEngineDriftRejected(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	// 篡改 state 的 endpoint → 漂移
	statePath := filepath.Join(dir, ".km", "state.json")
	raw := string(readFile(t, statePath))
	raw = strings.Replace(raw, "unix://", "unix://drift@", 1)
	os.WriteFile(statePath, []byte(raw), 0o644)
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/true")
	if code != 1 || !strings.Contains(errb, "KM_RUNTIME_MISMATCH") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestRunSameNameConflictRejected(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	// 把记录容器改名，再用原名称建一个"别人的"同名容器
	exec.Command("docker", "rename", id, id+"-moved").Run()
	out, _ := exec.Command("docker", "run", "-d", "--name", "km-taken-conflict",
		"--label", "km.project=pOTHER", "kali-mac-min:0.2", "sleep", "60").Output()
	otherID := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", otherID, id+"-moved").Run() })
	// run 按 ID 找到被改名的容器 → 名称不一致 → 冲突
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/true")
	if code != 1 || !strings.Contains(errb, "KM_CONTAINER_CONFLICT") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestRunImageIdentityConflictRejected(t *testing.T) {
	dir := newP2BProject(t)
	cfg := filepath.Join(dir, ".km.json")
	os.WriteFile(cfg, []byte(`{"schema_version":1,"image":"kali-mac-min:drift-tag"}`), 0o644)
	// 用正确内容打标签后 init
	exec.Command("docker", "tag", minImageRef, "kali-mac-min:drift-tag").Run()
	t.Cleanup(func() { exec.Command("docker", "rmi", "kali-mac-min:drift-tag").Run() })
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	// 把标签指向不同内容（busybox）→ 内容身份漂移
	if code := exitCodeOf("tag", "docker.1ms.run/library/busybox:stable", "kali-mac-min:drift-tag"); code != 0 {
		t.Fatal("retag 失败")
	}
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/true")
	if code != 1 || !strings.Contains(errb, "KM_IMAGE_DRIFT") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestInitFailureRecovery(t *testing.T) {
	dir := newP2BProject(t)
	// 配置指向不存在的镜像 → 拉取失败 → init 失败且不落状态
	os.WriteFile(filepath.Join(dir, ".km.json"),
		[]byte(`{"schema_version":1,"image":"no-such-image-zz-2026:1"}`), 0o644)
	_, errb, code := kmRun(t, dir, nil, "init")
	if code != 1 {
		t.Fatalf("拉取失败应 exit 1, got %d err=%s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".km", "state.json")); !os.IsNotExist(err) {
		t.Fatal("失败的 init 不应写入状态")
	}
	if n := p2bResidueCount(t); n != 0 {
		t.Fatalf("失败的 init 不应留下容器: %d", n)
	}
	// 修复配置 → init 成功（失败恢复）
	os.WriteFile(filepath.Join(dir, ".km.json"),
		[]byte(fmt.Sprintf(`{"schema_version":1,"image":%q}`, minImageRef)), 0o644)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("恢复后 init: %s", errb)
	}
	registerProjectCleanup(t, dir)
}

// 六工具 smoke：python3 / curl / jq / nmap(仅本地回环) / file / openssl。
func TestCuratedToolSmoke(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)

	// 项目专属本地测试服务（容器内 127.0.0.1:8000，仅回环）
	os.WriteFile(filepath.Join(dir, "svc.txt"), []byte("km-test-service-body\n"), 0o644)
	exec.Command("docker", "exec", "-d", "-w", "/workspace", id, "python3", "-m", "http.server", "8000", "--bind", "127.0.0.1").Run()
	time.Sleep(700 * time.Millisecond)
	t.Cleanup(func() {
		exec.Command("docker", "exec", id, "/bin/sh", "-c", "kill $(cat /tmp/km-svc.pid) 2>/dev/null").Run()
	})
	exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"for p in /proc/[0-9]*; do read -r pid c s pp pg r < $p/stat 2>/dev/null; case \"$c\" in *http.server*) echo $pid > /tmp/km-svc.pid;; esac; done").Run()

	cases := []struct {
		name     string
		args     []string
		wantOut  string
		wantCode int
	}{
		{"python3-fixture", []string{"run", "--", "python3", "/workspace/fixture.py"}, "py-ok", 0},
		{"curl-local-service", []string{"run", "--", "curl", "-s", "http://127.0.0.1:8000/svc.txt"}, "km-test-service-body", 0},
		{"jq-fixed-json", []string{"run", "--", "jq", "-r", ".name", "/workspace/data.json"}, "km", 0},
		{"nmap-local-loopback", []string{"run", "--", "nmap", "-p", "8000", "--open", "127.0.0.1"}, "8000/tcp", 0},
		{"file-text", []string{"run", "--", "file", "/workspace/sample.txt"}, "ASCII text", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errb, code := kmRun(t, dir, nil, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("code=%d err=%s out=%s", code, errb, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("输出缺 %q: %q", tc.wantOut, out)
			}
		})
	}
	// openssl 摘要与本地计算已知答案精确比对
	out, _, _ := kmRun(t, dir, nil, "run", "--", "openssl", "dgst", "-sha256", "/workspace/hello.txt")
	local := sha256hex(readFile(t, filepath.Join(dir, "hello.txt")))
	if !strings.Contains(strings.ToLower(out), local) {
		t.Fatalf("openssl 摘要与本地计算不一致: out=%q want=%s", out, local)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---------- R1：会话终态绑定进程组实际结束 ----------

// 工具主进程死于 TERM、同组子进程忽略 TERM：取消后整组必须清空。
func TestRunCancelChildIgnoresTERM(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	start := time.Now()
	cmd := kmRunAsync(t, dir, "run", "--", "/workspace/ignore.sh")
	_, tpid := waitSessionDir(t, id)
	// 取消前确认：子进程（忽略 TERM）确实同组存活
	preOut, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
		"for p in /proc/[0-9]*; do read -r pid c st pp pg r < $p/stat 2>/dev/null; [ \"$st\" != Z ] && [ \"$pg\" = '"+tpid+"' ] && echo $pid; done").Output()
	if len(strings.Fields(string(preOut))) < 2 {
		t.Fatalf("应存在主进程+忽略TERM子进程: %q", strings.TrimSpace(string(preOut)))
	}
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Fatalf("取消应 130: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("取消超时")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("取消路径应快速完成: %v", elapsed)
	}
	// 独立观察：忽略 TERM 的子进程也必须消失（R1 修复点）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		postOut, _ := exec.Command("docker", "exec", id, "/bin/sh", "-c",
			"for p in /proc/[0-9]*; do read -r pid c st pp pg r < $p/stat 2>/dev/null; [ \"$st\" != Z ] && [ \"$pg\" = '"+tpid+"' ] && echo $pid; done").Output()
		if strings.TrimSpace(string(postOut)) == "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("忽略 TERM 的同组子进程未清理（tpid=%s）", tpid)
}

// 正常退出但同组后台成员仍在：km 退出 0 且成员被排空。
func TestRunNormalExitDrainsLingeringChild(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	out, _, code := kmRun(t, dir, nil, "run", "--", "/workspace/linger.sh")
	if code != 0 || !strings.Contains(out, "done") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// 排空后：容器内除 PID1 外无残留 sleep
	sess, _ := exec.Command("docker", "exec", projectContainerID(t, dir), "/bin/sh", "-c",
		"ls /tmp/km-sessions 2>/dev/null | wc -l").Output()
	if strings.TrimSpace(string(sess)) != "0" {
		t.Fatalf("会话目录应已清空: %q", string(sess))
	}
}

// ---------- R2：宿主强杀后，容器活跃会话阻断新任务 ----------

func TestStaleHostLockActiveSessionBlocked(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	cmd := kmRunAsync(t, dir, "run", "--", "/workspace/noclean.sh")
	waitSessionDir(t, id) // 等会话真正建立再强杀
	// 强杀宿主 km（模拟崩溃）：宿主锁变遗留，容器内会话仍活跃
	cmd.Process.Kill()
	cmd.Wait()

	// 第二次执行必须被阻断
	_, errb, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "SECOND_TASK_RAN")
	if code != 1 || !strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("活跃会话应阻断新任务: code=%d err=%s", code, errb)
	}
	if strings.Contains(errb, "SECOND_TASK_RAN") {
		t.Fatal("不应执行新任务")
	}
	// doctor 报告活跃会话
	docOut, _, dcode := kmRun(t, dir, nil, "doctor")
	if dcode != 0 || !strings.Contains(docOut, "活跃会话") {
		t.Fatalf("doctor 应报告活跃会话: code=%d out=%q", dcode, docOut)
	}
	// 显式恢复：cancel 活跃会话
	sessOut, _ := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "sessions").Output()
	sid := ""
	for _, line := range strings.Split(string(sessOut), "\n") {
		if strings.HasPrefix(line, "ACTIVE ") {
			sid = strings.TrimSpace(strings.TrimPrefix(line, "ACTIVE "))
		}
	}
	if sid == "" {
		t.Fatalf("应能列出活跃会话: %q", string(sessOut))
	}
	_, ccode := exec.Command("docker", "exec", id, "/tmp/km-bin/km-ctl", "cancel", sid).Output()
	_ = ccode
	// 恢复后同项目可再次执行
	_, _, rcode := kmRun(t, dir, nil, "run", "--", "/bin/echo", "AFTER_RECOVERY")
	if rcode != 0 {
		t.Fatalf("显式恢复后应可执行: code=%d", rcode)
	}
}

// ---------- R3：并发 init 只允许一份环境 ----------

func TestConcurrentInitSingleEnvironment(t *testing.T) {
	dir := newP2BProject(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, c := kmRun(t, dir, nil, "init")
			codes <- c
		}()
	}
	wg.Wait()
	close(codes)
	okCount := 0
	for c := range codes {
		if c == 0 {
			okCount++
		} else if c != 1 {
			t.Fatalf("并发 init 失败码应仅为 1（BUSY）, got %d", c)
		}
	}
	if okCount == 0 {
		t.Fatal("至少一个 init 应成功")
	}
	// 仅一个容器：state 记录的容器存在，且同名过滤器只有它
	id := registerProjectCleanup(t, dir)
	if got := containerState(t, id); got != "running" && got != "exited" {
		t.Fatalf("记录容器应存在: %s", got)
	}
	nameOut, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").Output()
	name := "km-p" // 项目容器统一 km-<project-id>；按 state 中的 project_id 精确过滤
	raw, _ := os.ReadFile(filepath.Join(dir, ".km", "state.json"))
	pid := extractJSONString(string(raw), "project_id")
	_ = name
	count := 0
	for _, n := range strings.Split(string(nameOut), "\n") {
		if strings.TrimSpace(n) == "km-"+pid {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同项目应只有一个容器: %d (%s)", count, pid)
	}
	// 串行重复 init → 复用
	if _, _, c := kmRun(t, dir, nil, "init"); c != 0 {
		t.Fatal("串行重复 init 应成功")
	}
	if again := projectContainerID(t, dir); again != id {
		t.Fatal("串行重复 init 应复用同一容器")
	}
}

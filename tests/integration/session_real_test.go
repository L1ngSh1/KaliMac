//go:build integration

// P2-A 真实 Docker 会话实验（任务三验收项）。运行方式：
//
//	go test -tags=integration -count=1 ./tests/integration/...
//
// 覆盖：argv 逐元素、二进制往返+stderr 独立、退出码 0/7/42、长任务不被管理
// 超时误杀、SIGINT 取消（noclean/selfclean/grandchild fixture）、重复取消、
// 启动阶段取消、多项目隔离、SIGKILL 客户端单独观察。
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"testing"
	"time"

	"kalimac/internal/session"
)

func newManager(t *testing.T, bootstrapTimeout time.Duration) *session.Manager {
	t.Helper()
	return &session.Manager{
		Starter:        &session.ExecStarter{},
		Controller:     &session.DockerController{BootstrapTimeout: bootstrapTimeout},
		CleanupTimeout: 20 * time.Second,
	}
}

func mustRun(t *testing.T, m *session.Manager, ctx context.Context, container, dir string,
	stdin []byte, stdout, stderr *bytes.Buffer, tool string, args ...string) session.Result {
	t.Helper()
	res, err := m.Run(ctx, container, "/workspace", bytes.NewReader(stdin), stdout, stderr, tool, args)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	return res
}

// cancelResult 是一次「启动→快照 tpid→取消」的完整记录。
type cancelResult struct {
	res  session.Result
	tpid string
}

// runThenCancel：启动会话，等待会话目录与 pid 出现并快照（独立观察依据），
// 再触发取消，等待 Run 返回。
func runThenCancel(t *testing.T, m *session.Manager, container, dir, tool string, args []string, setupDelay time.Duration) cancelResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan session.Result, 1)
	go func() {
		r, err := m.Run(ctx, container, "/workspace", nil, &bytes.Buffer{}, &bytes.Buffer{}, tool, args)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- r
	}()
	_, tpid := waitSessionDir(t, container)
	time.Sleep(setupDelay)
	cancel()
	res := <-done
	return cancelResult{res: res, tpid: tpid}
}

func TestRealSessionArgvPassthrough(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	writeFixture(t, dir, "argv.sh", "#!/bin/sh\ni=0\nfor a in \"$@\"; do i=$((i+1)); printf '%d[%s]\\n' \"$i\" \"$a\"; done\n")

	var out, errb bytes.Buffer
	res := mustRun(t, newManager(t, 0), context.Background(), id, dir,
		nil, &out, &errb, "/workspace/argv.sh", "", "a b", "中文参数", `he said "hi"`, "--help", "-x", "5 * ?")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, errb.String())
	}
	want := "1[]\n2[a b]\n3[中文参数]\n4[he said \"hi\"]\n5[--help]\n6[-x]\n7[5 * ?]\n"
	if out.String() != want {
		t.Fatalf("argv 逐元素不一致:\n got=%q\nwant=%q", out.String(), want)
	}
}

func TestRealSessionBinaryRoundtripAndStderr(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)

	raw := make([]byte, 65536)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	inSHA := sha256hex(raw)

	var out, errb bytes.Buffer
	res := mustRun(t, newManager(t, 0), context.Background(), id, dir,
		raw, &out, &errb, "/workspace/errcat.sh")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, errb.String())
	}
	if got := sha256hex(out.Bytes()); got != inSHA {
		t.Fatalf("二进制往返哈希不一致: in=%s out=%s", inSHA, got)
	}
	if errb.String() != "ERR_LINE\n" {
		t.Fatalf("stderr 应独立且不污染 stdout: %q", errb.String())
	}
}

func TestRealSessionExitCodes(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	m := newManager(t, 0)
	cases := []struct {
		tool string
		want int
	}{
		{"/bin/true", 0}, {"/workspace/exit7.sh", 7}, {"/workspace/exit42.sh", 42},
	}
	for _, tc := range cases {
		res := mustRun(t, m, context.Background(), id, dir, nil, &bytes.Buffer{}, &bytes.Buffer{}, tc.tool)
		if res.ExitCode != tc.want {
			t.Fatalf("%s: exit=%d want=%d", tc.tool, res.ExitCode, tc.want)
		}
	}
}

// 普通长任务不被管理查询的短超时误杀：bootstrap 限时 2s，工具跑 5s。
func TestRealSessionLongTaskNotKilledByMgmtTimeout(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	start := time.Now()
	res := mustRun(t, newManager(t, 2*time.Second), context.Background(), id, dir,
		nil, &bytes.Buffer{}, &bytes.Buffer{}, "/bin/sh", "-c", "sleep 5")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d（长任务被误杀？）", res.ExitCode)
	}
	if time.Since(start) < 5*time.Second {
		t.Fatalf("5s 任务提前返回: %v", time.Since(start))
	}
}

// 取消：不会自行清理子进程的 fixture（noclean）——子进程全部被会话清理。
func TestRealSessionCancelNocleanFixture(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	cr := runThenCancel(t, newManager(t, 0), id, dir, "/workspace/noclean.sh", nil, 300*time.Millisecond)
	res := cr.res
	if !res.Canceled || res.ExitCode != 130 || res.Detail != session.CancelConfirmed {
		t.Fatalf("取消结果不符合预期: %+v", res)
	}
	waitReaped(t, id, cr.tpid)
	if got := observeGroup(t, id, cr.tpid); len(got) != 0 {
		t.Fatalf("工具进程组应已清空: %v", got)
	}
}

// 取消：会自行清理子进程的 fixture（selfclean）——执行器不依赖 fixture 自清理。
func TestRealSessionCancelSelfcleanFixture(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	cr := runThenCancel(t, newManager(t, 0), id, dir, "/workspace/selfclean.sh", nil, 300*time.Millisecond)
	if !cr.res.Canceled || cr.res.ExitCode != 130 || cr.res.Detail != session.CancelConfirmed {
		t.Fatalf("取消结果不符合预期: %+v", cr.res)
	}
	waitReaped(t, id, cr.tpid)
}

// 取消：覆盖孙进程（同一进程组内的 sh→sleep 与直接 sleep）。
func TestRealSessionCancelGrandchild(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	cr := runThenCancel(t, newManager(t, 0), id, dir, "/workspace/grandchild.sh", nil, 300*time.Millisecond)
	if !cr.res.Canceled || cr.res.ExitCode != 130 {
		t.Fatalf("res=%+v", cr.res)
	}
	waitReaped(t, id, cr.tpid)
	if got := observeGroup(t, id, cr.tpid); len(got) != 0 {
		t.Fatalf("孙进程应一并清理: %v", got)
	}
}

// 重复取消：ctl 第二次调用按幂等处理（会话目录已清除 → 3）。
func TestRealSessionRepeatCancelIdempotent(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	cr := runThenCancel(t, newManager(t, 0), id, dir, "/workspace/noclean.sh", nil, 200*time.Millisecond)
	if !cr.res.Canceled || cr.res.ExitCode != 130 {
		t.Fatalf("res=%+v", cr.res)
	}
	ctl := &session.DockerController{}
	exit, _, err := ctl.Cancel(context.Background(), id, cr.res.SessionID)
	if err != nil {
		t.Fatalf("重复取消调用失败: %v", err)
	}
	if exit != 3 {
		t.Fatalf("重复取消应返回 3（会话已不存在）, got %d", exit)
	}
}

// 启动阶段取消：ctx 预先取消 → 不触碰容器、无会话残留。
func TestRealSessionStartupCancel(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	m := newManager(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := mustRun(t, m, ctx, id, dir, nil, &bytes.Buffer{}, &bytes.Buffer{}, "/workspace/noclean.sh")
	if !res.Canceled || res.ExitCode != 130 || res.Detail != session.CancelStartupAbort {
		t.Fatalf("res=%+v", res)
	}
	_, code := dexec(t, id, "test", "-d", "/tmp/km-sessions/"+res.SessionID)
	if code == 0 {
		t.Fatal("启动阶段取消不应留下会话目录")
	}
}

// 多项目隔离：项目 A 取消不影响项目 B。
func TestRealSessionMultiProjectIsolation(t *testing.T) {
	dirA := tempProject(t)
	dirB := tempProject(t)
	idA, _ := sessionContainer(t, dirA)
	idB, _ := sessionContainer(t, dirB)
	m := newManager(t, 0)

	crA := runThenCancelAsync(t, m, idA, dirA, "/workspace/noclean.sh", 300*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	var outB bytes.Buffer
	resB := mustRun(t, m, context.Background(), idB, dirB, nil, &outB, &bytes.Buffer{}, "/bin/sh", "-c", "sleep 2")

	resA := <-crA.done
	if !resA.Canceled || resA.ExitCode != 130 || resA.Detail != session.CancelConfirmed {
		t.Fatalf("A 取消结果: %+v", resA)
	}
	if resB.ExitCode != 0 {
		t.Fatalf("B 正常执行被影响: %+v", resB)
	}
	waitReaped(t, idA, crA.tpid)
	if got := observeGroup(t, idA, crA.tpid); len(got) != 0 {
		t.Fatalf("A 组应清空: %v", got)
	}
}

// SIGKILL/宿主失联单独观察：杀掉 docker exec 客户端后容器内任务继续运行
// （已记录风险），随后显式取消仍可清理。不把正常取消结果推广到此场景。
func TestRealSessionClientSigkillObservedSeparately(t *testing.T) {
	dir := tempProject(t)
	id, _ := sessionContainer(t, dir)
	ctl := &session.DockerController{}
	if err := ctl.Bootstrap(context.Background(), id); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	sid, _ := session.NewSessionID()
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(dockerBin(t), "exec", "-i", id, "/tmp/km-bin/km-run", sid, "/workspace/noclean.sh")
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, tpid := waitSessionDir(t, containerOf(sid, id))
	// SIGKILL 客户端（模拟宿主失联/终端被强杀）
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	time.Sleep(800 * time.Millisecond)
	// 风险观察：容器内任务仍在运行
	if !procExists(t, id, tpid) {
		t.Fatal("风险观察失败：SIGKILL 客户端后容器内任务竟已结束（语义变化？）")
	}
	// 显式取消仍可清理
	ctlExit, _, err := ctl.Cancel(context.Background(), id, sid)
	if err != nil || ctlExit != 0 {
		t.Fatalf("显式取消失败: exit=%d err=%v", ctlExit, err)
	}
	waitReaped(t, id, tpid)
	if got := observeGroup(t, id, tpid); len(got) != 0 {
		t.Fatalf("显式取消后组应清空: %v", got)
	}
}

// ---- 观察辅助 ----

type asyncCancel struct {
	done chan session.Result
	tpid string
}

// runThenCancelAsync 与 runThenCancel 相同，但取消时机由调用方控制，
// 用于多项目隔离（B 的执行穿插在 A 的取消窗口内）。
func runThenCancelAsync(t *testing.T, m *session.Manager, container, dir, tool string, setupDelay time.Duration) asyncCancel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan session.Result, 1)
	go func() {
		r, err := m.Run(ctx, container, "/workspace", nil, &bytes.Buffer{}, &bytes.Buffer{}, tool, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- r
	}()
	_, tpid := waitSessionDir(t, container)
	time.Sleep(setupDelay)
	cancel()
	return asyncCancel{done: done, tpid: tpid}
}

// containerOf 返回会话所在容器（SIGKILL 测试里只有一个容器）。
func containerOf(sid, container string) string { return container }

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

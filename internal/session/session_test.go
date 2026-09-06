package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- fakes ----------

type fakeProc struct {
	release  chan int
	killHook func()
}

func newFakeProc() *fakeProc { return &fakeProc{release: make(chan int, 1)} }

func (p *fakeProc) Wait() (int, error) {
	code := <-p.release
	if code < 0 {
		return code, fmt.Errorf("进程被终止")
	}
	return code, nil
}

func (p *fakeProc) Kill() error {
	if p.killHook != nil {
		p.killHook()
		return nil
	}
	select {
	case p.release <- -1:
	default:
	}
	return nil
}

type fakeStarter struct {
	mu       sync.Mutex
	once     sync.Once
	bin      string
	startErr error
	procs    []*fakeProc
	argvs    [][]string
	ready    chan struct{} // 构造时创建，首次 Start 后关闭
}

func newFakeStarter() *fakeStarter {
	return &fakeStarter{ready: make(chan struct{})}
}

func (f *fakeStarter) LookPath() (string, error) {
	if f.bin != "" {
		return f.bin, nil
	}
	return "/usr/bin/docker", nil
}

func (f *fakeStarter) Start(argv []string, stdin io.Reader, stdout, stderr io.Writer) (Proc, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.mu.Lock()
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	p := newFakeProc()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	f.once.Do(func() { close(f.ready) })
	return p, nil
}

// waitProc 等待首次 Start 完成（race 安全）。
func (f *fakeStarter) waitProc(t *testing.T) *fakeProc {
	t.Helper()
	select {
	case <-f.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Start 超时")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[len(f.procs)-1]
}

type fakeController struct {
	bootstrapErr  error
	bootstrapHeld chan struct{} // 非 nil 时阻塞直到 ctx 取消或关闭
	cancelHook    func()        // Cancel 调用时触发（用于确定性竞争注入）
	cancelCalls   int
	cancelPlan    []struct {
		exit int
		out  string
		err  error
	}
	sids []string
}

func (f *fakeController) Bootstrap(ctx context.Context, container string) error {
	if f.bootstrapHeld != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.bootstrapHeld:
			return f.bootstrapErr
		}
	}
	return f.bootstrapErr
}

func (f *fakeController) Sessions(ctx context.Context, container string) (string, string, int, error) {
	return "", "", 0, nil
}

func (f *fakeController) Alive(ctx context.Context, container, sid string) (int, string, error) {
	return 1, "", nil // 测试中默认 bash 已退出（走清理路径）
}

func (f *fakeController) Cancel(ctx context.Context, container, sid string) (int, string, error) {
	f.cancelCalls++
	if f.cancelHook != nil {
		f.cancelHook()
	}
	f.sids = append(f.sids, sid)
	if len(f.cancelPlan) == 0 {
		return 0, "", nil
	}
	plan := f.cancelPlan[0]
	f.cancelPlan = f.cancelPlan[1:]
	return plan.exit, plan.out, plan.err
}

// ---------- helpers ----------

func fastManager(st Starter, ctl Controller, diag io.Writer) *Manager {
	return &Manager{Starter: st, Controller: ctl, CleanupTimeout: 300 * time.Millisecond, Diag: diag}
}

const sidPattern = "^s[0-9a-f]{16}$"

// ---------- tests ----------

func TestNewSessionIDUniqueAndShaped(t *testing.T) {
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSessionID()
	if a == b {
		t.Fatalf("会话 ID 应随机: %q", a)
	}
	if len(a) != 17 || a[0] != 's' {
		t.Fatalf("ID 形状不正确: %q", a)
	}
}

func TestExecSessionArgs(t *testing.T) {
	got := ExecSessionArgs("/usr/bin/docker", "km-p1", "/workspace/中文 目录", "sabc", "nmap",
		[]string{"", "a b", "--help", "5 * ?"})
	want := []string{"/usr/bin/docker", "exec", "-w", "/workspace/中文 目录", "-i", "km-p1",
		"/tmp/km-bin/km-run", "sabc", "nmap", "", "a b", "--help", "5 * ?"}
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Fatalf("argv 不一致:\n got=%q\nwant=%q", got, want)
	}
}

func TestRunNormalExitNoCancel(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{}
	m := fastManager(st, ctl, nil)
	ctx := context.Background()
	go func() {
		st.waitProc(t).release <- 7
	}()
	res, err := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.ExitCode != 7 || res.Canceled || res.Detail != CancelNone {
		t.Fatalf("res=%+v", res)
	}
	if ctl.cancelCalls != 0 {
		t.Fatalf("正常退出不应触发取消, cancelCalls=%d", ctl.cancelCalls)
	}
}

func TestRunCanceledMidFlight(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{cancelPlan: []struct {
		exit int
		out  string
		err  error
	}{{exit: 0, out: "", err: nil}}}
	ctl.cancelHook = func() {
		st.waitProc(t).release <- 143 // ctl 终止工具组后 km-run 退出 143
	}
	m := fastManager(st, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		st.waitProc(t)
		cancel() // 工具运行中取消
	}()
	res, err := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "sleep", []string{"300"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.Canceled || res.ExitCode != 130 || res.Detail != CancelConfirmed {
		t.Fatalf("res=%+v", res)
	}
	if ctl.cancelCalls != 1 {
		t.Fatalf("应恰好一次取消调用: %d", ctl.cancelCalls)
	}
}

// 取消与工具自然结束竞争时，保留真实退出码（未误杀则不虚报 130）。
// 取消与完成竞争的确定性版本：fake ctl 被调用时工具刚以 0 结束——
// Manager 必须保留真实退出码而不是虚报 130。
func TestRunCanceledButToolAlreadyDone(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{}
	ctl.cancelHook = func() {
		p := st.waitProc(t)
		p.release <- 0 // 工具在取消过程中自然结束
	}
	m := fastManager(st, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		st.waitProc(t)
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res, _ := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if !res.Canceled || res.ExitCode != 0 {
		t.Fatalf("已完成工具的真实退出码应保留: %+v", res)
	}
}

func TestRunCancelCtlNotStarted(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{cancelPlan: []struct {
		exit int
		out  string
		err  error
	}{{exit: 3, out: "", err: nil}}}
	ctl.cancelHook = func() {
		st.waitProc(t).release <- 143
	}
	m := fastManager(st, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		st.waitProc(t)
		cancel()
	}()
	res, _ := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if !res.Canceled || res.ExitCode != 130 || res.Detail != CancelAlreadyGone {
		t.Fatalf("res=%+v", res)
	}
}

func TestRunCancelIncompleteFlagged(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{cancelPlan: []struct {
		exit int
		out  string
		err  error
	}{{exit: 4, out: "", err: nil}}}
	ctl.cancelHook = func() {
		st.waitProc(t).release <- 137
	}
	var diag bytes.Buffer
	m := fastManager(st, ctl, &diag)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		st.waitProc(t)
		cancel()
	}()
	res, _ := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if res.ExitCode != 130 || res.Detail != CancelIncomplete || res.CtlExit != 4 {
		t.Fatalf("res=%+v", res)
	}
	if !strings.Contains(diag.String(), "未在时限内确认") {
		t.Fatalf("清理失败应有诊断输出: %q", diag.String())
	}
}

// 清理调用本身失败 → 兜底强杀客户端，结果标记 failed。
func TestRunCancelCtlErrorBackstopKill(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{cancelPlan: []struct {
		exit int
		out  string
		err  error
	}{{exit: -1, out: "", err: errors.New("引擎失联")}}}
	m := fastManager(st, ctl, nil)
	m.CleanupTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		st.waitProc(t)
		cancel()
		// 不释放 proc：模拟主进程在清理失败后仍不退出
	}()
	// Kill 兜底由 fakeProc.killHook 默认行为覆盖：Kill→release -1
	res, _ := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if !res.Canceled || res.ExitCode != 130 || res.Detail != CancelFailed || res.CancelErr == nil {
		t.Fatalf("res=%+v", res)
	}
}

func TestStartupCancelBeforeTouch(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{}
	m := fastManager(st, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.Canceled || res.ExitCode != 130 || res.Detail != CancelStartupAbort {
		t.Fatalf("res=%+v", res)
	}
	if ctl.cancelCalls != 0 || len(st.procs) != 0 {
		t.Fatalf("启动前取消不应触碰容器: procs=%d cancels=%d", len(st.procs), ctl.cancelCalls)
	}
}

func TestStartupCancelDuringBootstrap(t *testing.T) {
	st := newFakeStarter()
	held := make(chan struct{})
	ctl := &fakeController{bootstrapHeld: held}
	m := fastManager(st, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	res, _ := m.Run(ctx, "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if !res.Canceled || res.Detail != CancelStartupAbort || len(st.procs) != 0 {
		t.Fatalf("res=%+v procs=%d", res, len(st.procs))
	}
}

func TestBootstrapErrPropagates(t *testing.T) {
	st := newFakeStarter()
	ctl := &fakeController{bootstrapErr: errors.New("cp 失败")}
	m := fastManager(st, ctl, nil)
	_, err := m.Run(context.Background(), "km-p1", "", nil, io.Discard, io.Discard, "tool", nil)
	if err == nil || !strings.Contains(err.Error(), "引导失败") {
		t.Fatalf("应报引导失败: %v", err)
	}
	if len(st.procs) != 0 {
		t.Fatal("引导失败不应启动工具")
	}
}

// 产物自检：Bootstrap 安装的 tar 必须含三个可执行脚本且会话路径已替换。
func TestScriptsTarContent(t *testing.T) {
	raw, err := scriptsTar()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		found[h.Name] = true
		if h.Typeflag == tar.TypeReg {
			if h.Mode != 0o755 {
				t.Fatalf("%s 权限应为 0755: %o", h.Name, h.Mode)
			}
			buf, _ := io.ReadAll(tr)
			if strings.Contains(string(buf), "__SESSIONS__") {
				t.Fatalf("%s 含未替换占位符", h.Name)
			}
			if h.Name != "km-bin/km-observe" && !strings.Contains(string(buf), "/tmp/km-sessions") {
				t.Fatalf("%s 缺会话路径", h.Name)
			}
		}
	}
	for _, want := range []string{"km-bin/", "km-bin/km-run", "km-bin/km-ctl", "km-bin/km-observe"} {
		if !found[want] {
			t.Fatalf("tar 缺少 %s（实际 %v）", want, found)
		}
	}
}

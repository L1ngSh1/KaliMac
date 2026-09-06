// Package session implements the minimal per-execution session kernel:
// each tool execution gets a unique session identity inside the container;
// a container-side supervisor (km-run) owns the tool's process group, and a
// separate control path (km-ctl) can cancel exactly that group. The tool's
// stdin/stdout/stderr are streamed through untouched; control information
// travels via separate exec invocations, never via the data streams.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// DefaultCleanupTimeout bounds the cancel path: ctl exec + grace wait for
// the main exec to exit. It is independent of the (already canceled) main
// context and never applies to normal tool runtime.
const DefaultCleanupTimeout = 15 * time.Second

// Proc is a started streaming process. Wait returns the final exit code
// (0..255) or -1 when the process ended abnormally (killed, no status).
// Lifecycle is caller-managed: the process is NOT killed automatically when
// a context is canceled — cancellation goes through the session control
// path, and Kill() is only the last-resort backstop.
type Proc interface {
	Wait() (code int, err error)
	Kill() error
}

// Starter launches streaming processes.
type Starter interface {
	LookPath() (bin string, err error)
	Start(argv []string, stdin io.Reader, stdout, stderr io.Writer) (Proc, error)
}

// Controller performs container-side control operations. Implementations
// must bound every call (they receive a context with a deadline for the
// cancel path and their own management bound for Bootstrap).
type Controller interface {
	// Bootstrap installs/refreshes the controller scripts (idempotent).
	Bootstrap(ctx context.Context, container string) error
	// Cancel asks the container-side controller to finalize one session.
	Cancel(ctx context.Context, container, sid string) (ctlExit int, output string, err error)
}

// CancelDetail classifies what the cancel path actually did.
type CancelDetail string

const (
	CancelNone         CancelDetail = ""
	CancelConfirmed    CancelDetail = "confirmed"     // ctl=0: group terminated and reaped
	CancelAlreadyGone  CancelDetail = "already-gone"  // ctl=0/3: session already finished or never materialized
	CancelIncomplete   CancelDetail = "incomplete"    // ctl=4: cleanup not confirmed within budget
	CancelStartupAbort CancelDetail = "startup-abort" // canceled before the tool could start
	CancelFailed       CancelDetail = "failed"        // ctl invocation itself failed
)

// Result reports one session execution.
type Result struct {
	SessionID string
	ExitCode  int // tool's code; 130 when cancellation terminated the work
	Canceled  bool
	Detail    CancelDetail
	CtlExit   int
	CtlOutput string
	CancelErr error
}

// Manager executes tools inside a container under session semantics.
type Manager struct {
	Starter        Starter
	Controller     Controller
	CleanupTimeout time.Duration // 0 → DefaultCleanupTimeout
	// Diag receives km diagnostics (never tool stdout/stderr). Optional.
	Diag io.Writer
	// SkipBootstrap：调用方已完成引导（幂等）时置位，避免重复 docker cp。
	SkipBootstrap bool
}

// NewSessionID returns a fresh session identity like "s1a2b3c4d5e6f7a8b9".
func NewSessionID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成会话 ID 失败: %w", err)
	}
	return "s" + hex.EncodeToString(buf), nil
}

// ExecSessionArgs builds the streaming exec argv for one session:
// docker exec [-w DIR] -i CONTAINER /tmp/km-bin/km-run SID TOOL ARG...
// The tool argv is preserved element-wise; nothing is shell-joined.
func ExecSessionArgs(bin, container, workdir, sid, tool string, toolArgs []string) []string {
	args := []string{bin, "exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, "-i", container, RunScriptPath, sid, tool)
	return append(args, toolArgs...)
}

func (m *Manager) cleanupTimeout() time.Duration {
	if m.CleanupTimeout > 0 {
		return m.CleanupTimeout
	}
	return DefaultCleanupTimeout
}

func (m *Manager) diagf(format string, a ...any) {
	if m.Diag != nil {
		fmt.Fprintf(m.Diag, "km: "+format+"\n", a...)
	}
}

type waitOutcome struct {
	code int
	err  error
}

// Run executes tool with toolArgs inside container under session semantics.
// workdir may be empty. stdin/stdout/stderr are streamed; km never buffers
// tool output. On context cancellation the independent cleanup path runs
// with a fresh bounded context.
func (m *Manager) Run(ctx context.Context, container, workdir string, stdin io.Reader, stdout, stderr io.Writer, tool string, toolArgs []string) (Result, error) {
	sid, err := NewSessionID()
	res := Result{SessionID: sid, Detail: CancelNone}
	if err != nil {
		return res, err
	}

	// 启动前取消：不触碰容器。
	if ctx.Err() != nil {
		res.Canceled, res.ExitCode, res.Detail = true, 130, CancelStartupAbort
		return res, nil
	}
	if !m.SkipBootstrap {
		if err := m.Controller.Bootstrap(ctx, container); err != nil {
			if ctx.Err() != nil {
				res.Canceled, res.ExitCode, res.Detail = true, 130, CancelStartupAbort
				return res, nil
			}
			return res, fmt.Errorf("会话脚本引导失败: %w", err)
		}
	}
	if ctx.Err() != nil { // bootstrap 与 Start 之间的取消窗口
		res.Canceled, res.ExitCode, res.Detail = true, 130, CancelStartupAbort
		return res, nil
	}

	bin, err := m.Starter.LookPath()
	if err != nil {
		return res, err
	}
	proc, err := m.Starter.Start(ExecSessionArgs(bin, container, workdir, sid, tool, toolArgs), stdin, stdout, stderr)
	if err != nil {
		if ctx.Err() != nil {
			res.Canceled, res.ExitCode, res.Detail = true, 130, CancelStartupAbort
			return res, nil
		}
		return res, fmt.Errorf("启动执行进程失败: %w", err)
	}

	waitCh := make(chan waitOutcome, 1)
	go func() {
		code, werr := proc.Wait()
		waitCh <- waitOutcome{code, werr}
	}()

	select {
	case <-ctx.Done():
		res.Canceled = true
		// 取消与完成竞争：优先采信已就绪的真实结果（工具已自行结束，
		// 无需容器侧清理，会话目录由 km-run 自行清理）。
		select {
		case wr := <-waitCh:
			res.Detail = CancelAlreadyGone
			return m.finish(res, wr, true), nil
		default:
		}
		res.Detail = m.cancelSession(container, sid, &res)
		// 清理后等主执行进程自然退出；超时则兜底强杀客户端。
		select {
		case wr := <-waitCh:
			return m.finish(res, wr, true), nil
		case <-time.After(m.cleanupTimeout()):
			m.diagf("会话 %s 清理后主进程未退出，强制结束客户端", sid)
			_ = proc.Kill()
			wr := <-waitCh
			return m.finish(res, wr, true), nil
		}
	case wr := <-waitCh:
		return m.finish(res, wr, false), nil
	}
}

func (m *Manager) finish(res Result, wr waitOutcome, canceled bool) Result {
	code := wr.code
	if wr.err != nil && code == 0 {
		code = -1 // Wait 出错且无状态可读（不应发生于正常路径）
	}
	if canceled {
		res.Canceled = true
		if res.Detail == CancelNone {
			res.Detail = CancelConfirmed
		}
		// 取消语义：工具确实被取消终止（信号致死/客户端被杀）→ 130；
		// 若工具在取消生效前已自然结束，保留真实退出码。
		if code < 0 || code >= 128 {
			code = 130
		}
	}
	res.ExitCode = code
	return res
}

// cancelSession runs the control-path cancellation with a fresh, bounded
// context (the main context is already canceled and must not be reused).
func (m *Manager) cancelSession(container, sid string, res *Result) CancelDetail {
	cctx, cancel := context.WithTimeout(context.Background(), m.cleanupTimeout())
	defer cancel()
	ctlExit, output, err := m.Controller.Cancel(cctx, container, sid)
	res.CtlExit, res.CtlOutput, res.CancelErr = ctlExit, output, err
	if err != nil {
		m.diagf("会话 %s 清理调用失败: %v", sid, err)
		return CancelFailed
	}
	switch ctlExit {
	case 0:
		return CancelConfirmed
	case 3:
		m.diagf("会话 %s 取消时会话已结束或尚未建立", sid)
		return CancelAlreadyGone
	case 4:
		m.diagf("会话 %s 清理未在时限内确认（ctl=4）；请人工核对容器内进程", sid)
		return CancelIncomplete
	default:
		m.diagf("会话 %s 清理返回未知状态 ctl=%d", sid, ctlExit)
		return CancelFailed
	}
}

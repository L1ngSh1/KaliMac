package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"
)

// SignalProc is a Proc that can receive signals (窗口尺寸转发需要)。
type SignalProc interface {
	Proc
	Signal(sig syscall.Signal) error
}

// ShellResult reports one interactive shell session.
type ShellResult struct {
	SessionID string
	ExitCode  int // bash 退出码原样；外部 SIGTERM/SIGHUP 终止 → 143
	Signaled  bool
}

// ShellScriptPath is the container-side shell entry point.
const ShellScriptPath = ScriptsDir + "/km-shell"

// ShellPS1 / ShellPromptCommand are injected into the shell environment:
// PS1 提供提示符同步标记；PROMPT_COMMAND 武装 EXIT trap（bash 退出不会自动
// SIGHUP 后台进程组，退出时需对全部作业发 TERM）。
const (
	ShellPS1           = "KM_SHELL> "
	ShellPromptCommand = "trap 'kill $(jobs -p) 2>/dev/null' EXIT"
)

// ShellArgv builds the interactive shell exec argv:
// docker exec -it [-w DIR] -e PS1 -e PROMPT_COMMAND -e KM_SESSION_ID
//
//	--detach-keys "" CONTAINER /tmp/km-bin/km-shell SID
func ShellArgv(bin, container, workdir, sid string) []string {
	args := []string{bin, "exec", "-it"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args,
		"-e", "PS1="+ShellPS1,
		"-e", "PROMPT_COMMAND="+ShellPromptCommand,
		"-e", "KM_SESSION_ID="+sid,
		"--detach-keys", "", // exec 无脱离机制；显式禁用避免歧义
		container, ShellScriptPath, sid)
	return args
}

// RunShell runs an interactive bash session inside container with the real
// terminal handed over to the Docker CLI（责任划分见 ADR-005）：
//   - km 只负责 tty 前置检查之外的进程生命周期、会话登记与外部信号兜底；
//   - Docker CLI 管理 raw mode/退出恢复/窗口尺寸（km 转发 SIGWINCH）；
//   - 键盘 Ctrl-C 是 raw mode 字节，不构成对 km 的信号；
//   - 外部 SIGINT 忽略；SIGTERM/SIGHUP → 终止客户端、以进入前快照恢复
//     termios、退出 143。
//
// 会话登记进 /tmp/km-sessions/<sid>（kind=shell），宿主崩溃遗留的活跃
// shell 会话因此被 M0 核验阻断。ctx 不参与生命周期（main 的全局取消
// ctx 不影响 shell，见 ADR-005）。
func (m *Manager) RunShell(container, workdir string, stdin *os.File, stdout, stderr io.Writer) (ShellResult, error) {
	sid, err := NewSessionID()
	res := ShellResult{SessionID: sid}
	if err != nil {
		return res, err
	}
	if !m.SkipBootstrap {
		bctx, cancel := context.WithTimeout(context.Background(), DefaultCleanupTimeout)
		defer cancel()
		if err := m.Controller.Bootstrap(bctx, container); err != nil {
			return res, fmt.Errorf("会话脚本引导失败: %w", err)
		}
	}
	bin, err := m.Starter.LookPath()
	if err != nil {
		return res, err
	}
	proc, err := m.Starter.Start(ShellArgv(bin, container, workdir, sid), stdin, stdout, stderr)
	if err != nil {
		return res, fmt.Errorf("启动 shell 失败: %w", err)
	}

	// 终端快照：仅在外部信号路径用作恢复兜底（Docker CLI 自管日常恢复）。
	saved, stateErr := term.GetState(int(stdin.Fd()))

	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	go func() {
		for s := range sigCh {
			switch s {
			case syscall.SIGWINCH:
				// 转发给客户端：由 Docker CLI 同步容器内窗口尺寸
				if sp, ok := proc.(SignalProc); ok {
					_ = sp.Signal(syscall.SIGWINCH)
				}
			case syscall.SIGINT:
				// raw mode 下键盘 Ctrl-C 是字节；外部 SIGINT 忽略
			default: // SIGTERM / SIGHUP
				fmt.Fprintf(m.diagWriter(), "\nkm: 收到 %v，终止 shell 并恢复终端\n", s)
				atomic.StoreInt32(&m.shellSignaled, 1)
				_ = proc.Kill()
				if stateErr == nil && saved != nil {
					_ = term.Restore(int(stdin.Fd()), saved)
				}
			}
		}
	}()

	// 启动后延迟主动同步一次窗口尺寸（docker exec 初始尺寸竞态，
	// ADR-005 风险 2；客户端未就绪时首个 SIGWINCH 可能丢失，故重试两次）
	if sp, ok := proc.(SignalProc); ok {
		go func() {
			for _, d := range []time.Duration{250 * time.Millisecond, 600 * time.Millisecond} {
				time.Sleep(d)
				_ = sp.Signal(syscall.SIGWINCH)
			}
		}()
	}

	code, waitErr := proc.Wait()
	if atomic.LoadInt32(&m.shellSignaled) == 1 {
		res.Signaled = true
		res.ExitCode = 143
		return res, nil
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if code == 0 {
			code = -1
		}
	}
	res.ExitCode = code
	return res, nil
}

func (m *Manager) diagWriter() io.Writer {
	if m.Diag != nil {
		return m.Diag
	}
	return io.Discard
}

// Proc 接口断言：osProc 支持 Signal（窗口尺寸转发用）。
var _ interface {
	Signal(syscall.Signal) error
} = &osProc{}

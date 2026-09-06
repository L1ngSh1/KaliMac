package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
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
	// Detached：客户端退出而 bash 仍在运行（脱离/失联）。登记保留，
	// 后续任务被阻断；调用方应保留诊断提示。
	Detached bool
	// CleanupUnconfirmed：会话收尾未确认完成（仍有同会话成员存活）。
	// 登记保留，后续任务被阻断；调用方应保留诊断提示。
	CleanupUnconfirmed bool
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
func ShellArgv(bin, container, workdir, sid, detachKeys string) []string {
	args := []string{bin, "exec", "-it"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args,
		"-e", "PS1="+ShellPS1,
		"-e", "KM_SESSION_ID="+sid,
		"--detach-keys", detachKeys, // 空串并非可靠禁用（见 ADR-005）；
		// 无论脱离与否，登记会话的阻断语义一致
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
	// 每次调用独立状态（review B：不共享 Manager 级标志，调用结束即回收）。
	var signaled int32
	done := make(chan struct{})     // 关闭 = RunShell 结束，辅助 goroutine 退出
	restored := make(chan struct{}) // 信号路径的 termios 恢复完成

	// 终端快照必须在启动客户端之前：Start 之后 Docker CLI 已把终端置为
	// raw mode，此刻快照会把 raw 状态当「进入前状态」保存（review B）。
	// 快照失败时不启动会话。
	saved, stateErr := term.GetState(int(stdin.Fd()))
	if stateErr != nil {
		return res, fmt.Errorf("获取终端状态失败: %w", stateErr)
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

	// 信号监听先于 Start 注册：消除启动期间的信号空窗（空窗内 SIGTERM
	// 会以默认行为直接杀死 km，termios 无法恢复）。
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	proc, err := m.Starter.Start(ShellArgv(bin, container, workdir, sid, m.ShellDetachKeys), stdin, stdout, stderr)
	if err != nil {
		signal.Stop(sigCh) // 启动失败收尾：立即停止监听，goroutine 尚未启动
		return res, fmt.Errorf("启动 shell 失败: %w", err)
	}

	var once sync.Once
	go func() {
		for {
			select {
			case s := <-sigCh:
				switch s {
				case syscall.SIGWINCH:
					// 转发给客户端：由 Docker CLI 同步容器内窗口尺寸
					if sp, ok := proc.(SignalProc); ok {
						_ = sp.Signal(syscall.SIGWINCH)
					}
				case syscall.SIGINT:
					// raw mode 下键盘 Ctrl-C 是字节；外部 SIGINT 忽略
				default: // SIGTERM / SIGHUP（可能重复送达，Once 保证单次执行）
					fmt.Fprintf(m.diagWriter(), "\nkm: 收到 %v，终止 shell 并恢复终端\n", s)
					once.Do(func() {
						atomic.StoreInt32(&signaled, 1)
						_ = proc.Kill()
						if saved != nil {
							_ = term.Restore(int(stdin.Fd()), saved)
						}
						close(restored) // 恢复完成后才放行主路径返回 143
					})
				}
			case <-done:
				return
			}
		}
	}()

	// 启动后延迟主动同步一次窗口尺寸（docker exec 初始尺寸竞态，
	// ADR-005 风险 2；客户端未就绪时首个 SIGWINCH 可能丢失，故重试两次）
	if sp, ok := proc.(SignalProc); ok {
		go func() {
			for _, d := range []time.Duration{250 * time.Millisecond, 600 * time.Millisecond} {
				select {
				case <-done:
					return
				case <-time.After(d):
					_ = sp.Signal(syscall.SIGWINCH)
				}
			}
		}()
	}

	code, waitErr := proc.Wait()
	close(done)
	if atomic.LoadInt32(&signaled) == 1 {
		// km 主动收尾路径（外部 SIGTERM/SIGHUP）：客户端已终止，清理同会话
		// bash 与全部作业（SID 域 TERM→KILL），完成后返回 143。
		fctx, fcancel := context.WithTimeout(context.Background(), DefaultCleanupTimeout)
		defer fcancel()
		fExit, fErrStr, ferr := m.Controller.Cancel(fctx, container, sid)
		if ferr != nil || fExit == 4 {
			fmt.Fprintf(m.diagWriter(), "km: 信号路径会话收尾未确认完成（exit=%d, err=%v, stderr: %s）；"+
				"登记保留，后续任务将被阻断，请人工核验\n", fExit, ferr, strings.TrimSpace(fErrStr))
			res.CleanupUnconfirmed = true
		}
		<-restored
		res.Signaled = true
		res.ExitCode = 143
		return res, nil
	}
	// 会话收尾：区分「bash 退出中（僵尸/未回收的瞬时状态）」与「真脱离」——
	//   bash 完全退出 → 与显式 cancel 同一 SID 域清理：TERM 全部同会话成员
	//     （含各作业组与管道成员）→ KILL 升级 → 确认后移除登记；
	//     未确认完成（exit 4）保留登记并阻断，绝不静默放行。
	//   bash 仍存活（脱离/失联，有界 2s 内未退出）→ 保留登记与会话
	//     （Detached=true），后续任务被 M0 阻断，显式 cancel 可清理；
	//     km 不掩盖脱离。
	fctx, fcancel := context.WithTimeout(context.Background(), DefaultCleanupTimeout)
	defer fcancel()
	detached := false
	for {
		aExit, _, aerr := m.Controller.Alive(fctx, container, sid)
		switch {
		case aerr != nil:
			fmt.Fprintf(m.diagWriter(), "km: shell 会话存活检查失败（%v）；登记保留，后续任务将被阻断\n", aerr)
			res.CleanupUnconfirmed = true
		case aExit == 1:
			// bash 已完全退出 → 清理同会话作业
			fExit, fErrStr, ferr := m.Controller.Cancel(fctx, container, sid)
			if ferr != nil || fExit == 4 {
				fmt.Fprintf(m.diagWriter(), "km: shell 会话收尾未确认完成（exit=%d, err=%v, stderr: %s）；"+
					"登记保留，后续任务将被阻断，请人工核验\n", fExit, ferr, strings.TrimSpace(fErrStr))
				res.CleanupUnconfirmed = true
			}
		case aExit == 0:
			if time.Now().After(fctxDeadline(fctx)) {
				detached = true
				fmt.Fprintf(m.diagWriter(), "km: shell 会话 %s 仍在运行（客户端脱离或失联）；"+
					"登记保留，后续任务将被阻断，显式 cancel 可清理\n", sid)
			} else {
				time.Sleep(150 * time.Millisecond)
				continue
			}
		default: // 3 = 登记缺失
			allOut, _, _, _ := m.Controller.Sessions(fctx, container)
			fmt.Fprintf(m.diagWriter(), "km: shell 会话 %s 登记缺失（sessions 输出: %q）；"+
				"后续任务将被阻断，请人工核验\n", sid, strings.TrimSpace(allOut))
		}
		break
	}
	if detached {
		res.Detached = true
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

// fctxDeadline 读取有界收尾 context 的截止时间（轮询窗口用）。
func fctxDeadline(fctx context.Context) time.Time {
	dl, ok := fctx.Deadline()
	if !ok {
		return time.Now().Add(DefaultCleanupTimeout)
	}
	return dl
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

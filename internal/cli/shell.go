package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"kalimac/internal/project"
	"kalimac/internal/runtime"

	"golang.org/x/term"
)

// runShellCommand implements `km shell`: interactive bash in the project
// container, with the real terminal handed over to the Docker CLI
// （责任划分与信号语义见 docs/adr-005-terminal-shell.md）。
func runShellCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km shell 不接受参数")
	}
	// tty 前置检查：在任何终端修改或会话启动之前失败（K 项）
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return envError(stderr, &runtime.Error{Code: runtime.CodeNotTTY,
			Msg: "km shell 需要 stdin/stdout 都是终端（非交互场景请使用 km run）"})
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	root, cfg, st, err := loadProjectStack(wd)
	if err != nil {
		return envError(stderr, err)
	}
	ep, err := verifyStackForMutation(ctx, dk, root, cfg, st)
	if err != nil {
		return envError(stderr, err)
	}

	containerDir, err := project.WorkspaceDir(root, wd)
	if err != nil {
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "工作目录映射失败", Err: err})
	}

	lock, err := project.AcquireLock(root)
	if err != nil {
		return envError(stderr, err) // KM_PROJECT_BUSY
	}
	defer lock.Release()
	if lock.BrokeStale() {
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）；正在核验容器内会话…\n")
	}

	// 临界区内：容器已停止 → 恢复
	if res, exists, _ := dk.InspectContainer(ctx, st.Container.ID); exists && res.State != "running" {
		fmt.Fprintf(stderr, "km: 容器已停止，正在恢复…\n")
		if err := dk.StartContainer(ctx, st.Container.ID); err != nil {
			return envError(stderr, err)
		}
	}

	// 会话核验（shell 会话同样登记，互相阻断）
	ctl := newSessionController(ep.Endpoint)
	if err := ctl.Bootstrap(ctx, st.Container.ID); err != nil {
		return envError(stderr, &runtime.Error{Code: runtime.CodeRuntimeOffline, Msg: "会话脚本引导失败", Err: err})
	}
	if err := verifyNoActiveSession(ctx, ctl, st.Container.ID, stderr); err != nil {
		return envError(stderr, err)
	}

	mgr := newSessionManager(ep.Endpoint, stderr, ctl)
	res, err := mgr.RunShell(st.Container.ID, containerDir, os.Stdin, stdout, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if res.Signaled {
		fmt.Fprintf(stderr, "km: shell 被外部信号终止（会话 %s）\n", res.SessionID)
	}
	// 正常退出后清扫自己的遗留会话目录（组已空，M0 语义下无害）
	if _, _, _, swerr := ctl.Sweep(context.Background(), st.Container.ID); swerr != nil {
		fmt.Fprintf(stderr, "km: 会话目录清扫失败（%v）；下次执行自动重试\n", swerr)
	}
	return res.ExitCode
}

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runStopCommand implements `km stop`: stop only the current project's
// identity-verified container, preserving container, files and image.
// Idempotent: an already-stopped container is success.
func runStopCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km stop 不接受参数")
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
	if _, err := verifyStackForMutation(ctx, dk, root, cfg, st); err != nil {
		return envError(stderr, err)
	}

	lock, err := project.AcquireLock(root)
	if err != nil {
		return envError(stderr, err) // KM_PROJECT_BUSY：执行中的任务不因 stop 被意外终止
	}
	defer lock.Release()
	if lock.BrokeStale() {
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）；这不代表容器内任务已结束，可用 km doctor 复核\n")
	}

	// 事务互斥（ADR §5.4）：存在未完成环境事务时 stop 阻断
	if err := refuseIfEnvTxnPending(root); err != nil {
		return envError(stderr, err)
	}

	res, exists, err := dk.InspectContainer(ctx, st.Container.ID)
	if err != nil {
		return envError(stderr, err)
	}
	if !exists {
		return envError(stderr, &runtime.Error{Code: runtime.CodeNotFound,
			Msg: "容器在检查与停止之间消失；运行 km init 恢复"})
	}
	if res.State != "running" {
		fmt.Fprintf(stdout, "km stop: 容器 %s 已是停止状态（幂等），数据保留\n", st.Container.Name)
		return ExitOK
	}
	if err := dk.StopContainer(ctx, st.Container.ID); err != nil {
		return envError(stderr, err)
	}
	fmt.Fprintf(stdout, "km stop: 容器 %s 已停止；容器与项目文件均保留，下次执行自动恢复\n", st.Container.Name)
	return ExitOK
}

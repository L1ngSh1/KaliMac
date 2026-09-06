package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// loadProjectStack finds the project upward and loads config + state.
func loadProjectStack(dir string) (root string, cfg *project.Config, st *project.State, err error) {
	var ok bool
	root, ok, err = project.FindConfig(dir)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "", nil, nil, &runtime.Error{Code: runtime.CodeProjectMissing,
			Msg: "从当前目录向上未找到 .km.json；请在项目根运行 km init"}
	}
	cfg, err = project.LoadConfig(filepath.Join(root, project.ConfigFileName))
	if err != nil {
		return "", nil, nil, err
	}
	st, err = project.LoadState(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil, &runtime.Error{Code: runtime.CodeProjectMissing,
				Msg: "本机状态缺失（.km/state.json 不存在）；请先运行 km init"}
		}
		return "", nil, nil, err
	}
	return root, cfg, st, nil
}

// verifyStackForMutation performs every read-only identity check that must
// pass before a mutating or executing command touches the container:
// local engine, recorded engine match, full container identity and image
// content identity. 返回解析并固定的 endpoint（R4）。
func verifyStackForMutation(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State) (runtime.EndpointInfo, error) {
	ep, err := resolveEngine(ctx, dk)
	if err != nil {
		return ep, err
	}
	if err := verifyProjectEngine(st, ep.Endpoint); err != nil {
		return ep, err
	}
	if st.Container.ID == "" {
		return ep, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "本机状态缺少容器 ID（旧版状态）；请重新 init 重建本机身份，不按名称接管"}
	}
	res, exists, err := dk.InspectContainer(ctx, st.Container.ID)
	if err != nil {
		return ep, err
	}
	if !exists {
		return ep, &runtime.Error{Code: runtime.CodeNotFound,
			Msg: fmt.Sprintf("记录的容器（ID %s…）不存在（可能被外部删除）；运行 km init 恢复", shortID(st.Container.ID))}
	}
	if err := verifyContainerIdentity(st, res, root); err != nil {
		return ep, err
	}
	cur, ok, err := dk.ImageID(ctx, cfg.Image)
	if err != nil {
		return ep, err
	}
	if !ok {
		return ep, &runtime.Error{Code: runtime.CodeRuntimeOffline,
			Msg: fmt.Sprintf("镜像 %s 已不在本地（可能被外部删除）；运行 km init 恢复", cfg.Image)}
	}
	if st.Container.ImageID != "" {
		if cur != st.Container.ImageID {
			return ep, &runtime.Error{Code: runtime.CodeImageDrift,
				Msg: fmt.Sprintf("镜像标签 %s 当前内容 %s… 与项目记录 %s… 不一致；镜像更新须走显式流程（重新 init）", cfg.Image, shortID(cur), shortID(st.Container.ImageID))}
		}
		if res.Image != "" && res.Image != st.Container.ImageID {
			return ep, &runtime.Error{Code: runtime.CodeContainerConflict,
				Msg: fmt.Sprintf("容器实际镜像 %s… 与项目记录 %s… 不一致", shortID(res.Image), shortID(st.Container.ImageID))}
		}
	}
	return ep, nil
}

// runToolCommand implements `km TOOL ARG...` and `km run -- TOOL ARG...`:
// the non-interactive execution path over the P2-A session kernel.
func runToolCommand(ctx context.Context, tool string, toolArgs []string, stdin io.Reader, stdout, stderr io.Writer, dk *runtime.Docker) int {
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
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）；这不代表容器内任务已结束，正在核验容器内会话…\n")
	}

	// 临界区内：容器已停止 → 恢复同一容器
	if res, exists, _ := dk.InspectContainer(ctx, st.Container.ID); exists && res.State != "running" {
		fmt.Fprintf(stderr, "km: 容器已停止，正在恢复…\n")
		if err := dk.StartContainer(ctx, st.Container.ID); err != nil {
			return envError(stderr, err)
		}
	}

	// 会话执行与取消使用同一固定 endpoint（R4）
	ctl := newSessionController(ep.Endpoint)
	if err := ctl.Bootstrap(ctx, st.Container.ID); err != nil {
		return envError(stderr, &runtime.Error{Code: runtime.CodeRuntimeOffline, Msg: "会话脚本引导失败", Err: err})
	}
	// 宿主锁之后核验容器内会话（R2）：区分活跃与遗留，遗留自动清扫
	if out, serr := ctl.Sessions(ctx, st.Container.ID); serr == nil {
		active := activeSessionIDs(out)
		if len(active) > 0 {
			return envError(stderr, &runtime.Error{Code: runtime.CodeSessionActive,
				Msg: fmt.Sprintf("容器内存在活跃会话 %v（疑似宿主中断遗留，任务可能仍在运行）；确认后执行 docker exec %s /tmp/km-bin/km-ctl cancel %s 显式清理，再重试", active, shortID(st.Container.ID), active[0])})
		}
		ctl.Sweep(ctx, st.Container.ID)
	}

	mgr := &session.Manager{
		Starter:       &session.ExecStarter{Env: []string{"DOCKER_HOST=" + ep.Endpoint}},
		Controller:    ctl,
		Diag:          stderr,
		SkipBootstrap: true,
	}
	res, err := mgr.Run(ctx, st.Container.ID, containerDir, stdin, stdout, stderr, tool, toolArgs)
	if err != nil {
		return envError(stderr, err)
	}
	if res.Canceled && res.Detail != session.CancelConfirmed && res.Detail != session.CancelAlreadyGone && res.Detail != session.CancelNone {
		// 清理异常已在 Diag 输出；这里只补充会话身份便于人工核对
		fmt.Fprintf(stderr, "km: 会话 %s 取消状态=%s，请用 km doctor 或容器内 /tmp/km-sessions 复核\n", res.SessionID, res.Detail)
	}
	return res.ExitCode
}

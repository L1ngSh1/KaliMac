package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// envError prints a KM_* error on stderr; the caller returns ExitEnv.
func envError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%v\n", err)
	return ExitEnv
}

// requireLocalEngine resolves the effective endpoint, refuses remote
// engines, and pins the endpoint for all subsequent calls of this run.
func requireLocalEngine(ctx context.Context, dk *runtime.Docker) error {
	ep, err := dk.EffectiveEndpoint(ctx)
	if err != nil {
		return err
	}
	if !runtime.IsLocalEndpoint(ep.Endpoint) {
		return &runtime.Error{Code: runtime.CodeEndpointRemote,
			Msg: fmt.Sprintf("有效 endpoint %s（来源 %s）不是本地引擎；v0.2 只使用本地引擎", ep.Endpoint, ep.Source)}
	}
	dk.EndpointOverride = ep.Endpoint
	return nil
}

// verifyProjectEngine compares the engine recorded in local state with the
// (already pinned) effective endpoint.
func verifyProjectEngine(st *project.State, epSource string) error {
	if st.Runtime.Endpoint != "" && epSource != st.Runtime.Endpoint {
		return &runtime.Error{Code: runtime.CodeRuntimeMismatch,
			Msg: fmt.Sprintf("本机状态记录的引擎 %s 与当前有效引擎 %s 不一致；显式 init 可迁移本机身份", st.Runtime.Endpoint, epSource)}
	}
	return nil
}

// verifyContainerIdentity checks the recorded full container ID against the
// live container: existence, name, project label and workspace mount.
func verifyContainerIdentity(st *project.State, res runtime.InspectResult, root string) error {
	if res.ID != st.Container.ID {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("inspect 返回的容器 ID 与记录不一致（返回 %s，记录 %s）", shortID(res.ID), shortID(st.Container.ID))}
	}
	if res.Name != st.Container.Name {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("容器名称 %q 与记录 %q 不一致", res.Name, st.Container.Name)}
	}
	if res.ProjectID != st.ProjectID {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("标签 km.project=%q 与本机状态 %q 不一致，不接管", res.ProjectID, st.ProjectID)}
	}
	if filepath.Clean(res.MountSource) != filepath.Clean(root) {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("/workspace 挂载源为 %q，预期 %q", res.MountSource, root)}
	}
	return nil
}

// runInitCommand implements `km init`: idempotent project bootstrap.
func runInitCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km init 不接受参数")
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	// 规范化（/tmp 与 /private/tmp 等价写法会导致身份假冲突）
	wd = project.CanonicalPath(wd)

	// 嵌套项目检测
	if parent, ok, perr := project.ParentProject(wd); perr == nil && ok {
		return envError(stderr, &runtime.Error{Code: runtime.CodeProjectNested,
			Msg: fmt.Sprintf("检测到父项目 %s；不自动创建嵌套项目（请在项目根直接运行 km）", parent)})
	}

	// 引擎（只读解析可在锁前；配置/状态/容器事务在锁内）
	ep, err := resolveEngine(ctx, dk)
	if err != nil {
		return envError(stderr, err)
	}

	// R3：项目锁覆盖整个 init 事务（配置→状态→容器→提交/回滚）；
	// 取得锁后所有状态判定都基于锁内新鲜读取。同项目任务执行中 → BUSY。
	lock, err := project.AcquireLock(wd)
	if err != nil {
		return envError(stderr, err)
	}
	defer lock.Release()
	if lock.BrokeStale() {
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）\n")
	}
	// 全新项目失败时不留 .km 空目录（先注册 → 在锁释放之后执行）
	preExistingState := project.StateExists(wd)
	defer func() {
		if !preExistingState {
			if _, statErr := os.Stat(project.StatePath(wd)); os.IsNotExist(statErr) {
				_ = os.Remove(filepath.Join(wd, project.StateDirName))
			}
		}
	}()

	// 配置：存在则校验（保留原文件），缺失则原子写入默认值
	cfgPath := filepath.Join(wd, project.ConfigFileName)
	var cfg *project.Config
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		cfg, err = project.LoadConfig(cfgPath)
		if err != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeConfigInvalid,
				Msg: "保留原文件；修复后重试", Err: err})
		}
	} else {
		cfg = &project.Config{SchemaVersion: project.SupportedSchemaVersion, Image: project.DefaultImage, Platform: project.DefaultPlatform}
		if err := writeConfigAtomic(cfgPath, cfg); err != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "写入默认配置失败", Err: err})
		}
	}

	// 镜像：本地缺失时显式拉取（有界），失败保留环境
	imageID, ok, err := dk.ImageID(ctx, cfg.Image)
	if err != nil {
		return envError(stderr, err)
	}
	if !ok {
		fmt.Fprintf(stderr, "km: 镜像 %s 不在本地，开始拉取（有界 %s）…\n", cfg.Image, runtime.PullTimeout)
		if err := dk.PullImage(ctx, cfg.Image); err != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeRuntimeOffline,
				Msg: "镜像拉取失败；旧环境保留，可重试 init", Err: err})
		}
		if imageID, ok, err = dk.ImageID(ctx, cfg.Image); err != nil || !ok {
			return envError(stderr, &runtime.Error{Code: runtime.CodeRuntimeOffline, Msg: "拉取后仍无法读取镜像身份"})
		}
	}

	// 幂等：已有状态且身份一致 → 复用（锁内读取）
	if project.StateExists(wd) {
		st, serr := project.LoadState(wd)
		if serr != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "保留原状态文件；如需重建请先删除 .km/", Err: serr})
		}
		if err := verifyProjectEngine(st, ep.Endpoint); err != nil {
			return envError(stderr, err)
		}
		if st.Container.ID != "" {
			res, exists, ierr := dk.InspectContainer(ctx, st.Container.ID)
			if ierr == nil && exists {
				if verr := verifyContainerIdentity(st, res, wd); verr != nil {
					return envError(stderr, verr)
				}
				if res.State != "running" {
					if err := dk.StartContainer(ctx, st.Container.ID); err != nil {
						return envError(stderr, err)
					}
				}
				fmt.Fprintf(stdout, "km init: 复用现有环境（project=%s container=%s image=%s）\n", st.ProjectID, shortID(st.Container.ID), shortID(st.Container.ImageID))
				return ExitOK
			}
			// 记录的容器不存在（被外部删除）→ init 恢复路径：按既有 project_id 重建
			fmt.Fprintf(stderr, "km: 记录的容器不存在，按既有项目身份重建…\n")
			return createContainerFor(ctx, dk, stdout, stderr, wd, cfg, st, ep.Endpoint, imageID)
		}
		// 旧版状态缺容器 ID：按记录的项目身份与名称重建/接管
		return createContainerFor(ctx, dk, stdout, stderr, wd, cfg, st, ep.Endpoint, imageID)
	}

	// 全新项目
	pid, err := project.NewProjectID()
	if err != nil {
		return envError(stderr, err)
	}
	st := &project.State{StateVersion: project.SupportedStateVersion, ProjectID: pid}
	st.Runtime.Context = ep.Context
	st.Runtime.Endpoint = ep.Endpoint
	st.CreatedAt = nowUTC()
	return createContainerFor(ctx, dk, stdout, stderr, wd, cfg, st, ep.Endpoint, imageID)
}

// createContainerFor creates (or recovers) the project container and stores
// identity atomically. Only resources created by this call are cleaned up
// on failure.
func createContainerFor(ctx context.Context, dk *runtime.Docker, stdout, stderr io.Writer, root string, cfg *project.Config, st *project.State, endpoint, imageID string) int {
	name := st.Container.Name
	if name == "" {
		name = runtime.ContainerName(st.ProjectID)
	}
	// 同名冲突：容器存在但归属不符 → 拒绝
	res, exists, err := dk.InspectContainer(ctx, name)
	if err != nil {
		return envError(stderr, err)
	}
	if exists {
		if res.ProjectID == st.ProjectID && filepath.Clean(res.MountSource) == filepath.Clean(root) {
			// 显式 init 恢复：标签与挂载均匹配，允许接管并记录完整 ID
			st.Container.ID = res.ID
			st.Container.Name = res.Name
			st.Container.ImageID = res.Image
			if err := project.SaveState(root, st); err != nil {
				return envError(stderr, err)
			}
			if res.State != "running" {
				if err := dk.StartContainer(ctx, res.ID); err != nil {
					return envError(stderr, err)
				}
			}
			fmt.Fprintf(stdout, "km init: 接管匹配的同名容器（project=%s container=%s）\n", st.ProjectID, shortID(res.ID))
			return ExitOK
		}
		return envError(stderr, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("容器名 %s 已被占用（km.project=%q，挂载 %q），不接管", name, res.ProjectID, res.MountSource)})
	}

	// 创建新容器；失败只清理本次创建的资源
	fullID, err := dk.CreateContainer(ctx, runtime.ContainerCreateOpts{
		Name: name, ProjectID: st.ProjectID, Image: cfg.Image, ProjectDir: root,
	})
	if err != nil {
		return envError(stderr, err)
	}
	st.Container.ID = fullID
	st.Container.Name = name
	st.Container.ImageID = imageID
	st.Runtime.Endpoint = endpoint
	if err := project.SaveState(root, st); err != nil {
		// 状态写入失败：回滚本次创建的容器，保留原环境
		_ = dk.RemoveContainer(context.Background(), fullID)
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "状态写入失败，已回滚本次创建的容器", Err: err})
	}
	fmt.Fprintf(stdout, "km init: 环境就绪（project=%s container=%s image=%s）\n", st.ProjectID, shortID(fullID), shortID(imageID))
	return ExitOK
}

func writeConfigAtomic(path string, cfg *project.Config) error {
	return project.WriteConfig(path, cfg)
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

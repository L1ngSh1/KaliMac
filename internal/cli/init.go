package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// envError prints a KM_* error on stderr; the caller returns ExitEnv.
func envError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%v\n", err)
	return ExitEnv
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

// initFlags 是 `km init` 的显式参数（合同见 docs/newuser-iteration-plan.md 包 A）。
type initFlags struct {
	image    string // --image：新项目写入配置；已有项目仅允许确认一致
	platform string // --platform：同上；声明缺失的旧配置一律拒绝
}

// parseInitFlags 解析 --image/--platform（两种写法均可），未知参数报用法错误。
func parseInitFlags(rest []string) (*initFlags, int, error) {
	f := &initFlags{}
	take := func(i int, name string) (string, int, error) {
		if i+1 >= len(rest) {
			return "", 0, fmt.Errorf("%s 需要一个值", name)
		}
		return rest[i+1], 2, nil
	}
	for i := 0; i < len(rest); {
		cur := rest[i]
		switch {
		case cur == "--image" || strings.HasPrefix(cur, "--image="):
			if strings.HasPrefix(cur, "--image=") {
				f.image = strings.TrimPrefix(cur, "--image=")
				if f.image == "" {
					return nil, 0, fmt.Errorf("--image 需要一个值")
				}
				i++
				continue
			}
			v, n, err := take(i, "--image")
			if err != nil {
				return nil, 0, err
			}
			f.image = v
			i += n
		case cur == "--platform" || strings.HasPrefix(cur, "--platform="):
			if strings.HasPrefix(cur, "--platform=") {
				f.platform = strings.TrimPrefix(cur, "--platform=")
				if f.platform == "" {
					return nil, 0, fmt.Errorf("--platform 需要一个值")
				}
				i++
				continue
			}
			v, n, err := take(i, "--platform")
			if err != nil {
				return nil, 0, err
			}
			f.platform = v
			i += n
		default:
			return nil, 0, fmt.Errorf("未知参数 %q（仅支持 --image / --platform）", cur)
		}
	}
	if f.image != "" && strings.ContainsAny(f.image, " \t\n") {
		return nil, 0, fmt.Errorf("--image 不能包含空白字符")
	}
	if f.platform != "" && strings.ContainsAny(f.platform, " \t\n") {
		return nil, 0, fmt.Errorf("--platform 不能包含空白字符")
	}
	return f, 0, nil
}

// hostNativePlatform 返回本机原生平台声明（km 为本机构建，GOARCH 即宿主架构）。
func hostNativePlatform() string {
	return "linux/" + goruntime.GOARCH
}

// curatedImageHint：精选工具镜像约定为 kali-mac-min: 前缀（README/指南文档化）。
func curatedImageHint(ref string) string {
	if strings.HasPrefix(ref, "kali-mac-min:") {
		return "精选工具镜像"
	}
	return "非精选镜像（可能不含精选工具集，见 README）"
}

// applyInitFlagsToNewConfig 处理新项目的参数落盘：--image/--platform 覆盖默认值。
// 返回待写入的配置与平台声明。
func applyInitFlagsToNewConfig(f *initFlags) *project.Config {
	cfg := &project.Config{SchemaVersion: project.SupportedSchemaVersion, Image: project.DefaultImage, Platform: hostNativePlatform()}
	if f != nil {
		if f.image != "" {
			cfg.Image = f.image
		}
		if f.platform != "" {
			cfg.Platform = f.platform
		}
	}
	cfg.PlatformDeclared = true
	return cfg
}

// checkFlagsAgainstConfig 校验已有项目上的显式参数：只允许「确认一致」；
// 任何不一致都是冲突——不覆盖配置、不重建容器（合同：已有配置 > 命令行参数）。
func checkFlagsAgainstConfig(f *initFlags, cfg *project.Config) error {
	if f == nil {
		return nil
	}
	if f.image != "" && f.image != cfg.Image {
		return &runtime.Error{Code: runtime.CodeConfigInvalid,
			Msg: fmt.Sprintf("--image %s 与已有配置 %s 不一致；不覆盖配置、不重建容器。如需更换镜像请编辑 .km.json 后重试", f.image, cfg.Image)}
	}
	if f.platform != "" {
		if !cfg.PlatformDeclared {
			return &runtime.Error{Code: runtime.CodeConfigInvalid,
				Msg: fmt.Sprintf(".km.json 未声明 platform（该容器按本机架构创建）；不接受 --platform %s。如需声明请编辑 .km.json 加 platform 字段后重试", f.platform)}
		}
		if f.platform != cfg.Platform {
			return &runtime.Error{Code: runtime.CodeConfigInvalid,
				Msg: fmt.Sprintf("--platform %s 与已有配置 %s 不一致；不覆盖配置、不重建容器。如需变更请编辑 .km.json 后重试", f.platform, cfg.Platform)}
		}
	}
	return nil
}

// createPlatform：参与创建/拉取的平台——仅兑现显式声明；未声明 = native（旧配置
// 的历史行为，绝不伪造声明）。
func createPlatform(cfg *project.Config) string {
	if cfg != nil && cfg.PlatformDeclared {
		return cfg.Platform
	}
	return ""
}

// runInitCommand implements `km init`: idempotent project bootstrap.
// 显式参数：--image <ref> / --platform <p>（新项目写入配置并兑现；已有项目仅允许
// 确认一致，不一致即冲突报错，不覆盖、不重建）。
func runInitCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	flags, _, perr := parseInitFlags(rest)
	if perr != nil {
		return usageError(stderr, "km init: %v", perr)
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

	// 配置：存在则校验（保留原文件），缺失则按参数/默认原子写入
	cfgPath := filepath.Join(wd, project.ConfigFileName)
	var cfg *project.Config
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		cfg, err = project.LoadConfig(cfgPath)
		if err != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeConfigInvalid,
				Msg: "保留原文件；修复后重试", Err: err})
		}
		// 已有项目：显式参数只允许确认一致（不覆盖、不重建）
		if err := checkFlagsAgainstConfig(flags, cfg); err != nil {
			return envError(stderr, err)
		}
	} else {
		cfg = applyInitFlagsToNewConfig(flags)
		if err := project.WriteConfig(cfgPath, cfg); err != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "写入配置失败", Err: err})
		}
	}

	// 镜像：本地缺失时显式拉取（有界），失败保留环境
	imageID, ok, err := dk.ImageID(ctx, cfg.Image)
	if err != nil {
		return envError(stderr, err)
	}
	if !ok {
		fmt.Fprintf(stderr, "km: 镜像 %s 不在本地，开始拉取（有界 %s）…\n", cfg.Image, runtime.PullTimeout)
		if err := dk.PullImage(ctx, cfg.Image, createPlatform(cfg)); err != nil {
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
				// 平台兑现（审查 P1）：声明平台必须与现有容器实际平台一致，
				// 不一致明确拒绝——不静默复用，也不自动重建。
				if verr := verifyActualPlatform(ctx, dk, cfg, res.Image); verr != nil {
					return envError(stderr, verr)
				}
				if res.State != "running" {
					if err := dk.StartContainer(ctx, st.Container.ID); err != nil {
						return envError(stderr, err)
					}
				}
				fmt.Fprintf(stdout, "km init: 复用现有环境（project=%s container=%s image=%s 镜像身份=%s 平台=%s %s）\n",
					st.ProjectID, shortID(st.Container.ID), cfg.Image, shortID(st.Container.ImageID),
					platformDisplay(cfg), curatedImageHint(cfg.Image))
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
			fmt.Fprintf(stdout, "km init: 接管匹配的同名容器（project=%s container=%s image=%s 平台=%s %s）\n",
				st.ProjectID, shortID(res.ID), cfg.Image,
				platformDisplay(cfg), curatedImageHint(cfg.Image))
			return ExitOK
		}
		return envError(stderr, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("容器名 %s 已被占用（km.project=%q，挂载 %q），不接管", name, res.ProjectID, res.MountSource)})
	}

	// 创建新容器；失败只清理本次创建的资源
	fullID, err := dk.CreateContainer(ctx, runtime.ContainerCreateOpts{
		Name: name, ProjectID: st.ProjectID, Image: cfg.Image, ProjectDir: root,
		Platform: createPlatform(cfg),
	})
	createFailed := err != nil
	var recoveredResult runtime.InspectResult
	if createFailed {
		// `docker run` is side-effecting: a timeout or client-side failure does
		// not prove that the daemon did not create the container.  Reconcile by
		// the preallocated unique name under an independent, bounded context;
		// never issue a second create from this invocation.
		res, recovered, rerr := recoverCreateResult(dk, name, st.ProjectID, root, imageID, err)
		if rerr != nil {
			return envError(stderr, rerr)
		}
		if !recovered {
			return envError(stderr, err)
		}
		recoveredResult = res
		fullID = res.ID
		fmt.Fprintf(stderr, "km: 创建命令失败后核实到已创建容器，正在登记（project=%s container=%s）…\n",
			st.ProjectID, shortID(fullID))
	}
	st.Container.ID = fullID
	st.Container.Name = name
	st.Container.ImageID = imageID
	st.Runtime.Endpoint = endpoint
	if err := project.SaveState(root, st); err != nil {
		// 状态写入失败：用独立预算回滚；失败时必须报告“未确认”，
		// 不能把清理命令已发送等价为资源已经消失。
		rctx, cancel := context.WithTimeout(context.Background(), runtime.DefaultManagementTimeout)
		rerr := dk.RemoveContainer(rctx, fullID)
		cancel()
		if rerr != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: fmt.Sprintf("状态写入失败，且容器回滚未确认（project=%s name=%s container=%s）；请修复状态目录后运行 km init 核实",
					st.ProjectID, name, fullID), Err: fmt.Errorf("保存状态: %v；回滚: %w", err, rerr)})
		}
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "状态写入失败，已回滚本次创建的容器", Err: err})
	}
	if createFailed {
		// recovered 仅在 daemon 已创建但原 docker run 报错时为真。
		// 状态先落盘，保证后续失败仍可按完整 ID 恢复。
		if recoveredResult.State != "running" {
			rctx, cancel := context.WithTimeout(context.Background(), runtime.DefaultManagementTimeout)
			serr := dk.StartContainer(rctx, fullID)
			cancel()
			if serr != nil {
				return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
					Msg: fmt.Sprintf("容器已登记但启动状态未确认（container=%s）；请运行 km init 恢复", fullID), Err: serr})
			}
		}
	}
	fmt.Fprintf(stdout, "km init: 环境就绪（project=%s container=%s image=%s 镜像身份=%s 平台=%s %s）\n",
		st.ProjectID, shortID(fullID), cfg.Image, shortID(imageID),
		platformDisplay(cfg), curatedImageHint(cfg.Image))
	return ExitOK
}

// recoverCreateResult reconciles an indeterminate docker run result without
// retrying the side-effecting create.  A matching exact name, ownership label,
// workspace mount and image content ID is required before adoption.
func recoverCreateResult(dk *runtime.Docker, name, projectID, root, imageID string, createErr error) (runtime.InspectResult, bool, error) {
	rctx, cancel := context.WithTimeout(context.Background(), runtime.DefaultManagementTimeout)
	defer cancel()
	res, exists, err := dk.InspectContainer(rctx, name)
	if err != nil {
		return runtime.InspectResult{}, false, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("容器创建结果未知（project=%s name=%s）；创建失败后核验也失败，请勿直接重复创建，运行 km init 重新核实", projectID, name),
			Err: fmt.Errorf("创建: %v；核验: %w", createErr, err)}
	}
	if !exists {
		return runtime.InspectResult{}, false, nil
	}
	if res.Name != name || res.ProjectID != projectID || filepath.Clean(res.MountSource) != filepath.Clean(root) || res.Image != imageID {
		return runtime.InspectResult{}, false, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("创建失败后发现同名容器但身份不匹配（name=%q project=%q mount=%q image=%q），不接管",
				res.Name, res.ProjectID, res.MountSource, res.Image)}
	}
	return res, true, nil
}

// verifyActualPlatform：配置显式声明平台时，核验现有镜像/容器的实际平台一致；
// 不一致明确拒绝（不自动重建）。未声明（旧配置）不检查——保持 native 历史行为。
func verifyActualPlatform(ctx context.Context, dk *runtime.Docker, cfg *project.Config, imageRef string) error {
	plat := createPlatform(cfg)
	if plat == "" || imageRef == "" {
		return nil
	}
	got, err := dk.ImageOSArch(ctx, imageRef)
	if err != nil {
		return err
	}
	if got != plat {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("配置声明平台 %s 与现有容器实际平台 %s 不一致；不自动重建。请按文档流程迁移（km stop 后删除容器再 init，或恢复原配置）", plat, got)}
	}
	return nil
}

// platformDisplay：声明平台原样展示；未声明显式标注（旧配置不伪造声明）。
func platformDisplay(cfg *project.Config) string {
	if cfg != nil && cfg.PlatformDeclared && cfg.Platform != "" {
		return cfg.Platform
	}
	return "未声明(本机架构)"
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

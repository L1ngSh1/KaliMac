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
	"kalimac/internal/session"
)

// doctorBudget caps the total wall time of one doctor run. Each docker
// query additionally carries its own (shorter) management timeout.
const doctorBudget = 45 * time.Second

// runDoctorCommand validates argv, bounds the whole run, and executes the
// read-only doctor.
func runDoctorCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km doctor 不接受参数")
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	ctx, cancel := context.WithTimeout(ctx, doctorBudget)
	defer cancel()
	return RunDoctor(ctx, wd, stdout, dk)
}

// reporter accumulates doctor findings and the final tally.
type reporter struct {
	w              io.Writer
	ok, warn, fail int
}

func (r *reporter) line(format string, a ...any) {
	fmt.Fprintf(r.w, format+"\n", a...)
}

func (r *reporter) item(mark, format string, a ...any) {
	switch mark {
	case "OK":
		r.ok++
	case "警告":
		r.warn++
	case "失败":
		r.fail++
	}
	r.line("  [%s] %s", mark, fmt.Sprintf(format, a...))
}

// RunDoctor performs the read-only checks. Daemon queries only happen after
// the effective endpoint has been resolved client-side and verified local;
// on a remote endpoint or a runtime-identity drift the dependent sections
// are skipped entirely. Environment problems are findings on stdout, not
// failures of the doctor itself, so the exit status is 0 unless km cannot
// inspect at all.
func RunDoctor(ctx context.Context, dir string, stdout io.Writer, dk *runtime.Docker) int {
	r := &reporter{w: stdout}
	r.line("km doctor（只读检查，不做任何变更）")
	r.line("")

	// 1. platform (no docker)
	r.line("平台:")
	r.item("OK", "本机: %s/%s (%s)", goruntime.GOOS, goruntime.GOARCH, macVersion(ctx, dk))

	// 2. project config + local state (client-side files only)
	cfg, st, root := checkProjectConfig(r, dir)

	// 3. effective endpoint + engine (daemon queries start here)
	r.line("运行时 (Docker):")
	ep, ok := checkEndpointAndEngine(r, ctx, dk)

	// 4. recorded runtime identity vs current engine
	if ok && st != nil {
		ok = checkRuntimeIdentity(r, st, ep)
	}

	// 5. container ownership + image content identity
	if ok && cfg != nil {
		checkContainerAndImage(r, ctx, dk, root, cfg, st)
	}

	r.line("")
	r.line("结论: %d 通过, %d 警告, %d 失败", r.ok, r.warn, r.fail)
	return ExitOK
}

func checkProjectConfig(r *reporter, dir string) (*project.Config, *project.State, string) {
	r.line("项目:")
	root, found, err := project.FindConfig(dir)
	if err != nil {
		r.item("失败", "查找 .km.json 失败: %v", err)
		return nil, nil, ""
	}
	if !found {
		r.item("警告", "%s: 从当前目录向上未找到 .km.json；请在项目根运行 km init", runtime.CodeProjectMissing)
		return nil, nil, ""
	}
	// 与 init/run 同一规范化：挂载源与项目身份比较前必须解析符号链接，
	// 否则 /tmp 与 /private/tmp 等价写法造成挂载归属假冲突。
	root = project.CanonicalPath(root)
	if canDir := project.CanonicalPath(dir); canDir != root {
		r.item("警告", "当前位于项目子目录，项目根: %s", root)
	} else {
		r.line("  项目根: %s", root)
	}

	cfg, cerr := project.LoadConfig(filepath.Join(root, project.ConfigFileName))
	if cerr != nil {
		r.item("失败", "%s: %v", runtime.CodeConfigInvalid, cerr)
		return nil, nil, root
	}
	r.item("OK", "配置: schema=%d image=%s platform=%s", cfg.SchemaVersion, cfg.Image, cfg.Platform)

	st, serr := project.LoadState(root)
	switch {
	case os.IsNotExist(serr):
		r.item("警告", "本机状态缺失（.km/state.json 不存在）：配置存在但从未完成 init")
		return cfg, nil, root
	case serr != nil:
		r.item("失败", "%s: %v", runtime.CodeStateInvalid, serr)
		return cfg, nil, root
	}
	detail := fmt.Sprintf("project_id=%s 容器名=%s context=%s", st.ProjectID, st.Container.Name, st.Runtime.Context)
	if st.Container.ID != "" {
		detail += fmt.Sprintf(" 容器ID=%s…", shortID(st.Container.ID))
	}
	r.line("  本机状态: %s", detail)
	return cfg, st, root
}

// checkEndpointAndEngine resolves the effective endpoint using client-side
// commands only; on a local endpoint it pins the endpoint for all subsequent
// child docker processes and checks the engine. ok=false means every
// engine-dependent section must be skipped.
func checkEndpointAndEngine(r *reporter, ctx context.Context, dk *runtime.Docker) (runtime.EndpointInfo, bool) {
	ep, err := dk.EffectiveEndpoint(ctx)
	if err != nil {
		r.item("失败", "解析 Docker endpoint 失败: %v", err)
		return runtime.EndpointInfo{}, false
	}
	if dh, dc := os.Getenv("DOCKER_HOST"), os.Getenv("DOCKER_CONTEXT"); dh != "" && dc != "" {
		r.item("警告", "DOCKER_HOST 与 DOCKER_CONTEXT 同时设置；DOCKER_HOST 生效（endpoint %s）", ep.Endpoint)
	}
	if !runtime.IsLocalEndpoint(ep.Endpoint) {
		r.item("失败", "%s: 有效 endpoint %s（来源 %s）不是本地引擎；仅支持本地引擎，已跳过所有引擎查询", runtime.CodeEndpointRemote, ep.Endpoint, ep.Source)
		return ep, false
	}
	dk.EndpointOverride = ep.Endpoint
	source := fmt.Sprintf("来源 %s", ep.Source)
	if ep.Context != "" {
		source += fmt.Sprintf("，context=%s", ep.Context)
	}
	r.item("OK", "有效 endpoint: %s（%s，本地）", ep.Endpoint, source)

	ver, err := dk.Version(ctx)
	if err != nil {
		if runtime.IsMissing(err) {
			r.item("失败", "%s: 找不到 docker CLI", runtime.CodeRuntimeMissing)
		} else {
			r.item("失败", "%s: 引擎不可达：%v", runtime.CodeRuntimeOffline, err)
		}
		return ep, false
	}
	r.item("OK", "client=%s server=%s (%s/%s)", ver.Client, ver.Server, ver.ServerOS, ver.ServerArc)
	return ep, true
}

// checkRuntimeIdentity compares the endpoint recorded in local state with
// the effective endpoint. On drift the container/image sections are skipped:
// the recorded container belongs to a different engine.
func checkRuntimeIdentity(r *reporter, st *project.State, ep runtime.EndpointInfo) bool {
	if st.Runtime.Endpoint == "" {
		return true
	}
	if st.Runtime.Endpoint != ep.Endpoint {
		r.item("失败", "%s: 本机状态记录的 endpoint %s 与当前有效 endpoint %s 不一致；跳过容器与镜像检查（显式 init 可迁移本机身份）", runtime.CodeRuntimeMismatch, st.Runtime.Endpoint, ep.Endpoint)
		return false
	}
	r.item("OK", "运行时身份与项目记录一致")
	return true
}

func checkContainerAndImage(r *reporter, ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State) {
	r.line("容器与镜像:")
	if st != nil {
		checkContainerOwnership(r, ctx, dk, root, st)
	}
	checkImage(r, ctx, dk, cfg, st)
}

// checkContainerOwnership verifies the container against the recorded full
// ID, expected name, project label and workspace mount. A same-name rebuild
// is detected and never adopted.
func checkContainerOwnership(r *reporter, ctx context.Context, dk *runtime.Docker, root string, st *project.State) {
	if st.Container.ID == "" {
		r.item("警告", "本机状态缺少容器 ID（旧版状态），无法核验容器归属；请重新 init 重建本机状态")
		return
	}
	res, exists, err := dk.InspectContainer(ctx, st.Container.ID)
	if err != nil {
		r.item("警告", "容器检查失败: %v", err)
		return
	}
	if !exists {
		if byName, nameExists, nerr := dk.InspectContainer(ctx, st.Container.Name); nerr == nil && nameExists {
			r.item("失败", "%s: 容器被同名重建：记录 ID %s…，现存同名容器 ID %s…；不接管", runtime.CodeContainerConflict, shortID(st.Container.ID), shortID(byName.ID))
		} else {
			r.item("警告", "记录的容器（ID %s…）不存在（可能被外部删除）；显式 init 可恢复", shortID(st.Container.ID))
		}
		return
	}
	r.item("OK", "容器: %s（%s…，%s，按记录 ID 查找）", res.Name, shortID(res.ID), res.State)
	// inspect 返回的 ID 必须与记录完全一致（按 ID 查询本应如此；
	// 显式比对防止任何按名回退或实现疏漏造成的身份错配）。
	if res.ID != st.Container.ID {
		r.item("失败", "%s: inspect 返回的容器 ID 与记录不一致（返回 %s…，记录 %s…）", runtime.CodeContainerConflict, shortID(res.ID), shortID(st.Container.ID))
	}
	if res.Name != st.Container.Name {
		r.item("失败", "%s: 容器名称 %q 与记录 %q 不一致", runtime.CodeContainerConflict, res.Name, st.Container.Name)
	}
	if res.ProjectID != st.ProjectID {
		r.item("失败", "%s: 标签 km.project=%q 与本机状态 %q 不一致，不接管", runtime.CodeContainerConflict, res.ProjectID, st.ProjectID)
	} else {
		r.item("OK", "标签归属: km.project=%s 匹配", st.ProjectID)
	}
	// 挂载源与项目根都做符号链接解析后比较：init 以 CanonicalPath 写入
	// docker -v，而 doctor 的项目根来自调用方 cwd 的写法，/tmp 与
	// /private/tmp 这类等价拼写必须判为同一目录。
	if project.CanonicalPath(res.MountSource) == root {
		r.item("OK", "挂载: %s => /workspace", root)
	} else {
		r.item("失败", "%s: /workspace 挂载源为 %q，预期 %q", runtime.CodeContainerConflict, res.MountSource, root)
	}
	if st.Container.ImageID != "" && res.Image != "" {
		if res.Image == st.Container.ImageID {
			r.item("OK", "容器镜像内容与项目记录一致")
		} else {
			r.item("失败", "%s: 容器实际镜像 %s… 与项目记录 %s… 不一致", runtime.CodeContainerConflict, shortID(res.Image), shortID(st.Container.ImageID))
		}
	}
	reportContainerSessions(r, ctx, dk, res.ID)
}

// reportContainerSessions 报告容器内会话状态（只读；R2 的可见性部分）。
// 检查本身失败必须计入统计（警告），不得降级为不计数的输出行；
// exit 127 = km-ctl 不在容器内（init 后首次 run/shell 之前属预期状态）。
func reportContainerSessions(r *reporter, ctx context.Context, dk *runtime.Docker, containerID string) {
	var ctl *session.DockerController = newSessionController(dk.EndpointOverride)
	out, errStr, exitCode, err := ctl.Sessions(ctx, containerID)
	active, stale, parseOK := session.ParseSessions(out)
	switch {
	case err != nil:
		r.item("警告", "会话检查失败: %v", err)
	case exitCode == 127:
		r.item("警告", "会话脚本未安装（init 后首次 run/shell 时安装；此前无法检查容器内会话）")
	case exitCode != 0 || !parseOK:
		r.item("警告", "会话检查失败（exit=%d，stderr: %s；输出不可解析）", exitCode, strings.TrimSpace(errStr))
	case len(active) == 0 && len(stale) == 0:
		r.item("OK", "容器内无活跃会话")
	default:
		if len(active) > 0 {
			r.item("警告", "容器内活跃会话: %v（若宿主已无对应 km 进程，可显式 cancel 清理）", active)
		}
		if len(stale) > 0 {
			r.item("警告", "容器内遗留会话目录: %v（组已空，下次执行自动清扫）", stale)
		}
	}
}

// checkImage verifies the declared image reference exists locally and, when
// the project records a content ID, that the tag still points at it.
func checkImage(r *reporter, ctx context.Context, dk *runtime.Docker, cfg *project.Config, st *project.State) {
	if cfg == nil {
		return
	}
	id, exists, err := dk.ImageID(ctx, cfg.Image)
	if err != nil {
		r.item("警告", "镜像检查失败: %v", err)
		return
	}
	if !exists {
		r.item("警告", "镜像 %s 不在本地（运行 km init 会自动拉取）", cfg.Image)
		return
	}
	if st != nil && st.Container.ImageID != "" && id != st.Container.ImageID {
		r.item("警告", "镜像标签 %s 当前内容 %s… 与项目记录 %s… 不一致（漂移）；镜像更新应走显式流程", cfg.Image, shortID(id), shortID(st.Container.ImageID))
		return
	}
	r.item("OK", "镜像: %s", id)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func macVersion(ctx context.Context, dk *runtime.Docker) string {
	if goruntime.GOOS != "darwin" {
		return "非 macOS"
	}
	bin, err := dk.Exec.LookPath("sw_vers")
	if err != nil {
		return "macOS（版本未知）"
	}
	stdout, _, err := dk.Exec.Run(ctx, bin, "-productVersion")
	if err != nil {
		return "macOS（版本未知）"
	}
	return "macOS " + strings.TrimRight(string(stdout), "\n")
}

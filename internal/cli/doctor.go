package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

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

// runDoctorCommand validates argv and executes the read-only doctor.
func runDoctorCommand(ctx context.Context, rest []string, stdout, stderr io.Writer) int {
	if len(rest) > 0 {
		return usageError(stderr, "km doctor 不接受参数")
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	return RunDoctor(ctx, wd, stdout, runtimeDocker())
}

func runtimeDocker() *runtime.Docker {
	return &runtime.Docker{Exec: runtime.CommandExecutor{}}
}

// RunDoctor performs the read-only checks. Environment problems are findings
// reported on stdout, not failures of the doctor itself, so the exit status
// is 0 unless km cannot inspect at all.
func RunDoctor(ctx context.Context, dir string, stdout io.Writer, dk *runtime.Docker) int {
	r := &reporter{w: stdout}
	r.line("km doctor（只读检查，不做任何变更）")
	r.line("")

	// 1. platform
	r.line("平台:")
	r.item("OK", "本机: %s/%s (%s)", goruntime.GOOS, goruntime.GOARCH, macVersion(ctx, dk))

	// 2. docker CLI + engine
	r.line("运行时 (Docker):")
	ver, err := dk.Version(ctx)
	if err != nil {
		if runtime.IsMissing(err) {
			r.item("失败", "%s: 找不到 docker CLI", runtime.CodeRuntimeMissing)
		} else {
			r.item("失败", "%s: 引擎不可达：%v", runtime.CodeRuntimeOffline, err)
		}
	} else {
		r.item("OK", "client=%s server=%s (%s/%s)", ver.Client, ver.Server, ver.ServerOS, ver.ServerArc)
	}

	// 3. context / endpoint
	r.line("Context:")
	if name, cerr := dk.ContextShow(ctx); cerr == nil {
		if host := os.Getenv("DOCKER_HOST"); host != "" && !isLocalEndpoint(host) {
			r.item("失败", "%s: DOCKER_HOST=%s 是远程 endpoint；v0.1 只使用本地引擎", runtime.CodeEndpointRemote, host)
		} else if host != "" {
			r.item("警告", "context=%s，但 DOCKER_HOST 覆盖了默认 endpoint", name)
		} else {
			r.item("OK", "context=%s（DOCKER_HOST 未设置，使用本地默认）", name)
		}
	} else {
		r.item("警告", "无法读取当前 context: %v", cerr)
	}

	// 4. project config, state, container, image
	r.line("项目:")
	checkProject(r, ctx, dk, dir)

	r.line("")
	r.line("结论: %d 通过, %d 警告, %d 失败", r.ok, r.warn, r.fail)
	return ExitOK
}

func checkProject(r *reporter, ctx context.Context, dk *runtime.Docker, dir string) {
	root, found, ferr := project.FindConfig(dir)
	switch {
	case ferr != nil:
		r.item("失败", "查找 .km.json 失败: %v", ferr)
		return
	case !found:
		r.item("警告", "%s: 从当前目录向上未找到 .km.json；init 将在 P2 提供", runtime.CodeProjectMissing)
		return
	}
	if abs, aerr := filepath.Abs(dir); aerr == nil && root != abs {
		r.item("警告", "当前位于项目子目录，项目根: %s", root)
	} else {
		r.line("  项目根: %s", root)
	}

	cfg, cerr := project.LoadConfig(filepath.Join(root, project.ConfigFileName))
	if cerr != nil {
		r.item("失败", "%s: %v", runtime.CodeConfigInvalid, cerr)
		return
	}
	r.item("OK", "配置: schema=%d image=%s platform=%s", cfg.SchemaVersion, cfg.Image, cfg.Platform)

	st, serr := project.LoadState(root)
	switch {
	case os.IsNotExist(serr):
		r.item("警告", "本机状态缺失（.km/state.json 不存在）：配置存在但从未完成 init")
	case serr != nil:
		r.item("失败", "%s: %v", runtime.CodeStateInvalid, serr)
	default:
		r.line("  本机状态: project_id=%s 容器名=%s context=%s", st.ProjectID, st.Container.Name, st.Runtime.Context)
		inspectContainer(r, ctx, dk, root, st)
	}

	if id, ok, ierr := dk.ImageID(ctx, cfg.Image); ierr != nil {
		r.item("警告", "镜像检查失败: %v", ierr)
	} else if !ok {
		r.item("警告", "镜像 %s 不在本地（首次准备需要下载，P2 由 init 处理）", cfg.Image)
	} else {
		r.item("OK", "镜像: %s", id)
	}
}

func inspectContainer(r *reporter, ctx context.Context, dk *runtime.Docker, root string, st *project.State) {
	res, exists, err := dk.InspectContainer(ctx, st.Container.Name)
	if err != nil {
		r.item("警告", "容器检查失败: %v", err)
		return
	}
	if !exists {
		r.item("警告", "容器 %s 不存在（可能被外部删除）；P2 将通过显式 init 恢复", st.Container.Name)
		return
	}
	r.item("OK", "容器: %s (%s…, %s)", res.Name, shortID(res.ID), res.State)
	if res.ProjectID != st.ProjectID {
		r.item("失败", "%s: 容器标签 km.project=%q 与本机状态 %q 不一致，不接管", runtime.CodeContainerConflict, res.ProjectID, st.ProjectID)
	} else {
		r.item("OK", "标签归属: km.project=%s 匹配", st.ProjectID)
	}
	if res.MountSource == root {
		r.item("OK", "挂载: %s => /workspace", root)
	} else {
		r.item("失败", "%s: /workspace 挂载源为 %q，预期 %q", runtime.CodeContainerConflict, res.MountSource, root)
	}
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

func isLocalEndpoint(host string) bool {
	lower := strings.ToLower(host)
	return strings.HasPrefix(lower, "unix://") || strings.HasPrefix(lower, "npipe://")
}

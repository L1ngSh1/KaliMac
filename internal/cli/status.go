package cli

// km status：只读状态聚合与结构化输出（合同见 docs/newuser-iteration-plan.md 包 B）。
//   - 人类输出与 JSON 由同一 projectStatus 渲染，天然一致；
//   - 只读：不启动容器、不引导脚本、不清扫登记、不写任何文件；
//   - state 枚举固定，查询失败/输出异常一律 unknown（绝不显示为无会话/正常）；
//   - 退出码：0=状态判定完成；1=unknown/identity_conflict；2=用法错误。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// 状态枚举（JSON state 字段取值，机器可比较）。
const (
	statusUninitialized    = "uninitialized"
	statusConfigIncomplete = "config_incomplete"
	statusContainerMissing = "container_missing"
	statusContainerStopped = "container_stopped"
	statusRunningIdle      = "running_idle"
	statusRunningActive    = "running_session_active"
	statusIdentityConflict = "identity_conflict"
	statusUnknown          = "unknown"
)

type statusProject struct {
	Root             string `json:"root"`
	Image            string `json:"image"`
	Platform         string `json:"platform"`
	PlatformDeclared bool   `json:"platform_declared"`
	ContainerID      string `json:"container_id,omitempty"`
}

type statusContainer struct {
	State    string `json:"state"`
	Name     string `json:"name"`
	ImageSHA string `json:"image_sha,omitempty"`
}

type statusSessions struct {
	Active []string `json:"active"`
	Stale  []string `json:"stale"`
}

type statusError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

type projectStatus struct {
	SchemaVersion int              `json:"schema_version"`
	State         string           `json:"state"`
	Project       *statusProject   `json:"project,omitempty"`
	Container     *statusContainer `json:"container,omitempty"`
	Sessions      *statusSessions  `json:"sessions,omitempty"`
	Advice        string           `json:"advice"`
	Error         *statusError     `json:"error,omitempty"`
}

func runStatusCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	jsonMode := false
	for _, a := range rest {
		if a == "--json" {
			jsonMode = true
			continue
		}
		return usageError(stderr, "km status 仅支持 --json，不接受位置参数")
	}
	ps := collectStatus(ctx, dk)
	if jsonMode {
		raw, merr := json.MarshalIndent(ps, "", "  ")
		if merr != nil {
			return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "状态序列化失败", Err: merr})
		}
		stdout.Write(append(raw, '\n'))
		return statusExit(ps)
	}
	renderStatusHuman(ps, stdout)
	return statusExit(ps)
}

func statusExit(ps projectStatus) int {
	if ps.State == statusUnknown || ps.State == statusIdentityConflict {
		return ExitEnv
	}
	return ExitOK
}

// statusFail 构造 unknown/冲突终态（exit 1）。
func statusFail(state, code, msg string) projectStatus {
	return projectStatus{
		SchemaVersion: 1,
		State:         state,
		Advice:        "先解决错误再操作；km doctor 提供完整诊断",
		Error:         &statusError{Code: code, Msg: msg},
	}
}

// collectStatus 按阶段聚合：无配置 → 配置未完成 → 引擎/容器身份 → 运行状态 → 会话。
// 任何阶段失败都收敛为明确的 state + error，绝不静默当作正常或无会话。
func collectStatus(ctx context.Context, dk *runtime.Docker) projectStatus {
	ps := projectStatus{SchemaVersion: 1}
	wd, err := os.Getwd()
	if err != nil {
		return statusFail(statusUnknown, "KM_ENV", fmt.Sprintf("无法获取当前目录: %v", err))
	}
	root, found, ferr := project.FindConfig(wd)
	if ferr != nil {
		return statusFail(statusUnknown, "KM_ENV", fmt.Sprintf("查找项目失败: %v", ferr))
	}
	if !found {
		ps.State = statusUninitialized
		ps.Advice = "当前目录不属于任何 km 项目；运行 km init 初始化（可选 --image 指定镜像）"
		return ps
	}
	cfg, cerr := project.LoadConfig(root + string(os.PathSeparator) + project.ConfigFileName)
	if cerr != nil {
		return statusFail(statusUnknown, runtime.CodeConfigInvalid, "配置读取失败: "+cerr.Error())
	}
	ps.Project = &statusProject{
		Root:             root,
		Image:            cfg.Image,
		Platform:         cfg.Platform,
		PlatformDeclared: cfg.PlatformDeclared,
	}
	st, serr := project.LoadState(root)
	if serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			ps.State = statusConfigIncomplete
			ps.Advice = "配置已存在但初始化未完成；运行 km init 完成初始化"
			return ps
		}
		return statusFail(statusUnknown, runtime.CodeStateInvalid, "状态文件损坏: "+serr.Error())
	}
	ps.Project.ContainerID = st.Container.ID

	ep, err := resolveEngine(ctx, dk)
	if err != nil {
		state := statusUnknown
		code := runtime.CodeRuntimeMissing
		var rerr *runtime.Error
		if errors.As(err, &rerr) {
			code = rerr.Code
			if rerr.Code == runtime.CodeEndpointRemote {
				state = statusIdentityConflict
			}
		}
		return statusFail(state, code, err.Error())
	}
	if verr := verifyProjectEngine(st, ep.Endpoint); verr != nil {
		return statusFail(statusIdentityConflict, runtime.CodeRuntimeMismatch, verr.Error())
	}

	res, exists, ierr := dk.InspectContainer(ctx, st.Container.ID)
	if ierr != nil {
		state := statusUnknown
		code := runtime.CodeRuntimeOffline
		var rerr *runtime.Error
		if errors.As(ierr, &rerr) {
			code = rerr.Code
		}
		return statusFail(state, code, "容器查询失败: "+ierr.Error())
	}
	if !exists {
		// 记录 ID 消失 ≠ 一定可恢复：同名容器可能已被他项目重建（同名重建），
		// 此时建议「km init 恢复」必然撞 KM_CONTAINER_CONFLICT——按合同归类为
		// 身份冲突并阻止误导性建议。
		if byName, exists2, err2 := dk.InspectContainer(ctx, st.Container.Name); err2 == nil && exists2 {
			return statusFail(statusIdentityConflict, runtime.CodeContainerConflict,
				fmt.Sprintf("记录的容器不存在，同名容器 %s 已被重建（project=%q，挂载 %q）；不接管。运行 km doctor 复核",
					st.Container.Name, byName.ProjectID, byName.MountSource))
		}
		ps.State = statusContainerMissing
		ps.Advice = "记录的容器不存在（可能被外部删除）；运行 km init 恢复"
		return ps
	}
	if verr := verifyContainerIdentity(st, res, root); verr != nil {
		return statusFail(statusIdentityConflict, runtime.CodeContainerConflict, verr.Error())
	}
	ps.Container = &statusContainer{State: res.State, Name: res.Name, ImageSHA: res.Image}

	if res.State != "running" {
		ps.State = statusContainerStopped
		ps.Sessions = &statusSessions{}
		ps.Advice = "容器已停止（数据保留）；下次 km run/shell 自动启动，km stop 幂等"
		return ps
	}

	ctl := newSessionController(ep.Endpoint)
	sOut, sErrStr, sExit, serr2 := ctl.Sessions(ctx, st.Container.ID)
	switch {
	case serr2 != nil:
		state := statusUnknown
		code := runtime.CodeSessionUnknown
		var rerr *runtime.Error
		if errors.As(serr2, &rerr) {
			code = rerr.Code
		}
		return statusFail(state, code, "会话状态查询失败: "+serr2.Error())
	case sExit != 0 && sExit != 126 && sExit != 127:
		return statusFail(statusUnknown, runtime.CodeSessionUnknown,
			fmt.Sprintf("会话状态查询退出码 %d（stderr: %s）", sExit, strings.TrimSpace(sErrStr)))
	}
	active, stale, parseOK := session.ParseSessions(sOut)
	if !parseOK {
		// 某些 docker 版本把 OCI exec 失败写到 stdout 且退出码不可靠——
		// km-ctl 缺失（脚本从未安装）必须识别为信息态，不得报 unknown。
		combined := sOut + sErrStr
		if sessionScriptMissing(combined) {
			ps.State = statusRunningIdle
			ps.Advice = "容器运行中；会话脚本尚未安装（首次 run/shell 时安装）"
			return ps
		}
		return statusFail(statusUnknown, runtime.CodeSessionUnknown,
			fmt.Sprintf("会话状态输出异常（%q）", sOut))
	}
	ps.Sessions = &statusSessions{Active: active, Stale: stale}
	if sExit == 126 || sExit == 127 {
		ps.State = statusRunningIdle
		ps.Advice = "容器运行中；会话脚本尚未安装（首次 run/shell 时安装）"
		return ps
	}
	if len(active) > 0 {
		ps.State = statusRunningActive
		ps.Advice = "有会话在使用中（可能是交互 shell 或运行中任务）；km sessions 查看详情，确认后 km cancel <id> 可显式结束"
		return ps
	}
	ps.State = statusRunningIdle
	if len(stale) > 0 {
		ps.Advice = "容器运行中、无活跃会话；组空遗留登记将随下次执行自动清扫"
	} else {
		ps.Advice = "容器运行中、无会话；可直接 km run/shell，或 km stop 停止容器"
	}
	return ps
}

// sessionScriptMissing 判断 km-ctl 查询的输出是否表明脚本从未安装
// （OCI exec 失败 + 指向 km-ctl 路径的 no such file）。
func sessionScriptMissing(out string) bool {
	return strings.Contains(out, session.CtlScriptPath) &&
		(strings.Contains(out, "no such file") ||
			strings.Contains(out, "OCI runtime exec failed") ||
			strings.Contains(out, "executable file not found"))
}

func renderStatusHuman(ps projectStatus, w io.Writer) {
	w.Write([]byte("km status（只读）\n"))
	fmt.Fprintf(w, "状态:   %s\n", ps.State)
	if ps.Project != nil {
		decl := "已声明"
		if !ps.Project.PlatformDeclared {
			decl = "未声明"
		}
		fmt.Fprintf(w, "项目:   %s（image=%s platform=%s[%s]）\n", ps.Project.Root, ps.Project.Image, ps.Project.Platform, decl)
	}
	if ps.Container != nil {
		fmt.Fprintf(w, "容器:   %s %s（镜像 %s…）\n", ps.Container.State, ps.Container.Name, shortID(ps.Container.ImageSHA))
	}
	if ps.Sessions != nil {
		fmt.Fprintf(w, "会话:   活跃 %d / 组空遗留 %d\n", len(ps.Sessions.Active), len(ps.Sessions.Stale))
	} else if ps.State == statusContainerStopped {
		fmt.Fprintf(w, "会话:   未查询（容器未运行）\n")
	}
	if ps.Error != nil {
		fmt.Fprintf(w, "错误:   %s（%s）\n", ps.Error.Msg, ps.Error.Code)
	}
	fmt.Fprintf(w, "建议:   %s\n", ps.Advice)
}

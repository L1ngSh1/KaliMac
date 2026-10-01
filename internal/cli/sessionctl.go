package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// newSessionController 是测试注入点：生产返回固定 endpoint 的控制器。
var newSessionController = func(endpoint string) *session.DockerController {
	return &session.DockerController{Endpoint: endpoint}
}

// newSessionManager 是测试注入点：生产返回固定 endpoint 的会话管理器
// （与核验用同一 Controller，取消路径共享固定连接信息）。
var newSessionManager = func(endpoint string, diag io.Writer, ctl *session.DockerController) *session.Manager {
	return &session.Manager{
		Starter:       &session.ExecStarter{Env: []string{"DOCKER_HOST=" + endpoint}},
		Controller:    ctl,
		Diag:          diag,
		SkipBootstrap: true,
	}
}

// verifyNoActiveSession 核验容器内会话状态（M0 语义）：
// 查询失败/退出码非 0/输出解析失败 → KM_SESSION_UNKNOWN 阻断；
// 活跃会话 → KM_SESSION_ACTIVE 阻断；组空遗留 → 清扫（失败仅告警）。
// run 与 shell 共用，保证两条入口的阻断语义一致。
func verifyNoActiveSession(ctx context.Context, ctl *session.DockerController, containerID string, stderr io.Writer) error {
	return verifySessionGate(ctx, ctl, containerID, stderr, true)
}

// verifySessionGate 是 verifyNoActiveSession 的可配置版本：sweep=false 时
// 只查询不清扫（env 预览等必须无副作用的路径）。阻断语义完全一致。
func verifySessionGate(ctx context.Context, ctl *session.DockerController, containerID string, stderr io.Writer, sweep bool) error {
	sOut, sErrStr, sExit, serr := ctl.Sessions(ctx, containerID)
	switch {
	case serr != nil:
		return &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: "容器内会话状态查询失败，无法确认是否仍有任务在运行；请人工检查后重试", Err: serr}
	case sExit != 0:
		return &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("容器内会话状态查询退出码 %d（stderr: %s）；请人工检查后重试", sExit, strings.TrimSpace(sErrStr))}
	}
	active, stale, parseOK := session.ParseSessions(sOut)
	if !parseOK {
		return &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("容器内会话状态输出异常（%q）；请人工检查后重试", strings.TrimSpace(sOut))}
	}
	if len(active) > 0 {
		return &runtime.Error{Code: runtime.CodeSessionActive,
			Msg: fmt.Sprintf("容器内存在活跃会话 %v（疑似宿主中断遗留，任务可能仍在运行）；运行 km sessions 查看详情，确认后 km cancel <会话ID> 显式清理，再重试", active)}
	}
	if sweep {
		if _, swErrStr, swExit, swErr := ctl.Sweep(ctx, containerID); swErr != nil || swExit != 0 {
			fmt.Fprintf(stderr, "km: 遗留会话清扫未完成（exit=%d, %s）；不影响本次执行\n", swExit, strings.TrimSpace(swErrStr))
		}
	} else if len(stale) > 0 {
		fmt.Fprintf(stderr, "km: 容器内存在遗留（STALE）会话 %v；不阻断，执行前将清扫\n", stale)
	}
	return nil
}

// resolveEngine 解析有效 endpoint、拒绝远程引擎并为 runtime.Docker 固定。
func resolveEngine(ctx context.Context, dk *runtime.Docker) (runtime.EndpointInfo, error) {
	ep, err := dk.EffectiveEndpoint(ctx)
	if err != nil {
		return ep, err
	}
	if !runtime.IsLocalEndpoint(ep.Endpoint) {
		return ep, &runtime.Error{Code: runtime.CodeEndpointRemote,
			Msg: fmt.Sprintf("有效 endpoint %s（来源 %s）不是本地引擎；仅支持本地引擎", ep.Endpoint, ep.Source)}
	}
	dk.EndpointOverride = ep.Endpoint
	return ep, nil
}

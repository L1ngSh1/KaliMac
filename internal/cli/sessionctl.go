package cli

import (
	"context"
	"fmt"
	"io"

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

// resolveEngine 解析有效 endpoint、拒绝远程引擎并为 runtime.Docker 固定。
func resolveEngine(ctx context.Context, dk *runtime.Docker) (runtime.EndpointInfo, error) {
	ep, err := dk.EffectiveEndpoint(ctx)
	if err != nil {
		return ep, err
	}
	if !runtime.IsLocalEndpoint(ep.Endpoint) {
		return ep, &runtime.Error{Code: runtime.CodeEndpointRemote,
			Msg: fmt.Sprintf("有效 endpoint %s（来源 %s）不是本地引擎；v0.2 只使用本地引擎", ep.Endpoint, ep.Source)}
	}
	dk.EndpointOverride = ep.Endpoint
	return ep, nil
}

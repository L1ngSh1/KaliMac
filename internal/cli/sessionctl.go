package cli

import (
	"context"
	"fmt"
	"strings"

	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// newSessionController 是测试注入点：生产返回固定 endpoint 的控制器。
var newSessionController = func(endpoint string) *session.DockerController {
	return &session.DockerController{Endpoint: endpoint}
}

// activeSessionIDs 从 km-ctl sessions 输出解析活跃会话 ID。
func activeSessionIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "ACTIVE "); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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

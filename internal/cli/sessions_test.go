package cli

// km sessions / km cancel <id> 的单元验收（行为合同见
// docs/session-recovery-plan.md「S2 合同冻结结论」）。
// 归属门禁用 doctorFake 的健康栈 fake；容器内 km-ctl 用 RunFn 注入。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

const (
	sessA = "saaaaaaaaaaaaaaaa" // 合法格式：s + 16 hex
	sessB = "sbbbbbbbbbbbbbbbb"
)

// sessFake 控制器：sessions 按脚本应答；cancel 按 map 应答（缺省 0）。
func sessFake(t *testing.T, root, sessionsOut string, sessionsExit int, sessionsErr error, cancelCodes map[string]int) *runtime.Docker {
	t.Helper()
	// 门禁按 loadProjectStack 的规范化结果比较挂载源，fake 必须对称
	root = project.CanonicalPath(root)
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController {
		return &session.DockerController{RunFn: func(_ context.Context, _ []byte, args []string) (string, string, int, error) {
			if len(args) >= 4 && args[3] == "sessions" {
				return sessionsOut, "", sessionsExit, sessionsErr
			}
			if len(args) >= 5 && args[3] == "cancel" {
				sid := args[4]
				if code, ok := cancelCodes[sid]; ok {
					return "", "", code, nil
				}
				return "", "", 0, nil
			}
			return "", "", 0, nil
		}}
	}
	t.Cleanup(func() { newSessionController = old })
	return &runtime.Docker{Exec: doctorFake(healthyContainerOut(root))}
}

func runSessions(t *testing.T, root string, dk *runtime.Docker, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := runSessionsCommand(context.Background(), args, &out, &errb, dk)
	return code, out.String(), errb.String()
}

func runCancel(t *testing.T, root string, dk *runtime.Docker, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := runCancelCommand(context.Background(), args, &out, &errb, dk)
	return code, out.String(), errb.String()
}

func TestSessionsEmpty(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	dk := sessFake(t, root, "", 0, nil, nil)
	code, out, _ := runSessions(t, root, dk)
	if code != ExitOK || !strings.Contains(out, "当前项目无会话") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestSessionsListsActiveAndStale(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	out0 := fmt.Sprintf("ACTIVE %s\nSTALE %s\n", sessA, sessB)
	dk := sessFake(t, root, out0, 0, nil, nil)
	code, out, errb := runSessions(t, root, dk)
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"ACTIVE " + sessA, "STALE " + sessB} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errb, "km cancel") {
		t.Fatalf("stderr 应含取消指引: %q", errb)
	}
}

// 脚本未安装（init 后首次 run/shell 前）：明确定义的非失败情形，退出 0。
func TestSessionsScriptMissing127(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	dk := sessFake(t, root, "", 127, nil, nil)
	code, out, _ := runSessions(t, root, dk)
	if code != ExitOK || !strings.Contains(out, "会话脚本未安装") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// 查询失败/输出异常：稳定错误标识 + 非零退出，绝不当作无会话。
func TestSessionsQueryFailuresNotAbsence(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	cases := []struct {
		name       string
		out        string
		exit       int
		err        error
		wantSubstr string
	}{
		{"engine-exit", "", 5, nil, "KM_SESSION_UNKNOWN"},
		{"engine-err", "", -1, fmt.Errorf("daemon down"), "KM_SESSION_UNKNOWN"},
		{"parse-garbage", "something unexpected\n", 0, nil, "KM_SESSION_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dk := sessFake(t, root, tc.out, tc.exit, tc.err, nil)
			code, _, errb := runSessions(t, root, dk)
			if code != ExitEnv || !strings.Contains(errb, tc.wantSubstr) {
				t.Fatalf("code=%d err=%q", code, errb)
			}
		})
	}
}

func TestSessionsRejectsArgs(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	dk := sessFake(t, root, "", 0, nil, nil)
	if code, _, errb := runSessions(t, root, dk, "extra"); code != ExitUsage || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("code=%d err=%q", code, errb)
	}
}

func TestCancelUsageAndFormat(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	dk := sessFake(t, root, "", 0, nil, nil)
	if code, _, errb := runCancel(t, root, dk); code != ExitUsage || !strings.Contains(errb, "km cancel") {
		t.Fatalf("缺参应 usage: code=%d err=%q", code, errb)
	}
	if code, _, errb := runCancel(t, root, dk, "abc"); code != ExitUsage || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("非法格式应 usage: code=%d err=%q", code, errb)
	}
	// 大写/短前缀都不放行：完整 ID 精确匹配
	if code, _, _ := runCancel(t, root, dk, "SAAAAAAAAAAAAAAA"); code != ExitUsage {
		t.Fatalf("大写 ID 应 usage: code=%d", code)
	}
}

func TestCancelUnknownIDRejected(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	// 列表里只有 sessB；取消 sessA 必须报错而非宣称成功（即使 km-ctl 会幂等返回 0）
	out0 := fmt.Sprintf("STALE %s\n", sessB)
	dk := sessFake(t, root, out0, 0, nil, map[string]int{sessA: 0})
	code, out, errb := runCancel(t, root, dk, sessA)
	if code != ExitEnv || !strings.Contains(errb, "KM_SESSION_UNKNOWN") || strings.Contains(out, "已取消") {
		t.Fatalf("未知 ID 不得宣称成功: code=%d out=%q err=%q", code, out, errb)
	}
}

func TestCancelActiveSessionVerified(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	out0 := fmt.Sprintf("ACTIVE %s\n", sessA)
	dk := sessFake(t, root, out0, 0, nil, map[string]int{sessA: 0})
	code, out, _ := runCancel(t, root, dk, sessA)
	if code != ExitOK || !strings.Contains(out, "已取消并确认收尾") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestCancelStaleIdempotent(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	out0 := fmt.Sprintf("STALE %s\n", sessB)
	dk := sessFake(t, root, out0, 0, nil, map[string]int{sessB: 0})
	code, out, _ := runCancel(t, root, dk, sessB)
	if code != ExitOK || !strings.Contains(out, "已结束，登记已清除") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestCancelAlreadyGoneExit3(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	out0 := fmt.Sprintf("ACTIVE %s\n", sessA)
	// 列表显示 ACTIVE，但 cancel 时已被清扫（3）→ 幂等成功且消息明确
	dk := sessFake(t, root, out0, 0, nil, map[string]int{sessA: 3})
	code, out, _ := runCancel(t, root, dk, sessA)
	if code != ExitOK || !strings.Contains(out, "已不存在") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestCancelUnconfirmedExit4Fails(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	t.Chdir(root)
	out0 := fmt.Sprintf("ACTIVE %s\n", sessA)
	dk := sessFake(t, root, out0, 0, nil, map[string]int{sessA: 4})
	code, _, errb := runCancel(t, root, dk, sessA)
	if code != ExitEnv || !strings.Contains(errb, "KM_SESSION_UNKNOWN") || !strings.Contains(errb, "未确认") {
		t.Fatalf("未确认必须非零并保留诊断: code=%d err=%q", code, errb)
	}
}

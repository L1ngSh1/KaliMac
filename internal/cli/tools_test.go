package cli

// km tools 单元验收（合同：docs/tool-discovery-plan.md 行为合同）。
// 反例先行：探测失败/协议异常绝不能显示为 MISSING。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// toolsOutput 生成探测 stdout（缺省全部 AVAILABLE:/usr/bin/<name>）。
func toolsOutput(missing map[string]bool, paths map[string]string) string {
	var b strings.Builder
	for _, t := range toolsChecklist {
		if missing[t] {
			b.WriteString(t + "=MISSING\n")
			continue
		}
		p := paths[t]
		if p == "" {
			p = "/usr/bin/" + t
		}
		fmt.Fprintf(&b, "%s=AVAILABLE:%s\n", t, p)
	}
	return b.String()
}

// toolsFake：注入控制器应答探测 exec；记录 exec 调用次数供只读/零调用断言。
func toolsFake(t *testing.T, root string, probeOut string, probeExit int, probeErr error) (*runtime.Docker, *int) {
	t.Helper()
	st, err := project.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	execCalls := 0
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController {
		return &session.DockerController{RunFn: func(_ context.Context, _ []byte, args []string) (string, string, int, error) {
			if len(args) >= 4 && args[1] == "exec" {
				execCalls++
			}
			return probeOut, "", probeExit, probeErr
		}}
	}
	t.Cleanup(func() { newSessionController = old })
	line := containerLine(st.Container.ID, st.Container.Name, "running", st.ProjectID, sdImageID, resolveDir(t, root))
	return &runtime.Docker{Exec: doctorFake(line)}, &execCalls
}

func runTools(t *testing.T, dir string, d *runtime.Docker, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb strings.Builder
	code := runToolsCommand(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

func toolsRunningFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	hooks := map[string]int{}
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	if code, _, errb := runInitArgs(t, d, dir); code != ExitOK {
		t.Fatalf("init: %s", errb)
	}
	return dir
}

func TestToolsAllAvailable(t *testing.T) {
	dir := toolsRunningFixture(t)
	d, _ := toolsFake(t, dir, toolsOutput(nil, nil), 0, nil)
	code, out, errb := runTools(t, dir, d)
	if code != ExitOK || !strings.Contains(out, "六项精选工具全部可用") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
	for _, tl := range toolsChecklist {
		if !strings.Contains(out, "AVAILABLE "+tl) {
			t.Fatalf("缺少 %s: %q", tl, out)
		}
	}
	// AVAILABLE 路径来自探测输出，而非镜像标签推测
	if !strings.Contains(out, "/usr/bin/python3") {
		t.Fatalf("应显示探测到的解析路径: %q", out)
	}
}

func TestToolsPartialMissing(t *testing.T) {
	dir := toolsRunningFixture(t)
	missing := map[string]bool{"nmap": true, "jq": true}
	paths := map[string]string{"python3": "/usr/local/bin/python3"}
	d, _ := toolsFake(t, dir, toolsOutput(missing, paths), 0, nil)
	code, out, _ := runTools(t, dir, d)
	if code != ExitOK || !strings.Contains(out, "MISSING  nmap") || !strings.Contains(out, "MISSING  jq") {
		t.Fatalf("缺失项应列出: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "2/6") || !strings.Contains(out, "Dockerfile") || !strings.Contains(out, "不构成可复现配置") {
		t.Fatalf("应给出维护指引: %q", out)
	}
	if !strings.Contains(out, "/usr/local/bin/python3") {
		t.Fatalf("AVAILABLE 应显示探测路径: %q", out)
	}
}

// 反例1：探测失败（权限 126）→ UNKNOWN 非零；stdout 不得出现任何 MISSING。
func TestToolsProbeFailureIsUnknown(t *testing.T) {
	dir := toolsRunningFixture(t)
	perm := "OCI runtime exec failed: exec: \"/tmp/km-bin/km-ctl\": permission denied"
	d, _ := toolsFake(t, dir, perm, 126, nil)
	code, out, errb := runTools(t, dir, d)
	if code != ExitEnv || !strings.Contains(errb, "工具可用性未知") {
		t.Fatalf("探测失败应 unknown: code=%d out=%q err=%q", code, out, errb)
	}
	if strings.Contains(out, "MISSING") {
		t.Fatalf("执行失败不得当作 MISSING: %q", out)
	}
}

// 协议异常三形态：行数不足 / 未知工具 / 重复条目 → KM_TOOLS_PROTOCOL。
func TestToolsProtocolViolations(t *testing.T) {
	dir := toolsRunningFixture(t)
	cases := []struct {
		name string
		out  string
	}{
		{"too-few", "python3=AVAILABLE:/p\n"},
		{"unknown-tool", toolsOutput(nil, nil) + "htop=AVAILABLE:/p\n"},
		{"duplicate", toolsOutput(nil, nil) + "python3=AVAILABLE:/dup\n"},
		{"bad-line", toolsOutput(nil, nil) + "nonsense\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := toolsFake(t, dir, tc.out, 0, nil)
			code, out, errb := runTools(t, dir, d)
			if code != ExitEnv || !strings.Contains(errb, "KM_TOOLS_PROTOCOL") {
				t.Fatalf("%s: code=%d err=%q", tc.name, code, errb)
			}
			if strings.Contains(out, "MISSING") {
				t.Fatalf("协议异常不得输出 MISSING: %q", out)
			}
		})
	}
}

// 引擎故障：RunFn 返回真实错误 → unknown（保留诊断）。
func TestToolsProbeError(t *testing.T) {
	dir := toolsRunningFixture(t)
	d, _ := toolsFake(t, dir, "", -1, fmt.Errorf("cannot connect to the docker daemon"))
	code, _, errb := runTools(t, dir, d)
	if code != ExitEnv || !strings.Contains(errb, "工具可用性未知") {
		t.Fatalf("引擎故障应 unknown: code=%d err=%q", code, errb)
	}
}

// 停止/暂停：明示状态与未检查原因（exit 1），且不发起任何探测 exec。
func TestToolsStoppedAndPausedNoExec(t *testing.T) {
	for _, state := range []string{"exited", "paused"} {
		t.Run(state, func(t *testing.T) {
			root := toolsRunningFixture(t)
			st, err := project.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			execCalls := 0
			old := newSessionController
			newSessionController = func(endpoint string) *session.DockerController {
				return &session.DockerController{RunFn: func(_ context.Context, _ []byte, _ []string) (string, string, int, error) {
					execCalls++
					return "", "", 0, nil
				}}
			}
			t.Cleanup(func() { newSessionController = old })
			// 容器表换成非 running 状态的 inspect 行
			line := containerLine(st.Container.ID, st.Container.Name, state, st.ProjectID, sdImageID, resolveDir(t, root))
			d := &runtime.Docker{Exec: doctorFake(line)}
			code, _, errb := runTools(t, root, d)
			if code != ExitEnv || !strings.Contains(errb, "未检查") {
				t.Fatalf("%s: code=%d err=%q", state, code, errb)
			}
			if execCalls != 0 {
				t.Fatalf("%s: 不得发起探测 exec（%d 次）", state, execCalls)
			}
		})
	}
}

// 未初始化 → KM_PROJECT_MISSING。
func TestToolsUninitialized(t *testing.T) {
	root := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	code, _, errb := runTools(t, root, d)
	if code != ExitEnv || !strings.Contains(errb, "KM_PROJECT_MISSING") {
		t.Fatalf("code=%d err=%q", code, errb)
	}
}

// 用法：额外参数 → KM_USAGE 退出 2。
func TestToolsUsage(t *testing.T) {
	dir := toolsRunningFixture(t)
	d, _ := toolsFake(t, dir, "", 0, nil)
	if code, _, errb := runTools(t, dir, d, "--json"); code != ExitUsage || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("code=%d err=%q", code, errb)
	}
}

// 身份冲突：归属不符 → KM_CONTAINER_CONFLICT，且不发起任何探测 exec。
func TestToolsIdentityConflictNoProbe(t *testing.T) {
	dir := toolsRunningFixture(t)
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	execCalls := 0
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController {
		return &session.DockerController{RunFn: func(_ context.Context, _ []byte, _ []string) (string, string, int, error) {
			execCalls++
			return "", "", 0, nil
		}}
	}
	t.Cleanup(func() { newSessionController = old })
	line := containerLine(st.Container.ID, st.Container.Name, "running", "p_other", sdImageID, resolveDir(t, dir))
	d := &runtime.Docker{Exec: doctorFake(line)}
	code, _, errb := runTools(t, dir, d)
	if code != ExitEnv || !strings.Contains(errb, "KM_CONTAINER_CONFLICT") {
		t.Fatalf("身份冲突应拒绝: code=%d err=%q", code, errb)
	}
	if execCalls != 0 {
		t.Fatalf("身份冲突时不得发起探测: %d 次", execCalls)
	}
}

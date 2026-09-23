package cli

// km status 单元验收（合同：docs/newuser-iteration-plan.md 包 B）。
// 覆盖全部状态枚举、只读性、人类/JSON 一致性、"查询失败不显示为无会话"。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

func runStatus(t *testing.T, dir string, d *runtime.Docker, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb strings.Builder
	code := runStatusCommand(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

func statusJSON(t *testing.T, out string) projectStatus {
	t.Helper()
	var ps projectStatus
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	return ps
}

// 同 scriptedDocker：sessions 可脚本化（其它子命令沿用 dockerResponder）。
func statusDocker(t *testing.T, containers map[string]string, sessionsOut string, sessionsExit int) *runtime.Docker {
	t.Helper()
	// 会话查询走 newSessionController 注入（与 sessions/cancel 单测同模式）
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController {
		return &session.DockerController{RunFn: func(_ context.Context, _ []byte, _ []string) (string, string, int, error) {
			return sessionsOut, "", sessionsExit, nil
		}}
	}
	t.Cleanup(func() { newSessionController = old })
	return mkDocker(func(args []string) (string, int) {
		return dockerResponder(t, containers, map[string]int{})(args)
	})
}

func TestStatusUninitialized(t *testing.T) {
	code, out, _ := runStatus(t, t.TempDir(), mkDocker(func([]string) (string, int) { return "", 0 }))
	if code != ExitOK || !strings.Contains(out, "uninitialized") || !strings.Contains(out, "km init") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestStatusConfigIncomplete(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"img:1"}`)
	code, out, _ := runStatus(t, dir, mkDocker(func([]string) (string, int) { return "", 0 }))
	if code != ExitOK || !strings.Contains(out, "config_incomplete") || !strings.Contains(out, "km init") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// statusContainersOf：按 fixture 项目的 state 构造「运行中且身份匹配」的容器表。
func statusContainersOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		st.Container.ID: inspectLine(st.Container.ID, st.Container.Name, "running", st.ProjectID, sdImageID, resolveDir(t, dir)),
	}
}

func statusRunningFixture(t *testing.T) (string, map[string]string, *runtime.Docker) {
	t.Helper()
	hooks := map[string]int{}
	dir := t.TempDir()
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	if code, _, errb := runInitArgs(t, d, dir); code != ExitOK {
		t.Fatalf("init: %s", errb)
	}
	st, _ := project.LoadState(dir)
	containers[st.Container.ID] = inspectLine(st.Container.ID, st.Container.Name, "running", st.ProjectID, sdImageID, resolveDir(t, dir))
	return dir, containers, d
}

func TestStatusRunningIdleAndActive(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	// 无会话 → running_idle
	d := statusDocker(t, containers, "", 0)
	code, out, _ := runStatus(t, dir, d)
	if code != ExitOK || !strings.Contains(out, "running_idle") || strings.Contains(out, "session_active") {
		t.Fatalf("idle: code=%d out=%q", code, out)
	}
	// 活跃会话 → running_session_active（中性表述）
	d2 := statusDocker(t, containers, "ACTIVE saaaaaaaaaaaaaaaa\n", 0)
	code2, out2, _ := runStatus(t, dir, d2)
	if code2 != ExitOK || !strings.Contains(out2, "running_session_active") {
		t.Fatalf("active: code=%d out=%q", code2, out2)
	}
	if strings.Contains(out2, "异常遗留") {
		t.Fatalf("活跃会话不得称为异常遗留: %q", out2)
	}
}

func TestStatusCanonicalizesSymlinkProjectRoot(t *testing.T) {
	realRoot, containers, _ := statusRunningFixture(t)
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "project-link")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	// os.Getwd may preserve a logical PWD when it identifies the same inode;
	// this reproduces /tmp -> /private/tmp without relying on host layout.
	t.Setenv("PWD", alias)
	d := statusDocker(t, containers, "", 0)
	code, out, errb := runStatus(t, alias, d, "--json")
	if code != ExitOK || errb != "" || !strings.Contains(out, `"state": "running_idle"`) {
		t.Fatalf("等价符号链接路径不应触发身份冲突: code=%d out=%q err=%q", code, out, errb)
	}
	ps := statusJSON(t, out)
	if ps.Project == nil || ps.Project.Root != project.CanonicalPath(realRoot) {
		t.Fatalf("项目根应规范化: %+v", ps.Project)
	}
}

// 查询失败/输出异常 → unknown 退出 1，绝不显示为无会话。
func TestStatusQueryFailureIsUnknown(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	cases := []struct {
		name  string
		out   string
		exit  int
		state string
	}{
		{"sessions-exit-9", "", 9, statusUnknown},
		{"sessions-garbage", "nonsense\n", 0, statusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := statusDocker(t, containers, tc.out, tc.exit)
			code, out, _ := runStatus(t, dir, d, "--json")
			if code != ExitEnv || !strings.Contains(out, `"state": "`+tc.state+`"`) {
				t.Fatalf("code=%d out=%q", code, out)
			}
			if strings.Contains(out, "running_idle") {
				t.Fatalf("查询失败不得显示为无会话: %q", out)
			}
			// 合同：unknown 时 error{code,msg} 必填，sessions 字段缺席
			var m map[string]any
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("JSON: %v", err)
			}
			errObj, _ := m["error"].(map[string]any)
			if errObj == nil || errObj["code"] == "" || errObj["msg"] == "" {
				t.Fatalf("error{code,msg} 必填: %v", m["error"])
			}
			if _, has := m["sessions"]; has {
				t.Fatalf("查询失败时不得伪造 sessions 字段: %v", m["sessions"])
			}
		})
	}
	// 真实缺陷形态（安装版验收发现）：OCI 失败文本经 stdout、退出码 0
	// → 必须识别为脚本未安装信息态，不得报 unknown
	oci := "OCI runtime exec failed: exec failed: unable to start container process: exec: \"/tmp/km-bin/km-ctl\": stat /tmp/km-bin/km-ctl: no such file or directory\r\n"
	d := statusDocker(t, containers, oci, 0)
	code3, out3, _ := runStatus(t, dir, d, "--json")
	if code3 != ExitOK || !strings.Contains(out3, "running_idle") || strings.Contains(out3, "unknown") {
		t.Fatalf("OCI/stdout 形态应信息态: code=%d out=%q", code3, out3)
	}
	// exit 126 + 双证据文本 = 脚本未安装的信息态：running_idle + 说明（非 unknown）
	sig := "OCI runtime exec failed: exec: \"/tmp/km-bin/km-ctl\": stat /tmp/km-bin/km-ctl: no such file or directory"
	d2 := statusDocker(t, containers, sig, 126)
	code, out, _ := runStatus(t, dir, d2)
	if code != ExitOK || !strings.Contains(out, "running_idle") || !strings.Contains(out, "会话脚本尚未安装") {
		t.Fatalf("126 信息态: code=%d out=%q", code, out)
	}
	_, jOut2, _ := runStatus(t, dir, d2, "--json")
	if !strings.Contains(jOut2, `"state": "running_idle"`) {
		t.Fatalf("126 信息态 JSON: %s", jOut2)
	}
}

func TestStatusStoppedAndMissingAndConflict(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	runInitArgs(t, d, dir)
	st, _ := project.LoadState(dir)

	// 停止
	containers[st.Container.ID] = inspectLine(st.Container.ID, st.Container.Name, "exited", st.ProjectID, sdImageID, resolveDir(t, dir))
	code, out, _ := runStatus(t, dir, statusDocker(t, containers, "", 0))
	if code != ExitOK || !strings.Contains(out, "container_stopped") {
		t.Fatalf("stopped: code=%d out=%q", code, out)
	}
	// 容器缺失
	delete(containers, st.Container.ID)
	code, out, _ = runStatus(t, dir, statusDocker(t, containers, "", 0))
	if code != ExitOK || !strings.Contains(out, "container_missing") || !strings.Contains(out, "km init") {
		t.Fatalf("missing: code=%d out=%q", code, out)
	}
	// 身份冲突：同名他人容器
	containers[st.Container.ID] = inspectLine(st.Container.ID, "km-其他项目", "running", "p_other", sdImageID, resolveDir(t, dir))
	code, out, _ = runStatus(t, dir, statusDocker(t, containers, "", 0))
	if code != ExitEnv || !strings.Contains(out, "identity_conflict") || !strings.Contains(out, "KM_CONTAINER_CONFLICT") {
		t.Fatalf("conflict: code=%d out=%q", code, out)
	}
}

// --json：结构化输出与人类输出表达同一状态；错误对象完整。
func TestStatusJSONMode(t *testing.T) {
	dir, _, _ := statusRunningFixture(t)
	d := statusDocker(t, statusContainersOf(t, dir), "ACTIVE saaaaaaaaaaaaaaaa\n", 0)
	code, out, _ := runStatus(t, dir, d, "--json")
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	var ps map[string]any
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	if ps["schema_version"] != float64(1) || ps["state"] != "running_session_active" {
		t.Fatalf("schema/state: %v %v", ps["schema_version"], ps["state"])
	}
	sess, _ := ps["sessions"].(map[string]any)
	if sess == nil || len(sess["active"].([]any)) != 1 {
		t.Fatalf("sessions: %v", ps["sessions"])
	}
	if adv, _ := ps["advice"].(string); adv == "" {
		t.Fatal("advice 不得为空")
	}
}

// 只读性：status 前后 .km 目录内容与状态文件逐字节一致。
func TestStatusIsReadOnly(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	snapshot := func() map[string]string {
		m := map[string]string{}
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				b, _ := os.ReadFile(p)
				m[p] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("快照失败: %v", err)
		}
		return m
	}
	before := snapshot()
	d := statusDocker(t, containers, "ACTIVE saaaaaaaaaaaaaaaa\n", 0)
	if code, _, _ := runStatus(t, dir, d); code != ExitOK {
		t.Fatal("status 失败")
	}
	if code, _, _ := runStatus(t, dir, d, "--json"); code != ExitOK {
		t.Fatal("json status 失败")
	}
	after := snapshot()
	if len(before) != len(after) {
		t.Fatalf(".km 文件数变化: %d -> %d", len(before), len(after))
	}
	for p, content := range before {
		if after[p] != content {
			t.Fatalf("只读性破坏: %s 被修改", p)
		}
	}
}

func TestStatusUsageErrors(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	d := statusDocker(t, containers, "", 0)
	if code, _, errb := runStatus(t, dir, d, "positional"); code != ExitUsage || !strings.Contains(errb, "KM_USAGE") {
		t.Fatalf("位置参数应 usage: code=%d err=%q", code, errb)
	}
}

// ===== 审查反例固化（修复前应失败、修复后通过）=====

// 反例1（P1）：km-ctl 因权限问题执行失败（exit 126 + permission denied）——
// 脚本存在但不可执行，属于真实执行失败，必须 unknown 非零，不得报空闲。
func TestStatusPermDeniedIsUnknown(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	perm := "OCI runtime exec failed: exec failed: unable to start container process: exec: \"/tmp/km-bin/km-ctl\": permission denied\r\n"
	d := statusDocker(t, containers, perm, 126)
	code, out, _ := runStatus(t, dir, d)
	if code != ExitEnv || !strings.Contains(out, statusUnknown) {
		t.Fatalf("权限失败应 unknown: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "running_idle") || strings.Contains(out, "脚本未安装") || strings.Contains(out, "尚未安装") {
		t.Fatalf("权限失败不得报为脚本未安装/空闲: %q", out)
	}
}

// 反例3（P2）：记录 ID 消失后，按名称的二次查询自身失败 → unknown（保留诊断），
// 不得在查询失败时断言 container_missing。
func TestStatusNameQueryFailureIsUnknown(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	runInitArgs(t, d, dir)
	st, _ := project.LoadState(dir)
	// 记录 ID 消失；同名容器查询模拟 daemon 故障（不可分类为 NotFound）
	respond := func(args []string) (string, int) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, st.Container.ID) {
			return "Error response from daemon: No such container: " + st.Container.ID, 1
		}
		if strings.Contains(joined, st.Container.Name) {
			return "Error response from daemon: bizzarro daemon failure", 1
		}
		return dockerResponder(t, containers, hooks)(args)
	}
	code, out, _ := runStatus(t, dir, mkDocker(respond))
	if code != ExitEnv || !strings.Contains(out, statusUnknown) ||
		!strings.Contains(out, "同名容器查询失败") || !strings.Contains(out, "KM_RUNTIME_OFFLINE") {
		t.Fatalf("同名查询失败应 unknown 且保留失败环节诊断: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "container_missing") {
		t.Fatalf("查询失败不得判为 container_missing: %q", out)
	}
}

// 反例4（P2）：paused 容器——任务仍在（挂起），不得报 container_stopped/推断无会话。
func TestStatusPausedIsItsOwnState(t *testing.T) {
	dir, containers, _ := statusRunningFixture(t)
	st, _ := project.LoadState(dir)
	containers[st.Container.ID] = inspectLine(st.Container.ID, st.Container.Name, "paused", st.ProjectID, sdImageID, resolveDir(t, dir))
	code, out, _ := runStatus(t, dir, statusDocker(t, containers, "", 0))
	if code != ExitOK || !strings.Contains(out, "container_paused") {
		t.Fatalf("paused 应为独立状态: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "container_stopped") {
		t.Fatalf("paused 不得报为已停止: %q", out)
	}
}

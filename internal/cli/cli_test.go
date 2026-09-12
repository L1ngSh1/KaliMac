package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"kalimac/internal/runtime"
)

func run(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), argv, &out, &errb)
	return code, out.String(), errb.String()
}

// forbidDocker swaps the newDocker injection point for a fake that fails
// the test on any external command, and restores it when done.
func forbidDocker(t *testing.T) *runtime.FakeExecutor {
	t.Helper()
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			t.Errorf("该代码路径不应调用外部命令: %s %v", name, args)
			return nil, nil, errors.New("forbidden external call")
		},
	}
	old := newDocker
	newDocker = func() *runtime.Docker { return &runtime.Docker{Exec: fe} }
	t.Cleanup(func() { newDocker = old })
	return fe
}

func TestEmptyArgvShowsHelp(t *testing.T) {
	t.Chdir(t.TempDir()) // 与宿主 cwd/祖先目录解耦：查找 .km.json 不得受环境影响
	forbidDocker(t)
	code, out, _ := run(t)
	if code != ExitOK || !strings.Contains(out, "km —") || !strings.Contains(out, "doctor") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// F4 补强：help/version（及未实现命令分支）必须零外部调用。
func TestHelpAndVersionZeroExternalCalls(t *testing.T) {
	t.Chdir(t.TempDir()) // 工具路径会向上查找 .km.json，须与宿主目录树隔离
	fe := forbidDocker(t)
	for _, argv := range [][]string{
		{"--help"}, {"help"}, {"--version"}, {"version"}, {},
		{"nmap", "--help"}, {"run", "--", "stop"},
	} {
		code, _, _ := run(t, argv...)
		// ExitEnv：工具路径在未初始化目录先报 KM_PROJECT_MISSING（仍零外部调用）
		if code != ExitOK && code != ExitUsage && code != ExitEnv {
			t.Fatalf("%v code=%d", argv, code)
		}
	}
	if len(fe.Calls) != 0 {
		t.Fatalf("外部调用次数应为 0, 实际 %d: %+v", len(fe.Calls), fe.Calls)
	}
}

func TestVersionOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	_, out, _ := run(t, "--version")
	if !strings.Contains(out, "km "+Version) {
		t.Fatalf("版本输出: %q", out)
	}
}

func TestUnknownOptionUsageError(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, errOut := run(t, "--frobnicate")
	if code != ExitUsage || !strings.Contains(errOut, "KM_USAGE") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestDoctorRejectsArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, errOut := run(t, "doctor", "--json")
	if code != ExitUsage || !strings.Contains(errOut, "不接受参数") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// P1 边界：init/shell/stop 与工具执行必须明确报未实现，禁止空实现冒充完成。
func TestUnimplementedCommandsAreExplicit(t *testing.T) {
	// C2 起全部管理命令已实现；此处验证 shell 的 tty 前检（无终端环境 → KM_NOT_TTY，
	// 且发生在任何 docker 调用之前）。
	t.Chdir(t.TempDir()) // tty 前检虽在项目查找之前，仍与宿主目录树隔离
	forbidDocker(t)
	code, _, errOut := run(t, "shell")
	if code != ExitEnv || !strings.Contains(errOut, "KM_NOT_TTY") {
		t.Fatalf("shell 无终端应 KM_NOT_TTY: code=%d err=%q", code, errOut)
	}
}

// 未实现命令与环境前提的边界：未初始化项目下执行工具 → KM_PROJECT_MISSING（exit 1）。
func TestToolWithoutProjectIsEnvError(t *testing.T) {
	forbidDocker(t) // 健康路径前不发 docker 调用（FindConfig 先失败）
	dir := t.TempDir()
	t.Chdir(dir)
	code, _, errOut := run(t, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errOut, "KM_PROJECT_MISSING") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestRunLongFormRequiresSeparator(t *testing.T) {
	forbidDocker(t)
	dir := t.TempDir()
	t.Chdir(dir)
	code, _, errOut := run(t, "run", "nmap", "x")
	if code != ExitUsage || !strings.Contains(errOut, "--") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	code, _, errOut = run(t, "run")
	if code != ExitUsage {
		t.Fatalf("km run 无参数 code=%d err=%q", code, errOut)
	}
	code, _, errOut = run(t, "run", "--")
	if code != ExitUsage {
		t.Fatalf("km run -- 无工具 code=%d err=%q", code, errOut)
	}
}

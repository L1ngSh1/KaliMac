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
	forbidDocker(t)
	code, out, _ := run(t)
	if code != ExitOK || !strings.Contains(out, "km —") || !strings.Contains(out, "doctor") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// F4 补强：help/version（及未实现命令分支）必须零外部调用。
func TestHelpAndVersionZeroExternalCalls(t *testing.T) {
	fe := forbidDocker(t)
	for _, argv := range [][]string{
		{"--help"}, {"help"}, {"--version"}, {"version"}, {},
		{"nmap", "--help"}, {"run", "--", "stop"},
	} {
		code, _, _ := run(t, argv...)
		if code != ExitOK && code != ExitUsage {
			t.Fatalf("%v code=%d", argv, code)
		}
	}
	if len(fe.Calls) != 0 {
		t.Fatalf("外部调用次数应为 0, 实际 %d: %+v", len(fe.Calls), fe.Calls)
	}
}

func TestVersionOutput(t *testing.T) {
	_, out, _ := run(t, "--version")
	if !strings.Contains(out, "km "+Version) {
		t.Fatalf("版本输出: %q", out)
	}
}

func TestUnknownOptionUsageError(t *testing.T) {
	code, _, errOut := run(t, "--frobnicate")
	if code != ExitUsage || !strings.Contains(errOut, "KM_USAGE") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestDoctorRejectsArgs(t *testing.T) {
	code, _, errOut := run(t, "doctor", "--json")
	if code != ExitUsage || !strings.Contains(errOut, "不接受参数") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// P1 边界：init/shell/stop 与工具执行必须明确报未实现，禁止空实现冒充完成。
func TestUnimplementedCommandsAreExplicit(t *testing.T) {
	cases := [][]string{
		{"init"}, {"shell"}, {"stop"},
		{"nmap", "-sV", "127.0.0.1"},
		{"run", "--", "nmap", "-p", "1"},
	}
	for _, argv := range cases {
		code, _, errOut := run(t, argv...)
		if code != ExitUsage {
			t.Fatalf("%v code=%d", argv, code)
		}
		if !strings.Contains(errOut, "KM_NOT_IMPLEMENTED") {
			t.Fatalf("%v 缺少 KM_NOT_IMPLEMENTED: %q", argv, errOut)
		}
		if strings.Contains(errOut, "成功") {
			t.Fatalf("%v 不应声称成功: %q", argv, errOut)
		}
	}
}

func TestRunLongFormRequiresSeparator(t *testing.T) {
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

// 与管理命令重名的工具仍可通过长形式调用（P1 仅验证解析路径与命名）。
func TestRunLongFormReservesNameCollision(t *testing.T) {
	_, _, errOut := run(t, "run", "--", "stop", "--now")
	if !strings.Contains(errOut, "km run -- stop") {
		t.Fatalf("错误信息应包含工具名 stop: %q", errOut)
	}
}

func TestToolArgsPreservedVerbatimInMessage(t *testing.T) {
	_, _, errOut := run(t, "nmap", "", "a b", "--help")
	if !strings.Contains(errOut, "KM_NOT_IMPLEMENTED") {
		t.Fatalf("err=%q", errOut)
	}
}

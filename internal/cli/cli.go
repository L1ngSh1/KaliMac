// Package cli implements the km command-line contract: dispatch, help,
// version, init/run/shell/stop and the read-only doctor.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"kalimac/internal/runtime"
)

// Version is the km build version reported by --version.
const Version = "0.4.0-p3"

// Exit codes per the CLI contract: 0 success, 1 environment failure,
// 2 usage error or unimplemented command.
const (
	ExitOK    = 0
	ExitEnv   = 1
	ExitUsage = 2
)

// management commands are matched by exact name in the first position.
var managementCommands = map[string]bool{
	"help": true, "version": true, "init": true, "shell": true,
	"doctor": true, "stop": true, "run": true,
	"sessions": true, "cancel": true, "status": true, "tools": true,
}

// newDocker is the injection point for tests: swapping it lets tests assert
// that some code paths (help/version) never touch an external command.
var newDocker = newProductionDocker

func newProductionDocker() *runtime.Docker {
	return &runtime.Docker{Exec: runtime.CommandExecutor{}}
}

// Run dispatches argv. Management options belong to km; everything after a
// tool name belongs to the tool and is preserved verbatim.
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		PrintHelp(stdout)
		return ExitOK
	}
	switch argv[0] {
	case "--help", "-h", "help":
		PrintHelp(stdout)
		return ExitOK
	case "--version", "version":
		PrintVersion(stdout)
		return ExitOK
	case "doctor":
		return runDoctorCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "run":
		return runLongForm(ctx, argv[1:], stdout, stderr)
	case "init":
		return runInitCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "stop":
		return runStopCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "shell":
		return runShellCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "sessions":
		return runSessionsCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "cancel":
		return runCancelCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "status":
		return runStatusCommand(ctx, argv[1:], stdout, stderr, newDocker())
	case "tools":
		return runToolsCommand(ctx, argv[1:], stdout, stderr, newDocker())
	}
	if strings.HasPrefix(argv[0], "-") {
		return usageError(stderr, "未知选项 %q；管理命令见 km --help", argv[0])
	}
	// short form tool invocation: the whole argv belongs to the tool
	return runToolCommand(ctx, argv[0], argv[1:], os.Stdin, stdout, stderr, newDocker())
}

// runLongForm parses `km run -- TOOL ARG...`. The `--` separator is required
// so that tools whose names collide with management commands stay callable.
func runLongForm(ctx context.Context, rest []string, stdout, stderr io.Writer) int {
	if len(rest) == 0 || rest[0] != "--" {
		return usageError(stderr, "km run 需要 -- 分隔符，用法: km run -- TOOL [ARG...]")
	}
	tool := rest[1:]
	if len(tool) == 0 {
		return usageError(stderr, "km run -- 之后必须跟工具名，用法: km run -- TOOL [ARG...]")
	}
	return runToolCommand(ctx, tool[0], tool[1:], os.Stdin, stdout, stderr, newDocker())
}

func usageError(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "%s: ", runtime.CodeUsage)
	fmt.Fprintf(stderr, format, a...)
	fmt.Fprintln(stderr)
	fmt.Fprintf(stderr, "用法: km --help 查看全部命令\n")
	return ExitUsage
}

// PrintHelp writes the short help. It must not touch Docker.
func PrintHelp(w io.Writer) {
	fmt.Fprint(w, `km — 在当前项目的 Kali 容器中执行工具（macOS CLI）

用法:
  km                          显示本帮助
  km --version                显示版本（不依赖 Docker）
  km doctor                   只读检查平台、Docker、项目配置与容器状态
  km init                     初始化当前项目（幂等；非交互最小可用版）
  km TOOL [ARG...]            在项目容器中执行工具（非交互）
  km run -- TOOL [ARG...]     同上，长形式，用于与 km 管理命令重名的工具
  km shell                    交互 bash（真实终端接管；Ctrl-C/Ctrl-D/作业控制）
  km stop                     停止当前项目容器，数据保留（幂等）
  km sessions                 查看当前项目的会话（只读）
  km cancel <id>              显式取消当前项目的指定会话（完整 ID 见 km sessions）
  km status [--json]          当前项目状态总览（只读；--json 结构化输出）
  km tools                    精选工具在容器内的可用性（只读；允许部分 MISSING）

说明:
  项目文件留在 Mac；工具输出与退出状态回到原终端。
  工具名之后的参数原样传给工具；需要 shell 语义时由你显式调用 sh -c。
`)
}

// PrintVersion writes the version line. It must not touch Docker.
func PrintVersion(w io.Writer) {
	fmt.Fprintf(w, "km %s\n", Version)
}

// Package cli implements the km command-line contract: dispatch, help,
// version and the read-only doctor. init/run/shell/stop are P2 work and
// report a clear "not implemented" error in this build.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kalimac/internal/runtime"
)

// Version is the km build version reported by --version.
const Version = "0.1.0-p1"

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
		return runDoctorCommand(ctx, argv[1:], stdout, stderr)
	case "run":
		return runLongForm(ctx, argv[1:], stdout, stderr)
	case "init", "shell", "stop":
		return notImplemented(argv[0], stderr)
	}
	if strings.HasPrefix(argv[0], "-") {
		return usageError(stderr, "未知选项 %q；管理命令见 km --help", argv[0])
	}
	// short form tool invocation: the whole argv belongs to the tool
	return notImplemented("工具执行（km "+argv[0]+" …）", stderr)
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
	return notImplemented("工具执行（km run -- "+tool[0]+" …）", stderr)
}

func notImplemented(what string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s: %s 在本构建（P1 骨架）中尚未实现。\n", runtime.CodeNotImplemented, what)
	fmt.Fprintf(stderr, "本构建仅提供: km --help / --version / doctor。init/run/shell/stop 将在 P2 交付。\n")
	return ExitUsage
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
  km init                     初始化当前项目（P2 提供）
  km TOOL [ARG...]            在项目容器中执行工具（P2 提供）
  km run -- TOOL [ARG...]     同上，长形式，用于与 km 管理命令重名的工具
  km shell                    进入项目容器交互终端（P2 提供）
  km stop                     停止当前项目容器，数据保留（P2 提供）

说明:
  项目文件留在 Mac；工具输出与退出状态回到原终端。
  工具名之后的参数原样传给工具；需要 shell 语义时由你显式调用 sh -c。
`)
}

// PrintVersion writes the version line. It must not touch Docker.
func PrintVersion(w io.Writer) {
	fmt.Fprintf(w, "km %s\n", Version)
}

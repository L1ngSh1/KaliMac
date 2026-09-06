// shellproto 是 P2-C C1/C2 阶段的交互终端实验二进制——非产品命令。
// C2 起委托 session.RunShell（与产品 km shell 同一实现）：引导/登记/信号/
// 收尾全部一致；本文件仅保留 tty 前检（K 项）与 detach 参数注入口。
package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"

	"kalimac/internal/session"
)

func main() { os.Exit(run()) }

func run() int {
	container := flag.String("container", "", "目标容器 ID")
	detachKeys := flag.String("detach-keys", "", "传给 docker exec 的 detach keys")
	flag.Parse()
	if *container == "" {
		fmt.Fprintln(os.Stderr, "KM_PROTO_USAGE: 需要 -container")
		return 2
	}

	// K：进入前检查 stdin/stdout 都是终端，失败不修改终端、不启动会话
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "KM_PROTO_NOT_TTY: stdin/stdout 必须是终端")
		return 2
	}

	mgr := &session.Manager{
		Starter:         &session.ExecStarter{},
		Controller:      &session.DockerController{},
		Diag:            os.Stderr,
		ShellDetachKeys: *detachKeys,
	}
	res, err := mgr.RunShell(*container, "", os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "KM_PROTO_FAIL:", err)
		return 2
	}
	if res.Detached {
		fmt.Fprintln(os.Stderr, "KM_PROTO_DETACHED: shell 仍在容器内运行；会话登记保留")
	}
	if res.CleanupUnconfirmed {
		fmt.Fprintln(os.Stderr, "KM_PROTO_CLEANUP_UNCONFIRMED: 会话收尾未确认；登记保留")
	}
	return res.ExitCode
}

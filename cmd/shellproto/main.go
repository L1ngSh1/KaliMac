// shellproto 是 P2-C C1 阶段的交互终端实验二进制——非产品命令，仅用于
// 验证「由 Docker CLI 接管真实终端」的方案（docs/adr-005-terminal-shell.md）。
//
// 责任划分：
//   - Docker CLI：宿主终端 raw mode 与退出恢复、窗口尺寸同步（SIGWINCH）、
//     容器内 PTY 分配与前台进程组。
//   - 本程序：tty 前置检查（K 项）、进程生命周期、外部信号兜底——
//     SIGTERM/SIGHUP 时以进入前快照恢复 termios（客户端被 SIGKILL 不会
//     自行恢复）；SIGINT 一律忽略（raw mode 下键盘 Ctrl-C 是字节不是信号）。
//
// 不复用非交互 setsid+后台脚本路径；不接入 km 产品入口。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"

	"golang.org/x/term"
)

func main() { os.Exit(run()) }

func run() int {
	container := flag.String("container", "", "目标容器 ID")
	detachKeys := flag.String("detach-keys", "", "传给 docker exec 的 detach keys（空串=禁用脱离）")
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
	saved, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "KM_PROTO_STATE: 获取 termios 失败:", err)
		return 2
	}

	bin, err := exec.LookPath("docker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "KM_PROTO_NO_DOCKER:", err)
		return 2
	}
	// 固定启动参数：--noprofile --norc 避免用户初始化脚本干扰实验；
	// PS1 标记供 PTY 驱动器做提示符同步（不用固定 sleep 作唯一同步手段）；
	// PROMPT_COMMAND 在首个提示符前武装 EXIT trap：bash 退出时对全部作业
	// 发 TERM（实验证实 bash 退出不会自动 SIGHUP 后台进程组）。
	argv := []string{bin, "exec", "-it", "-e", "PS1=KM_SHELL> ",
		"-e", "PROMPT_COMMAND=trap 'kill $(jobs -p) 2>/dev/null' EXIT",
		"--detach-keys", *detachKeys, *container,
		"/bin/bash", "--noprofile", "--norc", "-i"}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "KM_PROTO_START_FAIL:", err)
		return 2
	}

	// 信号语义：外部 SIGTERM/SIGHUP → 终止客户端并由 km 以快照恢复 termios，
	// 统一退出 143；SIGINT 忽略。用原子标志消除主 Wait 与信号路径的竞态。
	var signaled int32
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigCh {
			if s == syscall.SIGINT {
				continue // raw mode 下键盘 Ctrl-C 是字节不是信号；外部 SIGINT 忽略
			}
			fmt.Fprintf(os.Stderr, "\nKM_PROTO_SIGNAL: 收到 %v，终止客户端并恢复终端\n", s)
			atomic.StoreInt32(&signaled, 1)
			_ = cmd.Process.Kill()
			_ = term.Restore(int(os.Stdin.Fd()), saved)
		}
	}()

	err = cmd.Wait()
	if atomic.LoadInt32(&signaled) == 1 {
		return 143
	}
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code
}

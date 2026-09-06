#!/usr/bin/env python3
"""P0-E12/E13：PTY 模式下的中断实验。

用法: pty_driver.py <ctrl_c|sigint|sigkill> <container> <workspace_dir>

在有 PTY（docker exec -it）的容器会话中运行 sig.sh 长任务，然后按模式
注入中断：向 PTY 写入 0x03（Ctrl-C 字节），或向 docker 客户端进程发送
SIGINT/SIGKILL。从 Mac 侧直接读 workspace/sig.log（bind mount 双向可见），
避免容器内 cat 的竞争；再列出容器内 sig.sh/sleep 残留。

判读：动作后 sig.log 出现 TRAP = 信号到达容器内任务；无 TRAP 且进程仍在
= 客户端死亡不向容器内任务传播信号。
"""
import os
import pty
import signal
import sys
import time


def main() -> None:
    mode, container, workspace = sys.argv[1], sys.argv[2], sys.argv[3]
    siglog = os.path.join(workspace, "sig.log")
    if os.path.exists(siglog):
        os.remove(siglog)

    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("docker", ["docker", "exec", "-it", container, "/workspace/sig.sh"])

    def show(tag: str) -> None:
        print(f"--- {tag} 容器任务日志 ({siglog}) ---")
        try:
            print(open(siglog).read().strip() or "(空)")
        except FileNotFoundError:
            print("(尚未创建)")

    time.sleep(2.5)
    show("启动后")

    if mode == "ctrl_c":
        os.write(fd, b"\x03")
        print(">>> 已向 PTY 写入 0x03 (Ctrl-C 字节)")
    elif mode == "sigint":
        os.kill(pid, signal.SIGINT)
        print(f">>> 已向 docker 客户端({pid})发送 SIGINT")
    else:
        os.kill(pid, signal.SIGKILL)
        print(f">>> 已向 docker 客户端({pid})发送 SIGKILL")

    time.sleep(3)
    show("动作后")

    ps = os.popen(f"docker exec {container} ps -o pid,ppid,stat,comm 2>/dev/null").read()
    lines = [l for l in ps.splitlines() if "sig.sh" in l]
    print("--- 容器内残留 sig.sh 进程（STAT 列，Z=僵尸）---")
    print("\n".join(lines) if lines else "(无)")

    try:
        p, status = os.waitpid(pid, os.WNOHANG)
        if p == 0:
            print("docker 客户端: 仍存活，由运行器强制结束")
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
            print("docker 客户端: 已强制结束")
        else:
            print(f"docker 客户端: 已退出 wait status={status} "
                  f"exitcode={os.waitstatus_to_exitcode(status)}")
    except ChildProcessError:
        print("docker 客户端: 已被回收")
    os.close(fd)


if __name__ == "__main__":
    main()

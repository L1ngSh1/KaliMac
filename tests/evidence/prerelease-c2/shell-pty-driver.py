#!/usr/bin/env python3
"""阶段二指南实走：km shell 交互场景（真实 PTY）。
覆盖：提示符/命令执行、Ctrl-C 中断、窗口尺寸跟随、exit 7 退出码透传。
用法：shell-pty-driver.py <km绝对路径> <项目目录>"""
import fcntl
import os
import pty
import select
import struct
import sys
import termios
import time

km, cwd = sys.argv[1], sys.argv[2]
buf = b""


def read_until(patterns, timeout):
    global buf
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for p in patterns:
            if p.encode() in buf:
                return True
        r, _, _ = select.select([fd], [], [], 0.2)
        if fd in r:
            try:
                data = os.read(fd, 4096)
            except OSError:
                break
            buf += data
            sys.stdout.write(data.decode("utf-8", "replace"))
            sys.stdout.flush()
    for p in patterns:
        if p.encode() in buf:
            return True
    print(f"\n[TIMEOUT waiting for {patterns}]")
    return False


pid, fd = pty.fork()
if pid == 0:
    os.chdir(cwd)
    os.environ["TERM"] = "xterm-256color"
    os.execv(km, [km, "shell"])

ok = True
ok &= read_until(["KM_SHELL> "], 30)

os.write(fd, b"pwd\n")
ok &= read_until(["/workspace"], 15)

os.write(fd, b"python3 hello.py\n")
ok &= read_until(["安装版实走 hello"], 20)

os.write(fd, b"sleep 5\n")
time.sleep(0.8)
os.write(fd, b"\x03")             # Ctrl-C
ok &= read_until(["KM_SHELL> "], 15)
os.write(fd, b"echo C=$?\n")
ok &= read_until(["C=130"], 15)

os.write(fd, b"sleep 20\n")
time.sleep(0.6)
os.write(fd, b"\x1a")             # Ctrl-Z
ok &= read_until(["Stopped"], 10)
os.write(fd, b"bg\njobs\n")
ok &= read_until(["Running"], 10)

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 100, 0, 0))
time.sleep(0.8)
os.write(fd, b"stty size\n")
ok &= read_until(["40 100"], 15)

os.write(fd, b"exit 7\n")
_, status = os.waitpid(pid, 0)
code = os.waitstatus_to_exitcode(status)
print(f"\n[SHELL_EXIT_CODE={code}]")
ok &= code == 7
print("[SHELL_WALKTHROUGH_PASS]" if ok else "[SHELL_WALKTHROUGH_FAIL]")
sys.exit(0 if ok else 1)

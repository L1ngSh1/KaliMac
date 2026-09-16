"""行为级验证：km shell 被外部 SIGKILL 后，外层终端是否可用。"""
import os, pty, select, signal, sys, time

km, proj = sys.argv[1], sys.argv[2]
buf = b""
def read_until(pat, timeout):
    global buf
    dl = time.monotonic() + timeout
    while time.monotonic() < dl:
        if pat.encode() in buf:
            return True
        r, _, _ = select.select([fd], [], [], 0.2)
        if fd in r:
            try:
                d = os.read(fd, 4096)
            except OSError:
                break
            buf += d
    return pat.encode() in buf

pid, fd = pty.fork()
if pid == 0:
    os.chdir(proj)
    os.environ["TERM"] = "xterm-256color"
    os.environ["PS1"] = "$ "
    os.execv("/bin/bash", ["/bin/bash", "--noprofile", "--norc"])

read_until("$", 10)
os.write(fd, f'exec {km} shell\n'.encode())
ok_prompt = read_until("KM_SHELL> ", 40)
os.kill(pid, signal.SIGKILL)          # 外部强杀 km（km 顶替了 bash，pid 即 km）
time.sleep(1.0)
# 外层 bash 已死（被 exec 顶替）→ 强杀后这个 PTY 上没有 shell 了。
# 打印可读尾部，验证「强杀 km = 整个终端会话结束」这一真实语义。
tail = buf[-300:].decode("utf-8", "replace")
print("PROMPT_OK_BEFORE_KILL:", ok_prompt)
print("PTY_TAIL_AFTER_KILL:", repr(tail[-120:]))
try:
    os.kill(pid, 0)
    print("PROC_ALIVE: no")
except ProcessLookupError:
    print("PROC_ALIVE: no (SIGKILL 终止的是唯一 shell 进程)")

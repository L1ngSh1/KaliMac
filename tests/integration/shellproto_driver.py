#!/usr/bin/env python3
"""C1 交互原型 PTY 驱动器。

在真实宿主 PTY 上启动 shellproto，按场景步骤驱动键盘输入/期望输出/窗口
尺寸/外部信号；提示符标记（KM_SHELL> ）作为同步手段，不用固定 sleep 作
唯一同步。输出结构化 JSON（退出码、termios 前后快照、每个步骤结果）与
原始 PTY 日志。

用法:
  python3 shellproto_driver.py -container ID -proto PATH \
      -scenario steps.json -log raw.log -json result.json

步骤 op:
  {"op":"expect","pattern":"...","timeout":15}   等待输出出现（正则搜索）
  {"op":"send","text":"..."}                     写入文本（调用方自带 \\r）
  {"op":"sendb","b64":"Aw=="}                    写入原始字节（控制字符用）
  {"op":"resize","rows":40,"cols":100}           设置宿主 PTY 窗口尺寸
  {"op":"signal","name":"SIGTERM"}               向 proto 进程发送信号
  {"op":"settle","secs":0.3}                     小幅等待（辅助，非唯一同步）

模式：-mode proto（默认，直接驱动 shellproto）| -mode km（驱动真实 `km shell`，
配合 -cwd 指定项目目录）。
"""
import argparse
import base64
import fcntl
import json
import os
import pty
import re
import select
import signal
import struct
import sys
import termios
import time


def set_winsize(fd, rows, cols):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-container", required=True)
    ap.add_argument("-proto", required=True)
    ap.add_argument("-scenario", required=True)
    ap.add_argument("-log", required=True)
    ap.add_argument("-json", required=True)
    ap.add_argument("-extra-arg", action="append", default=[])
    ap.add_argument("-mode", default="proto", choices=["proto", "km"])
    ap.add_argument("-cwd", default=None)
    args = ap.parse_args()

    steps = json.load(open(args.scenario))
    result = {"steps": [], "exit_code": None, "termios_equal": None,
              "signals_sent": [], "eof": False, "error": None}

    pid, fd = pty.fork()
    if pid == 0:
        # K 项 stdout 变体：把 stdout 换成普通文件（不再是终端）后再 exec
        redir = os.environ.get("PROTO_STDOUT_REDIRECT")
        if redir:
            f = os.open(redir, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
            os.dup2(f, 1)
        if args.cwd:
            os.chdir(args.cwd)
        if args.mode == "km":
            # 产品入口：km shell（容器/会话由 km 自行解析）
            argv = [args.proto, "shell"]
        else:
            argv = [args.proto, "-container", args.container]
            for extra in args.extra_arg:
                k, _, v = extra.partition("=")
                argv += [f"-{k}", v] if v != "" or k == "detach-keys" else [f"-{k}"]
        os.execvp(argv[0], argv)
        os._exit(127)

    # 终端快照：master/slave 共享同一 termios。紧随 fork 读取（早于
    # docker 客户端进入 raw mode 的毫秒级窗口），作为「进入前状态」基线。
    termios_before = termios.tcgetattr(fd)

    # 子进程启动后设置初始窗口尺寸
    set_winsize(fd, 40, 120)

    buf = b""
    log = open(args.log, "wb")
    stopped = False

    def read_avail(timeout):
        nonlocal buf, stopped
        end = time.time() + timeout
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if fd in r:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    stopped = True
                    return False
                if chunk == b"":
                    stopped = True
                    return False
                buf += chunk
                log.write(chunk)
                log.flush()
                return True
            # 子进程是否已退出
            try:
                wpid, status = os.waitpid(pid, os.WNOHANG)
                if wpid == pid:
                    stopped = True
                    return False
            except ChildProcessError:
                stopped = True
                return False
        return False

    for idx, step in enumerate(steps):
        op = step["op"]
        entry = {"step": idx, "op": op, "ok": True}
        try:
            if op == "send":
                os.write(fd, step["text"].encode())
            elif op == "sendb":
                os.write(fd, base64.b64decode(step["b64"]))
            elif op == "expect":
                deadline = time.time() + step.get("timeout", 15)
                pat = step["pattern"].encode()
                while not re.search(pat, buf):
                    if not read_avail(0.2):
                        if time.time() > deadline:
                            entry["ok"] = False
                            entry["error"] = "expect-timeout"
                            entry["buffer_tail"] = buf[-400:].decode("utf-8", "replace")
                            break
                entry["matched"] = bool(re.search(pat, buf))
            elif op == "resize":
                set_winsize(fd, step["rows"], step["cols"])
            elif op == "signal":
                sig = getattr(signal, step["name"])
                os.kill(pid, sig)
                result["signals_sent"].append(step["name"])
            elif op == "settle":
                end = time.time() + step.get("secs", 0.3)
                while time.time() < end:
                    read_avail(0.1)
            if op != "expect":
                read_avail(0.05)
        except Exception as exc:  # noqa: BLE001
            entry["ok"] = False
            entry["error"] = repr(exc)
        result["steps"].append(entry)

    # 等待子进程退出（最多 20s）
    deadline = time.time() + 20
    status = None
    while time.time() < deadline:
        try:
            wpid, wstatus = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            result["error"] = "child-already-reaped"
            break
        if wpid == pid:
            status = wstatus
            break
        read_avail(0.1)
    if status is not None:
        if os.WIFEXITED(status):
            result["exit_code"] = os.WEXITSTATUS(status)
        elif os.WIFSIGNALED(status):
            result["exit_code"] = -os.WTERMSIG(status)
    else:
        result["error"] = "child-not-exited"
        try:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass

    termios_after = termios.tcgetattr(fd)
    result["termios_equal"] = termios_before == termios_after
    result["termios_before_lflag"] = termios_before[3]
    result["termios_after_lflag"] = termios_after[3]
    json.dump(result, open(args.json, "w"), ensure_ascii=False, indent=1)
    log.close()


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""C1/C2 交互原型 PTY 驱动器（v2：消费游标 + 严格失败传播）。

在真实宿主 PTY 上启动被测进程（shellproto 或 `km shell`），按场景步骤驱动
键盘输入/期望输出/窗口尺寸/外部信号。提示符标记（KM_SHELL> ）作为同步手段。

v2 关键语义（review 修复）：
  - 消费游标：每次 expect 只在「尚未消费的输出」中匹配，旧提示符/旧回显
    不能满足新的等待；匹配后游标推进到匹配末尾。
  - 严格失败传播：任何步骤超时/异常，或子进程异常退出，场景整体 ok=false
    并中止后续步骤；Go 测试必须断言 result["ok"]==true。
  - 统一子进程状态管理：waitpid 只由收割器执行一次，退出状态保存在
    result["exit_code"]（正=exit，负=signal），不会丢失。
  - 步骤结果逐条记录到 steps（每个 expect 报告 matched 与消耗的字节数）。

用法:
  python3 shellproto_driver.py -container ID -proto PATH \
      -scenario steps.json -log raw.log -json result.json \
      [-mode proto|km] [-cwd DIR] [-extra-arg k=v]...
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
    ap.add_argument("-container", default="unused")
    ap.add_argument("-proto", required=True)
    ap.add_argument("-scenario", required=True)
    ap.add_argument("-log", required=True)
    ap.add_argument("-json", required=True)
    ap.add_argument("-extra-arg", action="append", default=[])
    ap.add_argument("-mode", default="proto", choices=["proto", "km"])
    ap.add_argument("-cwd", default=None)
    ap.add_argument("-timeout", type=float, default=25.0, help="单次 expect 默认超时")
    args = ap.parse_args()

    steps = json.load(open(args.scenario))
    result = {"ok": True, "steps": [], "exit_code": None, "exit_sig": None,
              "termios_equal": None, "signals_sent": [], "error": None}

    pid, fd = pty.fork()
    if pid == 0:
        redir = os.environ.get("PROTO_STDOUT_REDIRECT")
        if redir:
            f = os.open(redir, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
            os.dup2(f, 1)
        if args.cwd:
            os.chdir(os.path.realpath(args.cwd))  # 解析符号链接，保证与 km Getwd 一致
        if args.mode == "km":
            argv = [args.proto, "shell"]
        else:
            argv = [args.proto, "-container", args.container]
            for extra in args.extra_arg:
                k, _, v = extra.partition("=")
                argv += [f"-{k}", v] if v != "" or k == "detach-keys" else [f"-{k}"]
        os.execvp(argv[0], argv)
        os._exit(127)

    # 进入前 termios 基线：紧随 fork 读取（早于 docker 客户端进入 raw mode）
    termios_before = termios.tcgetattr(fd)
    set_winsize(fd, 40, 120)

    buf = b""
    cursor = 0          # 消费游标：expect 只匹配 buf[cursor:]
    log = open(args.log, "wb")
    child_status = None  # 统一收割：None=未退出；(exit, sig) 元组
    abort = False

    def reap():
        """唯一的状态回收点：WNOHANG 收割一次并保存，绝不丢弃退出状态。"""
        nonlocal child_status
        if child_status is not None:
            return True
        try:
            wpid, wstatus = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            return False  # 已由别处收割（不应发生）
        if wpid == pid:
            if os.WIFEXITED(wstatus):
                child_status = (os.WEXITSTATUS(wstatus), None)
            elif os.WIFSIGNALED(wstatus):
                child_status = (None, os.WTERMSIG(wstatus))
            return True
        return False

    def read_avail(timeout):
        """读取当前可用输出；返回是否读到数据。子进程退出不在此判定。"""
        end = time.time() + timeout
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if fd in r:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    return False  # PTY 已关闭（子进程退出后常见）
                if chunk == b"":
                    return False
                nonlocal_buf[0] += chunk
                log.write(chunk)
                log.flush()
                return True
            time.sleep(0.005)
        return False

    # read_avail 需要 rewrite buf：用列表绕开 nonlocal 限制
    nonlocal_buf = [b""]

    def expect(pattern, timeout):
        """在 buf[cursor:] 中等待 pattern。返回 (matched, err)。"""
        nonlocal cursor
        pat = pattern.encode()
        deadline = time.time() + timeout
        while True:
            m = re.search(pat, nonlocal_buf[0][cursor:])
            if m:
                cursor += m.end()
                return True, None
            if reap():
                return False, "child-exited"
            if time.time() > deadline:
                return False, "expect-timeout"
            end = time.time() + 0.2
            got = False
            while time.time() < end:
                r, _, _ = select.select([fd], [], [], 0.05)
                if fd in r:
                    try:
                        chunk = os.read(fd, 65536)
                    except OSError:
                        # Linux：子进程退出后 master 读 EIO；诊断文本常与 EIO
                        # 同批到达缓冲区，先对已累积输出做最后一次匹配再报
                        # EOF（macOS 上 EOF 是空读，不走此分支）。
                        m = re.search(pat, nonlocal_buf[0][cursor:])
                        if m:
                            cursor += m.end()
                            return True, None
                        return False, "pty-eof"
                    if chunk:
                        nonlocal_buf[0] += chunk
                        log.write(chunk)
                        log.flush()
                        got = True
                time.sleep(0.005)
            if not got and reap():
                return False, "child-exited"

    for idx, step in enumerate(steps):
        op = step["op"]
        entry = {"step": idx, "op": op, "ok": True}
        if abort:
            entry["ok"] = False
            entry["error"] = "aborted"
            result["steps"].append(entry)
            continue
        try:
            if op == "send":
                os.write(fd, step["text"].encode())
                entry["sent"] = step["text"]
                time.sleep(0.05)
            elif op == "sendb":
                os.write(fd, base64.b64decode(step["b64"]))
                time.sleep(0.05)
            elif op == "expect":
                ok, err = expect(step["pattern"], step.get("timeout", args.timeout))
                entry["ok"] = ok
                entry["matched"] = ok
                entry["buffer_tail"] = nonlocal_buf[0][-260:].decode("utf-8", "replace")
                if err:
                    entry["error"] = err
                    abort = True
            elif op == "resize":
                set_winsize(fd, step["rows"], step["cols"])
            elif op == "signal":
                os.kill(pid, getattr(signal, step["name"]))
                result["signals_sent"].append(step["name"])
            elif op == "settle":
                end = time.time() + step.get("secs", 0.3)
                while time.time() < end:
                    read_avail(0.1)
            else:
                entry["ok"] = False
                entry["error"] = "unknown-op"
        except Exception as exc:  # noqa: BLE001
            entry["ok"] = False
            entry["error"] = repr(exc)
            abort = True
        if not entry["ok"]:
            result["ok"] = False
        result["steps"].append(entry)

    # 统一等待退出（最多 20s）；收割器保证状态不丢失
    deadline = time.time() + 20
    while time.time() < deadline:
        if reap():
            break
        read_avail(0.1)
    if child_status is None:
        result["ok"] = False
        result["error"] = "child-not-exited"
        try:
            os.kill(pid, signal.SIGKILL)
            _, wstatus = os.waitpid(pid, 0)
            if os.WIFSIGNALED(wstatus):
                result["exit_sig"] = os.WTERMSIG(wstatus)
        except (ProcessLookupError, ChildProcessError):
            pass
    else:
        exit_code, exit_sig = child_status
        result["exit_code"] = exit_code
        result["exit_sig"] = exit_sig

    termios_after = termios.tcgetattr(fd)
    result["termios_equal"] = termios_before == termios_after
    json.dump(result, open(args.json, "w"), ensure_ascii=False, indent=1)
    log.close()


if __name__ == "__main__":
    main()

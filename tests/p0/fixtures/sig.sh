#!/bin/sh
# P0 fixture：可辨识的长任务及其子进程。
# 向 /workspace/sig.log 记录启动、信号捕获与正常退出；收到 INT/TERM 时
# 杀掉子进程并退出 130，供容器内残留检查核对。
echo "start pid=$$ ppid=$PPID" >> /workspace/sig.log
trap 'echo "TRAP pid=$$ time=$(date +%s)" >> /workspace/sig.log; kill "$pid" 2>/dev/null; exit 130' INT TERM
sleep 300 &
pid=$!
echo "child=$pid" >> /workspace/sig.log
wait "$pid"
echo "normal-exit pid=$$" >> /workspace/sig.log

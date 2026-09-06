#!/bin/bash
# P0 实验重跑器：argv/stdio/退出码/路径/挂载/持久化/信号/PTY/Kali 基线。
#
# 用法: bash tests/p0/run-p0.sh [工具镜像] [kali镜像]
#
# 原始输出写入 tests/p0/evidence/<run-id>/，实验容器与临时项目在脚本内
# 清理并输出核对结果；证据目录保留在仓库中（不提交临时项目文件）。
# 只创建带唯一 RUN_ID 标签的资源，不触碰用户其他容器。
set -u
TOOL_IMAGE="${1:-docker.1ms.run/library/busybox:stable}"
KALI_IMAGE="${2:-docker.1ms.run/kalilinux/kali-rolling:latest}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RUN_ID="$(date +%Y%m%d-%H%M%S)"
EV="$REPO_ROOT/tests/p0/evidence/$RUN_ID"
PROJ="$(mktemp -d "${TMPDIR:-/tmp}/km-p0-${RUN_ID}-XXXXXX")"
mkdir -p "$EV"

CONTAINER="km-p0-${RUN_ID}-c1"
KALI_CONTAINER="km-p0-${RUN_ID}-k1"

cleanup() {
  docker rm -f "$CONTAINER" "$KALI_CONTAINER" >/dev/null 2>&1
  rm -rf "$PROJ"
}
trap cleanup EXIT

fail() { echo "FATAL: $*" >&2; exit 1; }

# ---------- 准备 ----------
mkdir -p "$PROJ/中文 目录/子 目"
cp "$REPO_ROOT/tests/p0/fixtures/argv.sh" "$PROJ/argv.sh"
cp "$REPO_ROOT/tests/p0/fixtures/sig.sh" "$PROJ/sig.sh"
chmod +x "$PROJ"/*.sh
printf 'mac 初始内容\n' > "$PROJ/中文 目录/hello.txt"
head -c 65536 /dev/urandom > "$PROJ/bin.in"
MAC_HASH="$(shasum -a 256 "$PROJ/bin.in" | awk '{print $1}')"

{
  echo "# 运行环境（run-id=${RUN_ID}）"
  sw_vers
  uname -m
  go version
  docker version
  echo "当前 context: $(docker context show)"
  echo "context endpoint: $(docker context inspect --format '{{.Endpoints.docker.Host}}' "$(docker context show)")"
  echo "工具镜像: $(docker image inspect --format '{{.Id}}' "$TOOL_IMAGE" 2>/dev/null || echo 缺失)"
  echo "Kali镜像: $(docker image inspect --format '{{.Id}}' "$KALI_IMAGE" 2>/dev/null || echo 缺失)"
  echo "输入 fixture bin.in sha256 = $MAC_HASH"
} > "$EV/00-env.log" 2>&1 || fail "环境记录失败"

docker run -d --name "$CONTAINER" \
  --label km.owner=km-p0-test --label km.project="$RUN_ID" \
  -v "$PROJ":/workspace "$TOOL_IMAGE" sleep 10800 > "$EV/01-container-create.log" 2>&1 \
  || fail "实验容器创建失败（镜像=$TOOL_IMAGE）"
C="$CONTAINER"

# ---------- 10 · argv / stdio / 退出码（E1–E6）----------
{
  echo "===== E1: argv 复杂参数（空/空格/中文/引号/前导-）====="
  docker exec "$C" /workspace/argv.sh "" "a b" "中文参数" 'he said "hi"' --help -x "5 * ?"
  echo "E1 exit=$?"
  echo "===== E2: 工具自身的 --help 留给工具 ====="
  docker exec "$C" /workspace/argv.sh --help
  echo "E2 exit=$?"
  echo "===== E3: 二进制 stdin/stdout 往返哈希（完整 64 位）====="
  echo "mac 侧: $MAC_HASH"
  echo "容器侧: $(docker exec -i "$C" cat < "$PROJ/bin.in" | shasum -a 256 | awk '{print $1}')"
  echo "E3 exit=$?"
  echo "===== E4: stdout/stderr 分离捕获 ====="
  docker exec "$C" sh -c 'echo OUT_LINE; echo ERR_LINE >&2' >"$PROJ/e4.out" 2>"$PROJ/e4.err"
  echo "E4 exit=$? stdout=[$(cat "$PROJ/e4.out")] stderr=[$(cat "$PROJ/e4.err")]"
  echo "===== E5: 退出码 0/7/42 ====="
  docker exec "$C" true;          echo "true exit=$?"
  docker exec "$C" sh -c 'exit 7';  echo "exit7 exit=$?"
  docker exec "$C" sh -c 'exit 42'; echo "exit42 exit=$?"
  echo "===== E6: 容器内命令不存在 ====="
  docker exec "$C" no-such-cmd-zzz 2>&1 | head -1
  docker exec "$C" no-such-cmd-zzz >/dev/null 2>&1; echo "E6 exit=$?（期望 127）"
} > "$EV/10-argv-stdio-exit.log" 2>&1

# ---------- 11 · 路径 / 挂载 / 持久化（E7–E9）----------
{
  echo "===== E7: 中文/空格路径 + 子目录 cwd ====="
  docker exec -w "/workspace/中文 目录" "$C" /bin/sh -c 'pwd; cat hello.txt'
  echo "E7a exit=$?"
  docker exec -w "/workspace/中文 目录/子 目" "$C" pwd
  echo "E7b exit=$?"
  echo "===== E8: 容器写 → Mac 读（双向）====="
  docker exec "$C" /bin/sh -c 'echo "容器侧写入" >> "/workspace/中文 目录/hello.txt"'
  echo "--- Mac 侧文件内容 ---"; cat "$PROJ/中文 目录/hello.txt"
  echo "E8 exit=$?"
  echo "===== E9a: 容器创建新文件，Mac 可编辑 ====="
  docker exec "$C" sh -c 'echo "容器生成的数据行1" > /workspace/created_in_container.txt'
  echo "Mac 追加编辑：" >> "$PROJ/created_in_container.txt"
  docker exec "$C" cat /workspace/created_in_container.txt
  echo "E9a exit=$?"
  echo "===== E9b: stop 再 start，数据与执行恢复 ====="
  docker stop "$C" >/dev/null; echo "stop exit=$?"
  docker start "$C" >/dev/null; echo "start exit=$?"
  docker exec "$C" cat /workspace/created_in_container.txt
  echo "E9b exit=$?"
  echo "===== E9c: Mac 侧文件完好 ====="
  cat "$PROJ/created_in_container.txt"
} > "$EV/11-path-mount-persist.log" 2>&1

# ---------- 12 · 非 PTY 信号残留（E10–E11）----------
sig_client() {
  local label="$1" sig="$2"; shift 2
  : > "$PROJ/sig.log"
  docker exec "$@" "$C" /workspace/sig.sh >/dev/null 2>&1 &
  local dp=$!
  sleep 2
  echo "--- 启动后 sig.log ---"; cat "$PROJ/sig.log"
  kill "-$sig" "$dp" 2>/dev/null || kill -9 "$dp"
  wait "$dp" 2>/dev/null; echo "客户端($dp) 收到 $sig 后 wait 退出码=$?"
  sleep 3
  echo "--- 动作后 sig.log（出现 TRAP = 信号到达容器内任务）---"; cat "$PROJ/sig.log"
  echo "--- 容器内残留 ---"
  docker exec "$C" ps -o pid,ppid,stat,comm 2>/dev/null | grep 'sig\.sh' || echo "(无)"
}
{
  echo "===== E10a: 非 PTY（无 -i -t），docker exec 客户端被 SIGKILL ====="
  sig_client E10a KILL
  echo "===== E10b: 非 PTY（无 -i -t），docker exec 客户端被 SIGINT ====="
  sig_client E10b INT
  echo "===== E11: -i 无 -t，docker exec 客户端被 SIGINT ====="
  sig_client E11 INT -i
  echo "结论判据：sig.log 无 TRAP 且 sig.sh/sleep 进程仍在 = 客户端死亡不传播信号"
} > "$EV/12-signals-nonpty.log" 2>&1

# ---------- 13 · docker stop 兜底 + 数据保留（E14）----------
{
  echo "===== E14a: stop 前容器内进程（上一步残留仍在）====="
  docker exec "$C" ps -o pid,ppid,stat,comm 2>/dev/null
  echo "===== E14b: docker stop（计时）====="
  docker stop "$C" >/dev/null & STPID=$!
  wait $STPID; echo "stop exit=$?"
  START_TS=$(date +%s)
  docker start "$C" >/dev/null; echo "start exit=$?"
  sleep 1
  echo "===== E14c: stop 后容器内进程（应仅剩 PID1 与 ps）====="
  docker exec "$C" ps -o pid,ppid,stat,comm 2>/dev/null
  echo "===== E14d: 挂载数据完好 ====="
  docker exec "$C" cat /workspace/created_in_container.txt
  echo "stop 耗时参考：PID1 为 sleep（不处理 SIGTERM）时通常等满 10s 宽限期"
} > "$EV/13-stop-cleanup-persist.log" 2>&1

# ---------- 14 · PTY 信号（E12–E13）----------
for mode in ctrl_c sigint sigkill; do
  python3 "$REPO_ROOT/tests/p0/pty_driver.py" "$mode" "$C" "$PROJ" \
    > "$EV/14-pty-$mode.log" 2>&1
done

# ---------- 15 · Kali 基线（K1）----------
{
  if docker image inspect "$KALI_IMAGE" >/dev/null 2>&1; then
    echo "===== K1: Kali 基线 ====="
    echo "镜像 ID: $(docker image inspect --format '{{.Id}}' "$KALI_IMAGE")"
    docker run -d --name "$KALI_CONTAINER" \
      --label km.owner=km-p0-test --label km.project="$RUN_ID" \
      -v "$PROJ":/workspace "$KALI_IMAGE" sleep 3600 >/dev/null || { echo "kali 容器创建失败"; exit 0; }
    docker exec "$KALI_CONTAINER" /bin/sh -c 'head -2 /etc/os-release; uname -m'
    echo "--- 候选工具预装情况 ---"
    for t in python3 curl jq nmap file openssl; do
      docker exec "$KALI_CONTAINER" which "$t" >/dev/null 2>&1 && echo "$t: 有" || echo "$t: 无"
    done
    echo "--- apt 源可达性（限时 25s，仅观察）---"
    docker exec "$KALI_CONTAINER" timeout 25 apt-get update 2>&1 | tail -2
  else
    echo "SKIPPED: kali 镜像不在本地: $KALI_IMAGE"
  fi
} > "$EV/15-kali-baseline.log" 2>&1

# ---------- 16 · 清理与核对 ----------
{
  echo "===== 清理（只处理本 run-id 资源）====="
  docker rm -f "$CONTAINER" "$KALI_CONTAINER" 2>&1
  rm -f "$PROJ/sig.log" "$PROJ/e4.out" "$PROJ/e4.err"
  echo "--- 残留核对（按 label km.project=${RUN_ID}，应无输出）---"
  docker ps -a --filter "label=km.project=$RUN_ID" --format '{{.Names}}'
  echo "残留数: $(docker ps -a --filter "label=km.project=$RUN_ID" -q | wc -l | tr -d ' ')"
  echo "--- 临时项目最终文件清单（证据摘要用）---"
  ls -la "$PROJ"
  echo "bin.in sha256 = $(shasum -a 256 "$PROJ/bin.in" | awk '{print $1}')（应与 $MAC_HASH 一致）"
} > "$EV/16-cleanup-verification.log" 2>&1

echo "P0 实验完成，证据目录: $EV"
echo "概览:"
grep -l . "$EV"/*.log | while read -r f; do echo "  $(basename "$f")"; done

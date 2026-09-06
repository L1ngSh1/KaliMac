#!/bin/bash
# P0 可判定实验运行器：在 run-p0.sh 的实验矩阵上加显式断言与结构化汇总。
#
# 用法:
#   bash tests/p0/checked/run-checked.sh            # 正常运行，全 PASS 退出 0
#   CHECKED_NEGATIVE=1 bash ... run-checked.sh      # 负向控制：注入错误期望，
#                                                   # 运行器必须非零退出（自检失败检测能力）
#
# 分类：NORMAL=正常验收；EXPECT_NONZERO=预期非零退出；RISK_OBSERVE=预期风险观察
# （docker exec 客户端死亡不传播信号——若风险行为消失，用例 FAIL，提示语义变化）。
#
# 断言一律基于显式输出文件（写入 $PROJ），不解析日志文本；命令退出码与断言分离。
# 证据：tests/p0/evidence/checked-<run-id>/（每用例独立日志 + summary.json + summary.txt）。
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

TOOL_IMAGE="${CHECKED_TOOL_IMAGE:-docker.1ms.run/library/busybox:stable}"
KALI_IMAGE="${CHECKED_KALI_IMAGE:-docker.1ms.run/kalilinux/kali-rolling:latest}"
NEGATIVE="${CHECKED_NEGATIVE:-0}"
REPO_ROOT="$(cd "$HERE/../../.." && pwd)"
RUN_ID="checked-$(date +%Y%m%d-%H%M%S)"
RUN_STARTED_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
EV_DIR="$REPO_ROOT/tests/p0/evidence/$RUN_ID"
PROJ="$(mktemp -d "${TMPDIR:-/tmp}/km-checked-${RUN_ID}-XXXXXX")"
mkdir -p "$EV_DIR"

CONTAINER="km-checked-${RUN_ID}-c1"
KALI_CONTAINER="km-checked-${RUN_ID}-k1"
WRONG_SHA="0000000000000000000000000000000000000000000000000000000000000000"

cleanup() {
  docker rm -f "$CONTAINER" "$KALI_CONTAINER" >/dev/null 2>&1
  rm -rf "$PROJ"
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || { echo "FATAL: Docker 引擎不可达" >&2; exit 2; }

assert_ge() { # 名 最小值 实际值
  if [ "$3" -ge "$2" ] 2>/dev/null; then assert_ok "$1"; else assert_fail "$1" "≥$2" "${3:-空}"; fi
}

# ---------- 准备 ----------
mkdir -p "$PROJ/中文 目录/子 目"
cp "$REPO_ROOT/tests/p0/fixtures/argv.sh" "$PROJ/argv.sh"
cp "$REPO_ROOT/tests/p0/fixtures/sig.sh" "$PROJ/sig.sh"
chmod +x "$PROJ"/*.sh
printf 'mac 初始内容\n' > "$PROJ/中文 目录/hello.txt"
head -c 65536 /dev/urandom > "$PROJ/bin.in"
INPUT_SHA=$(shasum -a 256 "$PROJ/bin.in" | awk '{print $1}')
IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$TOOL_IMAGE" 2>/dev/null || echo "缺失")

docker run -d --name "$CONTAINER" \
  --label km.owner=km-p0-test --label "km.project=$RUN_ID" \
  -v "$PROJ":/workspace "$TOOL_IMAGE" sleep 10800 > "$EV_DIR/00-create.log" 2>&1 \
  || { echo "FATAL: 实验容器创建失败" >&2; exit 2; }
C="$CONTAINER"

# ---------- 用例 ----------
case_begin argv-passthrough NORMAL "argv 复杂参数逐元素透传（空/空格/中文/引号/前导-）"
RUN_EVAL "docker exec $C /workspace/argv.sh '' 'a b' '中文参数' 'he said \"hi\"' --help -x '5 * ?' > '$PROJ/argv.out'"
assert_eq argv-退出码 "0" "$CUR_CMD_EXIT"
EXPECTED_ARGV=$'1[]\n2[a b]\n3[中文参数]\n4[he said "hi"]\n5[--help]\n6[-x]\n7[5 * ?]'
assert_eq argv-7行逐字匹配 "$EXPECTED_ARGV" "$(cat "$PROJ/argv.out" 2>/dev/null)"

case_begin tool-help-flag NORMAL "工具自身的 --help 留给工具"
RUN_EVAL "docker exec $C /workspace/argv.sh --help > '$PROJ/help.out'"
assert_eq help-退出码 "0" "$CUR_CMD_EXIT"
assert_eq help留给工具 "1[--help]" "$(cat "$PROJ/help.out" 2>/dev/null)"

case_begin binary-roundtrip NORMAL "二进制 stdin/stdout 往返哈希（完整64位）"
RUN_EVAL "docker exec -i $C cat < '$PROJ/bin.in' > '$PROJ/bin.out'"
assert_eq 往返退出码 "0" "$CUR_CMD_EXIT"
if [ "$NEGATIVE" = "1" ]; then
  # 负向控制：故意用错误哈希断言，运行器必须判 FAIL 并非零退出
  assert_sha 输出哈希等于输入-负向错误期望 "$PROJ/bin.out" "$WRONG_SHA"
else
  assert_sha 输出哈希等于输入 "$PROJ/bin.out" "$INPUT_SHA"
fi

case_begin stderr-separation NORMAL "stdout/stderr 分离捕获互不污染"
RUN_EVAL "docker exec $C sh -c 'echo OUT_LINE; echo ERR_LINE >&2' >'$PROJ/e4.out' 2>'$PROJ/e4.err'"
assert_eq 分离退出码 "0" "$CUR_CMD_EXIT"
assert_eq stdout内容 "OUT_LINE" "$(cat "$PROJ/e4.out" 2>/dev/null)"
assert_eq stderr内容 "ERR_LINE" "$(cat "$PROJ/e4.err" 2>/dev/null)"

case_begin exit-codes EXPECT_NONZERO "退出码 0/7/42 原样返回（7/42 为预期非零）"
RUN_EVAL "docker exec $C true"
assert_eq true退出码 "0" "$CUR_CMD_EXIT"
RUN_EVAL "docker exec $C sh -c 'exit 7'"
assert_eq exit7退出码 "7" "$CUR_CMD_EXIT"
RUN_EVAL "docker exec $C sh -c 'exit 42'"
assert_eq exit42退出码 "42" "$CUR_CMD_EXIT"

case_begin exec-missing EXPECT_NONZERO "容器内命令不存在 → 127"
RUN_EVAL "docker exec $C no-such-cmd-zzz >/dev/null 2>&1"
assert_eq 缺失命令退出码 "127" "$CUR_CMD_EXIT"

case_begin paths-persistence NORMAL "中文/空格路径、子目录 cwd、双向读写与 stop/start 持久化"
RUN_EVAL "docker exec -w '/workspace/中文 目录/子 目' $C pwd > '$PROJ/cwd.out'"
assert_eq 子目录cwd "/workspace/中文 目录/子 目" "$(cat "$PROJ/cwd.out" 2>/dev/null)"
RUN_EVAL "docker exec $C /bin/sh -c 'echo 容器侧写入 >> \"/workspace/中文 目录/hello.txt\"; echo 容器生成数据 > /workspace/created.txt'"
assert_eq 容器写退出码 "0" "$CUR_CMD_EXIT"
RUN_EVAL "docker stop $C >/dev/null && docker start $C >/dev/null"
assert_eq stopstart退出码 "0" "$CUR_CMD_EXIT"
RUN_EVAL "docker exec $C cat /workspace/created.txt > '$PROJ/created.readback'"
assert_eq stop后数据保留 "容器生成数据" "$(cat "$PROJ/created.readback" 2>/dev/null)"
assert_contains mac侧追加可见 "容器侧写入" "$(cat "$PROJ/中文 目录/hello.txt")"

case_begin signal-nonpty-residual RISK_OBSERVE "非 PTY 下杀死 docker exec 客户端：容器内任务应残留（已记录风险）"
RUN_EVAL "rm -f '$PROJ/sig.log'; docker exec $C /workspace/sig.sh >/dev/null 2>&1 & CPID=\$!; sleep 2; kill -KILL \$CPID; wait \$CPID 2>/dev/null; sleep 3; cat '$PROJ/sig.log' > '$PROJ/sig.readback' 2>/dev/null || true"
assert_contains 客户端已启动任务 "start pid=" "$(cat "$PROJ/sig.readback" 2>/dev/null)"
if grep -q "TRAP pid=" "$PROJ/sig.readback" 2>/dev/null; then
  assert_fail 风险观察-信号未到达容器任务 "无TRAP" "出现TRAP（docker语义已变化，需重新评估）"
else
  assert_ok 风险观察-信号未到达容器任务
fi
RUN_EVAL "docker exec $C ps -o pid,stat,comm 2>/dev/null | grep -c 'sig\\.sh' > '$PROJ/leftover1.txt' || true"
LEFTOVER=$(cat "$PROJ/leftover1.txt" 2>/dev/null)
assert_ge 风险观察-残留进程仍在 1 "${LEFTOVER:-0}"

case_begin pty-ctrlc NORMAL "PTY 中键入 Ctrl-C：信号到达容器任务，客户端退出码 130"
RUN_EVAL "python3 '$HERE/../pty_driver.py' ctrl_c '$C' '$PROJ' > '$PROJ/pty-ctrlc.out'"
assert_contains TRAP出现 "TRAP pid=" "$(cat "$PROJ/pty-ctrlc.out" 2>/dev/null)"
assert_contains 客户端退出码130 "exitcode=130" "$(cat "$PROJ/pty-ctrlc.out" 2>/dev/null)"

case_begin pty-sigkill RISK_OBSERVE "PTY 下客户端被 SIGKILL：无信号传播（已记录风险）"
RUN_EVAL "python3 '$HERE/../pty_driver.py' sigkill '$C' '$PROJ' > '$PROJ/pty-sigkill.out'"
if grep -q 'TRAP pid=' "$PROJ/pty-sigkill.out" 2>/dev/null; then
  assert_fail 风险观察-无TRAP "无TRAP" "出现TRAP"
else
  assert_ok 风险观察-无TRAP
fi
RUN_EVAL "docker exec $C ps -o pid,stat,comm 2>/dev/null | grep -c 'sig\\.sh' > '$PROJ/leftover2.txt' || true"
LEFTOVER2=$(cat "$PROJ/leftover2.txt" 2>/dev/null)
assert_ge 风险观察-残留进程仍在 1 "${LEFTOVER2:-0}"

case_begin kali-baseline NORMAL "Kali 基线：候选工具未预装（镜像缺失则 SKIP）"
if docker image inspect "$KALI_IMAGE" >/dev/null 2>&1; then
  RUN_EVAL "docker run -d --name $KALI_CONTAINER --label km.owner=km-p0-test --label 'km.project=$RUN_ID' '$KALI_IMAGE' sleep 900 >/dev/null && docker exec $KALI_CONTAINER /bin/sh -c 'head -2 /etc/os-release; uname -m; for t in python3 curl jq nmap file openssl; do command -v \$t >/dev/null 2>&1 && echo \"\$t: 有\" || echo \"\$t: 无\"; done' > '$PROJ/kali.out'"
  assert_eq kali容器创建退出码 "0" "$CUR_CMD_EXIT"
  assert_contains kali发行版 "Kali GNU/Linux" "$(cat "$PROJ/kali.out" 2>/dev/null)"
  assert_contains 无预装python "python3: 无" "$(cat "$PROJ/kali.out" 2>/dev/null)"
  assert_contains 无预装nmap "nmap: 无" "$(cat "$PROJ/kali.out" 2>/dev/null)"
else
  skip_case "kali 镜像不在本地: $KALI_IMAGE"
fi

# ---------- 清理与核对（手工构造用例，避免 trap 重复执行）----------
case_flush
docker rm -f "$CONTAINER" "$KALI_CONTAINER" >/dev/null 2>&1
LEFT=$(docker ps -a --filter "label=km.project=$RUN_ID" -q | wc -l | tr -d ' ')
FINAL_SHA=$(shasum -a 256 "$PROJ/bin.in" | awk '{print $1}')
CLLOG="$EV_DIR/case-cleanup-verification.log"
echo "cleanup 残留容器数=$LEFT; bin.in sha256=$FINAL_SHA (期望 $INPUT_SHA)" > "$CLLOG"
{
  if [ "$LEFT" = "0" ]; then echo "ASSERT-OK 零残留"; else echo "ASSERT-FAIL 零残留: 期望=0 实际=$LEFT"; fi
  if [ "$FINAL_SHA" = "$INPUT_SHA" ]; then echo "ASSERT-OK 哈希不变"; else echo "ASSERT-FAIL 哈希不变: 期望=$INPUT_SHA 实际=$FINAL_SHA"; fi
} >> "$CLLOG"
if [ "$LEFT" = "0" ] && [ "$FINAL_SHA" = "$INPUT_SHA" ]; then CL_RESULT=PASS; else CL_RESULT=FAIL; fi
CASE_IDS+=("cleanup-verification"); CASE_CLASSES+=("NORMAL"); CASE_RESULTS+=("$CL_RESULT")
CASE_EXITS+=("0"); CASE_DURS+=("0"); CASE_LOGS+=("$CLLOG"); CASE_DESCS+=("清理核对：零残留+哈希不变")

summary_emit "$TOOL_IMAGE" "$IMAGE_ID" "$INPUT_SHA" "$RUN_ID" "$NEGATIVE" "$NEGATIVE"
summary_exit_code
code=$?
echo "运行器退出码: ${code}（模式: $( [ "$NEGATIVE" = 1 ] && echo 负向控制-期望非零 || echo 正常-期望零 )）"
exit $code

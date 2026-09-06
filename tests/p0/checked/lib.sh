#!/bin/bash
# 可判定断言运行器库：命令退出状态与断言结果分离记录。
#
# 概念：
#   CASE <id> <class> <desc...>   开始用例；class ∈ NORMAL|EXPECT_NONZERO|RISK_OBSERVE
#   RUN_EVAL "<shell 片段>"        执行被测命令；退出码记入 CMD_EXIT（不与断言混用）
#   SKIP_CASE "<原因>"            标记跳过（如镜像缺失、引擎离线）
#   ASSERT_EQ <名> <期望> <实际>   字符串完全相等
#   ASSERT_CONTAINS <名> <子串> <全文>
#   ASSERT_SHA <名> <文件> <期望sha256>  完整 64 位哈希比对
#
# 结果：PASS / FAIL / SKIP；RISK_OBSERVE 用例的 PASS 表示“观察到已记录的
# 风险行为仍在”（例如 docker exec 客户端死亡不传播信号），行为改变会判 FAIL。
set -u

RESULTS_TSV=""    # id, class, result, cmd_exit, duration_ms, log

now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }
CASE_IDS=(); CASE_CLASSES=(); CASE_RESULTS=(); CASE_EXITS=(); CASE_DURS=(); CASE_LOGS=(); CASE_DESCS=()
CUR_ID=""; CUR_CLASS=""; CUR_DESC=""; CUR_LOG=""; CUR_START=0; CUR_ASSERT_FAIL=0; CUR_CMD_EXIT=""

case_begin() {
  case_flush
  CUR_ID="$1"; CUR_CLASS="$2"; shift 2
  CUR_DESC="$*"
  CUR_LOG="$EV_DIR/case-$CUR_ID.log"
  : > "$CUR_LOG"
  CUR_START=$(now_ms)
  CUR_ASSERT_FAIL=0
  CUR_CMD_EXIT=""
  {
    echo "## CASE $CUR_ID ($CUR_CLASS): $CUR_DESC"
    echo "## started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >> "$CUR_LOG"
}

case_flush() {
  [ -z "$CUR_ID" ] && return 0
  local result="PASS" end dur
  [ "$CUR_ASSERT_FAIL" -gt 0 ] && result="FAIL"
  end=$(now_ms)
  dur=$(( end - CUR_START ))
  {
    echo "## finished: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "## result: $result (cmd_exit=${CUR_CMD_EXIT:-n/a}, assertion_failures=$CUR_ASSERT_FAIL)"
  } >> "$CUR_LOG"
  CASE_IDS+=("$CUR_ID"); CASE_CLASSES+=("$CUR_CLASS"); CASE_RESULTS+=("$result")
  CASE_EXITS+=("${CUR_CMD_EXIT:-}"); CASE_DURS+=("$dur"); CASE_LOGS+=("$CUR_LOG"); CASE_DESCS+=("$CUR_DESC")
  CUR_ID=""
}

skip_case() {
  {
    echo "## SKIPPED: $1"
    echo "## finished: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >> "$CUR_LOG"
  end=$(now_ms)
  local dur=$(( end - CUR_START ))
  CASE_IDS+=("$CUR_ID"); CASE_CLASSES+=("$CUR_CLASS"); CASE_RESULTS+=("SKIP")
  CASE_EXITS+=(""); CASE_DURS+=("$dur"); CASE_LOGS+=("$CUR_LOG"); CASE_DESCS+=("$CUR_DESC")
  echo "SKIP $CUR_ID: $1" | tee -a "$CUR_LOG" >&2
  CUR_ID=""
}

assert_fail() {
  echo "ASSERT-FAIL $1: 期望=[$2] 实际=[$3]" >> "$CUR_LOG"
  CUR_ASSERT_FAIL=$(( CUR_ASSERT_FAIL + 1 ))
}

assert_ok() {
  echo "ASSERT-OK $1" >> "$CUR_LOG"
}

assert_eq() {
  if [ "$2" = "$3" ]; then assert_ok "$1"; else assert_fail "$1" "$2" "$3"; fi
}

assert_contains() {
  case "$3" in
    *"$2"*) assert_ok "$1" ;;
    *) assert_fail "$1" "*$2*" "(不匹配)" ;;
  esac
}

assert_sha() {
  local actual
  actual=$(shasum -a 256 "$2" 2>/dev/null | awk '{print $1}')
  if [ "$actual" = "$3" ]; then assert_ok "$1 ($2)"; else assert_fail "$1" "$3" "${actual:-读取失败}"; fi
}

# RUN_EVAL "<shell 片段>"：在子 shell 执行，stdout/stderr 追加到用例日志。
# CMD_EXIT 只保留被测命令的退出码；断言永远不使用该值以外的来源。
RUN_EVAL() {
  echo "\$ ${1}" >> "$CUR_LOG"
  bash -c "$1" >> "$CUR_LOG" 2>&1
  local code=$?
  CUR_CMD_EXIT=$code
  echo "[cmd exit=$code]" >> "$CUR_LOG"
  return 0
}

# 汇总输出；$1=镜像引用 $2=镜像ID $3=输入哈希 $4=run-id $5=负向控制标记
summary_emit() {
  local total pass fail skip i id cls res ex dur log desc
  total=${#CASE_IDS[@]}; pass=0; fail=0; skip=0
  for res in "${CASE_RESULTS[@]}"; do
    case "$res" in PASS) pass=$((pass+1));; FAIL) fail=$((fail+1));; SKIP) skip=$((skip+1));; esac
  done
  {
    echo "P0-CHECKED 汇总: PASS=$pass FAIL=$fail SKIP=$skip (共 $total)"
    i=0
    while [ $i -lt $total ]; do
      printf '  %-4s %-14s %-24s cmd_exit=%-4s %sms\n' "${CASE_RESULTS[$i]}" "${CASE_CLASSES[$i]}" "${CASE_IDS[$i]}" "${CASE_EXITS[$i]:--}" "${CASE_DURS[$i]}"
      i=$((i+1))
    done
  } | tee "$EV_DIR/summary.txt"

  # 结构化 JSON（字段受控，不引入任意转义需求）
  {
    printf '{\n'
    printf '  "run_id": "%s",\n' "$5"
    printf '  "negative_control": %s,\n' "$6"
    printf '  "started": "%s",\n' "$RUN_STARTED_UTC"
    printf '  "finished": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '  "tool_image": {"ref": "%s", "id": "%s"},\n' "$1" "$2"
    printf '  "input_sha256": "%s",\n' "$3"
    printf '  "totals": {"pass": %d, "fail": %d, "skip": %d},\n' "$pass" "$fail" "$skip"
    printf '  "cases": [\n'
    i=0
    while [ $i -lt $total ]; do
      id="${CASE_IDS[$i]}"; cls="${CASE_CLASSES[$i]}"; res="${CASE_RESULTS[$i]}"
      ex="${CASE_EXITS[$i]}"; dur="${CASE_DURS[$i]}"; log="${CASE_LOGS[$i]}"; desc="${CASE_DESCS[$i]}"
      [ -z "$ex" ] && ex=null
      printf '    {"id": "%s", "class": "%s", "desc": "%s", "result": "%s", "cmd_exit": %s, "duration_ms": %d, "log": "%s"}%s\n' \
        "$id" "$cls" "$desc" "$res" "$ex" "$dur" "$log" "$([ $((i+1)) -lt $total ] && echo ,)"
      i=$((i+1))
    done
    printf '  ]\n}\n'
  } > "$EV_DIR/summary.json"
}

summary_exit_code() {
  local res fail=0 pass=0 skip=0
  for res in "${CASE_RESULTS[@]}"; do
    case "$res" in FAIL) fail=$((fail+1));; PASS) pass=$((pass+1));; SKIP) skip=$((skip+1));; esac
  done
  if [ "$fail" -gt 0 ]; then return 1; fi
  if [ "$pass" -eq 0 ]; then return 2; fi
  return 0
}

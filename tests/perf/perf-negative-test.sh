#!/bin/bash
# perf-baseline.sh 的负向回归（goal M4）：
#   1) init 失败（注入 stub km）→ FATAL 非零退出，不产出任何 baseline.json；
#   2) 热调用注入失败（PERF_INJECT_WARM_FAIL=1，真实 km 跑 /bin/false 透传退出码 1）
#      → 失败样本不进统计（有效样本=0），success=false，脚本非零退出，
#        且产物基线不得标记 success=true。
# 需要本机 Docker 引擎与 kali-mac-min 镜像（同 perf-baseline.sh 前提）。
set -u
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
PERF="$REPO/tests/perf/perf-baseline.sh"
KM="$REPO/bin/km"
[ -x "$KM" ] || { echo "FATAL: 先 make build" >&2; exit 2; }
docker info >/dev/null 2>&1 || { echo "FATAL: docker 引擎不可达，负向验证未执行" >&2; exit 2; }

fail=0

echo "== 负向 1：init 失败 =="
STUB="$(mktemp -d)/km"
printf '#!/bin/sh\necho "stub: init 失败" >&2\nexit 1\n' > "$STUB"
chmod +x "$STUB"
out="$(PERF_KM_BIN="$STUB" PERF_ROUNDS=1 PERF_SAMPLES=2 bash "$PERF" 2>&1)"
rc=$?
echo "$out" | tail -2
[ "$rc" -ne 0 ] || { echo "FAIL: init 失败应非零退出 (rc=$rc)"; fail=1; }
# stub init 失败时不落 state，脚本应在 init 阶段退出；最新 evidence 目录不应有 success 基线
latest="$(ls -td "$REPO"/tests/perf/evidence/*/ 2>/dev/null | head -1)"
if [ -f "$latest/baseline.json" ] && grep -q '"success": true' "$latest/baseline.json"; then
  echo "FAIL: init 失败不得产出成功基线: $latest"; fail=1
else
  echo "OK: init 失败无非零退出码且无成功基线 (rc=$rc)"
fi

echo "== 负向 2：热调用注入失败 =="
out="$(PERF_INJECT_WARM_FAIL=1 PERF_ROUNDS=1 PERF_SAMPLES=3 bash "$PERF" 2>&1)"
rc=$?
echo "$out" | tail -3
[ "$rc" -ne 0 ] || { echo "FAIL: 注入失败应非零退出 (rc=$rc)"; fail=1; }
latest="$(ls -td "$REPO"/tests/perf/evidence/*/ | head -1)"
python3 - "$latest" <<'PY' || fail=1
import json, sys
with open(sys.argv[1] + "/baseline.json") as f:
    b = json.load(f)
assert b["success"] is False, f"success 应为 false: {b['success']}"
assert b["injected_warm_fail"] is True
assert b["warm_km_valid_samples"] == 0, f"失败样本不得进入统计: {b['warm_km_valid_samples']}"
assert b["failures"], "应记录失败样本"
assert b["p50_warm_km_ms"] is None, "失败样本不得产生统计值"
print("OK: 失败样本不进统计、success=false")
PY
[ "$fail" -eq 0 ] && echo "PERF-NEGATIVE-PASS" || echo "PERF-NEGATIVE-FAIL"

# 资源残留断言：上述两轮运行不得留下任何 km-perf-* 容器
leftover="$(docker ps -a --format '{{.ID}} {{.Names}}' | grep 'km-perf-' || true)"
if [ -n "$leftover" ]; then
  echo "FAIL: perf 运行残留容器:"; echo "$leftover"
  exit 1
fi
echo "OK: 无 km-perf-* 残留容器"
exit "$fail"

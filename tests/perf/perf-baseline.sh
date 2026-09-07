#!/bin/bash
# P3 性能基线：首次准备 / 冷启动 / 热调用，与直接 docker exec 对比。
# 输出 tests/perf/evidence/<run-id>/baseline.json + baseline.txt
set -u
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
KM="$REPO/bin/km"
[ -x "$KM" ] || { echo "先 make build" >&2; exit 2; }
RUN_ID="$(date +%Y%m%d-%H%M%S)"
EV="$REPO/tests/perf/evidence/$RUN_ID"
mkdir -p "$EV"
PROJ="$(mktemp -d "${TMPDIR:-/tmp}/km-perf-${RUN_ID}-XXXXXX")"
IMAGE="${PERF_IMAGE:-kali-mac-min:0.2}"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1; rm -rf "$PROJ"; }
trap cleanup EXIT

docker info >/dev/null 2>&1 || { echo "FATAL: 引擎不可达" >&2; exit 2; }

# 1) 首次准备（init 含镜像已存在/容器创建；镜像下载单独计时）
T0=$(python3 -c 'import time;print(time.time())')
cd "$PROJ"
echo "{\"schema_version\":1,\"image\":\"$IMAGE\"}" > .km.json
if ! "$KM" init > "$EV/init.log" 2>&1; then
  echo "FATAL: init 失败，日志：" >&2
  cat "$EV/init.log" >&2
  exit 2
fi
T1=$(python3 -c 'import time;print(time.time())')
INIT_MS=$(python3 -c "print(int(($T1-$T0)*1000))")
CONTAINER=$(python3 -c "import json;print(json.load(open('.km/state.json'))['container']['id'])")

# 2) 冷启动（stop 后第一次 run：含容器 start）
"$KM" stop >/dev/null 2>&1
T2=$(python3 -c 'import time;print(time.time())')
"$KM" run -- /bin/true >/dev/null 2>&1
T3=$(python3 -c "import time;print(time.time())")
COLD_MS=$(python3 -c "print(int(($T3-$T2)*1000))")

# 3) 热调用：km run vs 直接 docker exec，交替 20 轮取中位数
KM_TIMES=(); DOCKER_TIMES=()
for i in $(seq 1 20); do
  T=$(python3 -c 'import time;print(time.time())')
  "$KM" run -- /bin/true >/dev/null 2>&1
  T2=$(python3 -c 'import time;print(time.time())')
  KM_TIMES+=($(python3 -c "print(int(($T2-$T)*1000))"))
  T=$(python3 -c 'import time;print(time.time())')
  docker exec "$CONTAINER" /bin/true >/dev/null 2>&1
  T2=$(python3 -c 'import time;print(time.time())')
  DOCKER_TIMES+=($(python3 -c "print(int(($T2-$T)*1000))"))
done
median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{print (NR%2)?a[(NR+1)/2]:int((a[NR/2]+a[NR/2+1])/2)}'; }
KM_MED=$(median "${KM_TIMES[@]}"); DOCKER_MED=$(median "${DOCKER_TIMES[@]}")
KM_MIN=$(printf '%s\n' "${KM_TIMES[@]}" | sort -n | head -1)
DOCKER_MIN=$(printf '%s\n' "${DOCKER_TIMES[@]}" | sort -n | head -1)

cat > "$EV/baseline.json" <<JSON
{
  "run_id": "$RUN_ID",
  "image": "$IMAGE",
  "container": "$CONTAINER",
  "init_first_prepare_ms": $INIT_MS,
  "cold_start_ms": $COLD_MS,
  "warm_km_median_ms": $KM_MED,
  "warm_km_min_ms": $KM_MIN,
  "warm_docker_exec_median_ms": $DOCKER_MED,
  "warm_docker_exec_min_ms": $DOCKER_MIN,
  "rounds": 20,
  "km_samples_ms": [$(printf '%s\n' "${KM_TIMES[@]}" | paste -sd, -)],
  "docker_samples_ms": [$(printf '%s\n' "${DOCKER_TIMES[@]}" | paste -sd, -)]
}
JSON
{
  echo "run-id=$RUN_ID image=$IMAGE"
  echo "首次准备(init,镜像已缓存): ${INIT_MS}ms"
  echo "冷启动(stop→run 含 start): ${COLD_MS}ms"
  echo "热调用中位数(20轮): km=${KM_MED}ms  docker exec=${DOCKER_MED}ms  开销=$((KM_MED-DOCKER_MED))ms"
  echo "热调用最小: km=${KM_MIN}ms  docker=${DOCKER_MIN}ms"
} | tee "$EV/baseline.txt"

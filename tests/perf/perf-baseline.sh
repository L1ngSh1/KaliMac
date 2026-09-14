#!/bin/bash
# P3 性能基线（v2）：首次准备(init，缺镜像时单独计时 pull) / 冷启动(stop→run) /
# 热调用(km run 与裸 docker exec 交替配对对照)。
#
# 相对 v1 的修正（goal M4）：
#   - 单调时钟（time.monotonic）；全部测量在单一 python 驱动进程内完成，
#     计时工具自身启动成本不进入测量窗口（另测 harness 底噪供参照）；
#   - 每个样本检查退出码：失败样本单独记录、不进入统计；任何失败 →
#     基线 success=false 且脚本非零退出（不产出"成功"基线）；
#   - 记录 OS/架构、Go/Docker 版本、代码版本、镜像内容 ID、样本数与每样本退出码；
#   - 多轮：默认 3 轮 × 每轮 20 个有效配对样本；
#   - 资源登记：本脚本创建的容器在退出时按完整 ID 清理（仅本项目）。
#
# 环境变量：
#   PERF_IMAGE            项目镜像（默认 kali-mac-min:0.2）
#   PERF_ROUNDS           热调用轮数（默认 3）
#   PERF_SAMPLES          每轮配对样本数（默认 20）
#   PERF_KM_BIN           被测二进制（默认 $REPO/bin/km；测试可注入）
#   PERF_INJECT_WARM_FAIL 负向注入：热调用样本改跑 /bin/false（km 透传退出码 1），
#                         用于验证失败样本不进统计、不生成成功基线、脚本非零退出
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
KM="${PERF_KM_BIN:-$REPO/bin/km}"
[ -x "$KM" ] || { echo "FATAL: 被测二进制不存在或不可执行: $KM（先 make build 或设 PERF_KM_BIN）" >&2; exit 2; }

CODE_REV="$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
RUN_ID="$(date +%Y%m%d-%H%M%S)-$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo norev)"
EV="$REPO/tests/perf/evidence/$RUN_ID"
mkdir -p "$EV"
PROJ="$(mktemp -d "${TMPDIR:-/tmp}/km-perf-${RUN_ID}-XXXXXX")"
IMAGE="${PERF_IMAGE:-kali-mac-min:0.2}"
ROUNDS="${PERF_ROUNDS:-3}"
SAMPLES="${PERF_SAMPLES:-20}"

CONTAINER=""
cleanup() {
  # 兜底：正常路径在驱动返回后读取；驱动中途崩溃时从 state 兜底读取，
  # 保证本轮容器按完整 ID 清理（读取失败可观察）。
  if [ -z "$CONTAINER" ] && [ -f "$PROJ/.km/state.json" ]; then
    CONTAINER="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["container"]["id"])' "$PROJ/.km/state.json" 2>/dev/null || true)"
  fi
  if [ -n "$CONTAINER" ]; then
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || echo "WARN: 清理容器 $CONTAINER 失败（需人工核对）" >&2
  fi
  rm -rf "$PROJ"
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || { echo "FATAL: docker 引擎不可达" >&2; exit 2; }

cd "$PROJ"
printf '{"schema_version":1,"image":"%s"}\n' "$IMAGE" > .km.json

# 环境元数据（bash 采集一次，python 驱动合并进基线）
GO_VER="$(go version 2>/dev/null || echo unknown)"
DOCKER_VER="$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)"
OS_ARCH="$(uname -s 2>/dev/null)/$(uname -m 2>/dev/null)"
IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || echo missing)"

# 镜像缺失时单独计时 pull（与 init 分离，pull 不计入 init）
PULL_MS="null"
if [ "$IMAGE_ID" = "missing" ]; then
  echo "镜像不在本地，单独计时 pull: $IMAGE" >&2
  PULL_MS="$(python3 -c '
import subprocess, sys, time
t0 = time.monotonic()
p = subprocess.run(["docker", "pull", sys.argv[1]], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
print(int((time.monotonic()-t0)*1000) if p.returncode == 0 else "null")' "$IMAGE")"
  [ "$PULL_MS" != "null" ] || { echo "FATAL: 镜像拉取失败: $IMAGE" >&2; exit 2; }
  IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || echo missing)"
fi

# 测量主循环：单一 python 驱动（单调时钟；退出码逐样本检查）
KM="$KM" PROJ="$PROJ" ROUNDS="$ROUNDS" SAMPLES="$SAMPLES" \
PULL_MS="$PULL_MS" IMAGE_ID="$IMAGE_ID" IMAGE="$IMAGE" \
RUN_ID="$RUN_ID" CODE_REV="$CODE_REV" GO_VER="$GO_VER" DOCKER_VER="$DOCKER_VER" OS_ARCH="$OS_ARCH" \
INJECT="${PERF_INJECT_WARM_FAIL:-0}" \
python3 - <<'PY' > "$EV/measure.raw.json"
import json, os, subprocess, sys, time

km = os.environ["KM"]
rounds, samples = int(os.environ["ROUNDS"]), int(os.environ["SAMPLES"])
inject = os.environ["INJECT"] == "1"

failures = []
def timed(cmd):
    t0 = time.monotonic()
    p = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    dt = (time.monotonic() - t0) * 1000.0
    return round(dt, 1), p.returncode

def expect(phase, rc, ms):
    if rc != 0:
        failures.append({"phase": phase, "exit_code": rc, "ms": ms})
    return rc == 0

# 底噪：裸 subprocess 启动 true 的驱动开销（供参照，不计入结论）。
# 用 /usr/bin/true：macOS 无 /bin/true；容器内的测量仍用容器路径 /bin/true。
floor_ms, floor_rc = timed(["/usr/bin/true"])

# 1) 首次准备
init_ms, init_rc = timed([km, "init"])
expect("init", init_rc, init_ms)
with open(os.path.join(os.environ["PROJ"], ".km", "state.json")) as f:
    container = json.load(f)["container"]["id"]

# 2) 冷启动：stop（rc 必查）后第一次 run（含容器 start）
stop_ms, stop_rc = timed([km, "stop"])
expect("stop_before_cold", stop_rc, stop_ms)
cold_ms, cold_rc = timed([km, "run", "--", "/bin/true"])
expect("cold_run", cold_rc, cold_ms)

# 3) 热调用：km 与 docker exec 交替配对；失败样本不进统计
warm_tool = "/bin/false" if inject else "/bin/true"
km_ms, dex_ms, km_rc, dex_rc = [], [], [], []
for r in range(1, rounds + 1):
    for i in range(1, samples + 1):
        ms, rc = timed([km, "run", "--", warm_tool])
        km_rc.append(rc)
        if expect(f"warm_km_round{r}_{i}", rc, ms):
            km_ms.append(ms)
        ms, rc = timed(["docker", "exec", container, "/bin/true"])
        dex_rc.append(rc)
        if expect(f"warm_dex_round{r}_{i}", rc, ms):
            dex_ms.append(ms)

def pct(vals, q):
    if not vals:
        return None
    vals = sorted(vals)
    k = (len(vals) - 1) * q
    f = int(k)
    c = min(f + 1, len(vals) - 1)
    return round(vals[f] + (vals[c] - vals[f]) * (k - f), 1)

json.dump({
    "success": len(failures) == 0,
    "injected_warm_fail": inject,
    "failures": failures,
    "harness_floor_ms": floor_ms,
    "env": {
        "run_id": os.environ["RUN_ID"],
        "code_rev": os.environ["CODE_REV"],
        "os_arch": os.environ["OS_ARCH"],
        "go": os.environ["GO_VER"],
        "docker_server": os.environ["DOCKER_VER"],
        "image": os.environ["IMAGE"],
        "image_content_id": os.environ["IMAGE_ID"],
        "image_pull_ms": None if os.environ["PULL_MS"] == "null" else int(os.environ["PULL_MS"]),
        "rounds": rounds,
        "samples_per_round": samples,
    },
    "init_ms": init_ms, "init_exit_code": init_rc,
    "stop_ms": stop_ms, "stop_exit_code": stop_rc,
    "cold_start_ms": cold_ms, "cold_exit_code": cold_rc,
    "warm_km_valid_samples": len(km_ms),
    "warm_dex_valid_samples": len(dex_ms),
    "p50_warm_km_ms": pct(km_ms, 0.5), "p95_warm_km_ms": pct(km_ms, 0.95),
    "p50_warm_dex_ms": pct(dex_ms, 0.5), "p95_warm_dex_ms": pct(dex_ms, 0.95),
    "warm_km_samples_ms": km_ms,
    "warm_dex_samples_ms": dex_ms,
    "warm_km_exit_codes": km_rc,
    "warm_dex_exit_codes": dex_rc,
}, sys.stdout, ensure_ascii=False, indent=1)
PY

# 驱动成功返回后读取容器 ID（cleanup 另有 state 兜底读取）
CONTAINER="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["container"]["id"])' "$PROJ/.km/state.json" 2>/dev/null || true)"

# 汇总产物：baseline.json / baseline.csv / baseline.txt；失败 → 非零退出
KM="$KM" EV="$EV" python3 - <<'PY'
import csv, json, os, sys

ev = os.environ["EV"]
with open(os.path.join(ev, "measure.raw.json")) as f:
    r = json.load(f)

with open(os.path.join(ev, "baseline.json"), "w") as f:
    json.dump(r, f, ensure_ascii=False, indent=1)

with open(os.path.join(ev, "baseline.csv"), "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(["metric", "round", "index", "exit_code", "duration_ms"])
    w.writerow(["init", 0, 0, r["init_exit_code"], r["init_ms"]])
    w.writerow(["stop", 0, 0, r["stop_exit_code"], r["stop_ms"]])
    w.writerow(["cold_start", 0, 0, r["cold_exit_code"], r["cold_start_ms"]])
    n = r["env"]["samples_per_round"]
    for i, (ms, rc) in enumerate(zip(r["warm_km_samples_ms"], r["warm_km_exit_codes"])):
        w.writerow(["warm_km", i // n + 1, i % n + 1, rc, ms])
    for i, (ms, rc) in enumerate(zip(r["warm_dex_samples_ms"], r["warm_dex_exit_codes"])):
        w.writerow(["warm_docker_exec", i // n + 1, i % n + 1, rc, ms])

e = r["env"]
lines = [
    f"run-id={e['run_id']} code={e['code_rev'][:12]} success={r['success']}",
    f"env: {e['os_arch']} go={e['go'].split()[2] if len(e['go'].split()) > 2 else e['go']} docker={e['docker_server']} image={e['image']} id={e['image_content_id'][:20]}",
    f"样本: km 有效 {r['warm_km_valid_samples']}/{e['rounds']*e['samples_per_round']}  dex 有效 {r['warm_dex_valid_samples']}/{e['rounds']*e['samples_per_round']}  底噪 {r['harness_floor_ms']}ms",
    f"首次准备 init: {r['init_ms']}ms (rc={r['init_exit_code']})" + (f"  镜像 pull: {e['image_pull_ms']}ms" if e["image_pull_ms"] else ""),
    f"冷启动 stop→run: {r['cold_start_ms']}ms (rc={r['cold_exit_code']})",
    f"热调用 p50: km={r['p50_warm_km_ms']}ms  docker exec={r['p50_warm_dex_ms']}ms  开销={None if None in (r['p50_warm_km_ms'], r['p50_warm_dex_ms']) else round(r['p50_warm_km_ms'] - r['p50_warm_dex_ms'], 1)}ms",
    f"热调用 p95: km={r['p95_warm_km_ms']}ms  docker exec={r['p95_warm_dex_ms']}ms",
]
if r["failures"]:
    lines.append(f"FAILURES({len(r['failures'])}): " + json.dumps(r["failures"][:5], ensure_ascii=False))
with open(os.path.join(ev, "baseline.txt"), "w") as f:
    f.write("\n".join(lines) + "\n")
print("\n".join(lines))

sys.exit(0 if r["success"] else 1)
PY

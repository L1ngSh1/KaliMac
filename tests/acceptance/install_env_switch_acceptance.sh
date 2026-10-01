#!/bin/bash
# 环境切换功能的安装版验收（计划缺口 1：从 dist 安装后完整操作，
# 不依赖开发 bin/km、源码路径或开发环境状态）。
#
# 流程：打包（dist）→ 校验 SHA256 → 解包 → 安装到临时 PREFIX →
#       在仓库外的临时项目里跑真实 env switch/rollback/recover 全流程 →
#       资源账本核对 → 卸载 → 清单核验 → 清理容器。
#
# 用法： tests/acceptance/install_env_switch_acceptance.sh
# 前提：Docker 引擎可达；kali-mac-min:0.2 镜像在本地（缺失则由脚本构建）。
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d /tmp/km-install-accept.XXXXXX)"
PASS=0
FAIL=0

note() { echo "[acceptance] $*"; }
ok()   { PASS=$((PASS+1)); echo "[PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }

assert_contains() { # assert_contains <描述> < haystack 文件> < needle>
  if grep -q "$3" "$2"; then ok "$1"; else bad "$1（缺 \"$3\"）"; fi
}

cleanup() {
  if [ -n "${PROJ:-}" ] && [ -f "$PROJ/.km/state.json" ]; then
    PID="$(python3 -c 'import json;print(json.load(open("'"$PROJ"'/.km/state.json"))["project_id"])' 2>/dev/null || true)"
    if [ -n "$PID" ]; then
      docker ps -aq --filter "label=km.project=$PID" | xargs -r docker rm -f >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# ---------- 0. 前提 ----------
docker info --format '{{.ServerVersion}}' >/dev/null || { echo "FATAL: Docker 不可达" >&2; exit 2; }
note "Docker 引擎可达"
if ! docker image inspect kali-mac-min:0.2 >/dev/null 2>&1; then
  note "构建基础镜像 kali-mac-min:0.2"
  (cd "$REPO" && docker build -t kali-mac-min:0.2 images/kali) >/dev/null
fi
for t in km-envtest-a km-envtest-b; do
  if ! docker image inspect "$t:local" >/dev/null 2>&1; then
    D="$(mktemp -d)"; printf "FROM kali-mac-min:0.2\nRUN mkdir -p /opt/km-env && printf '%s\\n' > /opt/km-env/marker\n" "$t" > "$D/Dockerfile"
    docker build -q -t "$t:local" "$D" >/dev/null; rm -rf "$D"
  fi
done
note "测试镜像就绪（km-envtest-a/b:local，marker 内容差异）"

HEAD_SHA="$(git -C "$REPO" rev-parse HEAD)"

# ---------- 1. 打包（在仓库外调用，行为须一致） ----------
note "运行 scripts/package.sh（cwd=仓库外，DIST 指向临时目录）"
( cd "$WORK" && DIST="$WORK/dist" bash "$REPO/scripts/package.sh" ) > "$WORK/package.log" 2>&1 \
  || { bad "package.sh"; cat "$WORK/package.log"; exit 1; }
ok "package.sh 完成"
VERSION="$(cd "$REPO" && go run ./cmd/km --version | awk '{print $2}')"
PKG="$WORK/dist/km-$VERSION-darwin-arm64.tar.gz"
[ -f "$PKG" ] || { bad "arm64 压缩包存在"; exit 1; }
ok "arm64 压缩包存在：$(basename "$PKG")"
( cd "$WORK/dist" && shasum -c SHA256SUMS ) > "$WORK/sha.log" 2>&1 \
  && ok "SHA256SUMS 校验通过" || { bad "SHA256SUMS 校验"; cat "$WORK/sha.log"; }

# ---------- 2. 解包并安装到临时 PREFIX ----------
mkdir -p "$WORK/pkg" "$WORK/prefix"
tar -xzf "$PKG" -C "$WORK/pkg"
note "install.sh 安装到 $WORK/prefix"
( cd "$WORK/pkg/km-$VERSION-darwin-arm64" && PREFIX="$WORK/prefix" ./install.sh ) > "$WORK/install.log" 2>&1 \
  && ok "安装完成" || { bad "安装"; cat "$WORK/install.log"; }
KM="$WORK/prefix/bin/km"
[ -x "$KM" ] && ok "安装产物可执行" || { bad "安装产物可执行"; exit 1; }
for f in share/km/VERSION share/km/BUILD-INFO share/km/manifest.txt; do
  [ -f "$WORK/prefix/$f" ] && ok "清单文件 $f" || bad "清单文件 $f 缺失"
done
"$KM" version --verbose > "$WORK/version.log" 2>&1
grep -q "commit: $HEAD_SHA" "$WORK/version.log" && ok "构建身份=HEAD $HEAD_SHA" || bad "构建身份不符"
EXPECTED_WT="clean"; [ -n "$(git -C "$REPO" status --porcelain)" ] && EXPECTED_WT="dirty"
grep -q "worktree: $EXPECTED_WT" "$WORK/version.log" && ok "worktree 如实记录（${EXPECTED_WT}）" || bad "worktree 记录不符（期望 ${EXPECTED_WT}）"
grep -q "target: darwin/arm64" "$WORK/version.log" && ok "target: darwin/arm64（本机实测）" || bad "target 不符"

# ---------- 3. 仓库外真实全流程（必须 cd 进项目，km 按所在目录发现项目） ----------
PROJ="$(mktemp -d "$WORK/proj.XXXXXX")"
cd "$PROJ"
note "仓库外项目 ${PROJ}：init → switch → rollback → recover"
"$KM" init --image km-envtest-a:local > "$WORK/step-init.log" 2>&1 \
  && ok "init（镜像 A）" || { bad "init"; cat "$WORK/step-init.log"; }
"$KM" run -- cat /opt/km-env/marker > "$WORK/step-run-a.log" 2>&1
grep -q "env-a" "$WORK/step-run-a.log" && ok "初始环境实际执行 → env-a" || bad "初始环境 marker"
"$KM" env switch --image km-envtest-b:local --dry-run > "$WORK/step-dry.log" 2>&1 \
  && grep -q "允许执行" "$WORK/step-dry.log" && ok "switch --dry-run 预览" || { bad "dry-run"; cat "$WORK/step-dry.log"; }
"$KM" env switch --image km-envtest-b:local --yes > "$WORK/step-switch.log" 2>&1 \
  && ok "switch A→B --yes" || { bad "switch"; cat "$WORK/step-switch.log"; }
"$KM" run -- cat /opt/km-env/marker > "$WORK/step-run-b.log" 2>&1
grep -q "env-b" "$WORK/step-run-b.log" && ok "新环境实际执行 → env-b" || bad "新环境 marker"
"$KM" env rollback --yes > "$WORK/step-rollback.log" 2>&1 \
  && ok "rollback B→A" || { bad "rollback"; cat "$WORK/step-rollback.log"; }
"$KM" run -- cat /opt/km-env/marker > "$WORK/step-run-a2.log" 2>&1
grep -q "env-a" "$WORK/step-run-a2.log" && ok "回退后实际执行 → env-a" || bad "回退后 marker"
"$KM" env recover > "$WORK/step-recover.log" 2>&1 \
  && grep -q "无需恢复" "$WORK/step-recover.log" && ok "recover：无需恢复（exit 0）" || { bad "recover"; cat "$WORK/step-recover.log"; }
PID="$(python3 -c 'import json;print(json.load(open("'"$PROJ"'/.km/state.json"))["project_id"])')"
# ---------- 3b. 资源查看与显式清理（list / remove） ----------
"$KM" env list > "$WORK/step-list.log" 2>&1   && grep -q "RETAINED" "$WORK/step-list.log" && ok "env list：显示 RETAINED（回退撤下的 B）"   || { bad "env list"; cat "$WORK/step-list.log"; }
G1_FULL="$(docker container inspect --format '{{.Id}}' "km-$PID-g1" 2>/dev/null || true)"
# PID 在 init 后即可用；此处的 gen1 容器是 rollback 撤下的 retained
G1_FULL="$(docker container inspect --format '{{.Id}}' "$(docker ps -aq --filter "label=km.project=$PID" --filter "name=km-$PID-g1" | head -1)" 2>/dev/null || true)"
[ -n "$G1_FULL" ] && ok "取得 retained 容器完整 ID" || { bad "取得完整 ID"; }
"$KM" env remove "$G1_FULL" --dry-run > "$WORK/step-rm-dry.log" 2>&1   && grep -q "允许执行" "$WORK/step-rm-dry.log" && ok "remove --dry-run 预览" || { bad "remove dry-run"; cat "$WORK/step-rm-dry.log"; }
"$KM" env remove "$G1_FULL" --yes > "$WORK/step-rm.log" 2>&1   && ok "remove --yes 删除 retained 容器" || { bad "remove"; cat "$WORK/step-rm.log"; }
docker container inspect "$G1_FULL" >/dev/null 2>&1 && bad "容器应已删除" || ok "容器已删除（不可逆）"
"$KM" env remove "$G1_FULL" --yes > "$WORK/step-rm2.log" 2>&1   && { bad "二次删除应拒绝"; cat "$WORK/step-rm2.log"; } || grep -q "KM_NOT_FOUND" "$WORK/step-rm2.log" && ok "二次删除按未知目标拒绝"
"$KM" run -- cat /opt/km-env/marker > "$WORK/step-run-a3.log" 2>&1
grep -q "env-a" "$WORK/step-run-a3.log" && ok "清理后当前环境仍正常执行" || bad "清理后执行"
"$KM" doctor > "$WORK/step-doctor.log" 2>&1
assert_contains "doctor：资源账本与实际容器一致" "$WORK/step-doctor.log" "资源账本与实际容器一致"

# 账本恒等式：retained B 已被 remove 删除 → 实际容器 = 当前代（A）= 1
PID="$(python3 -c 'import json;print(json.load(open("'"$PROJ"'/.km/state.json"))["project_id"])')"
N="$(docker ps -aq --filter "label=km.project=$PID" | wc -l | tr -d ' ')"
[ "$N" = "1" ] && ok "账本恒等式：实际容器=1（当前代；retained 已显式清理）" || bad "账本恒等式（实际 ${N}）"

cd "$REPO"

# ---------- 4. 卸载与清单核验 ----------
( cd "$WORK/pkg/km-$VERSION-darwin-arm64" && PREFIX="$WORK/prefix" ./uninstall.sh ) > "$WORK/uninstall.log" 2>&1 \
  && ok "uninstall.sh 完成" || { bad "uninstall"; cat "$WORK/uninstall.log"; }
[ ! -x "$KM" ] && ok "卸载后 bin/km 已移除" || bad "bin/km 残留"
[ ! -e "$WORK/prefix/share/km" ] && ok "卸载后 share/km 已移除" || bad "share/km 残留"
[ ! -d "$PROJ/.km/env" ] || true

echo
echo "[acceptance] 结果：$PASS 通过, $FAIL 失败"
[ "$FAIL" = "0" ]

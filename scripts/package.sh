#!/bin/bash
# km 本地打包：macOS 分架构压缩包 + SHA256 校验和 + 构建元数据 → dist/。
#
# 注意：宿主架构为实测（本机运行与集成套件验证）；另一架构为交叉编译产物，
# 未在真实硬件上做过端到端测试（见 docs/goal-progress.md 平台边界）。
# 用法： scripts/package.sh
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${DIST:-$REPO/dist}"
cd "$REPO"   # go build/go run/git 均按仓库根解析；任意 cwd 调用行为一致

# 版本来源=当前源码（go run 即时构建），与可能过期的 bin/km 无关（包 C 合同）
VERSION="$(go run ./cmd/km --version 2>/dev/null | awk '{print $2}')"
[ -n "$VERSION" ] || { echo "FATAL: 无法获取版本（检查源码/go 环境）" >&2; exit 2; }

CODE_REV="$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
# 脏工作区记录：HEAD SHA 不暗示产物来自干净提交
DIRTY_N="$(git -C "$REPO" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
if [ "$DIRTY_N" = "0" ]; then WORKTREE_STATE="clean"; else WORKTREE_STATE="dirty"; fi
# 打包不访问 Docker。需要关联已核验镜像时由调用方显式传入内容 ID。
IMAGE_ID="${KM_IMAGE_CONTENT_ID:-unknown}"
BUILT_AT="$(date '+%Y-%m-%dT%H:%M:%S%z')"

rm -rf "$DIST"
mkdir -p "$DIST"

for ARCH in arm64 amd64; do
  TARGET="darwin/$ARCH"
  PKG="km-$VERSION-darwin-$ARCH"
  STAGE="$DIST/.stage-$ARCH/$PKG"
  mkdir -p "$STAGE"
  LDFLAGS="-X kalimac/internal/cli.BuildCommit=$CODE_REV -X kalimac/internal/cli.BuildWorktree=$WORKTREE_STATE -X kalimac/internal/cli.BuildTarget=$TARGET"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/km" ./cmd/km
  cp scripts/install.sh scripts/uninstall.sh "$STAGE/"
  cp docs/package-quickstart.md "$STAGE/QUICKSTART.md"
  mkdir -p "$STAGE/images"
  cp -R images/kali "$STAGE/images/"
  cat > "$STAGE/version-metadata.txt" <<EOF
version: $VERSION
code_rev: $CODE_REV
worktree: $WORKTREE_STATE
worktree_changed_paths: $DIRTY_N
target: $TARGET
built_at: $BUILT_AT
builder_go: $(go version)
host: $(uname -s)/$(uname -m)
toolchain: CGO_ENABLED=0 GOOS=darwin GOARCH=$ARCH go build -trimpath
image_ref: kali-mac-min:0.2
image_content_id: $IMAGE_ID
hardware_validation: pending
EOF
  # macOS tar 默认保存扩展属性为 AppleDouble；发布包只包含显式文件。
  (cd "$DIST/.stage-$ARCH" && COPYFILE_DISABLE=1 tar -czf "$DIST/$PKG.tar.gz" "$PKG")
done

rm -rf "$DIST/.stage-arm64" "$DIST/.stage-amd64"
( cd "$DIST" && shasum -a 256 ./*.tar.gz > SHA256SUMS )

echo "打包完成 → $DIST"
cat "$DIST/SHA256SUMS"

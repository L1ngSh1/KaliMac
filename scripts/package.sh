#!/bin/bash
# km 本地打包（goal M5）：macOS 双架构产物 + SHA256 校验和 + 版本元数据 → dist/。
#
# 注意：宿主架构为实测（本机运行与集成套件验证）；另一架构为交叉编译产物，
# 未在真实硬件上做过端到端测试（见 docs/goal-progress.md 平台边界）。
# 用法： scripts/package.sh
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$REPO/dist"

VERSION="$("$REPO/bin/km" --version 2>/dev/null | awk '{print $2}')"
[ -n "$VERSION" ] || { echo "FATAL: 无法获取版本（先 make build）" >&2; exit 2; }

rm -rf "$DIST"
mkdir -p "$DIST"

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$DIST/km-$VERSION-darwin-arm64" ./cmd/km
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o "$DIST/km-$VERSION-darwin-amd64" ./cmd/km

CODE_REV="$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
IMAGE_ID="$(docker image inspect --format '{{.Id}}' kali-mac-min:0.2 2>/dev/null || echo "未构建（见 images/kali 与 tests/evidence/kali-mac-min-0.2/build-evidence.txt）")"

cat > "$DIST/version-metadata.txt" <<EOF
version: $VERSION
code_rev: $CODE_REV
built_at: $(date '+%Y-%m-%dT%H:%M:%S%z')
builder_go: $(go version)
host: $(uname -s)/$(uname -m)
toolchain: CGO_ENABLED=0 go build -trimpath ./cmd/km
image_ref: kali-mac-min:0.2
image_content_id: $IMAGE_ID
platforms:
  - darwin/arm64（宿主架构：本机实测）
  - darwin/amd64（交叉编译：未在真实硬件测试）
EOF

( cd "$DIST" && shasum -a 256 km-$VERSION-darwin-* > SHA256SUMS )

echo "打包完成 → $DIST"
cat "$DIST/version-metadata.txt"
echo
cat "$DIST/SHA256SUMS"

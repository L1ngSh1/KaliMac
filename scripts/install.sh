#!/bin/bash
# km 本地安装（goal M5）。
#
# 用法：
#   scripts/install.sh                     # 安装到 /usr/local（需相应写权限）
#   PREFIX=$HOME/.local scripts/install.sh # 用户级前缀
#   DESTDIR=/tmp/stage PREFIX=/usr/local scripts/install.sh  # 打包/暂存（不落真实路径）
#   KM_BIN=dist/km-…-darwin-arm64 PREFIX=… scripts/install.sh  # 安装指定二进制（如打包产物）
#
# 行为：只安装清单内文件（二进制 + 版本元数据 + 清单自身）；清单写在
# $PREFIX/share/km/manifest.txt（相对 PREFIX 的路径），卸载（scripts/uninstall.sh）
# 只删除清单内文件。默认不触碰 PATH 与 shell 配置。
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
PREFIX="${PREFIX:-/usr/local}"
DESTDIR="${DESTDIR:-}"
if [ -n "${KM_BIN:-}" ]; then
  BIN_SRC="$KM_BIN"
elif [ -x "$SCRIPT_DIR/km" ]; then
  BIN_SRC="$SCRIPT_DIR/km"
else
  BIN_SRC="$REPO/bin/km"
fi
[ -x "$BIN_SRC" ] || { echo "FATAL: 先 make build（缺 $BIN_SRC）" >&2; exit 2; }

ROOT="$DESTDIR$PREFIX"
BINDIR="$ROOT/bin"
SHAREDIR="$ROOT/share/km"
mkdir -p "$BINDIR" "$SHAREDIR"

install -m 0755 "$BIN_SRC" "$BINDIR/km"
VERSION="$("$BINDIR/km" --version)"
printf '%s\n' "$VERSION" > "$SHAREDIR/VERSION"
if [ -f "$SCRIPT_DIR/version-metadata.txt" ]; then
  install -m 0644 "$SCRIPT_DIR/version-metadata.txt" "$SHAREDIR/BUILD-INFO"
else
  "$BINDIR/km" version --verbose > "$SHAREDIR/BUILD-INFO"
fi
printf 'bin/km\nshare/km/VERSION\nshare/km/BUILD-INFO\n' > "$SHAREDIR/manifest.txt"

echo "已安装 $VERSION → $ROOT"
echo "  清单: $SHAREDIR/manifest.txt（卸载只删除清单内文件）"
[ -n "$DESTDIR" ] && echo "  （DESTDIR 暂存模式：未写入系统路径）" || :
case ":$PATH:" in
  *":$PREFIX/bin:"*) : ;;
  *) echo "提示：$PREFIX/bin 不在 PATH；或直接用绝对路径调用" ;;
esac

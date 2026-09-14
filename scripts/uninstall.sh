#!/bin/bash
# km 卸载（goal M5）：只删除 scripts/install.sh 写入清单的文件，
# 然后尝试移除因此清空的目录。绝不递归删除、不触碰清单外文件。
# 用法： DESTDIR=... PREFIX=... scripts/uninstall.sh
set -euo pipefail
PREFIX="${PREFIX:-/usr/local}"
DESTDIR="${DESTDIR:-}"
ROOT="$DESTDIR$PREFIX"
MANIFEST="$ROOT/share/km/manifest.txt"
[ -f "$MANIFEST" ] || { echo "未找到安装清单: $MANIFEST（未安装或已卸载）" >&2; exit 1; }

while IFS= read -r f; do
  [ -n "$f" ] || continue
  target="$ROOT/$f"
  if [ -e "$target" ]; then
    rm -f "$target"
    echo "已删除 $target"
  else
    echo "跳过（不存在）$target"
  fi
done < "$MANIFEST"

# 清单自身也是安装文件之一：最后删除
rm -f "$MANIFEST"
echo "已删除 $MANIFEST"

# 清理因卸载而空的目录（rmdir 只删空目录，安全）
rmdir "$ROOT/share/km" 2>/dev/null && echo "已移除空目录 $ROOT/share/km" || :
rmdir "$ROOT/share" 2>/dev/null && echo "已移除空目录 $ROOT/share" || :
echo "卸载完成"

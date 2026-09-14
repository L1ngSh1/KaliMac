#!/bin/sh
# 回滚本轮全部 tracked 变更（相对基线 d3cbd27）：在仓库根执行。
# untracked 新增文件不在补丁内，如需完全回滚请另行删除：
#   internal/residue/ scripts/ CHANGELOG.md .github/ tests/perf/perf-negative-test.sh
set -e
git apply -R "$(dirname "$0")/final.patch"
echo "final.patch 已回滚"

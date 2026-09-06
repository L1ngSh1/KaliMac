#!/bin/sh
# P0 fixture：逐项回显 argv。证明 docker exec/km 对参数数组逐元素透传。
i=0
for a in "$@"; do
  i=$((i+1))
  printf '%d[%s]\n' "$i" "$a"
done

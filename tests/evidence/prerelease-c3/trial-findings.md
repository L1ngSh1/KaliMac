# 阶段三试用记录（2026-09-16）

试用方式：3 个拟真独立项目（A 日志分析 20 万行 access.log / B web 检查 loopback /
C 笔记整理中文路径），由执行者按用户真实路径驱动；全部证据在本目录，试用项目与
容器已按完整 ID 清理（trial-cleanup.txt），零残留。

## 演练矩阵结果（全部「通过」或「按设计行为成立」）

| 演练 | 项目 | 结果 | 证据 |
| --- | --- | --- | --- |
| 连续使用：10 条短命令逐条计时 | A | 229–305ms/条（与基线 ~205ms + 工具执行时间一致） | trialA-latency.txt |
| 异常退出：SIGKILL 宿主 km → 阻断 → 按指引恢复 | B | 阻断 rc=1，报错含可复制 cancel 指令；doctor 报活跃会话；恢复 rc=0 | trialB-kill-recovery.txt |
| 会话恢复：stop→run 保数据 | C | 同容器恢复，数据保留 | trialC-recovery-sharing.txt |
| 文件共享：双向 + 中文空格子目录 cwd 映射 | C | 容器/宿主 SHA256 一致 | 同上 |
| 隔离：A 忙碌时 B/C 正常；同项目并发 BUSY | A/B/C | B/C rc=0；BUSY rc=1 带持有人 pid 与时间戳 | trial-isolation.txt |
| 终端强杀：SIGKILL km shell → 阻断 → cancel → 恢复 | A | 全链路闭环 | trial-terminal-kill.txt、trialA-cancel-recover.txt |

## 问题记录（模板：复现/预期与实际/影响/频率/证据）

### F1 恢复摩擦：异常退出后必须手抄 docker exec 命令
- 复现：任意 km 任务运行中 `kill -9 <km pid>` → 再次执行 → 按 KM_SESSION_ACTIVE
  报错文本手工拼 `docker exec <容器ID> /tmp/km-bin/km-ctl cancel <会话ID>`。
- 预期/实际：保护性阻断本身正确（任务可能仍在跑，不应自动杀）；实际恢复需从报错
  文本中拷贝两段 ID 拼一条长命令，试用中 3 次强杀 3 次都需要。
- 影响：异常退出后的恢复摩擦是本轮试用中最高的人为步骤成本。
- 频率：每次异常退出必现（3/3）。
- 证据：trialB-kill-recovery.txt、trial-isolation.txt、trial-terminal-kill.txt。

### F2 km-ctl cancel 诊断噪音
- 复现：SIGKILL 后按指引 cancel 时，容器内 km-ctl 偶发输出
  `/tmp/km-bin/km-ctl: 7: cannot open /proc/27/stat: No such file`。
- 预期/实际：清理成功、退出码 0，但输出含吓人的错误行（进程在列出与统计之间
  消失的竞态）。
- 影响：观感/信任度；无功能影响。
- 频率：3 次 cancel 中出现 1 次。
- 证据：trialB-kill-recovery.txt。

### F3 热调用延迟（已知基线的真实体感）
- 复现：连发短命令（A 项目 10 连发）。
- 预期/实际：229–305ms/条；开销主体为 km 固有的 ~6 次 docker CLI 启动（~160ms），
  非回归。
- 影响：脚本化连发场景的等待感；交互 shell 场景不受影响。
- 频率：每条非 shell 命令必现。
- 证据：trialA-latency.txt、goal-1789353851 性能基线。

## 正确行为确认（非缺陷，记录防误报）

- BUSY/会话阻断语义与退出码（1）符合 cli-contract。
- SIGKILL 后容器内任务存活属设计语义（ADR-004），阻断消息明示「任务可能仍在运行」。
- 多项目隔离、同容器恢复保数据、双向共享、中文空格路径全部通过。

## 优先级待办（试用产出）

1. **P1 会话查看与显式恢复入口**：`km sessions`（列出活跃/遗留）+ `km cancel <id>`
   （或 `km resume`）类子命令，消除 F1 的手抄长命令。需先明确交互与行为合同。
2. **P2 km-ctl cancel 竞态噪音**：stat 前做存在性检查或容忍 ENOENT（小修）。
3. **P3 热调用延迟**：维持基线观察，除非出现以 shell 为主的用户仍不满意的实际反馈；
   优化须保留前后对照与功能回归。

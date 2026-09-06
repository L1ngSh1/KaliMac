# ADR-004：最小会话执行与取消方案（P2-A）

日期：2026-09-06 ｜ 状态：已验证（真实 Docker 集成 11/11 PASS）｜ 关联：ADR-001（信号路径）、docs/phase-0.md

## 决策

采用**容器内最小会话控制器**（方案 a）实现工具执行与取消，不引入 Docker Go SDK、不常驻守护进程、不使用宽泛进程名匹配。

```text
km 客户端                                容器
─────────                                ────
docker cp - <c>:/tmp        ──────────▶  /tmp/km-bin/{km-run,km-ctl,km-observe}
                                          （tar 流引导，无 shell 字符串，幂等）
docker exec [-w DIR] -i <c> \
  /tmp/km-bin/km-run <sid> \
  TOOL ARG...               ──────────▶  km-run: mkdir /tmp/km-sessions/<sid>
  （stdin/stdout/stderr 流式，            setsid TOOL ARG... <&3 &   ← 工具独立会话/进程组
    不缓存、不拼接 shell 串）              写 pid → wait → 写 exit → rm -rf 目录 → 退出码原样
ctx 取消（SIGINT/SIGTERM）：
  新建 context.WithTimeout(Background,15s)  ← 不复用已取消的主 ctx
docker exec <c> /tmp/km-bin/km-ctl \
  cancel <sid>              ──────────▶  TERM 工具进程组 → 等待终态 → KILL 兜底 → 报告
主执行进程自然退出（或 15s 后 Kill 兜底）
```

## 实验依据

- phase-0 E10–E13：杀死 docker exec 客户端不传播信号 → 必须有容器内侧主动清理路径（本方案）。
- fd3 探针：POSIX 把后台任务 stdin 指到 /dev/null，`exec 3<&0` + `<&3` 实现二进制 stdin 接力（64KiB 往返哈希一致）。
- 时序取证：km-run 在 wait 返回后 ~1ms 内写 exit 并删除目录 → ctl 以「目录消失」为成功终态（曾因只认 exit 文件而误报 4，已修复并有回归）。
- `--init`（tini）作为 PID1 回收孤儿，取消后 /proc 中 tpid 消失（含无僵尸），由测试独立确认。

## 语义（实验定义，非假设）

| 场景 | 行为 | 验证 |
|---|---|---|
| 正常完成 | 退出码原样透传（0/7/42 实测） | 集成 + fake |
| 正常取消（SIGINT） | ctl TERM→确认回收→ExitCode **130** | 集成（noclean/selfclean/grandchild） |
| 取消与完成竞争 | 工具已自然结束则保留真实退出码，不虚报 130 | fake（确定性时序） |
| 重复取消 | ctl 幂等：目录已清 → 协议码 3 → 成功 | 集成 |
| 启动阶段取消 | 不触碰容器（预取消）或 ctl 报 3 → 130 + 无会话残留 | 集成 + fake |
| 清理未确认 | ctl=4 → Detail=incomplete + stderr 诊断；客户端仍被兜底终止 | fake |
| ctl 调用失败 | Detail=failed + 诊断；Kill 兜底（15s 预算独立于主 ctx） | fake |
| SIGKILL 客户端/宿主失联 | **风险观察**：容器内任务继续运行（与 P0 一致）；随后显式 ctl 仍可清理。不把正常取消结果推广到该场景 | 集成（单独用例） |
| 管理超时隔离 | 流式执行路径无管理超时；2s bootstrap 超时下 5s 工具正常完成 | 集成 |
| 多项目隔离 | 会话身份唯一且目录隔离；A 取消不影响 B | 集成 |

会话作用域：ctl 只杀 pid 文件记录的进程组（`kill -TERM -<pgid>`），观察用 `km-observe pgid:<n>`（/proc 遍历，非进程名匹配）。逃逸出进程组的守护化子进程不在保证范围内（ADR-001 同样如此，文档明示）。

## 控制流与数据流分离

- 数据流：工具 stdin/stdout/stderr 逐字节流式，km 不缓存、不加内容；退出码即结果。
- 控制流：bootstrap（docker cp tar）与 cancel（独立 exec）均为有界捕获式调用；控制信息不进入工具三流。

## 修订（审阅加固轮，2026-09-06）

1. **会话终态 = 进程组实际结束**（R1）：km-run 在工具主进程退出后执行排空
   （对组内余留成员 TERM → 有界轮询 → KILL），确认组空（/proc 扫描，排除僵尸）
   后才写 exit 并删除目录；km-ctl 以「组空/目录消失/exit 文件」三者任一为成功
   终态。此前"目录消失即成功"会被"主进程死于 TERM、同组子进程忽略 TERM"穿透
   （真实复现）。逃逸出组的守护化子进程仍不在保证范围。
2. **遗留会话阻断**（R2）：宿主锁（含遗留锁接管）之后核验容器内会话
   （`km-ctl sessions`）：组仍活跃 → `KM_SESSION_ACTIVE` 拒绝新任务并给出显式
   清理指引；组已空的遗留目录由 `km-ctl sweep` 自动清扫。doctor 报告会话状态。
3. **连接信息贯穿**（R4）：本轮解析的 endpoint 以替换式 DOCKER_HOST
   （`runtime.ReplaceEnv`，不产生重复键）注入管理查询、docker cp 引导、流式
   执行与 cancel 全部子进程路径，不继承宿主可能变化的默认 context。

## 未决与限制

1. SIGKILL/宿主失联后容器内任务持续运行（设计已知）；P2-B 的 doctor/锁只提示，不自动清理他机会话。
2. 每次 Run 都 docker cp 引导（~100ms）；P2-B 可按容器缓存跳过（脚本内容 ID 比对）。
3. km-ctl 等待启动的固定 5s 窗口：极端慢引擎下启动期取消可能报 already-gone 而实际随后启动——窗口在 P2-B 以「先 bootstrap 后取消检查」的顺序缓解。

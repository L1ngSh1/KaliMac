# ADR-005：交互终端（km shell）方案——C1 原型验证结论

日期：2026-09-06 ｜ 状态：**C1 验证通过**（真实 PTY 集成 11/11 PASS）｜ 关联：ADR-004、docs/phase-0.md
范围：P2-C 原型验证。`km shell` 产品入口本轮**未接入**（仍为 KM_NOT_IMPLEMENTED）。

## 决策

采用 **Docker CLI 全权接管真实终端** 的方案：km 以薄父进程身份启动
`docker exec -it <c> /bin/bash --noprofile --norc -i`，stdio 直通。

| 职责 | 归属 | 依据 |
|---|---|---|
| 宿主终端 raw mode 与退出恢复 | Docker CLI（km 不触碰 termios） | 客户端在 stdin 为 TTY 时自行进入 raw mode 并在退出时恢复（实测 normal/启动失败路径 termios 不变） |
| 容器内 PTY、控制终端、前台进程组 | Docker CLI（`-t`）+ bash | 实测 bash 为会话首进程（PID==PGID==SID）、拥有 ctty（TPGID≠0）、命令执行瞬间位于前台组（TPGID==命令 PID） |
| 窗口尺寸同步 | Docker CLI（SIGWINCH → exec resize API） | 实测两次尺寸变化容器内 stty size 跟随；注意信号→API 落地有毫秒级延迟，紧随 resize 的立即读取会竞态（测试在 resize 后 settle 0.4s） |
| 进程生命周期、tty 前检、外部信号兜底 | km | K 项前检（stdin/stdout 均须 TTY，否则 exit 2 不发起任何 exec）；外部 SIGTERM/SIGHUP 时以进入前快照恢复 termios（客户端被 SIGKILL 不会自行恢复）并统一退出 143 |

依赖：新增 `golang.org/x/term v0.27.0`（官方；仅用于 IsTerminal 前检与信号路径的
GetState/Restore 兜底，raw mode 日常管理不在 km）。获取经 goproxy.cn（proxy.golang.org
在本机网络不可达）。

## 信号语义（实验定义）

- **键盘 Ctrl-C**：raw mode 下是 0x03 字节经 PTY 线路规程直达 bash，对 km 不构成
  信号——`signal.NotifyContext` 不会取消 shell 会话；实验证实 sleep 中 Ctrl-C →
  命令 130、shell 存活，提示符处 Ctrl-C → shell 不退出。
- **外部 SIGINT**（对 km 进程）：忽略（键盘 Ctrl-C 才是约定中断路径）。
- **外部 SIGTERM/SIGHUP**：km 兜底——SIGKILL 客户端、以快照恢复 termios、退出 143。
  客户端被 SIGKILL 不自行恢复 termios，故该路径必须由 km 恢复。
- 实现注意：信号路径与主 Wait 存在竞态，用原子标志统一在主路径返回 143
  （初版竞态导致 255，已修）。

## 启动方式与作业清理

- 启动：`--noprofile --norc` 隔离用户初始化脚本；`-e PS1=KM_SHELL>\ ` 提供同步标记。
- **bash 退出不会自动 SIGHUP 后台进程组**（实验证实：普通 bg sleep 在 shell 退出后
  存活）。shell 会话通过 `-e PROMPT_COMMAND='trap '\''kill $(jobs -p) 2>/dev/null'\'' EXIT'`
  在退出时对全部作业发 TERM。忽略 HUP/TERM 的作业属记录在案的逃逸者：可观测
  （doctor/ps）、按记录 pid 显式清理，不用容器停止或进程名匹配掩盖。

## detach keys（实验结论，review D 更正）

更正初版结论：`docker exec` **存在** detach 机制。显式启用 keys 后，客户端收到
序列即脱离退出（exit 1，输出 "read escape sequence"），而容器内 bash **继续运行**
——脱离 ≠ 会话清理。另经两轮实测，`--detach-keys ""` 并非可靠的"禁用"（一次字节
直达 bash、一次触发默认序列脱离）。

km 的处理不依赖 detach 的具体行为：无论客户端以何种方式退出（正常退出/脱离），
finalize 先判定 bash 存活性——已退出则按 SID 域清理同会话作业；仍存活则保留登记
（Detached），后续任务被 `KM_SESSION_ACTIVE` 阻断，显式 cancel 可清理。两种结果
都不会卡死或静默吞任务。

## 失败处理

- stdin/stdout 任一非终端 → exit 2（KM_PROTO_NOT_TTY），不修改终端、不发起 exec
  （stderr 变体经驱动器 dup2 实测）。
- 启动失败（容器不存在等）→ docker 客户端错误透传（exit 1），termios 保持进入前状态。
- SIGKILL/宿主失联：容器内 bash 存活（实测），属记录在案的风险边界——不套用正常
  清理保证；按记录 pid 显式清理。M0 的 `KM_SESSION_*` 阻断机制（若后续 shell 会话
  纳入会话登记）承接后续任务的防护。

## 与 ADR-004 的关系

非交互路径（km-run/km-ctl、argv/三流/退出码/取消语义）保持不变。交互 shell 不复用
setsid+后台启动脚本——控制终端需要 bash 作为会话首进程，由 `-t` 直接满足。

## C2：产品接入（2026-09-06，km 0.3.0-p2）

`km shell` 已接入产品入口（internal/cli/shell.go + internal/session/shell.go）：

1. **会话登记**：容器侧新增 `km-shell` 脚本——登记 pid/kind=shell 后 `exec bash`。
   shell 会话与工具会话同住 /tmp/km-sessions，M0 的核验语义（KM_SESSION_ACTIVE/
   KM_SESSION_UNKNOWN）对 shell 同样生效：宿主强杀后的活跃 shell 会阻断后续
   run/shell（集成实测），显式 cancel 后恢复。
2. **信号整合**：`cmd/km/main.go` 的全局 `signal.NotifyContext` 对 shell 无影响——
   RunShell 不接收 ctx（Starter 用普通 exec.Command，非 CommandContext），并安装
   自有 handler（SIGWINCH 转发、SIGTERM/SIGHUP 兜底恢复+143、SIGINT 忽略）。
   NotifyContext 在 shell 期间收到的信号只会标记 ctx（无人消费），退出后恢复默认。
3. **锁与串行**：shell 全程持有项目锁——shell 期间第二个 shell/run → BUSY；
   容器侧活跃 shell（崩溃遗留）→ KM_SESSION_ACTIVE。
4. **尺寸同步**：启动后延迟重发两次 SIGWINCH 给客户端（客户端转发 exec resize），
   转发即时生效于后续窗口变化。
5. **退出清扫**：正常退出后 km 主动 Sweep 自己的会话目录；异常路径遗留 STALE
   由下次执行清扫（M0 语义）。SIGTERM 路径退出 143，bash 存活属 ADR-004/005
   已记录的失联语义。
6. 复用 `verifyNoActiveSession`：run 与 shell 共享同一核验实现（review 发现的
   重复已消除）。

## C2 加固轮（review 修复，2026-09-07）

三个真 bug（均有现场取证与回归）：

1. **/proc/stat 字段错位**：km-ctl 的 read 把第 5 字段 pgrp 当 session 读，
   判定与清理漏掉全部作业组——「sleep A | sleep B & 泄漏」的真正根因
   （管道成员 pgrp=组长 pid ≠ bash pid）。修正为第 6 字段 sess；
   sessions/sweep/cancel 三处一致。
2. **km-ctl alive 漏 shift**：拿 "alive" 字面量当 sid 查询 → 恒 exit 3 →
   finalize 误判「登记缺失」永不清理。补 shift；alive 排除僵尸态。
3. **项目根未规范化**：os.Getwd 信任 stat 等价的 $PWD（macOS /tmp 与
   /private/tmp），经符号链接进入项目即假报 KM_CONTAINER_CONFLICT。
   新增 project.CanonicalPath 统一 init/loadProjectStack/doctor。

RunShell 生命周期：快照移到 Start 前（失败不启动）；Notify 提前消除空窗；
SIGTERM/SIGHUP 路径「杀客户端→SID 域清理→恢复 termios→同步→143」；
finalize 轮询区分「bash 退出中」与「真脱离」（Detached 保留登记阻断）。

## Remaining risks（接入 km shell 前需处理）

1. shell 会话尚未纳入 km-ctl 会话登记：bash 逃逸作业只能靠 doctor/ps 观测；接入时
   评估把 shell 会话注册进 /tmp/km-sessions（proto 阶段未做，避免与 M0 阻断语义耦合）。
2. 首次窗口尺寸竞态：resize 需在客户端就绪后才可靠（原型以 settle 缓解；接入时可
   考虑启动后主动同步一次尺寸——需 exec resize API 或等价物）。
3. `signal.NotifyContext`（cmd/km/main.go）与 shell 信号语义的整合：接入产品时 shell
   命令必须使用自有 handler（本 ADR），不复用全局取消 ctx。
4. 环境限制记录：proxy.golang.org 不可达，依赖获取需 GOPROXY=goproxy.cn。

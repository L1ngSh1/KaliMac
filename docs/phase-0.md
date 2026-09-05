# P0 技术验证记录（2026-09-06）

环境：macOS 26.6.2 / Apple Silicon (arm64)；Go 1.26.6 darwin/arm64；Docker CLI 29.6.1；Docker Desktop 引擎 29.6.1（linux/aarch64，10 CPU，8 GB）。
Docker Hub 直连超时；实际经镜像站 `docker.1ms.run` 拉取镜像。此为网络观察，不改变"本机 Docker Desktop"这一验证平台。

实验资源全部使用唯一标识 `km-p0-20260906a`（容器、/tmp 项目目录），实验后清理。

## 实验矩阵与结论

| 实验 | 内容 | 结果 | 关键证据 |
|---|---|---|---|
| E1 | argv 透传：空串/空格/中文/引号/前导`-`/通配符字面量 | PASS | 容器内逐项回显，逐字匹配 |
| E2 | 工具 `--help` 留给工具 | PASS | argv.sh 收到 `--help` 而非被 docker/km 解释 |
| E3 | 二进制 stdin/stdout 往返 | PASS | 64 KiB 随机数据 `docker exec -i cat` 哈希一致 (`f45cd2c2…`) |
| E4 | stdout/stderr 分离 | PASS | 两条流分别重定向后内容各自正确 |
| E5 | 退出码 0/7/42 | PASS | `docker exec` 逐一原样返回 |
| E6 | 容器内命令不存在 | PASS | 退出码 127 + OCI runtime 错误信息（分类依据） |
| E7 | 中文/空格路径 + 子目录 cwd | PASS | `-w "/workspace/中文 目录/子 目"` 正确，读写正常 |
| E8/E9 | 挂载双向读写、容器 stop/start 后数据保留 | PASS | 容器写→Mac 读、Mac 写→容器读、重启后文件完好 |
| E10a | 非 PTY，docker exec 客户端被 SIGKILL | **残留** | 容器内 sig.sh+子进程继续运行，sig.log 无 TRAP |
| E10b/E11 | 非 PTY（无/有 `-i`），客户端被 SIGINT | **残留** | 客户端退出码 0，容器内任务无任何信号 |
| E12 | PTY（`-it`），键入 Ctrl-C（0x03 字节） | PASS | TRAP 在容器内触发，客户端退出码 130 回传 |
| E13a | PTY，docker 客户端被外部 SIGINT | **残留** | 客户端退出码 0，容器内无 TRAP |
| E13b | PTY，docker 客户端被 SIGKILL（模拟关终端） | **残留** | 容器内任务继续运行 |
| E14 | `docker stop` 兜底 | PASS | 全部 exec 进程清除；数据保留；stop 耗时 10.1s（PID 1 为 `sleep` 不处理 SIGTERM，等满宽限期） |
| K1 | Kali rolling (arm64) 基线 | PASS | 镜像 ID `sha256:ed99295a…`；6 个候选工具均未预装；`apt-get update` 可达（21.6MB/5s） |

## 三个硬问题结论

**A. 文件与工作目录**：bind mount 单项目映射可靠。中文/空格路径、子目录 cwd、双向读写、停止后保留全部通过。项目外路径默认不可见（未挂载即不可达）。

**B. argv / I/O / 退出码**：argv 数组逐元素透传完全可靠，无需 shell 字符串。stdio 三流独立可捕获。退出码 0/7/42/127 原样回传。`docker exec` 不做任何参数改写。

**C. 中断与进程清理（核心风险，证实）**：
- 杀死 `docker exec` 客户端（无论 SIGINT/SIGKILL、无论 PTY 与否）**从不**向容器内进程发信号——计划中的技术闸门被实验证实。
- 唯一可靠的用户交互中断路径：PTY 中键入 Ctrl-C（0x03 经容器内 PTY 线路规程变成 SIGINT），任务可自行捕获，客户端退出码 130 正确回传。
- 兜底清理：`docker stop` 可靠清除所有 exec 进程，但中断用户其他会话，且 PID 1 不处理 SIGTERM 时要等满 10s 宽限期。

## 对 P2 的直接约束

1. 非 PTY 工具执行（`km TOOL`）不能依赖客户端死亡传播中断。km 进程收到 SIGINT/SIGTERM 后必须主动清理容器内对应任务。
2. 可选方案（按侵入性排序，P2 实现时基于本记录选择）：
   - a) 会话执行桥：容器内常驻极小 supervisor（每个 exec 会话一个，argv 身份含随机会话 ID），km 客户端退出前通过第二个 exec 通知其终止目标会话；范围受控，只动自己创建的会话。
   - b) Docker Exec API（Go SDK）：km 直接持有 exec 连接，客户端进程即 km 自身，Ctrl-C 处理在同一进程内完成；仍无法对已 detach 的 exec 发信号，但不存在"第三方客户端"存活问题。
   - c) 最小可用：非 PTY 工具执行时 km 捕获 SIGINT 后执行 `docker exec <c> pkill -f <session-id>`——只能作为过渡，进程匹配范围必须限定会话 ID。
3. 容器主进程应处理 SIGTERM（如 `sleep infinity` 换成可捕获信号的小 init），否则 stop 恒等 10s。
4. 镜像策略确认：裸 Kali 无任何候选工具，需在本地构建精选镜像（apt 源可达）。

## 原始证据位置

实验脚本与输出：`/tmp/km-p0-20260906a/`（e1-argv-stdio-exit.sh、e9-persist.sh、e10-e13-signal.sh、pty_driver.py；清理前的输出摘要已收录本文件表格）。

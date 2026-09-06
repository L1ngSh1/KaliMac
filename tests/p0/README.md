# tests/p0 — P0 技术验证实验（可重跑）

这是 P0 阶段三个硬问题（文件映射、I/O 与退出码、信号清理）的最小可重跑实验集，对应 docs/phase-0.md 的结论。上一轮证据曾放在 /tmp 并被清理（review F5），现已入库：脚本在 `tests/p0/`，原始输出在 `tests/p0/evidence/<run-id>/`。

## 运行

```bash
bash tests/p0/run-p0.sh [工具镜像] [kali镜像]
# 默认: docker.1ms.run/library/busybox:stable + docker.1ms.run/kalilinux/kali-rolling:latest
```

脚本行为：创建带唯一 run-id 标签的实验容器与临时项目（含中文/空格目录、二进制 fixture、信号 fixture），依次执行下列实验，全部原始输出（含完整哈希与镜像 ID）写入证据目录，结束后删除本 run-id 容器与临时目录并输出核对结果。保留证据目录。不执行全局 prune，不触碰用户其他容器。

## 实验与判据

| 实验 | 证据文件 | 判据 |
|---|---|---|
| E1–E6 argv/stdio/退出码 | `10-argv-stdio-exit.log` | argv 逐项回显逐字匹配；bin.in 哈希两侧一致（完整 64 位）；退出码 0/7/42/127 |
| E7–E9 路径/挂载/持久化 | `11-path-mount-persist.log` | 中文/空格 cwd 正确；容器写→Mac 读；stop/start 后数据保留 |
| E10/E11 非 PTY 信号 | `12-signals-nonpty.log` | **预期失败场景**：客户端被杀后 sig.log 无 TRAP、容器内 sig.sh/sleep 仍存活（证明 docker exec 客户端死亡不传播信号） |
| E14 stop 兜底 | `13-stop-cleanup-persist.log` | stop 后容器内仅剩 PID1；挂载数据完好；记录 stop 计时 |
| E12/E13 PTY 信号 | `14-pty-*.log` | ctrl_c：TRAP 出现、客户端退出码 130；sigint/sigkill：无 TRAP（外部杀客户端不传播） |
| K1 Kali 基线 | `15-kali-baseline.log` | 候选工具预装情况；apt 源可达性观察 |
| 清理核对 | `16-cleanup-verification.log` | 本 run-id 容器残留数 0；fixture 哈希前后一致 |

文件清单：`run-p0.sh`（编排）、`pty_driver.py`（PTY 中断注入）、`fixtures/argv.sh`、`fixtures/sig.sh`（容器内 fixture）。

注意：E10–E13 是"探明风险"的实验，其"失败"即结论本身；km 自身信号处理方案在 P2 实现后，以同样的 fixture 重跑并断言 TRAP 与清理。

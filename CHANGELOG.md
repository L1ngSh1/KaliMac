# CHANGELOG

版本约定：`主.次.修-阶段`（如 `0.3.0-p2`）；版本常量在 `internal/cli/cli.go` 的 `Version`。
历史轮次的详细验证记录见 [docs/verification.md](docs/verification.md)。

## 未发布（本轮 goal 1789353851 的变更；候选版本号、tag 与发布命令为待审步骤，尚未执行）

### 修复

- 集成测试资源生命周期：`TestC1DetachKeys` 与 C2 探针分支中 `km init` 创建的项目容器
  纳入登记清理（此前任何中途失败/甚至正常通过都会泄漏容器，第六轮验证记录的遗留问题定案）。
- 套件终检与残留核对区分「确认消失 / 确认残留 / 无法核实」（新增 `internal/residue`）：
  Docker 查询失败不再被等价为「容器不存在」，无法核实时套件以非零退出并输出
  `P2B-CLEANUP-UNVERIFIED`。
- 集成 TestMain 增加引擎预检：Docker 不可达时显式 `P2-INTEGRATION-SKIP` 跳过，
  `-run '^$'` 不再访问引擎或触发镜像构建。
- doctor：会话检查自身失败现在计入摘要统计（警告）；exit 127（init 后首次 run/shell 前
  km-ctl 未安装）单列并说明为预期状态，不再出现「检查失败却被摘要为 0 失败 0 警告」。

### 新增

- `internal/residue`（可注入 fake 的资源终检判定，含负向单测）。
- CI 工作流 `.github/workflows/ci.yml`（verify：fmt/vet/test/race/build；独立 integration
  作业，显式引擎预检与镜像构建前提）。远端 CI 在首次 push 前不会运行。
- `make fmt-check`、`make integration`。
- `scripts/install.sh` / `scripts/uninstall.sh`：可配置 `PREFIX`/`DESTDIR` 的清单式安装与卸载。
- `scripts/package.sh`：macOS 双架构构建 + SHA256SUMS + 版本元数据 → `dist/`。
- `tests/perf/perf-baseline.sh` 重写（v2）：单调时钟、单一驱动进程计时、逐样本退出码检查、
  3 轮 × 20 配对 `docker exec` 对照、p50/p95 与原始样本（JSON/CSV）；配套
  `tests/perf/perf-negative-test.sh` 负向回归。

### 性能基线（macOS 26.6.2 / arm64，go1.26.6，Docker 29.6.1，2026-09-14）

- 热调用 p50：km 203.1–207.1ms vs 裸 `docker exec` 44.7–45.7ms（三次独立运行，每轮 60 对
  有效样本）；开销约 157–162ms，主要由约 6 次 docker CLI 子进程启动构成（每次 25–35ms）。
- 唯一重复查询（锁前身份核验与锁内恢复检查的两次 container inspect）为有意的 TOCTOU 设计；
  去除/合并会改变 BUSY 语义或削弱活跃会话保护，且节省低于噪声（p50–p95 差约 24ms），
  故保留原实现。

## 0.3.0-p2

交互版（P2-C）：`init → run → shell → stop → 恢复` 闭环 + 交互 bash（PTY 接管、作业控制、
窗口跟随、外部信号终端恢复、SIGKILL 后活跃会话保护与显式 cancel 恢复）。详见
[docs/verification.md](docs/verification.md) 与 [docs/user-guide.md](docs/user-guide.md)。

## 0.2.0-p2 / 0.1.x

非交互最小闭环（P2-B）与 P0/P1 基础语义、doctor 身份核验。详见 [docs/verification.md](docs/verification.md)。

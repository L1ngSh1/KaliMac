# CHANGELOG

版本约定：`主.次.修`，候选版使用 `-rc.N`（历史 `-pN` 保留原记录）；版本常量在 `internal/cli/cli.go` 的 `Version`。
历史轮次的详细验证记录见 [docs/verification.md](docs/verification.md)。

## 0.4.0-rc.1（首个公开预发布候选）

从内部 `0.4.0-p3` 候选收敛而来。本轮冻结新功能：统一发布身份、更新首次上手手册，
增加针对最终安装包的可重跑验收。下列功能与修复均汇入本次首发；历史验收记录保留原时点，
本版的发布门禁与平台边界见 [发布说明](docs/releases/v0.4.0-rc.1.md)。

### 新增

- `km env list`：只读查看当前项目的环境资源总览（角色 CURRENT/PREVIOUS/RETAINED/
  TRANSACTION/UNTRACKED、完整容器 ID、实际状态、可删除性）；MISSING/UNKNOWN/
  CONFLICT 严格区分；不取锁、不写任何文件。
- `km env remove <完整容器ID>`：显式删除一个已停止（exited）的 retained 保留容器。
  仅接受完整 ID；当前环境/回退目标/事务资源始终受保护；普通 docker rm（无 -f/-v，
  不自动 stop、不强制删除）；事务化账本收尾（journal 先行、rm 后确认不存在、
  外科手术式移除条目），中断由 km env recover 收尾（永不重建容器）；失效记录
  （容器已被外部删除）仅清理账本并明确说明。
- `km env switch/rollback/recover`：项目环境切换与单代回退（合同冻结于
  docs/adr-environment-transactions.md）。switch 只接受本地引擎已存在的同平台镜像
  （不拉取不构建）：只读探测会话依赖 → 按镜像内容 ID 创建候选容器 → 停止并保留旧容器；
  同内容为明确 no-op；目标引用漂移在提交前拒绝。rollback 单代回退、消费回退槽位、
  被撤容器计入 retained 账本；引用漂移明确拒绝。recover 按冻结规则恢复未完成事务
  （提交点前回前态、之后完成新态，幂等）。`--dry-run` 全程只读；非交互须 `--yes`。
  事务记录（.km/env/，不入库）携带操作 ID/前后快照/哈希备份；旧二进制经 state_version 2
  门槛明确拒绝（实测）；run/init/stop 在事务未完成时阻断；status/doctor 展示当前代、
  未完成事务与账本恒等式。新增稳定码 `KM_TRANSACTION_PENDING`、`KM_NO_PREVIOUS`、
  `KM_PLATFORM_MISMATCH`。
- `km tools`：只读查看精选六项工具（python3、curl、jq、file、openssl、nmap）在当前
  项目容器内的可用性（AVAILABLE 附解析路径 / MISSING）；清单不枚举容器全部软件，
  AVAILABLE 不保证版本或执行结果；缺失时给出维护镜像 Dockerfile 的指引（临时安装
  不构成可复现配置）。执行失败/协议异常 → `KM_TOOLS_PROTOCOL` 等稳定码非零退出，
  绝不把失败当作 MISSING；容器停止/暂停时明示未检查原因；不取执行锁、不改任何状态。
- `km sessions`：只读列出当前项目的容器内会话（`ACTIVE <id>` / `STALE <id>`，完整 ID
  可复制）；无会话、脚本未安装、容器未运行均有明确定义的非失败输出；查询失败/输出
  异常 → `KM_SESSION_UNKNOWN` 非零退出。
- `km cancel <id>`：显式取消当前项目的指定会话并核验终态。完整 ID 精确匹配（非法格式
  为用法错误）；列表中不存在的 ID 明确报错、不宣称成功、不操作其他项目；活跃取消 →
  「已取消并确认收尾」；已结束/已清扫 → 幂等说明；km-ctl 退出码 4 → 非零并保留诊断。
  两命令均不取项目执行锁（必须恰在锁持有人卡死/死亡时可用），并发正确性由容器侧
  km-ctl 协议（0/3/4）幂等保证。
- `KM_SESSION_ACTIVE` 阻断提示与帮助文本指向新恢复入口；底层
  `docker exec … km-ctl cancel` 保留为高级排障手段。

### 修复

- 发布打包排除 macOS `._*` 扩展属性文件；安装包验收直接检查 tar 成员，避免把资源分叉当成普通文件交付。

- F2（试用记录）：容器脚本中 `/proc` 读写的 shell 重定向顺序缺陷——POSIX 重定向自左
  向右生效，旧写法 `< "$p/stat" 2>/dev/null` 使输入重定向的 "cannot open" 错误在
  stderr 重定向生效前逃逸到用户终端。纯观感缺陷（清理逻辑与退出码一直正确）；5 处
  统一修复并加回归守卫测试；脚本随 run/shell 引导刷新后生效。

### 行为澄清（非变更）

- 被 `km cancel` 外部取消的运行中任务，客户端退出码为工具真实状态（TERM=143 原样
  透传）；Ctrl-C 的 130 语义不变。

### 历史会话恢复阶段验收（非本版全量计数；macOS/arm64）

- 新增单测 12 项（两命令合同全覆盖）+ 容器脚本重定向守卫。
- 真实集成：关键端到端链路（长任务→强杀→阻断→sessions→cancel→核验→恢复）连续
  3/3 轮通过；参数/隔离/重复与自然退出竞争/F2 零噪音 4 项通过；全套件回归
  143s 零 SKIP 零残留；gofmt/vet（含 integration tag）/单测/race/build 全绿。

## 未发布（上一轮 goal 1789353851 的变更；已在 2026-09-16 随预发布验收推送）

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

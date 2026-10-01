# Kali-Mac 架构短记

目标不变：Mac 终端输入 `km 工具 参数`，在当前项目的 Kali 容器执行；文件留在 Mac，输出与退出状态回原终端。

当前形态（P2-C 交互版）：`init → run → shell → stop → 恢复` 闭环 + 交互 bash（PTY）。

```text
Mac 终端
  └─ km（Go, 本仓库）
      ├─ internal/cli        分派/帮助/版本/doctor/init/run/shell/stop；工具 argv 原样透传
      ├─ internal/project    .km.json 校验；.km/ 本机状态（v1/v2）、项目锁与身份
      ├─ internal/runtime    docker CLI 封装（Executor 可注入，测试用 fake；endpoint 解析与固定）
      ├─ internal/envtxn     环境切换事务记录层：transaction/previous/retained 的
      │                      schema 校验、原子写、哈希备份（不接触 Docker，ADR §2）
      ├─ internal/session    会话内核：唯一会话身份、容器内侧进程组清理、km-ctl/km-run 协议、
      │                      DockerController/ExecStarter（P2 执行与交互 shell 的事实标准，见 ADR-004/005）
      ├─ internal/residue    测试终检的资源判定纯函数：「确认消失/确认残留/无法核实」三类区分，
      │                      查询失败不等价为资源不存在（internal 集成套件终检复用）
      └─ cmd/{km,shellproto} 入口；shellproto 为 C1 实验驱动器
```

## P0 改变/确认的设计决策

1. **信号路径（ADR-001）**：实验证实杀死 `docker exec` 客户端不传播任何信号到容器内任务；只有 PTY 中键入的 Ctrl-C（0x03 字节）可靠到达任务。因此 P2 的非交互工具执行必须由 km 进程主动做会话级清理，方案选择见 docs/phase-0.md「对 P2 的直接约束」。
2. **endpoint 先行（ADR-002，review F1）**：任何引擎查询之前，仅用客户端命令解析有效 endpoint（DOCKER_HOST > DOCKER_CONTEXT > `context inspect` 当前 context）。非本地 endpoint 直接失败且不发引擎请求；本地 endpoint 通过给子进程注入 `DOCKER_HOST` 固定，保证一次 km 运行的所有调用命中同一引擎，并与 `.km/state.json` 记录的引擎身份比对。
3. **容器主进程**：必须处理 SIGTERM（避免 stop 恒等 10s 宽限期）。
4. **镜像**：裸 `kalilinux/kali-rolling` 无候选工具；P2 起本地构建精选镜像（apt 源可达性已实测）。本机拉取走镜像站 `docker.1ms.run`（Docker Hub 直连超时的网络观察）。
5. **身份以不可变 ID 为准（ADR-003，review F2/F3）**：容器归属检查从记录的完整容器 ID 出发（名称只作展示与同名重建诊断）；镜像有引用（标签）、init 时记录的内容 ID、容器实际内容 ID 三层，doctor 比对后两层并报告漂移。
6. **管理命令有界（review F4）**：docker 管理查询单项 10s 超时、doctor 整体 45s 预算；超时/取消均终止子进程并返回 `KM_TIMEOUT`/`KM_CANCELED`。不适用于 P2 长工具执行。
7. **运行时层保持单实现**：只有 docker CLI 一个后端；Executor 接口仅为可测试性存在，不做成插件框架。
8. **测试资源生命周期（goal 1789353851 收口）**：集成测试创建的每个容器按完整 ID 登记进套件终检
   （TestMain）；终检区分「确认消失/确认残留/无法核实」，查询失败响亮失败而非静默通过；
   Docker 引擎不可达时 TestMain 预检显式 `P2-INTEGRATION-SKIP` 跳过（不在 `-run '^$'` 下访问引擎或构建镜像）。
9. **锁与恢复命令的协作（会话恢复轮）**：项目执行锁只保护 run/shell/init 的执行临界区；
   `km sessions`（只读）与 `km cancel <id>`（定向控制）**不取执行锁**——它们必须恰在锁
   持有人卡死或死亡时可用。并发正确性不由锁保证，而由容器侧 km-ctl 协议幂等性保证
   （0=已收尾、3=已消失、4=未确认）；cancel 前先列会话确认归属与存在，列表与取消之间
   的状态变化由协议码兜底。归属门禁复用 run 的容器身份检查（不含镜像内容检查：镜像
   漂移不阻止只读恢复）。
10. **环境切换是显式事务（ADR：docs/adr-environment-transactions.md）**：`km env
   switch/rollback/recover` 以项目本机状态目录 `.km/env/` 的事务记录（操作 ID + 前后
   快照 + 配置/状态哈希备份）与冻结的恢复表实现可中断切换；提交点 COMMIT_INTENT 之前
   失败收敛回前态、之后收敛到新态，recover 幂等。旧二进制兼容以 state_version 2 为
   门槛（旧构建在接触 Docker 前明确拒绝，已用基线提交二进制实测）；候选容器按镜像
   内容 ID 创建并携带 km.op/km.gen/km.role 标签，创建响应丢失按 op 标签核验登记；
   资源账本恒等式（当前代 + 上一代 + retained + 事务资源 = 实际容器）由 doctor 只读
   核验。run/init/stop 在取锁后检查未完成事务并阻断；sessions/cancel 保持可用。

## 错误模型

工具退出码原样返回；km 基础设施错误一律携带 `KM_*` 稳定标识（见 docs/cli-contract.md），避免与工具退出码数值重叠混淆。

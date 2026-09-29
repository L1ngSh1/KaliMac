# 验证记录

## 第九轮：环境切换与单代回退（2026-09-28，environment-switch 计划）

计划与冻结合同：docs/environment-switch-plan.md + docs/adr-environment-transactions.md（P0 冻结）。
进度与需求映射：docs/environment-switch-progress.md。
证据：tests/evidence/environment-switch/1790579945/。本轮未提交/未推送（计划约定）。

### 交付

- `km env switch --image <ref> [--dry-run] [--yes]` / `km env rollback [--dry-run] [--yes]` /
  `km env recover [--dry-run] [--yes]`；`env` 为管理命令，重名工具仍可 `km run -- env …`。
- 事务层 `internal/envtxn`（transaction/previous/retained，env_version 严格校验、原子写、
  哈希备份）；state_version 2（env 块）作为旧二进制明确拒绝门槛，v1 项目完全兼容（不批量迁移）。
- 冻结的恢复表：COMMIT_INTENT 前失败收敛前态、之后收敛新态；recover 幂等；
  资源身份无法确认时停止写操作并保留事务；外部修改 config/state 拒绝覆盖。
- run/init/stop 取锁后事务阻断（KM_TRANSACTION_PENDING）；sessions/cancel 不受影响。
- status 新增 `env_transaction_pending` 状态与当前代展示；doctor 新增环境记录节
  （事务/槽位/retained 健康 + 账本恒等式只读核验）。
- 新增稳定码：KM_TRANSACTION_PENDING / KM_NO_PREVIOUS / KM_PLATFORM_MISMATCH（已入 cli-contract.md）。

### 验证（本机 darwin/arm64 实测；Docker Engine 29.6.1 本轮启动）

- `gofmt -l .`：本轮改动文件干净（docs/review-p2-20260906/ 为先于本轮存在的未跟踪备份，
  其中 1 文件本就未格式化，按计划原样保留未触碰）。
- `go vet ./...`、`go vet -tags=integration ./...`：PASS。
- `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go build ./...`：PASS。
- `go test -tags=integration -count=1 -timeout 15m -v ./tests/integration/`：
  **67 RUN / 62 PASS / 0 FAIL / 0 SKIP（161.997s）**，含本轮新增真实集成：
  TestEnvSwitchRealHappyRollback（dry-run 零写入 → A→B 实际执行 marker 证明 → 旧容器停止
  保留 → 回退 → marker 证明回到 A → 文件改动不回滚 → 槽位消费 → 二次回退拒绝 → 账本恒等式
  实际容器=当前代+retained=2）、TestEnvSwitchRealNoOp、TestEnvSwitchRealMissingImage。
  零清理标记；引擎复核 `docker ps -a --filter label=km.owner=km` 为 0 个残留。
- fake 层覆盖（internal/cli/env_*_test.go，24 项）：负向拒绝矩阵（镜像缺失/平台冲突/
  身份冲突/会话活跃/未知/事务阻断/用法错误，全部断言零容器调用与零文件写入）、
  全链路 switch/rollback、no-op、槽位轮换、恢复表全阶段（PREPARED→CURRENT_COMMITTED、
  rollback 前后、资源消失拒绝）、创建响应丢失（op 标签核验）、并发互斥、多项目隔离、
  损坏记录阻断、非交互 --yes 合同、拒绝确认退出码 1（KM_CANCELED，冻结值）。
- 旧二进制兼容实测（无需 Docker）：以基线 1d3eaab 构建 km，对 v2 状态夹具
  `km status`/`km stop` 均在接触 Docker 前拒绝（`.km/state.json: 不支持的状态版本 2`，
  退出 1）；当前构建对同一夹具正常通过状态解析。证据见 summary.md。

### 独立审查与修复（同轮）

独立审查（重点：测试是否覆盖声称的失败路径）结论 FIX-FIRST，全部修复：

- **P0**：二次及以后 switch 在 COMMIT_INTENT 崩溃时 recover 死锁（槽位轮换窗口未覆盖）。
  修复：恢复路径完成槽位轮换（同一归属核验）；回归 TestEnvRecoverCrashAtCommitIntentWithSlot。
- **P1**：取锁后重读 cfg/st（switch/rollback）；retained.json 预检；外部编辑冲突测试
  （提交前检测 → 拒绝并收敛前态，外部修改保持原样；recover 侧拒绝 + 恢复后收敛）。
- **P2**：no-op 后置于全部门禁；op 资源删除需名称+镜像内容匹配；switch/rollback 校验
  容器实际镜像↔记录；recover 确认后事务变化放弃；补 CANDIDATE_VERIFIED、rollback
  PREPARED、init 事务阻断、sessions 不阻断、阶段写点/SavePrevious IO 注入测试；
  删除同义反复的 v1-only 兼容测试（以真实 1d3eaab 旧二进制实测为准）。
- 修复后全量复跑：unit/race/vet(双 tag)/build PASS；集成套件 **62 PASS / 0 FAIL**，
  env 三测全过，零清理标记，引擎复核 km.owner=km 残留为 0
  （integration-suite-post-review.log）。

### 验收缺口补齐（同轮追加，2026-09-29）

第一轮交付明确记录两个缺口，均已补齐并有可重复入口：

- **安装版验收（原 NOT-RUN → PASS）**：`tests/acceptance/install_env_switch_acceptance.sh`
  ——package.sh（仓库外调用、临时 DIST）→ SHA256 → 临时 PREFIX 安装 → 仓库外临时项目
  以安装产物执行 init/run/dry-run/switch/rollback/recover/doctor → 账本恒等式 → 卸载。
  **24/24 PASS**，构建身份=HEAD 2baa188、darwin/arm64 本机实测。
  **发现并修复真实缺陷**：doctor 账本核验按完整 ID 精确比对，被真实 `docker ps` 的
  12 位短 ID 全部误报"账本外"（fake 返回全 ID 故未暴露）；修复为前缀比对
  （internal/cli/doctor.go），env fake 的 ps 输出与容器引用对齐真实行为
  （TestDoctorShowsEnvRecords 锁定回归）。
- **真实 kill -9 阶段边界实验（原"未执行" → PASS）**：
  `tests/integration/environment_switch_kill_test.go` —— 独立进程组启动 switch/rollback，
  按阶段条件（事务出现/OLD_STOPPED/COMMIT_INTENT/CURRENT_COMMITTED/rollback 停止后）
  对进程组 SIGKILL，断言恢复不变量：recover 幂等收敛、事务清除、文件↔运行时一致、
  账本恒等式、doctor 无账本外告警、status 正常态。5 场景 PASS（4 场景真实命中
  SIGKILL；1 场景"提交点"进程在命中前已完整成功，竞态下两种落点不变量均成立）。
- 修复后全量复跑：unit/race/vet(双 tag)/build PASS；集成套件 **73 RUN / 64 PASS /
  0 FAIL / 0 SKIP**，引擎复核 km.owner=km 残留 = 0。
- 证据：tests/evidence/environment-switch/1790579945/{install-acceptance.log,
  kill9-stage-experiments.log, summary.md}。

### 边界与未验证（更新后）

- Intel 实机未做（amd64 仅 CI 交叉编译）；真实 kill 实验的落点存在观测竞态
  （不断言精确杀死时刻，以恢复不变量为验收标准）。

## 第八轮：会话查看与显式恢复（2026-09-16，session-recovery 计划）

计划与行为合同：docs/session-recovery-plan.md（S1 审查结论与 S2 冻结合同在该文件）。
证据：tests/evidence/session-recovery/。本轮未推送（计划约定不自动 push）。

### 交付

- `km sessions`（只读）与 `km cancel <id>`（定向取消），行为合同冻结于计划文档并落进
  cli-contract.md。两命令不取项目执行锁（S1 审查：锁只保护执行临界区；恢复必须恰在
  锁持有人卡死/死亡时可用），并发正确性由容器侧 km-ctl 协议幂等（0/3/4）保证；
  归属门禁复用 run 的容器身份检查，**不含镜像内容检查**（镜像漂移不阻止恢复）。
- `KM_SESSION_ACTIVE` 阻断提示改为指向 `km sessions` / `km cancel <id>`；帮助文本同步。
- 共享逻辑整理：run.go 抽出 verifyStackForQuery（verifyStackForMutation 复用，run 既有
  行为不变——既有单测/集成全绿佐证）。

### F2 根因与修复

试用观察到的 `/tmp/km-bin/km-ctl: 7: cannot open /proc/27/stat` 为 **shell 重定向顺序
缺陷**：POSIX 重定向自左向右生效，`< "$p/stat" 2>/dev/null` 的输入重定向先失败，错误
消息在 stderr 重定向前逃逸；`|| continue` 语义与清理结果始终正确 → 纯竞争噪音。
修复：5 处（km-run 1、km-ctl 3、km-observe 1）统一为 `2>/dev/null < "$p/stat"`，加
守卫测试 TestContainerScriptsRedirectOrder。生效时机=脚本随 run/shell 引导刷新。

### 验证（本地，未推送）

- 新增单测 12 项：sessions（空表/列表/127/查询失败三态/带参拒绝）、cancel（缺参/
  格式/未知 ID 不宣称成功/ACTIVE 收尾/STALE 幂等/exit3 幂等/exit4 非零）。
- 真实集成：关键端到端（长任务→强杀→KM_SESSION_ACTIVE 阻断含新入口指引→
  sessions 列出完整 ID→cancel「已取消并确认收尾」→列表空→run 恢复）**连续 3/3 轮
  通过**；参数/未知 ID/隔离与持锁可用性/重复与自然退出竞争 4 项通过。
- 全套件回归 143.4s 零 SKIP 零残留；gofmt/vet（含 integration tag）/单测/race/build
  全绿（s4-final-battery.txt）。
- 行为澄清（测试中发现并回填合同）：外部 `km cancel` 取消运行中任务 → 客户端退出码
  为工具真实状态 TERM=143（原样透传）；Ctrl-C 的 130 不变。重复 cancel 已清除的会话
  → 按合同报 KM_SESSION_UNKNOWN 退出 1（不把未知报成成功）。

### 审查收口（2026-09-17，外部静态审查三项发现全部修复）

1. **信息状态与未知 ID 混淆**：容器未运行/脚本未安装时 cancel 误报 KM_SESSION_UNKNOWN
   退出 1，与冻结合同「说明原因退出 0」不一致。修复：collectSessions 以 info 状态
   区分「查询不可执行」（说明原因退出 0）与「查询成功但 ID 不存在」（KM_SESSION_UNKNOWN
   退出 1）；新增单测 TestCancelInfoStatesExitZero。
2. **F2 证据未覆盖修复路径**：原「20 次未知 ID 零噪音」在 CLI 前置检查即返回，未进入
   容器侧 /proc 扫描——表述撤回，按下述三层定稿（F2 的修复依据是调整输入与 stderr
   重定向的先后顺序）：
   1) **源代码守卫**：检查预期的重定向顺序，防止旧写法重新引入；该测试验证代码形态，
      不单独证明运行行为。另有重定向机制行为测试（相同 shell、输入重定向指向确定
      不存在的文件）：旧顺序 stderr 出现 "cannot open"、新顺序为空——稳定验证重定向
      机制本身。
   2) **容器内对照实验**：本次进程翻涌实验观察到旧写法产生 220 条诊断，新写法为 0 条
      （s4c-f2-churn-demo.txt）。该结果支持修复有效，但并发负载存在随机性，不作为
      每次必现的保证。
   3) **端到端回归**：10 次真实活跃会话取消完成且未观察到该噪音。旧写法在另一轮
      10 次取消中同样未复现，因此该测试用于检查真实取消链路，不作为稳定区分新旧
      实现的依据。
3. **跨项目测试未用真实 ID**：改为取 A 的真实活跃会话 ID 在 B 中取消 → 断言拒绝
   （KM_SESSION_UNKNOWN 退出 1）且 A 任务仍存活（A 列表仍 ACTIVE）→ 从 A 正常取消
   成功（143 客户端语义验证保留）。

收口后全量回归：gofmt 零脏、vet（含 integration tag）PASS、单测/race 全 ok、build PASS、
全套件 145.3s 零 SKIP 零残留（s5-review-battery.txt）。

## 第七轮：goal 1789353851（2026-09-14，资源生命周期收口 + 端到端实走）

计划见 docs/goal-plan.md，逐项状态与证据见 docs/goal-progress.md（run-id `goal-1789353851`）。

### 测试资源生命周期（M1，关闭第六轮遗留「待查」）

第六轮记录的集成套件泄漏（km-p7a0d0063de，TestC1DetachKeys）本轮定案：`TestC1DetachKeys`
中 `km init` 创建的项目容器从未登记——既不在套件终检清单，也无 `t.Cleanup`；标签残留核对
（km.project=p2a-*）与套件终检都覆盖不到 km 命名容器。负向证明：还原基线后用例 PASS 但
`docker ps` 出现泄漏容器（km-pe37ee18ff5，Up 21s）；补 `registerProjectCleanup` 后连续三轮
`-count=3` 通过且零残留。同类分支全库排查：另修 C2_PROBE 探针分支（shell_c2_test.go）。

终检/残留核对不再把「查询失败」等价为「容器不存在」：新增 `internal/residue`（可注入
fake 的三态判定：确认消失/确认残留/无法核实），TestMain 终检与 `p2bResidueCount` 改用之，
无法核实 → `P2B-CLEANUP-UNVERIFIED` 且套件 rc=1（该路径在首轮修复回归中被真实触发过一次
——stderr 匹配过严，终检响亮失败而非静默通过，修正后复绿）。TestMain 增加引擎预检：
Docker 不可达时 `P2-INTEGRATION-SKIP` 显式跳过，`-run '^$'` 不再访问引擎或触发镜像构建。

### doctor 会话检查分类（M2 实走发现）

新项目 init 后立即 doctor：会话检查因 km-ctl 未安装失败（exit 127），但旧实现用 `r.line`
输出、不计入摘要——「0 警告 0 失败」掩盖未完成的检查。修复：检查失败一律计入 `警告`，
127 单列说明「会话脚本未安装（首次 run/shell 时安装）」。负向证明（旧码两测试 FAIL）+
真实新旧项目 doctor 输出对照。回归：TestDoctorSessionCheckFailureCounted /
TestDoctorSessionScriptNotInstalled127。

### 全套验证

`gofmt -l` 空；`go vet ./...`（含 integration tag）零告警；`go test -count=1 ./...` 全 ok
（新增 internal/residue）；`go test -race -count=1 ./...` 全 ok；`go build ./...` OK；
`go test -tags=integration -count=1 -timeout 15m ./tests/integration/` ok（133.7s），
套件终检零残留、套件外无本轮容器。用户指南从零实走（init→run→双向共享→PTY shell
（Ctrl-C 130/作业控制/窗口 40×100 跟随/exit 7 透传）→doctor→stop→恢复）全部通过，
演示资源按完整 ID 清理核对。回滚脚本在独立 worktree 验证可逆。

## 第六轮：审计修复（2026-09-12）

用户发起冗余代码审计，实测冒烟中发现两个行为缺陷，连同审计死代码一并修复。

### 行为修复

| 项 | 问题 | 修复 | 回归 |
|---|---|---|---|
| B1 doctor 挂载归属假冲突 | /tmp 项目 doctor 判 `KM_CONTAINER_CONFLICT`（挂载源 `/private/tmp/...` vs 项目根 `/tmp/...`）：init/run 均以 CanonicalPath 比较，唯独 doctor 未规范化 | `checkProjectConfig` 规范化项目根；`checkContainerOwnership` 挂载源与项目根**对称**做 CanonicalPath 后比较（等价拼写=同一物理目录） | TestDoctorMountSourceCanonicalized（macOS TempDir 非规范路径，fake 挂载源用规范化路径）；修复后 /tmp 真实冒烟 doctor 11 通过 0 失败 |
| B2 家目录成为隐式项目 | 家目录曾被误 init 成项目（残留 `~/.km.json`+容器）：任意目录下的 km 调用经向上查找被劫持到挂载整个 HOME 的容器，且污染 `TestHelpAndVersionZeroExternalCalls` | `FindConfig` 家目录硬边界：家目录不作为项目根，家目录之下的查找到 HOME 即止 | TestFindConfigHomeIsBoundary（t.Setenv HOME，覆盖家目录本身/子目录止步/HOME 内正常项目三态） |
| B3 cli 单测依赖宿主目录树 | `TestHelpAndVersionZeroExternalCalls` 等从包目录运行，向上查找会撞上宿主任意祖先目录里的 `.km.json`（环境相关失败） | 六个用例补 `t.Chdir(t.TempDir())`，与宿主 cwd/环境解耦 | 单测在污染前后的家目录状态下均稳定绿色 |

### 死代码与文案清理

- 删除零调用：`cli.notImplemented`（含过期"P1 骨架"文案）、`cli.requireLocalEngine`（与 `resolveEngine` 重复）、`runtime.ExecToolArgs`（生产路径被 `session.ExecSessionArgs` 取代，仅剩自测）、`runtime.CodeNotImplemented`、`executor.errors_As`（改用 stdlib `errors.As`）、`prod.baseName`（改用 `filepath.Base`）、`init.writeConfigAtomic`（透传别名，直接调 `project.WriteConfig`）。
- 过期文案：doctor "init 将在 P2 提供"/"P2 由 init 处理"→ 现行指引；endpoint 报错去掉 v0.1/v0.2 版本号措辞；cli.go 包注释、cli-contract.md 错误标识列表（移除 KM_NOT_IMPLEMENTED）同步。

### 环境清理

- 删除家目录残留项目：容器 km-pdbecd9fe4a（挂载整个 HOME，exited）+ `~/.km.json` + `~/.km/`（内容核验为 9 月 6 日测试残留后删除）。
- 集成套件本轮泄漏 1 个容器（km-p7a0d0063de，TestC1DetachKeys 临时项目，running）——按挂载源为 go test 临时目录核验后删除；"终检零残留"在该用例上未生效，待查。

### 全套验证

`gofmt -l` 空；`go vet ./...` 零告警；`go test -count=1 ./...` 全 ok；`go test -race -count=1 ./...` 全 ok；`go test -tags=integration -count=1 ./tests/integration/` ok（172s）；修复后 /tmp 真实冒烟（init→run→doctor→stop）doctor 11 通过 0 警告 0 失败。

## 第五轮：审阅加固（R1–R5，2026-09-06，未提交）

针对外部审阅的五个发现全部修复并有回归：

| 项 | 修复 | 回归 |
|---|---|---|
| R1 会话目录消失≠组清空 | km-run 主进程退出后排空组（TERM→有界轮询→KILL，/proc 扫描排除僵尸）再写终态删目录；km-ctl 以组空/目录消失/exit 文件任一为成功终态 | 集成 TestRunCancelChildIgnoresTERM（忽略 TERM 的同组子进程被清空）、TestRunNormalExitDrainsLingeringChild（正常退出残留子进程被排空） |
| R2 遗留锁后放行新任务 | 取锁后引导脚本并核验容器内会话：活跃 → `KM_SESSION_ACTIVE` 阻断+显式 cancel 指引；组空遗留 → sweep 清扫；doctor 报告会话 | 集成 TestStaleHostLockActiveSessionBlocked（SIGKILL 宿主 → 第二任务被阻 → doctor 报告 → 显式恢复后可执行） |
| R3 并发 init 双环境 | init 全程持项目锁（锁内读状态），锁原子化：唯一临时文件完整内容 + link(2) 发布（消除"先建后写"窗口），Release 带 token 所有权核验 | 单测 TestConcurrentAcquireSingleHolder、TestReleaseOwnershipGuard、TestEmptyStaleLockTakeover；集成 TestConcurrentInitSingleEnvironment（并发双 init → 恰一成功/一 BUSY、单容器、串行重试复用） |
| R4 endpoint 未贯穿 | 解析出的 endpoint 以替换式 `runtime.ReplaceEnv` 注入管理查询、docker cp、流式 exec、cancel 全部子进程（不产生重复键遮蔽）；`ExecStarter.Env`/`DockerController.Endpoint` | 单测 TestExecStarterPinsEndpointEnv、TestDockerControllerPinsEndpointEnv、TestCommandExecutorReplaceEnvNoDuplicate |
| R5 pull/stop 超时被 10s 截断 | `runWithTimeout` 按操作类型一次生效（pull 10m、stop 30s），上层更短预算仍优先 | 单测 deadline 观察器四例（pull≈10m、stop≈30s、管理≈10s、外层 2s 优先） |

修复过程中的测试缺陷也一并处理：SIGKILL 后 `cmd.Wait()` 被孤儿 docker exec 管道阻塞 → `WaitDelay`；tpid 快照改轮询；`TestInitFailureRecovery` 与重名 fixture 第二项目的容器泄漏 → 纳入登记清理。

全套验证：`gofmt -l` 空；`go vet ./...`（含 integration tag）零告警；`go test -count=1 ./...` 全 ok；`go test -race -count=1 ./...` 全 ok；`go build ./...` OK。真实集成 **26/26 PASS**（原 23 项 + 新增 3 项），套件按完整容器 ID 终检零残留；另清理了历史运行遗留的 31 个测试容器（逐个核验挂载源为 go test 临时目录后按 ID 删除）。

## 第四轮：P2-B 非交互最小闭环（2026-09-06，0.2.0-p2，未提交）

### fake/单元（`go test -count=1 ./...`、`-race` 全绿）

- 锁：获取/释放、活跃持有人 BUSY（KM_PROJECT_BUSY）、遗留锁接管（BrokeStale）、损坏锁接管；释放幂等。
- cwd 映射：根/中文空格子目录、符号链接逃逸拒绝、根经符号链接访问。
- init：全新创建（配置+状态+容器身份完整落盘）、幂等复用同一容器、停止容器复用时自动启动、缺失镜像拉取一次、同名冲突拒绝且不写状态、父项目拒绝（KM_PROJECT_NESTED）、引擎漂移拒绝（KM_RUNTIME_MISMATCH）。
- run 错误路径：容器缺失（KM_NOT_FOUND→init 恢复）、镜像内容漂移（KM_IMAGE_DRIFT）、忙碌、旧版状态拒绝接管。
- stop：运行中停止、已停止幂等（不再调用 stop）。

### 真实集成（`go test -tags=integration -count=1 ./tests/integration/`：23/23 PASS，47s）

P2-B 黑盒矩阵（TestMain 构建 km 二进制 + 确认/构建 kali-mac-min:0.2）：

| 验收项 | 结果 |
|---|---|
| init→run→stop→再次执行恢复闭环（同一容器、数据保留） | PASS |
| 重复 init（复用、零新增资源） | PASS |
| 复杂 argv 逐元素（空/中文/引号/前导-） | PASS |
| 二进制管道 32KiB 哈希一致 + stderr 独立 | PASS |
| 退出码 0/7/42 原样 | PASS |
| 中文/空格路径 + 子目录 cwd 映射 | PASS |
| SIGINT 取消 → 130 + 会话目录清空 + tpid 回收 | PASS |
| 多项目隔离 + 同项目 BUSY + 取消不停止容器 | PASS |
| 引擎漂移 / 同名冲突 / 镜像身份冲突 | PASS |
| 初始化失败恢复（拉取失败不落状态不留容器→修复后成功） | PASS |
| 六工具 smoke（python3/curl/jq/nmap 仅回环/file/openssl 已知答案） | PASS |
| P2-A 会话实验 11 项回归 | PASS |

清理：套件按完整容器 ID 登记，TestMain 终检全部消失（exit 0）；宿主残留 0；context 未修改。

### 最小镜像留证

tests/evidence/kali-mac-min-0.2/build-evidence.txt：基础镜像本地 ID（kali-rolling，sha256:ed99295a…）、构建产物 ID（sha256:f3e7ae6b…，arm64）、dpkg 实查包版本（python3 3.14.6-1、curl 8.21.0-2、nmap 7.99+dfsg-1kali1 等）。包源：构建时显式改写镜像自身 kali.sources 为 USTC 公开镜像（默认 http.kali.org 经本机代理间歇 502），不触碰用户全局配置。

### 结论与边界

P2-B 达成：非交互最小闭环（init→run→stop→恢复）全部验收通过。**这是非交互最小可用版：shell/PTY 属 P2-C 尚未实现**；SIGKILL/宿主失联后容器任务继续运行的风险语义见 ADR-004（doctor/锁只提示不自动清理）。

## 第三轮：P2-A 会话执行与取消验证（2026-09-06，未提交）

### fake/单元（`go test -count=1 ./...` 与 `-race` 全绿）

- state.container.id 校验：合法 64 hex/空 ID/短 ID/大写/非 hex/带前缀/65 位/含空格（8 例）。
- doctor：inspect 返回 ID 与记录不一致 → 冲突；挂载路径规范化；非法 ID 状态损坏。
- session 内核（fake）：argv 逐元素、正常退出无取消、取消中/取消竞争/未启动/不完整/失败兜底、启动前与引导中取消、tar 产物自检、Controller argv 与协议码 3/4 透传（新增，堵住 km-ctl "cancel" 参数漏检）。
- 竞争修复：Manager 取消分支先非阻塞采信已就绪结果；fake ready 通道构造期初始化（曾致 race 模式死锁 603s，已修）。

### 真实 Docker（`go test -tags=integration -count=1 ./tests/integration/`，11/11 PASS，23.8s）

argv 逐元素（含空串/中文/引号/前导-）、64KiB 二进制往返哈希一致 + stderr 独立、退出码 0/7/42、5s 长任务不被 2s 管理超时误杀、SIGINT 取消（noclean/selfclean/grandchild 三种 fixture 均 130 + 进程组清空 + tpid 被回收）、重复取消幂等（协议码 3）、启动阶段取消无残留、多项目隔离（A 取消 B 正常）、SIGKILL 客户端单独观察（任务存活风险确认 + 显式取消仍可清理）。
资源核对：run-id 标签残留容器 0。

### 可判定 P0 实验器（tests/p0/checked/）

正常模式 12 用例全 PASS、exit 0；负向控制（注入错误哈希期望）正确判定 FAIL 且 exit 1。证据：tests/p0/evidence/checked-*/（summary.json 含分类/PASS-FAIL-SKIP/耗时/完整哈希/镜像 ID/日志路径）。

### 结论

P2-A 通过：会话执行与取消验收全部成立，具备进入 P2-B（init→run→stop→恢复闭环）的条件。执行内核（internal/session）已就绪供 P2-B 复用。

## 第二轮：review 修复（2026-09-06，版本 0.1.1-p1）

针对外部 review（F1–F5）的修复验证：

```text
$ gofmt -l .                   → 无输出
$ go vet ./...                 → PASS
$ go test ./...                → ok internal/cli (0.9s) / internal/project / internal/runtime
$ go build -o ./bin/km ./cmd/km → PASS（版本 0.1.1-p1）
```

新增回归（fake Docker，断言调用序列）：
- F1：远程 context（`context inspect` 得 ssh://）→ `KM_ENDPOINT_REMOTE` 且零 daemon 查询；`DOCKER_CONTEXT` 覆盖同样拦截；状态 endpoint 漂移 → `KM_RUNTIME_MISMATCH` 且跳过容器/镜像查询；`DOCKER_HOST`+`DOCKER_CONTEXT` 冲突提示；`DOCKER_HOST` 本地 tcp 正常放行。
- F2：同名重建（按记录 ID 查无、按名查得新 ID）→ 冲突不接管；旧状态缺容器 ID → 警告且不按名称查询。
- F3：标签内容漂移 → 警告；容器实际镜像与记录不符 → 失败。
- F4：卡死的 docker（真实 sleep 脚本经 DockerPath 注入）→ `KM_TIMEOUT`，300ms 内返回；取消 → `KM_CANCELED`；help/version/未实现命令外部调用次数断言为 0。
- F5：P0 实验集入库并可重跑（tests/p0/），重跑证据与首轮结论一致（tests/p0/evidence/20260906-102425/）。

真实 Docker 冒烟（本机 desktop-linux，临时项目唯一标签，见本轮汇报）：
- 健康栈（state 含真实容器 ID + 镜像内容 ID + endpoint）→ 0 失败；
- 同名重建容器 → `KM_CONTAINER_CONFLICT`（同名重建）；
- 镜像内容漂移（state 记录假 ID）→ 漂移警告 + 容器内容冲突；
- 旧状态（删 container.id）→ 不完整警告，不接管；
- `DOCKER_HOST=ssh://fixture.invalid` → `KM_ENDPOINT_REMOTE`，无引擎查询、立即返回；
- 远程分类单元表（unix/npipe/tcp 回环/ssh/tcp 非回环）通过。
- 测试容器清理后 km.owner=km-p0-test 残留 0；context 未修改。

## 第一轮：P0＋P1 基线（2026-09-06，版本 0.1.0-p1）

## 环境检查（只读）

| 项 | 值 |
|---|---|
| 平台 | macOS 26.6.2, arm64 |
| Go | go1.26.6 darwin/arm64 |
| Docker CLI | 29.6.1 (`/usr/local/bin/docker`) |
| 引擎 | Docker Desktop 29.6.1, linux/aarch64（本轮启动；启动前离线） |
| 当前 context | desktop-linux（本地 unix socket；DOCKER_HOST 未设置） |
| 网络 | Docker Hub 直连超时；镜像站 docker.1ms.run 可用；Kali apt 源可达 |

## Go 验证（PASS）

```text
$ gofmt -l .                 → 无输出（已格式化）
$ go vet ./...               → PASS，无告警
$ go test ./...              → ok  kalimac/internal/cli      (0.7s)
                               ok  kalimac/internal/project (cached)
                               ok  kalimac/internal/runtime (0.8s)
$ go build -o ./bin/km ./cmd/km → PASS
$ ./bin/km --version         → km 0.1.0-p1
$ ./bin/km --help            → 帮助文本（不触发 docker 调用，由单测 failIfRun 断言）
```

单测覆盖：复杂 argv（空/空格/中文/引号/前导 `-`）、`--help` 留给工具、`run --` 解析与重名、未实现命令显式报错、配置损坏（坏 JSON/未知版本/未知字段/缺 image/类型错）、状态损坏、doctor 八种场景（无 CLI/引擎离线/远程 endpoint/无项目/坏配置/坏状态/健康栈/标签冲突/容器缺失/镜像缺失）、doctor 只读性快照断言。

## 真实 Docker 验证

| 项 | 结果 |
|---|---|
| E1–E9 基础语义（argv/stdio/退出码/路径/挂载/持久） | PASS（证据见 docs/phase-0.md） |
| E10–E13 信号实验 | 完成并给出结论；非 PTY 与外部杀客户端场景容器内残留（这正是要验证的风险） |
| E14 stop 兜底 | PASS（10.1s 宽限期观察记录） |
| K1 Kali 基线 | PASS（无预装工具；apt 源可达） |
| km doctor 真实集成（临时项目+真实容器+标签/挂载核验） | PASS：8 通过 0 警告 0 失败 |
| T01 无 Docker 时 help/version | 真实停机场景 SKIPPED（不停止用户运行中的 Docker Desktop）；由单测 failIfRun 保证代码路径不触碰 Docker |

mock/fake 与真实容器证据分开报告，未混用。

## 资源清理

本轮创建的容器（km-p0-20260906a-c1、km-p0-20260906a-k1、km-p1doctor2026）与 /tmp 临时项目在轮末删除并核对（见汇报）。保留：`docker.1ms.run/kalilinux/kali-rolling` 与 busybox 两个镜像（P2 直接复用，避免重新下载）；未执行任何全局 prune；用户既有容器与数据未触碰。

## 遗留风险

1. 信号清理的 P2 方案未选定（三个候选见 phase-0.md），这是 P2 主要工作量。
2. `docker stop` 10s 宽限期问题需容器主进程可捕获 SIGTERM 才能消除。
3. 无 Linux CI；macOS 为唯一验证平台（符合 v0.1 范围）。
4. PTY 下窗口尺寸变化（T22）未测，留 P2 shell 实现。

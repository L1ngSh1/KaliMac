# 环境切换与单代回退：进度

> 状态：**P0–P5 全部实现并通过验证**（本轮：2026-09-28，分支 `codex/environment-switch`，基线 `1d3eaab`）。
> 设计合同（冻结）：[docs/adr-environment-transactions.md](adr-environment-transactions.md)。
> 计划原文：[docs/environment-switch-plan.md](environment-switch-plan.md)。
> 证据：[tests/evidence/environment-switch/1790579945/](../tests/evidence/environment-switch/1790579945/)。
> 未经后续授权未提交、未推送、未打 tag；所有变更停留在工作区，可整体审查。

## 阶段状态

| 阶段 | 状态 | 说明 |
| --- | --- | --- |
| P0 基线与冻结设计 | 完成 | 基线核对（HEAD 1d3eaab 领先 master 5 提交，含 tool-discovery）；ADR 冻结命令合同/旧二进制策略/提交点/恢复表/回退轮换/不变量 |
| P1 预览与前置门禁 | 完成 | 负向测试先行；拒绝场景断言零容器调用、零文件写入、无锁残留 |
| P2 事务存储与恢复内核 | 完成 | `internal/envtxn` 版本化记录 + 原子写 + 严格校验；恢复表全阶段 fake 覆盖 |
| P3 真实 switch | 完成 | fake 全链路 + 真实 Docker 集成（A→B 实际执行证明） |
| P4 rollback 与重复操作 | 完成 | fake + 真实集成（B→A、槽位消费、二次回退拒绝、轮换） |
| P5 回归、审查与交付 | 完成 | 全量回归全绿；文档与证据交付 |

## 交付物

- `internal/envtxn/`：事务/槽位/账本记录层（env_version 严格校验、原子写、哈希备份）。
- `internal/cli/env.go / env_switch.go / env_rollback.go / env_recover.go`：
  命令分派、参数合同、门禁、预览、确认、事务执行与收敛恢复。
- `internal/project/state.go`：state_version 2（env 块）；v1 完全兼容。
- `internal/runtime/docker.go`：CreateContainer 扩展（额外标签/只读挂载/自定义命令）+ km.op/km.gen/km.role 标签。
- `internal/cli/status.go / doctor.go`：当前代、未完成事务（`env_transaction_pending`）、env 记录健康与账本恒等式核验。
- `internal/cli/run.go / stop.go / init.go`：取锁后事务阻断（KM_TRANSACTION_PENDING）。
- 测试：`internal/envtxn`（11）、`internal/project`（6）、`internal/cli`（151 项，其中 env 系列 38）+ 集成 3（真实 Docker）。
- 文档：ADR（冻结）、本进度、cli-contract.md（env 行 + 3 个新错误码 + state v2/env 记录）。

## 需求到测试对应（节选全部关键项）

| 计划要求 | 测试 | 结果 |
| --- | --- | --- |
| 预览无变更、与执行影响一致 | TestEnvSwitchDryRunNoWrites / 集成 dry-run 步 | PASS |
| 显式确认后可靠切换同平台镜像 | TestEnvSwitchHappyPath / TestEnvSwitchRealHappyRollback | PASS |
| 活跃/未知会话不绕过 | TestEnvSwitchRefusals/{会话活跃,会话未知} | PASS |
| 身份冲突/不支持容器状态不绕过 | TestEnvSwitchRefusals/身份冲突；gates（paused 等独立处理） | PASS |
| 内容身份固定、标签漂移不静默改变目标 | 候选按内容 ID 创建断言 / TestEnvSwitchRealNoOp | PASS |
| 提交前引用复验（过程中漂移 → 拒绝并收敛前态） | TestEnvSwitchRefDriftAtCommitRefused / TestEnvRollbackRefDriftAtCommitRefused | PASS |
| 容器状态门禁（仅 running/exited；paused 等独立拒绝） | TestEnvPausedContainerRefused | PASS |
| 单代回退兑现配置/容器/原运行状态 | TestEnvFullCycleSwitchSwitchRollbackRollback / 集成回退 | PASS |
| 环境回退不回滚共享文件 | 集成 note.txt 断言 | PASS |
| 各阶段失败/强杀恢复 | TestEnvRecover*（fake 全阶段）+ 真实 kill -9 实验（tests/integration/environment_switch_kill_test.go：switch 4 阶段 + rollback，5 场景 PASS） | PASS |
| 操作成功但响应丢失 | TestEnvSwitchCreateResponseLost（op 标签核验登记，不二次创建） | PASS |
| 旧状态 schema 兼容实测 | TestStateV2RejectedByV1OnlyReader + 真实 1d3eaab 旧二进制实测（status/stop 拒绝） | PASS |
| 资源账本恒等式 | 集成账本断言 + doctor checkEnvRecords | PASS |
| 原有命令回归 | 全量单测 + 全量集成（67 RUN 全过） | PASS |
| no-op 不耗槽位 | TestEnvSwitchNoOp / 集成 NoOp | PASS |
| 镜像缺失不拉取 | TestEnvSwitchRefusals/镜像缺失 / 集成 MissingImage | PASS |
| 平台按实际元数据判断 | 平台冲突用例（KM_PLATFORM_MISMATCH） | PASS |
| 并发互斥 | TestEnvSwitchBusyLockRefused（持锁 → KM_PROJECT_BUSY） | PASS |
| 多项目隔离 | TestEnvSwitchProjectIsolation | PASS |
| 事务记录损坏拒绝 | TestEnvDamagedTxnRecordBlocksAll（switch/rollback/recover/run 全阻断，原始文件保留） | PASS |
| 外部编辑不覆盖 | TestEnvSwitchExternalEditConflict / TestEnvRecoverExternalEditConflict（提交前拒绝 + recover 侧拒绝/恢复后收敛） | PASS |
| I/O 故障（阶段写点/SavePrevious） | TestEnvSwitchIOFailureAtStageWriteConvergesPreCommit / TestEnvRecoverIOFailureAtSavePreviousThenCompletes | PASS |
| 安装包验收 | scripts/package.sh 流程未改；本轮未重跑安装版验收（见未验证项） | NOT-RUN |

## 验证命令（2026-09-28 本机 arm64 实测）

```text
gofmt -l .                     # 仅 docs/review-p2-20260906/（先于本轮存在的未跟踪备份）1 文件未格式化，按要求未触碰
go vet ./...                   # PASS
go vet -tags=integration ./... # PASS
go test -count=1 ./...         # PASS
go test -race -count=1 ./...   # PASS
go build ./...                 # PASS
go test -tags=integration -count=1 -timeout 15m -v ./tests/integration/
                               # PASS：67 RUN / 62 PASS / 0 FAIL / 0 SKIP，161.997s，零清理标记
```

## 独立审查（P5）与修复

按计划要求执行了独立审查（重点：测试是否覆盖声称的失败路径），结论 FIX-FIRST，全部修复并回归：

| 级别 | 发现 | 修复 |
| --- | --- | --- |
| P0 | 二次及以后的 switch 在 COMMIT_INTENT 崩溃时，recover 因"槽位与事务记录不一致"永远拒绝（死锁，违背 ADR §4 收敛规则） | `rotatePreviousOnRecover` 完成槽位轮换（同一套归属核验 → 旧槽位转 retained → 写入新槽位）；回归测试 TestEnvRecoverCrashAtCommitIntentWithSlot |
| P1-1 | 取锁后复用锁外 cfg/st（状态派生事实过期，并发完成的 switch 会被覆盖） | switch/rollback 取锁后 `reloadProjectFiles` 重读（对齐 init 的做法） |
| P1-2 | 门禁 9 只实现一半：retained.json 从不预检，损坏账本会在提交点后才失败 | planSwitch/planRollback 增加 retained 可解析预检 |
| P1-3 | ADR §9 声称的外部编辑冲突覆盖缺失 | 提交点前增加 config/state 外部编辑检测（失败可完整收敛前态）；测试覆盖提交前拒绝、recover 侧拒绝+恢复后收敛 |
| P2 | no-op 先于门禁 9 短路；op 资源删除身份核验缺名称/镜像比对；switch/rollback 未校验容器实际镜像↔记录；recover 确认 TOCTOU；缺 CANDIDATE_VERIFIED/rollback-PREPARED/init-阻断/sessions-不阻断/IO 写点注入测试；一个同义反复的兼容测试 | 全部修复（no-op 后置；删除需名称+镜像内容匹配；补校验；确认后 op/stage 变化放弃；补 6 个测试；删除该测试并以真实旧二进制实测为准） |

审查确认已覆盖的声称失败路径（原报告"不知所列"即无发现）：恢复方向规则、写入顺序、
全部提交前/后崩溃阶段、响应丢失、身份不可确认保留事务、槽位轮换、拒绝矩阵零变更断言、
退出码合同、旧二进制实测等。

## 验收缺口补齐（第二轮，2026-09-29）

第一轮交付时明确记录了两个验收缺口，均已补齐：

1. **安装版验收**：新增可重复脚本 `tests/acceptance/install_env_switch_acceptance.sh`
   （打包 → SHA256 → 安装到临时 PREFIX → 仓库外真实 env 全流程 → 账本恒等式 → 卸载），
   24/24 PASS。过程中发现并修复真实缺陷：doctor 账本核验按完整 ID 精确比对，
   被真实 docker 的 12 位短 ID 全部误判为账本外；改为前缀比对，fake 同步对齐真实行为。
2. **真实 kill -9 实验**：新增 `tests/integration/environment_switch_kill_test.go`
   （5 场景：switch 事务建立/停止后/提交点/提交完成 + rollback 停止后），对进程组
   SIGKILL 后断言恢复不变量（recover 幂等收敛、文件↔运行时一致、账本恒等式、
   doctor 无账本外告警）。4 场景真实命中 SIGKILL，1 场景（提交点）进程在命中前
   已完整成功——两种落点不变量均成立。套件复跑 73 RUN / 64 PASS / 0 FAIL / 0 SKIP。

## 剩余问题与边界

1. **（已补齐）安装版验收**：见"验收缺口补齐"第 1 条；正式发布（tag/发布命令）
   仍需额外授权。
2. **（已补齐）真实 kill -9 实验**：见"验收缺口补齐"第 2 条；实验覆盖
   switch 4 阶段窗口 + rollback 停止后窗口，均以不变量断言（落点存在竞态，
   不断言精确杀死时刻）。
3. **Intel 实机**：本机 arm64 实测；amd64 仅交叉编译验证（CI 覆盖），未做 Intel 实机验收。
7. **真实宿主 kill -9 与安装版验收**：见上（fake 注入覆盖机制；package.sh 无改动未重跑）。
4. **remote endpoint**：单元测试覆盖拒绝逻辑；未连真实远程引擎。
5. **会话门禁例外**：running 容器执行路径会先 Bootstrap 安装 km-ctl（与 km run 同一
   幂等动作）；拒绝场景会留下惰性脚本文件——已记录为合同唯一例外（ADR §5.7）。
6. **retained 清理**：账本只增不删（合同如此）；人工清理方法在 ADR §2，
   doctor 会对已消失的 retained 条目给出警告。
7. **fake/真实差异教训**：本轮安装版验收抓到 fake 用全 ID、真实 docker ps 返回
   短 ID 造成的 doctor 账本误报——env fake 的 ps 输出已对齐为短 ID，避免复发。

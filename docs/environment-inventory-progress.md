# 环境资源查看与显式清理：进度

> 状态：**S0–S5 全部实现并通过验证**（本轮：2026-10-01，分支 `codex/environment-inventory`，基线 `c9f297b`）。
> 计划：[environment-inventory-plan.md](environment-inventory-plan.md)；冻结合同：[ADR §10](adr-environment-transactions.md)。
> 证据：[tests/evidence/environment-inventory/1790846119/](../tests/evidence/environment-inventory/1790846119/)。
> 按计划约定本轮**未提交/未推送**；变更停留在工作区待审查。

## 阶段状态

| 阶段 | 状态 | 说明 |
| --- | --- | --- |
| S0 基线与合同冻结 | 完成 | 分支基于 c9f297b；ADR §10 冻结 kind=remove 门禁/记录布局/阶段写点/资格判定/普通 rm/错误码 |
| S1 只读 list | 完成 | 角色/存在性/冲突/只读性；负向测试先行（10 项） |
| S2 remove 门禁与预览 | 完成 | 保护矩阵/跨项目真实 ID/状态拒绝/用法错误；反例测试先行 |
| S3 普通删除、账本收尾与 recover | 完成 | argv 断言（无 -f/-v）；全部写点与响应丢失路径故障注入 |
| S4 真实演练与安装版验收 | 完成 | A→B→C 演练 + remove 真实 kill -9 + 安装版验收扩展（31/31） |
| S5 文档与审查 | 完成 | CLI 合同/指南/CHANGELOG/ADR；本进度与证据 |

## 需求到测试对应（验收矩阵 → 证据）

| 计划 §9 类别 | 测试/证据 | 结果 |
| --- | --- | --- |
| list（v1/仅当前代/多角色/空 retained） | TestEnvListV1Project / RolesAfterSwitchAndRollback | PASS |
| 查询分类（MISSING vs UNKNOWN） | TestEnvListMissingRetainedEntry / InspectFailureIsUnknown / EnumerationFailure | PASS |
| 身份（名称复用/短 ID/标签/镜像） | TestEnvRemoveUsageErrors / IdentityMismatchRefused；短 ID/名称拒绝 | PASS |
| 角色保护（当前/槽位/事务/重复角色） | TestEnvRemoveProtectedTargets / TestEnvListRoleConflictProtected | PASS |
| 并发（预览后变化/取锁重验） | TestAuditRemoveReloadsStateAfterConfirmation（确认时 state 变化）+ removePlanStillValid 完整快照比对 + 指纹校验；pending 阻断 | PASS（审计后补测重评） |
| 确认（拒绝/非交互/dry-run） | TestEnvRemoveConfirmation / DryRun | PASS |
| 删除成功 | TestEnvRemoveHappyPath（rm argv 无 -f/-v 由 runtime 层测试锁定） | PASS |
| 失效记录（同名容器在场） | TestEnvRemoveStaleRecord（同名新容器不被误删） | PASS |
| 响应丢失 | TestEnvRemoveResponseLost（rm 已生效按实态收尾，不重复删除） | PASS |
| I/O 故障 | TestEnvRemoveJournalWriteFailureNoDelete（journal 失败不删）/ LedgerWriteFailure + TestFinishRemoveLedgerWriteFailureMessage（记录待收尾） | PASS |
| 中断（真实 kill） | TestEnvRemoveRealKill9（SIGKILL 命中，recover 收敛，容器不重建） | PASS |
| schema/旧二进制 | 旧二进制（c9f297b）实测 kind=remove fail-closed；新二进制识别 pending（old-binary-remove-gate.txt） | PASS |
| 跨项目 | TestEnvRemoveCrossProjectRefused（B 容器与账本原样） | PASS |
| 不可逆边界 | TestEnvRemoveRealDrillABC（可写层金丝雀随容器消失；项目文件金丝雀保留；镜像未删） | PASS |
| 正常回归 | 演练内 C 执行/rollback B；全量 unit/race/integration 全绿（审计修复后复跑） | PASS |
| 审计反例（8 项/12 场景） | env_audit_test.go 等 3 个文件入库，全部通过 | PASS |
| 安装版 | 安装版验收扩展 list/remove/recover → 31/31 PASS | PASS |

## 验证命令（2026-10-01 本机 arm64 实测）

```text
go vet ./... / -tags=integration     PASS
go test -count=1 ./...               PASS
go test -race -count=1 ./...         PASS
go build ./...                       PASS
go test -tags=integration -v ./tests/integration/
                                     66 PASS / 0 FAIL / 0 SKIP，零残留
tests/acceptance/install_env_switch_acceptance.sh（扩展后）
                                     31/31 PASS
旧二进制 kind=remove 门禁实测         旧 fail-closed / 新识别 pending
```

## 独立审计与修复（第二轮，2026-10-01）

外部独立审计发现 8 项可复现问题（2×P1、5×P2、1×P3），已全部修复并把全部反例
纳入正式测试（internal/cli/env_audit_test.go、env_audit_display_test.go、
internal/runtime/env_audit_test.go）：

| 级别 | 问题 | 修复 |
| --- | --- | --- |
| P1 | recover 绕过保护：中断后手工把目标记为 CURRENT/PREVIOUS/改身份，恢复仍执行删除 | 抽出 `verifyRemoveTarget` 共用资格核验（保护集合基于锁内新鲜记录 + retained 精确匹配 + 扩展 inspect 身份 + exited 门禁），普通 remove 与 recover 同一套；另比对确认快照（retained 记录被外部改名/换镜像 → 拒绝） |
| P1 | 确认后复查沿用旧 state：等待 yes 期间目标被改为 CURRENT 仍被删 | 确认后锁内 `reloadProjectFiles` 重读再 planRemove；removePlanStillValid 扩展为完整快照比对（ID/absent/state/image/条目全字段/probe 标记）。switch/rollback 的确认后复查同模式加固 |
| P2 | UNTRACKED 的 inspect 失败被吞，list 仍 0 | 失败 → UNKNOWN + 诊断 + Consistent=false（exit 1）；枚举后消失 → MISSING |
| P2 | 缺 /workspace 挂载时 list 标可删、remove 拒绝 | 共用 `envIdentityProblems`（缺失挂载视为身份不符，两处一致；list 对 RETAINED 行同样应用） |
| P2 | 并发切换时 list 输出混合角色、真实 CURRENT 标 UNTRACKED | state/previous/retained/transaction 前后指纹校验 + 有界重读一次；持续变化 → 标注未核实并 exit 1（保持只读） |
| P2 | remove 恢复预览套用切换/回退方向文案 | 独立 `renderRemoveRecoverPlan`：按阶段区分“重试不可逆删除”与“仅账本收尾”，不承诺恢复原环境/还原文件 |
| P2 | 含逗号项目路径被扩展 inspect 截断 | 挂载源与 RW 拆为独立模板字段（10 字段），不再用逗号切路径 |
| P3 | `0（原始代）` 按字节截断产生非法 UTF-8 | 按字符截断（复用 truncate） |

审计报告：/Workspace/.review-artifacts/kalimac-inventory-20261001-8ntdu8_h/REVIEW.md
（反例测试已全部入库并通过；同引擎跨项目用例与"明确 post-rm 落点"按审计建议在
修复轮补强：强杀实验记录命中阶段，恢复断言容器不重建 + 账本收尾。）

## 剩余边界

1. Intel 实机未测（本机 arm64；amd64 为交叉编译产物，CI 覆盖构建）。
2. 真实 kill 落点存在观测竞态：以恢复不变量为验收标准（remove 场景本轮真实命中 SIGKILL）。
3. remove 不删除镜像/卷/共享文件（合同如此）；retained 账本外科手术式更新保留外部编辑。

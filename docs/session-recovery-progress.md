# 会话恢复轮执行进度（2026-09-16）

- 计划与行为合同：`docs/session-recovery-plan.md`（S1 审查结论、S2 冻结合同已回填）
- 基线：`f479acc`（= origin/master，远端 CI 三次 success）
- 证据目录：`tests/evidence/session-recovery/`
- 执行约定遵守：本轮**未推送**（计划「不做」清单明确排除自动推送）；提交在本地。

## 阶段状态

| 阶段 | 状态 | 关键产物 |
| --- | --- | --- |
| S0 基线 | passed | `s0/baseline.txt`：HEAD/版本/平台/镜像内容 ID/本地测试全绿/远端 CI success；install.sh KM_BIN 行为变更已在 prerelease 阶段二完成真实验收；amd64 未实测事实保留 |
| S1 专项审查 | passed | 六问逐项结论（含 F2 根因）写入计划文档 S1 节 |
| S2 合同冻结 | passed | `km sessions` / `km cancel <id>` 合同冻结（计划文档 S2 节 + cli-contract.md） |
| S3 小步实现 | passed | ①F2 修复+守卫 ②verifyStackForQuery 抽取 ③sessions ④cancel ⑤阻断提示/help 更新 |
| S4 验证与真实验收 | passed | 见下「S4 结果」 |
| S5 文档与交付 | passed | cli-contract / user-guide / architecture / CHANGELOG / verification 均更新 |

## S4 结果

- 单测：新增 12 项全绿（sessions 6、cancel 6 维度）；全量单测/race/vet（含 integration
  tag）/build 全绿（`s4-final-battery.txt`）。
- 关键端到端（长任务→强杀→阻断→sessions→cancel→核验→恢复）：`-count=3` **3/3 轮通过**
  （`s4-chain-3rounds.txt`）。
- 参数/未知 ID/隔离与持锁可用性/重复与自然退出竞争：4 项集成通过（`s4-args-isolation-f2.txt`）。
- F2 定向证据：连续 20 次 cancel 零 `/proc` 噪音、退出码符合合同（同上文件）。
- 全套件回归：143.4s，零 SKIP，零残留（`s4-full-suite.txt`）。

## 测试中发现并回填合同的行为澄清

1. 外部 `km cancel` 取消运行中任务 → 客户端退出码 = 工具真实状态（TERM=143 原样透传）；
   130 专属客户端自取消（Ctrl-C）。
2. 已被清除的会话再次 cancel → 按冻结合同报 `KM_SESSION_UNKNOWN` 退出 1（不把未知
   报成成功）；STALE（仍登记、组空）会话的 cancel 才是幂等清除路径。

## 审查收口（2026-09-17，外部静态审查三项发现全部修复）

| 发现 | 修复 | 证据 |
| --- | --- | --- |
| 容器停止/脚本缺失时 cancel 误报 KM_SESSION_UNKNOWN（与冻结合同不一致） | collectSessions 以 info 状态区分「查询不可执行」（两命令均说明原因退出 0）与「查询成功但 ID 不存在」（KM_SESSION_UNKNOWN 退出 1）；合同补充进计划文档；新增单测 TestCancelInfoStatesExitZero | `s4b-review-tests.txt` |
| 「20 次未知 ID 零噪音」未覆盖修复路径（CLI 前置检查即返回） | 表述撤回，按三层表述收口（证据文件：守卫=`internal/session/scripts_test.go`、重定向机制行为测试=`TestRedirectionOrderBehavior`、翻涌对照=`s4c-f2-churn-demo.txt`、端到端=`s4b-review-tests.txt`）。三层表述见下「F2 证据表述（审查定稿）」 |

### F2 证据表述（审查定稿，2026-09-17）

F2 的修复依据是调整输入与 stderr 重定向的先后顺序。验证分为三层：

1. **源代码守卫**：检查预期的重定向顺序，防止旧写法重新引入；该测试验证代码形态，不单独证明运行行为。
2. **容器内对照实验**：本次进程翻涌实验观察到旧写法产生 220 条诊断，新写法为 0 条。该结果支持修复有效，但并发负载存在随机性，不作为每次必现的保证。
3. **端到端回归**：10 次真实活跃会话取消完成且未观察到该噪音。旧写法在另一轮 10 次取消中同样未复现，因此该测试用于检查真实取消链路，不作为稳定区分新旧实现的依据。

另有重定向机制行为测试（相同 shell、输入重定向指向确定不存在的文件）：旧顺序 stderr 出现 "cannot open"、新顺序为空——稳定验证重定向机制本身；进程翻涌与真实取消验证实际应用场景。三者职责分开。
| 跨项目测试未用 A 的真实 ID | 改为 A 真实活跃 ID 在 B 中取消 → 拒绝（KM_SESSION_UNKNOWN）→ A 列表仍 ACTIVE（任务存活）→ 从 A 正常取消（143 客户端语义验证保留）；B 侧先跑一次命令安装脚本使「列表成功但 ID 不存在」路径可测 | 同左 |

收口后全量回归：gofmt 零脏、vet（含 integration tag）PASS、单测/race 全 ok、build PASS、
全套件 145.3s 零 SKIP 零残留（`s5-review-battery.txt`）。

## 最终完成判定对照

| 判定 | 证据 |
| --- | --- |
| 用户无需手写 Docker 内部命令即可恢复普通遗留会话 | 关键链路 3/3：阻断→sessions→cancel→恢复全程仅 km 命令 |
| 查询不改变运行状态，取消只作用于明确指定的当前项目会话 | sessions 只读不取锁不安装不清扫（实现+单测）；跨项目/未知 ID 拒绝（集成） |
| 未知和失败状态不会被误报为成功 | 未知 ID/exit4/查询失败全部非零 + KM_SESSION_UNKNOWN（单测+集成） |
| 现有执行、隔离、终端与数据行为没有回归 | 全套件 143s 零 SKIP 零残留 + 全量单测/race 绿 |
| 文档、代码和本轮证据一致 | S5 五份文档更新；本目录证据与命令一一对应 |

## 回滚

- `patches/session-recovery.patch`：tracked 变更反向应用即回滚；
  新增 untracked 文件（internal/cli/sessions*.go、tests/integration/session_recovery_test.go、
  docs/session-recovery-plan.md 等）需一并删除。已在独立 worktree 验证
  （`patches/rollback-verified.txt`）。

## 剩余问题与发布前检查清单

- amd64 仍未实机验证（沿用 prerelease 记录）。
- 远端 CI 未运行本轮提交（推送后首跑核对：verify+integration、SKIP 扫描、零残留）。
- 发布前：~~候选版本号决定~~ 已定 **0.4.0-p3**（新增 sessions/cancel 功能 → 次版本 +1，阶段号递进）、tag、CHANGELOG 定稿、双架构打包与校验、
  真人按 user-guide §7 走一遍 KM_SESSION_ACTIVE 恢复流程。

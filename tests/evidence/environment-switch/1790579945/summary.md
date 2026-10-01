# 环境切换与单代回退：执行证据（run 1790579945）

- 分支：codex/environment-switch（基于 1d3eaab，含 tool-discovery）
- 平台：darwin/arm64，本机实测；Docker Engine 29.6.1（本轮启动，仅运行本套件）

## 验证命令与结果

| 命令 | 结果 |
| --- | --- |
| gofmt -l .（本轮改动文件） | 干净（docs/review-p2-20260906/ 为本轮之前已存在的未跟踪备份，其中 1 个文件本就未格式化，按要求原样保留未触碰） |
| go vet ./... | PASS |
| go vet -tags=integration ./... | PASS |
| go test -count=1 ./... | PASS（全部包） |
| go test -race -count=1 ./... | PASS（全部包） |
| go build ./... | PASS |
| go test -tags=integration -count=1 -timeout 15m -v ./tests/integration/ | PASS：67 RUN / 62 PASS（含子测试聚合）/ 0 FAIL / 0 SKIP，161.997s；零清理标记 |
| 同套件在补齐状态门禁与提交前漂移复验后复跑（integration-suite-final.log） | PASS：62 PASS / 0 FAIL；env 三测全过；零清理标记；引擎复核 km.owner=km 容器 = 0 |

集成日志：integration-suite.log。其中本轮新增：
- TestEnvSwitchRealHappyRollback PASS（3.87s）：dry-run 零写入 → switch A→B → 新环境实际执行（marker 证明）→ 旧容器停止保留 → 回退 → 新环境 marker 证明回到 A → 文件改动不回滚 → 槽位消费 → 二次回退 KM_NO_PREVIOUS → 账本恒等式（实际容器 = 当前代 + retained = 2）
- TestEnvSwitchRealNoOp PASS（0.82s）：同内容不同标签 → no-op，无新代/无容器变更/状态不升版
- TestEnvSwitchRealMissingImage PASS（0.62s）：KM_NOT_FOUND 拒绝，零资源创建

## 独立审查与修复

独立审查结论 FIX-FIRST（P0×1 / P1×3 / P2 若干），全部修复并复跑：

- P0：recover 对"带旧槽位的 COMMIT_INTENT 崩溃"永远拒绝 → 修复为完成槽位轮换
  （TestEnvRecoverCrashAtCommitIntentWithSlot）。
- P1：取锁后重读配置/状态；retained 预检；外部编辑冲突测试与提交点前检测。
- P2：no-op 后置门禁、op 资源删除身份收紧（名称+镜像内容）、容器实际镜像↔记录
  校验、recover 确认后事务变化放弃、补充 6 项测试、删除同义反复测试。
- 复跑：unit/race/vet/build PASS；集成 62 PASS / 0 FAIL 零残留
  （integration-suite-post-review.log）。

## 验收缺口补齐（第二轮）

### 1. 安装版验收（原 NOT-RUN → PASS，24/24）

可重复脚本：tests/acceptance/install_env_switch_acceptance.sh（本轮新增）。
流程：scripts/package.sh（cwd=仓库外、DIST 指向临时目录）→ SHA256SUMS 校验 →
解包 → install.sh 安装到临时 PREFIX → 仓库外临时项目中以安装产物执行真实全流程
（init → run marker=env-a → dry-run → switch --yes → marker=env-b → rollback --yes →
marker=env-a → recover 无需恢复 → doctor 账本一致）→ 账本恒等式（实际容器=2）→
uninstall.sh 清单核验。构建身份=HEAD 2baa188，target darwin/arm64（本机实测），
worktree 如实记录（dirty，因计划约定保留的未跟踪文档）。

**发现并修复一个真实产品缺陷**：真实 `docker ps --format {{.ID}}` 返回 12 位短 ID，
doctor 账本核验原先按完整 ID 精确比对，把全部容器误报为"账本外"（fake 测试返回
全 ID 所以未暴露）。修复为前缀比对（internal/cli/doctor.go），env fake 的 ps 输出
对齐为短 ID、容器引用支持短 ID 前缀（回归由 TestDoctorShowsEnvRecords 锁定）。
修复后安装版验收 24/24 PASS。

### 2. 真实 kill -9 阶段边界实验（原"未执行" → PASS，5 场景）

可重复实验：tests/integration/environment_switch_kill_test.go（本轮新增，
真实 Docker）。方法：独立进程组启动 switch/rollback，轮询观测阶段条件（事务
出现 / stage=OLD_STOPPED / COMMIT_INTENT / CURRENT_COMMITTED / rollback 停止后）
命中即对进程组 SIGKILL（含 docker 子进程，模拟"操作成功但响应丢失"）。

| 场景 | kill 命中 | 结果 |
| --- | --- | --- |
| switch·事务建立窗口 | 是 | PASS |
| switch·旧容器停止后 | 是 | PASS |
| switch·提交点 | 否（进程已完整成功，竞态） | PASS（不变量仍成立） |
| switch·提交完成 | 是 | PASS |
| rollback·停止后 | 是 | PASS |

不变量断言（每场景）：recover --dry-run/--yes 幂等收敛、事务清除、二次 recover
"无需恢复"；.km.json 引用 ↔ 运行时实际执行（marker）一致；账本恒等式（实际容器 =
当前代 + 槽位 + retained）；doctor 无账本外告警；status 为正常态。
日志：kill9-stage-experiments.log。套件复跑（含新实验）：73 RUN / 64 PASS /
0 FAIL / 0 SKIP，引擎复核 km.owner=km 残留 = 0。

## 旧二进制兼容实测（无需 Docker）

用基线提交 1d3eaab 构建 /tmp/km-old-bin（go build ./cmd/km），
对同一 v2 状态夹具项目（.km/state.json state_version=2 + env 块）：

```
=== 旧二进制(1d3eaab) km status ===
错误:   状态文件损坏: .km/state.json: 不支持的状态版本 2（KM_STATE_INVALID）
exit=1
=== 旧二进制 km stop ===
.km/state.json: 不支持的状态版本 2
exit=1
=== 新二进制（当前分支构建）同一夹具 ===
通过状态解析，进入引擎比对（夹具 endpoint 与本机不同 → KM_RUNTIME_MISMATCH，符合预期）
```

结论：v2 状态对旧二进制是明确拒绝门槛（在任何 Docker 接触之前），新二进制双向兼容 v1/v2。

## 资源账本

- 引擎核验：docker ps -a --filter label=km.owner=km → 0 个残留容器
- 本轮创建的容器（init/switch/候选/探测/retained）全部由测试清理并复核
- 新增本地测试镜像（确定性 marker 内容差异，可复用）：km-envtest-a:local、km-envtest-b:local、km-envtest-a-alias:local（由 kali-mac-min:0.2 本地构建，不依赖外网）
- 用户既有容器与镜像未触碰（按标签核验 km.owner=km 为空）

## 未验证/边界

- 强杀（阶段边界 kill 宿主进程）在 fake 层以“事务记录+容器实态”模拟覆盖（TestEnvRecover*），未做真实宿主 kill -9 实验
- Intel (amd64) 本机实测未做（本机 arm64）；跨架构切换按合同拒绝（KM_PLATFORM_MISMATCH）
- remote endpoint 场景按单元测试覆盖（KM_ENDPOINT_REMOTE），未连真实远程引擎

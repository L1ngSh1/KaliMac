# Goal 执行进度（goal-1789353851）

- 计划：`docs/goal-plan.md`
- 基线 HEAD：`d3cbd272bb257635e5181426f41b9d192c8185cd`（工作区仅新增 `docs/goal-plan.md` 与本证据目录）
- 证据目录：`tests/evidence/goal-1789353851/`
- Goal 工具说明：本会话工具集中无 `get_goal`/`create_goal` MCP 工具，无法程序化登记 goal。
  按计划实质以本文件承担 goal 状态跟踪；完成定义以 `docs/goal-plan.md` 为准，
  仅在 M1–M5 全部验收满足时将本文件状态标为 complete。

## 环境基线（2026-09-14 10:44 +0800）

| 项 | 值 |
| --- | --- |
| OS / 架构 | macOS 26.6.2 / arm64 |
| Go | go1.26.6 darwin/arm64 |
| Docker CLI / Server | 29.6.1 / 29.6.1（Docker Desktop，本轮 `open -a Docker` 启动） |
| 镜像 | `kali-mac-min:0.2` = sha256:bd829afd…（本地已有）；`docker.1ms.run/kalilinux/kali-rolling:latest` = sha256:ed99295a…（本地已有） |

变更前基线（`tests/evidence/goal-1789353851/baseline/`）：

- `go test -count=1 ./...` → rc=0（cli/project/runtime/session 全 ok）
- `go test -race -count=1 ./...` → rc=0
- `go vet ./...` → rc=0；`go build ./...` → rc=0
- 变更前文件哈希：`baseline/before.sha256`

## 阶段状态

| 阶段 | 状态 | 说明 |
| --- | --- | --- |
| M1 测试资源生命周期收口 | **passed** | 审计+修复+负向证明+三轮回归+预检+回滚验证全部完成（见下） |
| M2 真实端到端验收 | **passed** | 全套件 133.7s 全绿零残留；用户指南实走全过；发现并修复 doctor 会话检查分类缺陷 |
| M3 文档与自动检查 | **passed** | architecture/user-guide/verification/README/CHANGELOG 更新；CI 工作流+Makefile 目标已加并本地验证 |
| M4 可信性能实验 | **passed** | 脚本重写；3 次独立基线（3 轮×20 配对样本/次）；负向验证通过；热点有据不优化 |
| M5 本地预发布准备 | **passed** | install/uninstall 清单式脚本临时验收通过；双架构打包+校验和通过；平台边界明示 |

## M1 审计结论（修复前记录）

逐文件核对 `tests/integration/` 全部 init/create 分支：

| 位置 | 分支 | 清理登记 | 结论 |
| --- | --- | --- | --- |
| `shell_c1_test.go` TestC1DetachKeys | `km init` | **缺失** | 确认泄漏点：km 创建的项目容器既不在套件 `registered` 清单（TestMain 终检查不到），也没有 `t.Cleanup(rm -f)`；中途失败即残留 |
| `shell_c2_test.go` TestC2ShellInteractiveAndRegistered 的 `C2_PROBE=1` 探针分支 | `km init` | **缺失** | 同类问题（探针分支，仅环境变量触发时执行） |
| `matrix_test.go` 全部 11 处 `km init` | init | `registerProjectCleanup` ✓ | 无遗漏（含 TestInitFailureRecovery 的「预期失败不落状态」分支：失败路径不产生容器，属正确不登记） |
| `shell_c2_test.go` 其余 / `shell_c2_hardening_test.go` 全部 | init | ✓ | 无遗漏 |
| `session_real_test.go` / `harness_test.go` | `sessionContainer(Image)` 容器 | `t.Cleanup(rm -f)` + `guardResidue` ✓ | 无遗漏 |
| `shell_c1_test.go` TestC1ProjectIsolation dirB | init | ✓ | 无遗漏 |

终检/残留核对缺陷（不依赖具体用例）：

1. `closedloop_test.go` TestMain 终检与 `p2bResidueCount` 用 `exitCodeOf`：docker CLI 连接失败等
   非退出码错误返回 -1，与「容器不存在」（退出码非 0）混在一起，**把查询失败等价为容器不存在**。
2. `harness_test.go` `assertNoResidue` 丢弃 `docker ps` 退出码：查询失败 → 空输出 → 误判零残留。
3. TestMain 在 Docker 不可达时仍会先构建二进制再尝试镜像构建并 `os.Exit(2)`，`-run '^$'` 也逃不开
   （计划已知问题）。
4. `sessionContainerImage` 预检的是 `toolImage` 而非实际传入的 `image` 参数（C1 传 `minImageRef`
   时门控错误）；清理 `rm -f` 错误被静默吞掉。

## M1 变更（小步批次）

### 批次 1：residue 分类器 + fake 负向测试 + 集成测试修复

意图：把「确认消失 / 确认残留 / 无法核实」三类判定抽成可注入 fake 的纯函数（`internal/residue`），
修复终检与残留核对，并补齐 TestC1DetachKeys、C2_PROBE 探针分支的清理登记。

- [x] `internal/residue`：`Classify(ids, inspect)` + `DockerInspect`（stderr 含 "No such object" 才算不存在，
      其余失败返回错误）；fake 单测覆盖空表/全消失/残留/查询错误/混合（负向回归：查询错误不得判为不存在）
- [x] TestMain 终检改用 `residue.Classify`：残留 → `P2B-CLEANUP-FAIL`；无法核实 → `P2B-CLEANUP-UNVERIFIED`，两者都置 rc=1
- [x] `p2bResidueCount` 返回（残留数, 无法核实数），`TestP2BResidueCheck`、`TestInitFailureRecovery` 同步断言两类
- [x] `assertNoResidue` 检查 `docker ps` 退出码，失败 → `t.Errorf`（可观察）
- [x] TestC1DetachKeys：`km init` 后补 `registerProjectCleanup(t, dir)`
- [x] C2_PROBE 探针分支：补 `registerProjectCleanup(t, dir)`
- [x] `sessionContainerImage` 预检改为实际 `image` 参数；清理 `rm -f` 失败 `t.Logf` 可观察
- [x] TestMain 预检：Docker 不可达 → 打印 `P2-INTEGRATION-SKIP` 显式标记后跳过套件（不在 `-run '^$'` 下访问 Docker/构建镜像）

验证：

- `go test -count=1 ./internal/residue/`（fake，无需 Docker）→ PASS（见 `logs/m1-unit.txt`）
- `go vet ./...`、`go build ./...`、全量单测 → 见 `logs/m1-unit.txt`
- 真实回归（Docker 就绪后执行，见批次 2）：TestC1DetachKeys 连续 3 轮 PASS + 零残留

### 批次 2：真实回归（M1 验收）— 完成

- **负向证明（修复前准确失败）**：临时将 `shell_c1_test.go` 还原到基线（无登记），
  `TestC1DetachKeys` 用例本身 PASS 但泄漏 km init 容器 `km-pe37ee18ff5`（`Up 21 seconds`，
  终检与标签残留核对均发现不了）→ 证据 `logs/detach-prefix-leak.txt`；该演示容器已按 ID 清理。
- **修复后三轮回归**：`go test -tags=integration -count=3 -run '^TestC1DetachKeys$'` →
  ok 53.2s，零残留（`logs/detach-postfix-3rounds.txt`，前后差集 `logs/detach-postfix-residue.txt` 为空）。
- **终检「无法核实」路径被真实触发**：第一轮修复后回归中，`DockerInspect` 误配 stderr 串
  （`No such container` vs `No such object`），终检把已消失容器报为 UNVERIFIED 并置 rc=1——
  证明终检现在对「无法核实」响亮失败而非静默通过；修正匹配后复跑通过。
- **全套件无回归**：`go test -tags=integration -count=1 -timeout 15m ./tests/integration/` →
  ok 133.7s rc=0，零清理警告，套件外新增容器差集为空（`logs/integration-full-run1.txt`）。
- **TestMain 预检**：`DOCKER_HOST=unix:///tmp/…不存在…` 下 `-run '^$'` → rc=0 且输出
  `P2-INTEGRATION-SKIP`（`logs/m1-preflight-skip.txt`）；不访问 Docker、不触发镜像构建。
- **回滚验证**：独立 worktree（d3cbd27）上 forward apply batch1 补丁（5 文件）→
  `rollback.sh` 反向应用 → tracked tree clean（`patches/rollback-verified.txt`）。
- M1 遗留说明：历史 verification.md 第六轮「终检零残留在 Detach 用例上未生效，待查」
  本轮定案（根因=km init 容器未登记），M3 时补记入验证记录。

## M2 真实端到端验收（进行中）

- [x] 预检：endpoint/引擎可达、两镜像本地已有（`baseline/env.txt`）
- [x] `go test -count=1 ./...`、`-race`、`vet`、`build` 全绿（`baseline/*-baseline.txt`，终稿再重跑）
- [x] 全量集成套件 133.7s 全绿零残留（同上）
- [x] 用户指南实走：隔离临时项目 init → run → 双向文件共享 → shell（PTY 实操：
      Ctrl-C/作业控制/窗口跟随/退出码透传）→ doctor → stop → 再次 run → 清理
      （`logs/m2-walkthrough/`；两个演示项目按完整 ID 停止、删除、核对零残留）

### 批次 3：M2 实走发现的产品缺陷修复 — doctor 会话检查分类

现象：新项目 `init` 后立即 `doctor`，会话检查因 `km-ctl` 未安装而失败
（exit 127），但该失败经 `r.line` 输出、**不计入摘要统计**——「10 通过, 0 警告,
0 失败」掩盖了一次未完成的检查（`logs/m2-walkthrough/walkthrough-noninteractive.txt`）。

修复（`internal/cli/doctor.go` reportContainerSessions）：

- 会话检查自身失败（err / exit≠0 / 不可解析）→ 计入统计的 `警告` 项
- exit 127 单列：`会话脚本未安装（init 后首次 run/shell 时安装…）`，预期状态可辨

验证：

- 新增单测 `TestDoctorSessionCheckFailureCounted` / `TestDoctorSessionScriptNotInstalled127`
- 负向证明：还原基线 doctor.go → 两测试 FAIL（输出正是「0 警告 0 失败」的旧摘要）；
  恢复修复 → PASS
- 真实验证：新项目 init→doctor 显示 `[警告] 会话脚本未安装`、`10 通过, 1 警告, 0 失败`；
  有 run 历史后显示 `[OK] 容器内无活跃会话`、`11 通过, 0 警告, 0 失败`
  （`logs/m2-walkthrough/walkthrough-stop-restore.txt`）
- 全量复跑归入交付收口

### M2 场景覆盖对照

| 计划场景 | 覆盖 |
| --- | --- |
| 退出码透传 | 实走 `sh -c 'exit 7'`→7、shell `exit 7`→7；套件 exit 0/7/42 |
| 二进制管道 | 套件 TestRunBinaryPipeAndStderr（32KiB 哈希）、TestRealSessionBinaryRoundtrip（64KiB） |
| 中文/空格路径 | 实走 pipe/redirect；套件 TestRunChineseSpacePathsAndSubdirCwd |
| Ctrl-C | 实走 PTY（sleep 5 中断→C=130）；套件 TestC1CtrlC*/TestC2ShellCtrlC/TestRunCancelCleansSession |
| 作业控制 | 实走 PTY（Ctrl-Z→bg→jobs Running）；套件 TestC1JobControl |
| 窗口跟随 | 实走 PTY（ioctl 40×100→stty 40 100）；套件 TestC1WindowSizeFollows |
| 外部 SIGTERM/SIGHUP | 套件 TestC1TermiosRestored、TestC2ShellExternalSignals（143+termios 恢复） |
| SIGKILL 后保护与显式恢复 | 套件 TestStaleHostLockActiveSessionBlocked、TestC2ShellSessionBlocksAfterHostKill、TestC1SigkillObservedSeparately |
| init→run→共享→shell→doctor→stop→再 run | 实走全流程（含同一容器恢复、数据保留） |

## M3 文档与自动检查 — 完成

- `docs/architecture.md`：以 `internal/session` 为准重画组件图，补 `internal/residue` 与
  测试资源生命周期约定；删除不存在的 `internal/terminal` 与「P0 后」过时标题。
- `docs/user-guide.md`：删除已完成的旧待办（§9 重写为现状与下一步）；补 doctor「会话脚本
  未安装」预期警告说明；热调用开销改为引用本轮实测基线（移除过时的「约 150ms」）。
- `docs/verification.md`：新增第七轮记录（M1 定案第六轮遗留、doctor 修复、全套验证）。
- `README.md`：开发命令更新（fmt-check/integration/perf/CI 说明）；新增安装/打包节与平台边界。
- `CHANGELOG.md`：新建；「未发布」段记录本轮变更，不虚构历史版本。
- CI：`.github/workflows/ci.yml`（verify：fmt/vet/test/race/build；integration 独立作业，
  引擎预检不可达必须失败 + 显式构建最小镜像，不把不执行当成功）。**远端 CI 未运行（未 push，
  如实标注）**。
- Makefile：`fmt-check`、`integration` 目标；`all` 含格式检查。
- 验证：workflow YAML 解析通过；全仓 .md 相对链接检查无断链；`make fmt-check/vet` 本地通过；
  CI 各步骤对应命令均在本轮本地实跑。

## M4 可信性能实验 — 完成

脚本重写（`tests/perf/perf-baseline.sh` v2）修复项逐条对账：

| 计划指出的问题 | 修复 |
| --- | --- |
| 未初始化变量（CONTAINER） | `CONTAINER=""` 初始化；cleanup 判空 |
| 早退清理 | trap EXIT 带 WARN 输出，容器按完整 ID 清理 |
| 失败样本被统计 | 逐样本退出码检查，失败样本不进统计、记录 `failures[]` |
| 未检查 stop/run/exec 退出码 | 全部检查；任何失败 → success=false + 非零退出 |
| 非单调时钟 | `time.monotonic()`，单一 python 驱动进程计时 |
| 计时工具启动成本进入窗口 | 测量循环在驱动进程内；另测底噪（≈2ms）供参照 |
| 缺环境元数据 | OS/架构、Go/Docker 版本、code rev、镜像内容 ID、pull 单独计时 |
| 缺 p50/p95、CSV | JSON+CSV+txt，p50/p95+原始样本+每样本退出码 |
| 样本量不足 | 默认 3 轮 × 20 配对样本，可 `PERF_ROUNDS/PERF_SAMPLES` 调 |

- 负向验证（`tests/perf/perf-negative-test.sh`，Docker 就绪实跑）：init 失败 → 非零退出且无
  成功基线；热调用注入 `/bin/false` → 失败样本 exit 1 全记录、有效样本=0、p50=None、
  success=false、脚本非零（`logs/m4-negative.txt`，PERF-NEGATIVE-PASS）。
- 可信基线（3 次独立运行 × 3 轮 × 20 配对 = 每侧 180 有效样本，全部 rc=0）：
  km 热调用 p50 203.1 / 205.0 / 207.1ms，p95 227.1 / 227.4 / 225.3ms；裸 docker exec p50
  45.7 / 44.7 / 45.0ms。开销 ≈157–162ms。init（镜像已缓存）153ms；冷启动 stop→run 273ms。
  原始样本与元数据：`tests/perf/evidence/20260914-*/baseline.{json,csv,txt}`、
  `logs/m4-baseline-{full,run2,run3}.txt`。
- 热点剖析与优化结论：docker CLI shim 记录一次 `km run` 共 9 次调用（`logs/m4-docker-call-profile.txt`）。
  开销主体 ≈6 次 docker CLI 子进程启动（每次 25–35ms）。唯一重复查询（`run.go:60` 锁前身份
  核验与 `run.go:124` 锁内恢复检查）为有意的锁内 TOCTOU 复查；去除会改变 BUSY/漂移报错
  语义，合并 sessions+sweep 会触及 R2 活跃会话保护结构，且可省 ≈27ms 低于噪声
  （p50–p95 差 ≈24ms）。**按计划保留原实现，不强行改动**；未绕过镜像/容器身份、endpoint
  固定、锁与活跃会话检查。

### M4 批次修正（终检残留核对发现的自身缺陷，如实记录）

最终残留核对发现 6 个本轮容器泄漏：5 个来自 perf v2 脚本（bash 侧 `CONTAINER` 在重写中
失去赋值点——容器 ID 读取被移进 python 驱动，cleanup 判空跳过 `docker rm`），1 个来自
剖析用的手动 shim 实验。这恰是原脚本同类缺陷在重写版中的复发，被终检流程抓出。
修复：cleanup 从 `.km/state.json` 兜底读取容器 ID（读取失败可观察）+ 驱动返回后正常赋值；
`perf-negative-test.sh` 增加 `km-perf-*` 残留断言。6 个容器按完整 ID 清理
（`logs/m4-leak-cleanup.txt`）；修复后负向 + 冒烟复验零残留（`logs/m4-negative-postfix.txt`）。

## M5 本地预发布准备 — 完成

- `scripts/install.sh`：`PREFIX`（默认 /usr/local）+ `DESTDIR`（暂存）可配置；清单式安装
  （`$PREFIX/share/km/manifest.txt`），不改 PATH/shell 配置。
- `scripts/uninstall.sh`：只删清单内文件 + 清单自身，rmdir 清空目录；绝不递归删除。
- `scripts/package.sh`：darwin/arm64 + darwin/amd64（`-trimpath`，CGO=0）→ `dist/`，
  SHA256SUMS + 版本元数据（版本/代码 rev/工具链/镜像内容 ID/平台边界）。
- 验收（`logs/m5-install-uninstall.txt`、`m5-uninstall-retest.txt`、`m5-package.txt`）：
  临时目录安装 → 安装产物 help/version 正常 → 卸载后零残留；清单外金丝雀文件保留；
  双架构产物 SHA256 校验通过，arm64 实跑 `--version` 正常，amd64 为真实 x86_64 Mach-O
  （**交叉编译，未在真实硬件端到端测试**，元数据与 README 均明示）。
- 镜像来源与复现：`images/kali/Dockerfile`（基础镜像 kali-rolling，apt 源构建时显式改写为
  USTC 公开镜像）；真实内容 ID sha256:bd829afd…（本轮实测）与包版本记录见
  `tests/evidence/kali-mac-min-0.2/build-evidence.txt`，未虚构锁定信息、未更换镜像来源。
- 版本与发布：版本常量 `internal/cli/cli.go` `Version = "0.3.0-p2"` 沿用仓库阶段命名约定；
  候选版本号、tag、发布命令均为**待审步骤**，未打 tag、未提交、未发布。`dist/` 已加入
  `.gitignore`。

## 回滚

- `patches/final.patch`：工作副本相对基线 d3cbd27 的全部 tracked 变更；`patches/rollback.sh`
  对工作副本 `git apply -R` 反向应用。
- 新增 untracked 文件不在 patch 内，完全回滚需一并删除：
  `internal/residue/`、`scripts/`、`CHANGELOG.md`、`.github/`、`tests/perf/perf-negative-test.sh`。
- 已在独立 worktree（d3cbd27）验证 forward apply → rollback → tracked tree clean
  （`patches/rollback-verified.txt`，final.patch 同机制复验见 `patches/rollback-final-verified.txt`）。

## 交付与最终验收

- 最终全量验证（gofmt / vet 含 integration tag / 单测 / race / build / 全量集成）：见
  `logs/final-battery.txt`（集成套件在 doctor 修复后复跑，130.3s 全绿；集成终检与标签
  残留核对均无警告）。
- 残留核对（如实记录全过程）：集成侧全程零残留；最终扫描发现 6 个 perf/剖析轮容器泄漏
  （见「M4 批次修正」），按完整 ID 清理后，perf 脚本修复并复验，最终 `docker ps -a`
  无任何本轮容器；用户既有容器（含 km-review-repro 等）未触碰。
- git diff 复查：见 `logs/final-git-status.txt`；未纳入缓存、凭据、机器敏感信息或构建产物
  （`dist/`、`bin/` 均在 .gitignore）。本轮产生的 `tests/evidence/`（goal 证据、5 个
  p2c-shellproto 运行目录）与 `tests/perf/evidence/`（基线产物）为有意保留的验证记录，
  是否随本轮变更入库由作者决定。
- Goal 状态：M1–M5 全部 passed，完成定义满足 → complete。
  （本会话无 get_goal/create_goal 工具，goal 状态以本文件为准。）
- 剩余风险与边界：
  1. 远端 CI 未运行（不自动 push）；integration 作业的 linux/amd64 首跑结果待真实 push 后确认。
  2. darwin/amd64 产物为交叉编译，未在真实硬件端到端测试。
  3. 性能基线为单机单环境（macOS 26.6.2/arm64 + Docker Desktop 29.6.1）结论，不构成通用性能承诺。
  4. Docker Desktop 由本轮 `open -a Docker` 启动，保持运行状态（未改动其配置）。

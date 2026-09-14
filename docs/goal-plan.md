# KaliMac Goal 执行计划

## 目标与完成定义
将当前 0.3.0-p2 从交互 MVP 推进到可复验、可安装的本地预发布候选。先完成 P2-C 验收收口，再建立可信 P3 性能实验与发布工程。所有完成项必须有当前代码对应的证据；历史结果不能替代本轮测试。无需新增工具功能或大重构。不自动 push、创建远端 release、发布镜像或修改用户全局配置。

## 已知基线（须复核）
- 审计起点提交 d3cbd27；单测、race、vet、build 通过。
- 单测 statement coverage：cli 71.6%、project 73.7%、runtime 80.1%、session 47.7%；覆盖率不作为唯一验收目标。
- Docker socket 在审计时不存在，真实集成未复验。TestMain 即使在 -run '^$' 下也会访问 Docker 并可能构建镜像。
- TestC1DetachKeys 的 km init 容器疑似未登记清理，与历史泄漏记录吻合，需负向回归证明。
- 性能历史样本：km 热调用中位数 223ms，docker exec 59ms；不是当前性能承诺。

## 执行约定
1. 读取适用 AGENTS.md、相关 skills、git 状态与此计划。先定位后修改，保留用户已有改动。
2. 使用 Goal 模式：调用 get_goal 检查；无未完成 goal 时调用 create_goal，以本计划的完成定义为 objective，不设置 token_budget。已有匹配 goal 则继续；不覆盖其他未完成目标。
3. 每次只实施一个可验证的小步；修改前记录意图和基线，重要配置先备份。禁止为了达标删除断言、隐去失败或削弱身份/会话保护。
4. 建立 docs/goal-progress.md 与 tests/evidence/goal-<run-id>/；按阶段记录 pending/running/passed/blocked，以及命令、退出码、PASS/FAIL/SKIP、证据路径和当前 SHA/工作区差异。
5. 为每个改动批次保留 diff、原始文件哈希和可执行 rollback 脚本；在独立副本验证回滚，不覆盖工作副本最终改动。若环境使执行不成立，明确记录未验证，不能构造成功证据。
6. 所有资源由本轮完整 ID 登记并核验归属，仅清理本轮创建的临时项目/容器；不运行全局 prune、不删除用户现有资源、不输出凭据。
7. Docker 不可用时继续不依赖 Docker 的阶段，记录所需外部动作。Goal 仅全部验收完成时标 complete；blocked 遵守工具的连续阻塞判定，不因单次失败或停止回复而宣告完成。

## M1：测试资源生命周期收口（最高优先级）
- 检查 TestC1DetachKeys 中 km init 与实验容器两条资源生命周期，补齐项目容器登记。
- 精确检查所有集成测试的 init/create 分支，防止同类遗漏。
- 清理错误必须可观察；终检须能发现未清理资源，不将 Docker 查询错误等价为容器不存在。
- 覆盖初始化成功后失败、断言失败、驱动器失败、取消等路径；优先用可注入 fake 验证失败路径。
- 验收：回归在修复前准确失败、修复后通过；Docker 就绪时 Detach 用例至少连续三轮通过且本轮容器零残留。

## M2：真实端到端验收
- 预检本地 endpoint、Docker 可达性和所需镜像，避免无意义自动重建；明确区分缺前提与产品失败。
- 执行 go test -count=1 ./...、go test -race -count=1 ./...、go vet ./...、go build ./...。
- 执行 go test -tags=integration -count=1 -timeout 15m ./tests/integration/，保留完整日志与结构化结果。
- 按用户指南在隔离临时项目实走 init → run → 文件双向共享 → shell → doctor → stop → 再次 run。
- 覆盖退出码、二进制管道、中文空格路径、Ctrl-C、作业控制、窗口跟随、外部 SIGTERM/SIGHUP、SIGKILL 后保护和显式恢复。
- 验收：真实用例通过、无意外 SKIP、终端恢复、无本轮资源残留；测试不通过则先修根因再重跑相关及全量。

## M3：文档与自动检查
- 更新 README、architecture、user-guide 的当前状态；架构以 internal/session 为准，删除已完成的旧待办但保留历史验证记录。
- 当前计划与历史记录分开；明确 SIGKILL、脱离会话等行为约定。
- 添加 CI：格式检查、vet、单测、race、build；integration 独立并明确执行平台和镜像前提，不把不执行当成功。
- 验收：命令本地可运行、工作流语法有效、文档链接有效；远端 CI 未运行需如实标注，不自动 push 触发。

## M4：可信性能实验与小步优化
- 修复 tests/perf/perf-baseline.sh：未初始化变量、早退清理、失败样本被统计、未检查 stop/run/exec 退出码等问题。
- 测量使用单调时钟；隔离计时工具启动成本，区分镜像拉取、初始化、冷启动、热调用。
- 记录 OS/架构、Go/Docker 版本、代码版本、镜像内容 ID、样本数、每次退出码；输出 JSON/CSV 和 p50/p95。
- fake 负向测试证明失败命令不会生成成功基线且脚本非零退出。
- Docker 就绪后至少三轮、每轮至少 20 个有效热样本；比较配对 docker exec 对照，保留原始样本。
- 只优化有证据的热点，优先减少重复查询或脚本复制；不得绕过镜像/容器身份、endpoint 固定、锁和活跃会话检查。
- 验收：先有可信基线。若实施优化，提供同环境前后对比及完整功能回归；收益低于噪声则保留原实现并记录结论，不强行改动。

## M5：本地预发布准备
- 提供可配置 PREFIX/DESTDIR 的安装与卸载方式；先在临时目录验证，不直接安装到用户 PATH。
- 提供 macOS arm64/amd64 构建说明或打包脚本、校验和、版本元数据和 CHANGELOG；交叉编译不等于双架构实机测试。
- 明确镜像来源与构建复现方法；可验证时记录真实 digest/包版本，不虚构锁定信息，不擅自更换镜像来源。
- 版本号遵循仓库约定；候选版本、tag 与正式发布命令作为待审步骤，不自动打 tag、提交或发布。
- 验收：临时安装后 help/version 可运行、卸载只移除安装清单文件；包清单和校验和核验通过；支持与未测平台明确。

## 交付与最终验收
- 交付小步代码变更、测试、文档、CI、性能脚本及可信证据、本地安装/打包产物与回滚记录。
- docs/goal-progress.md 必须有 M1–M5 状态、changed files、validation commands、test results、remaining risks。
- 最后重跑格式、vet、单测、race、build；Docker 可用时重跑真实集成并核对残留。
- 复查 git diff，不把缓存、凭据、无关机器信息或大型构建产物误纳入版本控制。
- 重新打开所有声称交付的文档/报告并验证路径。完成全部验收后才将 Goal 标 complete。

## 可复用执行提示词
```text
请以 Goal 模式执行 docs/goal-plan.md，而不是只输出建议。先检查当前 goal；没有未完成 goal 时创建与计划完成定义一致的目标，不设置 token budget。按 M1→M2→M3→M4→M5 小步执行，外部环境阻塞时先推进独立项。持续更新 docs/goal-progress.md，保留真实测试、失败与回滚证据。不要自动提交、推送、打 tag 或发布。最终仅在所有完成定义满足时标记 goal complete；未完成项明确记录状态和下一动作。
```

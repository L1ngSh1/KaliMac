# 小任务包：km tools 工具可用性查看

状态：待执行；本轮仅创建分支与计划，不实现功能。
分支：`codex/tool-discovery`
基线：`20eb4cf`（创建分支时工作区干净）

## 1. 目标
让用户在当前项目运行 `km tools`，知道常用工具是否能从容器 PATH 中找到，以及缺失时如何准备环境。解决“容器已启动，但不知道有什么工具”的问题。

## 2. 第一版范围
- 新增 `km tools`，固定检查 python3、curl、jq、file、openssl、nmap。
- 每个工具显示 AVAILABLE（附解析路径）或 MISSING；该列表只是精选清单，不代表枚举容器全部软件。
- 工具缺失时给出维护镜像 Dockerfile 的指引；明确临时容器安装不等于可复现配置。
- 查询异常显示 UNKNOWN/稳定 KM_* 错误，不把权限、引擎或协议错误当作工具缺失。
- AVAILABLE 仅表示可从 PATH 定位，不保证版本、执行成功或功能完整；不运行工具及其 --version。

非目标：自动安装、apt 操作、版本管理、全量包枚举、自定义工具清单、JSON、GUI、镜像重建。

## 3. 行为合同
| 场景 | 行为 | 退出码 |
| --- | --- | --- |
| 运行中且身份匹配 | 返回固定六项检查结果，允许部分 MISSING | 0 |
| 未初始化/配置损坏/身份冲突 | 现有稳定错误分类与下一步指引 | 1 |
| 容器停止、暂停、重启中 | 明示当前状态与未检查原因，不自动启动或恢复 | 1 |
| 引擎故障、超时、检查协议异常 | UNKNOWN，保留诊断，不输出误导性缺失结论 | 1 |
| 额外参数或未知选项 | 用法提示 | 2 |

只读约束：不改配置、不装脚本、不清扫会话、不安装软件；只在当前项目容器执行短时查找命令。容器内会产生临时查询进程，不宣称完全没有运行时活动。
并发约束：已有 run/shell 时仍能查询；不持长任务执行锁，不改变任务或会话状态。
复用本地 endpoint 固定、完整容器 ID 与项目归属校验；新命令 tools 重名工具仍能通过 `km run -- tools ...` 调用。
固定清单作为参数传递，不拼接任意用户 shell 文本；批量查询设置整体超时，核验结果齐全、无重复、无未知条目。

## 4. 实施顺序
1. 定位 sessions/status 的查询门禁与 runtime executor；确认可复用部分，不大重构。
2. 先写反例：查询失败不是 MISSING，停止/暂停不执行 exec，归属不符不查询工具。
3. 实现批量可用性探测、严格结果解析、CLI 分发/help 和可操作提示。
4. 补正常、部分缺失、并发、参数错误测试；更新用户指南与 CHANGELOG。
5. 执行本地回归与真实安装版验收，记录证据和剩余问题。

## 5. 验收清单
- [ ] 六项齐全可用；部分缺失能明确列出；未知输出不误判。
- [ ] AVAILABLE 路径来自实际查询，而非根据镜像标签推测。
- [ ] 固定使用已校验的 endpoint 与完整容器 ID。
- [ ] 未初始化、身份冲突、停止、暂停、超时均覆盖。
- [ ] 查询前后项目配置、状态文件、容器状态和会话登记保持一致。
- [ ] 活跃任务不受影响；多项目结果不串用；仅清理本轮测试资源。
- [ ] 格式检查、go vet ./...、go test -count=1 ./...、go test -race -count=1 ./...、go build ./... 通过。
- [ ] Docker 可用时真实集成通过；不可用如实记未验证，不使用历史结果替代。
- [ ] 从 dist 安装的二进制完成工具查看，不依赖开发目录 bin/km。

## 6. 交付与回滚
交付：实现、回归测试、用户指南/CHANGELOG 更新、简明验证记录。每个逻辑改动独立且可回滚，保留基线、diff 与验证结果；不自动提交、推送、打 tag 或发布。
完成定义：上述验收逐项有证据；当前阶段不为追求更多功能扩大范围。

## 7. 后续执行提示词
```text
在 codex/tool-discovery 分支按 docs/tool-discovery-plan.md 实现 km tools。
先核对工作区和合同，再写负向测试、小步实现并验证；本轮不实现自动安装或 JSON。
完成后报告 changed files、validation commands、test results、remaining risks。
不自动提交、推送或发布；未执行的真实测试不得标为通过。
```

## 8. S0 组件定位（执行前核对，2026-09-18）

| 需求 | 可复用组件 | 位置 |
| --- | --- | --- |
| 项目栈加载（wd→root/config/state） | `loadProjectStack` | internal/cli/run.go |
| endpoint 解析+本地校验+固定 | `resolveEngine` | internal/cli/sessionctl.go |
| 归属门禁（不含镜像检查的只读组合） | `verifyStackForQuery` | internal/cli/run.go |
| 容器状态分类参照（running/paused/created/exited/其余未知） | `collectStatus` 的 switch | internal/cli/status.go |
| 控制器注入测试模式 | `newSessionController` / `scriptedDocker` | sessionctl.go、p2b_test.go |
| 会话查询（tools 不需要，但并发参照） | `DockerController.Sessions` | internal/session/prod.go |

缺口（实现时新增）：runtime 需要一个**有界 capture exec**（管理类，默认 10s 超时）——
现有 executor 的流式 exec 不适用；`docker exec` 退出码 126/127 与「脚本缺失」双证据
判定的经验（见 session-recovery 轮）在此同样适用：退出码不可靠，按输出特征判定。

## 9. 探测协议草案（实现阶段冻结）

单次批量 exec（整体 10s 超时），固定脚本、工具名经 argv 传递（无任意 shell 拼接）：

```text
docker exec <完整容器ID> /bin/sh -c '<固定循环: 对每个 $1..$n 执行 command -v 并输出
  "<tool>=AVAILABLE:<path>" 或 "<tool>=MISSING">' sh python3 curl jq file openssl nmap
```

严格解析：恰好 6 行；每行匹配 `NAME=AVAILABLE:<path>` 或 `NAME=MISSING`；NAME ⊆
固定清单且不重复。违反任一 → UNKNOWN（`KM_SESSION_UNKNOWN` 复用或新增
`KM_TOOLS_PROTOCOL`，实现时二选一并写入 cli-contract）。退出码语义照行为合同表。

## 执行结果（2026-09-18）

- 状态：**passed**（第一版范围全部完成；非目标未实现）。
- 实现：`internal/session/prod.go` ExecCapture（有界 capture exec）、
  `internal/cli/tools.go`（固定清单/探测协议/严格解析/UNKNOWN 分类）、cli.go 分发
  与帮助、新增稳定码 `KM_TOOLS_PROTOCOL`。
- 单测 9 项（tools_test.go）：全可用/部分缺失（指引断言）/探测失败（权限 126）→
  unknown 且 stdout 无 MISSING、协议异常四形态（KM_TOOLS_PROTOCOL）、引擎故障、
  停止/暂停零 exec、未初始化、usage、身份冲突零探测。
- 真实集成（TestNewUserToolsReal/Stopped）：精选镜像六项 AVAILABLE + 并发持锁可用
  + 只读逐字节对照 + 停止容器 exit 1 且不自动启动。
- 反例先行证据：naive 分类（探测失败→全 MISSING）下 ProbeFailure/ProtocolViolations
  如期失败，修正后全绿。

## 10. 反例测试清单（先写，修复前必须失败）

1. exec 非零/garbage 输出/超时 → UNKNOWN，stdout 无任何 MISSING 字样（负向断言）。
2. 容器 stopped/paused → fake 断言 exec 调用次数为 0 + 状态说明 + exit 1。
3. 未初始化/身份冲突 → 不发起 exec（调用计数为 0）+ 既有 KM_* 分类。
4. 额外参数/未知选项/`--json`（本轮非目标）→ usage exit 2，零 docker 调用。
5. 并发：长任务持锁时 tools 正常返回（集成，run 进行中断言）。
6. 只读性：.km 前后逐字节一致 + 容器 state 不变（集成，status 只读测试同模式）。
7. 部分缺失：六项中强制两个 MISSING → 消息列出缺失项 + Dockerfile 维护指引 +
   「临时安装不可复现」提示；AVAILABLE 附解析路径。

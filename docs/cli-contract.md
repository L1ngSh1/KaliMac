# km CLI 合同（v0.1，随实现演进）

退出码：成功 0；环境前提错误 1；用法错误/未实现命令 2。km 自身错误带 `KM_...` 稳定标识，与工具退出码区分。

## 命令

| 命令 | 状态（P1） | 合同 |
|---|---|---|
| `km` / `km --help` | 已实现 | 简短帮助；零 Docker 依赖、零副作用（单测断言外部调用次数为 0） |
| `km --version` | 已实现 | 版本行；零 Docker 依赖 |
| `km version --verbose` | 已实现 | 版本、提交、工作区标记、目标架构；零 Docker 依赖 |
| `km CMD --help` / `km help CMD` | 已实现 | 同一份管理命令帮助，exit 0；工具名后的 `--help` 仍透传 |
| `km doctor` | 已实现 | 只读检查（含容器内活跃/遗留会话报告）。流程：①平台；②项目配置与本机状态（纯客户端文件）；③仅用客户端命令解析有效 endpoint（DOCKER_HOST > DOCKER_CONTEXT > 当前 context inspect），非本地 endpoint 判 `KM_ENDPOINT_REMOTE` 并跳过一切引擎查询；本地则把该 endpoint 固定（DOCKER_HOST 注入）给本次所有后续调用并查引擎版本；④比较 `.km/state.json` 记录的 endpoint，漂移判 `KM_RUNTIME_MISMATCH` 并跳过容器/镜像检查；⑤容器归属按记录的完整容器 ID 检查（名称、`km.project` 标签、`/workspace` 挂载源、容器实际镜像内容全部比对），同名重建判冲突不接管；⑥镜像标签内容与项目记录比对，漂移报警告。环境问题作为检查结果输出（stdout），doctor 自身完成即返回 0 |
| `km init` | 已实现（非交互最小可用版） | 幂等：身份一致时复用；缺失镜像显式拉取（有界）；容器按记录完整 ID 校验/启动/重建；状态与配置原子写入；失败只回滚本次创建的资源；检测父项目（KM_PROJECT_NESTED） |
| `km TOOL ARG...` | 已实现（非交互） | 会话内核执行（ADR-004）：argv 逐元素、三流流式、cwd 映射（符号链接逃逸拒绝）、退出码原样（取消 130）；停止的容器自动恢复；引擎/容器/镜像身份不符显式报错不静默重建；同项目串行（KM_PROJECT_BUSY），遗留锁清理不等于容器任务结束 |
| `km run -- TOOL ARG...` | 已实现（非交互） | 同上长形式；`--` 必需，解决工具与 km 管理命令重名 |
| `km shell` | 已实现（C2） | 交互 bash：Docker CLI 接管真实终端（raw mode/恢复/尺寸归客户端）；会话登记进 /tmp/km-sessions（与工具会话互斥，崩溃遗留阻断后续任务）；stdin/stdout 非终端 → KM_NOT_TTY（exit 1，进入前失败）；外部 SIGTERM/SIGHUP → 恢复终端并退出 143；键盘 Ctrl-C/Ctrl-D/作业控制直达 bash；退出码原样透传 |
| `km stop` | 已实现 | 只停止当前项目已验证身份的容器；不删除容器/文件/镜像；幂等；执行中返回 KM_PROJECT_BUSY |
| `km sessions` | 已实现（会话恢复） | 只读列出当前项目容器内会话：stdout 为 `ACTIVE <id>` / `STALE <id>` 行（完整 ID 可复制），辅助提示走 stderr；无会话输出「当前项目无会话」退出 0；脚本未安装（127）说明为预期状态退出 0；容器未运行时说明登记将随下次执行清扫退出 0（不启动容器）；查询失败/输出异常 → `KM_SESSION_UNKNOWN` 退出 1。门禁=只读归属校验（endpoint 固定 + 引擎匹配 + 容器 ID/名称/标签/挂载），**不含镜像内容检查**（镜像漂移不阻止恢复）；不取项目执行锁，不安装脚本、不清扫、不改变任何运行状态 |
| `km tools` | 已实现（工具发现） | 只读探测精选六项（python3/curl/jq/file/openssl/nmap）在当前项目容器内是否可从 PATH 定位：`AVAILABLE <name> <路径>` 或 `MISSING <name>`，允许部分缺失（exit 0）。清单为编译期固定集合（argv 传入固定脚本，无 shell 拼接），不枚举容器全部软件；AVAILABLE 不保证版本/执行结果。门禁=verifyStackForQuery（endpoint 固定+引擎匹配+容器归属，不含镜像检查）；不取执行锁、不装脚本、不清扫、不启动容器。容器非 running（停止/暂停/其他）→ 明示状态与未检查原因退出 1；探测执行失败/超时 → 稳定码非零并保留诊断；输出协议异常（行数/未知工具/重复）→ `KM_TOOLS_PROTOCOL`。三种情形都绝不把失败输出为 MISSING |
| `km cancel <id>` | 已实现（会话恢复） | 显式取消当前项目的指定会话并核验终态。完整 ID 精确匹配（`s`+16 hex，非法 → KM_USAGE 退出 2）；先列会话确认归属，列表中不存在的 ID → `KM_SESSION_UNKNOWN` 退出 1（不宣称成功，不操作其他项目）；活跃会话取消成功 → 「已取消并确认收尾」退出 0；已结束/已被清扫 → 幂等消息（「已结束，登记已清除」/「会话已不存在」）退出 0；km-ctl 退出码 4 或查询/取消失败 → `KM_SESSION_UNKNOWN` 退出 1 并保留诊断。不停止容器、不取执行锁；被外部取消的客户端退出码为工具真实状态（TERM=143 原样透传）；主动 setsid 脱离的进程不在保证范围（ADR-004） |
| `km env switch --image <引用> [--dry-run] [--yes]` | 已实现（环境切换） | 切换到本地引擎上已存在的同平台镜像（不拉取）：门禁 = 无未完成事务 + verifyStackForQuery + 容器状态 ∈ {running, exited}（exited 免会话查询：容器内无进程）+ 会话门禁（ACTIVE/UNKNOWN 阻断）+ 目标镜像本地存在 + 实际平台一致（`KM_PLATFORM_MISMATCH`）。执行序列冻结于 docs/adr-environment-transactions.md §4：只读探测容器（工作区 `:ro`，验证 km 会话依赖，缺依赖 → KM_RUNTIME_MISSING 拒绝；协议异常 → KM_TOOLS_PROTOCOL）→ 候选容器 `km-<pid>-g<N>`（按内容 ID 创建，标签 km.op/km.gen/km.role；创建响应丢失按 km.op 标签核验登记，绝不二次创建）→ inspect 级候选身份核验 → 停止旧容器 → COMMIT_INTENT → previous.json（旧槽位先轮换进 retained）→ 提交 config/state（写前哈希比对，外部修改拒绝覆盖）→ 不变量核验。目标内容与当前一致 → 明确 no-op 退出 0（不创建新代/不停止容器/不消费槽位）。`--dry-run` 只读预检与影响预览（含槽位轮换列明），不取锁不写任何文件，允许 0 / 拒绝 1。确认：交互终端展示影响后要求 `y`/`yes`；非交互须显式 `--yes`（否则用法错误 2）；拒绝 → `KM_CANCELED` 退出 1（冻结）。switch 是镜像漂移的唯一显式出口（放行 cfg.Image↔记录 的漂移检查）；`--yes` 不绕过任何门禁 |
| `km env rollback [--dry-run] [--yes]` | 已实现（单代回退） | 回退到最近一次成功 switch 的上一代：只能使用项目自己的 previous.json 记录（不接受任意容器 ID）。门禁 = 无未完成事务 + 当前容器身份 + 上一代容器实存且身份/镜像内容与记录一致 + 上一代镜像引用本地仍解析到记录内容 ID（缺失或漂移 → `KM_IMAGE_DRIFT` 拒绝并提示恢复引用）+ 平台一致 + 会话门禁。执行：停止当前代 → COMMIT_INTENT → 按上一代切换前 running/exited 语义激活 → config.image 恢复为上一代引用 → state 指向上一代 → 当前代计入 retained（reason=rolled-back）→ 消费槽位 → 不变量核验。成功后再次 rollback → `KM_NO_PREVIOUS` 退出 1（不在两代间往返）。文件边界固定提示：环境回退不撤销项目文件改动、可写层不迁移 |
| `km env recover [--dry-run] [--yes]` | 已实现（事务恢复） | 恢复未完成环境事务；收敛方向冻结（ADR §4）：stage < COMMIT_INTENT → 前态（删除本事务候选/探测容器〔按 km.op 标签发现，身份核验后才删除〕、恢复旧容器运行状态、还原状态备份）；stage ≥ COMMIT_INTENT → 新态（幂等补齐 config/state/previous、激活候选、清理探测残留）。资源身份无法确认 → 停止写操作并保留事务（不把查询错误当资源不存在）；外部修改 config/state → 拒绝覆盖并给出冲突诊断；无法自动恢复时输出完整容器 ID/阶段/原目标关系。幂等可重复调用；无事务 → 「无需恢复」退出 0（不要求 --yes）。存在事务时 run/init/stop 一律 `KM_TRANSACTION_PENDING` 阻断；sessions/cancel 不受影响。损坏的事务记录 → `KM_STATE_INVALID` 拒绝一切变更并保留原始文件。status 显示 `env_transaction_pending`（退出 1）；doctor 报告事务、槽位与账本恒等式 |

## 解析规则

- 第一段匹配管理命令（help/version/init/shell/doctor/stop/run/sessions/cancel/status/tools/env）→ km 解析；其余一律视为工具调用，工具名之后的所有 argv 原样属于工具。
- `env` 为管理命令后，重名工具仍可 `km run -- env ...` 调用（长形式带 `--` 分隔符绕过管理分派）。
- `km env` 子命令（switch/rollback/recover）只接受合同列出的参数；未知、重复、空值一律 `KM_USAGE` 退出 2；`rollback`/`recover` 不接受 `--image` 与位置参数。
- `--help`/`--version` 在任何管理命令名之前识别；未知 `-` 开头首参 → `KM_USAGE` 退出 2。
- `km run` 必须带 `--`；`--` 后第一段是工具名，即使它叫 `stop`/`run` 也属于工具。
- 工具 argv 永远逐元素传递（argv 数组），km 不拼接 shell 字符串；用户要 shell 语义需显式 `sh -c '...'`。

## 输出

- 帮助、版本、doctor 报告 → stdout（这些是产品输出）。
- km 诊断、进度、错误 → stderr。工具 stdout/stderr 分别透传，前后不附加任何 km 内容。
- 常规 Ctrl-C 目标：工具进程返回 130（最终语义以 P2 实现与复测为准）。

## 错误标识

`KM_PROJECT_NESTED`（嵌套 init；init 全程持项目锁，同项目并发/任务执行中返回 KM_PROJECT_BUSY）、`KM_PROJECT_BUSY`（同项目执行中）、`KM_IMAGE_DRIFT`（镜像标签内容与项目记录不一致；env rollback 的上一代引用缺失或漂移同样归入此类）、`KM_SESSION_ACTIVE`（容器内仍有活跃会话，宿主疑似中断遗留；阻断新任务并指向 `km sessions` / `km cancel <id>` 恢复入口）、`KM_SESSION_UNKNOWN`（会话状态查询失败/输出异常/取消未确认/指定 ID 不在当前项目会话列表中；绝不把未知或失败报成成功）、`KM_RESOURCE_UNKNOWN`（有副作用的创建或清理结果无法核实；不得报告资源不存在或清理成功）、`KM_TOOLS_PROTOCOL`（km tools 探测输出不符合协议：行数/工具名/重复/路径形态异常，或容器处于未支持状态；不把协议异常输出为 MISSING；env 候选探测输出异常同归此类）、`KM_RUNTIME_MISSING`（无 docker CLI / 容器内命令缺失 / context 无 endpoint；env 目标镜像缺少 km 会话依赖同归此类）、`KM_RUNTIME_OFFLINE`（引擎不可达）、`KM_ENDPOINT_REMOTE`（远程 endpoint，拒绝且不发引擎查询）、`KM_PROJECT_MISSING`、`KM_CONFIG_INVALID`、`KM_STATE_INVALID`（含 env 记录损坏/未知版本/组合不一致；旧构建遇 state_version 2 同样以此拒绝）、`KM_CONTAINER_CONFLICT`（ID/名称/标签/挂载/镜像内容任一不符或同名重建，不接管）、`KM_RUNTIME_MISMATCH`（状态记录的引擎与当前有效 endpoint 漂移，跳过容器/镜像检查）、`KM_TIMEOUT`（管理查询超时）、`KM_CANCELED`（km 收到取消，子进程已终止；用户拒绝 env 变更确认亦归此类）、`KM_TRANSACTION_PENDING`（存在未完成环境事务：switch/rollback/run/init/stop 阻断并指向 km env recover）、`KM_NO_PREVIOUS`（回退槽位为空，无可回退记录）、`KM_PLATFORM_MISMATCH`（目标/上一代与当前容器实际平台不一致，不跨架构迁移）、`KM_USAGE`。

## 超时

管理查询（version/inspect/ps/image 等）单项默认 10 秒上限，doctor 整体预算 45 秒；超时返回 `KM_TIMEOUT`，用户取消返回 `KM_CANCELED`，两种情况子进程都会被终止。该超时只用于管理操作；P2 的长工具执行不套用。

## 项目身份

- `.km.json`（可共享，声明式）：`schema_version`（当前仅 1）、`image`（必填）、`name`/`platform`（可选）。未知字段即报错并指出字段名；km 不执行其中任何内容。
- `.km/state.json`（本机，勿提交）：`state_version`（1=旧格式；2=已采纳环境切换，本构建起最低支持）、`project_id`（随机 `p`+10 hex）、`container.id`（完整不可变容器 ID，归属检查的唯一入口）、`container.name`（`km-<project_id>` 或代际名 `km-<project_id>-g<N>`，仅信息展示与同名重建诊断）、`container.image_id`（当前代的镜像内容 ID）、docker context/endpoint 身份、创建时间；v2 附加 `env{env_version, generation}`（当前代号，0=原始代）。缺少容器 ID 的旧状态会被判为不完整，不按名称接管；高于本构建支持的版本一律明确拒绝，不猜测修复。
- `.km/env/`（本机，勿提交；仅采纳环境切换后出现）：`transaction.json`（未完成事务：op_id/kind/stage/前后快照/config+state 备份及哈希/探测登记；存在即中间态）、`previous.json`（回退槽位：上一代容器身份 + 切换前 config.image 引用 + was_running）、`retained.json`（有意保留容器的资源账本，按容器 ID 去重只增不删）。各记录 `env_version=1`，未知版本拒绝读写；账本恒等式 = 实际项目容器 ⊆ 当前代 + 上一代 + retained + 事务资源。
- 归属认定 = 记录的容器 ID 存在 + 名称 + `km.project` 标签 + `/workspace` 挂载源 + 容器实际镜像内容全部匹配；记录 ID 不存在而同名容器存在时判"同名重建"冲突。缺一按 `KM_CONTAINER_CONFLICT` 处理。

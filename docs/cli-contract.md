# km CLI 合同（v0.1，随实现演进）

退出码：成功 0；环境前提错误 1；用法错误/未实现命令 2。km 自身错误带 `KM_...` 稳定标识，与工具退出码区分。

## 命令

| 命令 | 状态（P1） | 合同 |
|---|---|---|
| `km` / `km --help` | 已实现 | 简短帮助；零 Docker 依赖、零副作用（单测断言外部调用次数为 0） |
| `km --version` | 已实现 | 版本行；零 Docker 依赖 |
| `km doctor` | 已实现 | 只读检查（含容器内活跃/遗留会话报告）。流程：①平台；②项目配置与本机状态（纯客户端文件）；③仅用客户端命令解析有效 endpoint（DOCKER_HOST > DOCKER_CONTEXT > 当前 context inspect），非本地 endpoint 判 `KM_ENDPOINT_REMOTE` 并跳过一切引擎查询；本地则把该 endpoint 固定（DOCKER_HOST 注入）给本次所有后续调用并查引擎版本；④比较 `.km/state.json` 记录的 endpoint，漂移判 `KM_RUNTIME_MISMATCH` 并跳过容器/镜像检查；⑤容器归属按记录的完整容器 ID 检查（名称、`km.project` 标签、`/workspace` 挂载源、容器实际镜像内容全部比对），同名重建判冲突不接管；⑥镜像标签内容与项目记录比对，漂移报警告。环境问题作为检查结果输出（stdout），doctor 自身完成即返回 0 |
| `km init` | 已实现（非交互最小可用版） | 幂等：身份一致时复用；缺失镜像显式拉取（有界）；容器按记录完整 ID 校验/启动/重建；状态与配置原子写入；失败只回滚本次创建的资源；检测父项目（KM_PROJECT_NESTED） |
| `km TOOL ARG...` | 已实现（非交互） | 会话内核执行（ADR-004）：argv 逐元素、三流流式、cwd 映射（符号链接逃逸拒绝）、退出码原样（取消 130）；停止的容器自动恢复；引擎/容器/镜像身份不符显式报错不静默重建；同项目串行（KM_PROJECT_BUSY），遗留锁清理不等于容器任务结束 |
| `km run -- TOOL ARG...` | 已实现（非交互） | 同上长形式；`--` 必需，解决工具与 km 管理命令重名 |
| `km shell` | 已实现（C2） | 交互 bash：Docker CLI 接管真实终端（raw mode/恢复/尺寸归客户端）；会话登记进 /tmp/km-sessions（与工具会话互斥，崩溃遗留阻断后续任务）；stdin/stdout 非终端 → KM_NOT_TTY（exit 1，进入前失败）；外部 SIGTERM/SIGHUP → 恢复终端并退出 143；键盘 Ctrl-C/Ctrl-D/作业控制直达 bash；退出码原样透传 |
| `km stop` | 已实现 | 只停止当前项目已验证身份的容器；不删除容器/文件/镜像；幂等；执行中返回 KM_PROJECT_BUSY |
| `km sessions` | 已实现（会话恢复） | 只读列出当前项目容器内会话：stdout 为 `ACTIVE <id>` / `STALE <id>` 行（完整 ID 可复制），辅助提示走 stderr；无会话输出「当前项目无会话」退出 0；脚本未安装（127）说明为预期状态退出 0；容器未运行时说明登记将随下次执行清扫退出 0（不启动容器）；查询失败/输出异常 → `KM_SESSION_UNKNOWN` 退出 1。门禁=只读归属校验（endpoint 固定 + 引擎匹配 + 容器 ID/名称/标签/挂载），**不含镜像内容检查**（镜像漂移不阻止恢复）；不取项目执行锁，不安装脚本、不清扫、不改变任何运行状态 |
| `km cancel <id>` | 已实现（会话恢复） | 显式取消当前项目的指定会话并核验终态。完整 ID 精确匹配（`s`+16 hex，非法 → KM_USAGE 退出 2）；先列会话确认归属，列表中不存在的 ID → `KM_SESSION_UNKNOWN` 退出 1（不宣称成功，不操作其他项目）；活跃会话取消成功 → 「已取消并确认收尾」退出 0；已结束/已被清扫 → 幂等消息（「已结束，登记已清除」/「会话已不存在」）退出 0；km-ctl 退出码 4 或查询/取消失败 → `KM_SESSION_UNKNOWN` 退出 1 并保留诊断。不停止容器、不取执行锁；被外部取消的客户端退出码为工具真实状态（TERM=143 原样透传）；主动 setsid 脱离的进程不在保证范围（ADR-004） |

## 解析规则

- 第一段匹配管理命令（help/version/init/shell/doctor/stop/run/sessions/cancel）→ km 解析；其余一律视为工具调用，工具名之后的所有 argv 原样属于工具。
- `--help`/`--version` 在任何管理命令名之前识别；未知 `-` 开头首参 → `KM_USAGE` 退出 2。
- `km run` 必须带 `--`；`--` 后第一段是工具名，即使它叫 `stop`/`run` 也属于工具。
- 工具 argv 永远逐元素传递（argv 数组），km 不拼接 shell 字符串；用户要 shell 语义需显式 `sh -c '...'`。

## 输出

- 帮助、版本、doctor 报告 → stdout（这些是产品输出）。
- km 诊断、进度、错误 → stderr。工具 stdout/stderr 分别透传，前后不附加任何 km 内容。
- 常规 Ctrl-C 目标：工具进程返回 130（最终语义以 P2 实现与复测为准）。

## 错误标识

`KM_PROJECT_NESTED`（嵌套 init；init 全程持项目锁，同项目并发/任务执行中返回 KM_PROJECT_BUSY）、`KM_PROJECT_BUSY`（同项目执行中）、`KM_IMAGE_DRIFT`（镜像标签内容与项目记录不一致）、`KM_SESSION_ACTIVE`（容器内仍有活跃会话，宿主疑似中断遗留；阻断新任务并指向 `km sessions` / `km cancel <id>` 恢复入口）、`KM_SESSION_UNKNOWN`（会话状态查询失败/输出异常/取消未确认/指定 ID 不在当前项目会话列表中；绝不把未知或失败报成成功）、`KM_RUNTIME_MISSING`（无 docker CLI / 容器内命令缺失 / context 无 endpoint）、`KM_RUNTIME_OFFLINE`（引擎不可达）、`KM_ENDPOINT_REMOTE`（远程 endpoint，拒绝且不发引擎查询）、`KM_PROJECT_MISSING`、`KM_CONFIG_INVALID`、`KM_STATE_INVALID`、`KM_CONTAINER_CONFLICT`（ID/名称/标签/挂载/镜像内容任一不符或同名重建，不接管）、`KM_RUNTIME_MISMATCH`（状态记录的引擎与当前有效 endpoint 漂移，跳过容器/镜像检查）、`KM_TIMEOUT`（管理查询超时）、`KM_CANCELED`（km 收到取消，子进程已终止）、`KM_USAGE`。

## 超时

管理查询（version/inspect/ps/image 等）单项默认 10 秒上限，doctor 整体预算 45 秒；超时返回 `KM_TIMEOUT`，用户取消返回 `KM_CANCELED`，两种情况子进程都会被终止。该超时只用于管理操作；P2 的长工具执行不套用。

## 项目身份

- `.km.json`（可共享，声明式）：`schema_version`（当前仅 1）、`image`（必填）、`name`/`platform`（可选）。未知字段即报错并指出字段名；km 不执行其中任何内容。
- `.km/state.json`（本机，勿提交）：`project_id`（随机 `p`+10 hex）、`container.id`（完整不可变容器 ID，归属检查的唯一入口）、`container.name`（`km-<project_id>`，仅信息展示与同名重建诊断）、`container.image_id`（init 时的镜像内容 ID）、docker context/endpoint 身份、创建时间。缺少容器 ID 的旧状态会被判为不完整，不按名称接管。
- 归属认定 = 记录的容器 ID 存在 + 名称 + `km.project` 标签 + `/workspace` 挂载源 + 容器实际镜像内容全部匹配；记录 ID 不存在而同名容器存在时判"同名重建"冲突。缺一按 `KM_CONTAINER_CONFLICT` 处理。

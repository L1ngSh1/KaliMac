# km CLI 合同（v0.1，随实现演进）

退出码：成功 0；环境前提错误 1；用法错误/未实现命令 2。km 自身错误带 `KM_...` 稳定标识，与工具退出码区分。

## 命令

| 命令 | 状态（P1） | 合同 |
|---|---|---|
| `km` / `km --help` | 已实现 | 简短帮助；零 Docker 依赖、零副作用 |
| `km --version` | 已实现 | 版本行；零 Docker 依赖 |
| `km doctor` | 已实现 | 只读检查：平台、Docker CLI/引擎、context/endpoint、项目配置、本机状态、容器归属（标签+挂载）、镜像。环境问题作为检查结果输出（stdout），doctor 自身完成即返回 0 |
| `km init` | 未实现（KM_NOT_IMPLEMENTED，退出 2） | P2：初始化项目 + 准备镜像 + 启动容器；幂等；检测到父项目明确提示 |
| `km TOOL ARG...` | 未实现（KM_NOT_IMPLEMENTED，退出 2） | P2：在项目容器执行工具 |
| `km run -- TOOL ARG...` | 未实现（KM_NOT_IMPLEMENTED，退出 2） | P2：同上长形式；`--` 必需，解决工具与 km 管理命令重名 |
| `km shell` | 未实现（KM_NOT_IMPLEMENTED，退出 2） | P2：交互终端 |
| `km stop` | 未实现（KM_NOT_IMPLEMENTED，退出 2） | P2：停止项目容器，数据保留，幂等 |

## 解析规则

- 第一段匹配管理命令（help/version/init/shell/doctor/stop/run）→ km 解析；其余一律视为工具调用，工具名之后的所有 argv 原样属于工具。
- `--help`/`--version` 在任何管理命令名之前识别；未知 `-` 开头首参 → `KM_USAGE` 退出 2。
- `km run` 必须带 `--`；`--` 后第一段是工具名，即使它叫 `stop`/`run` 也属于工具。
- 工具 argv 永远逐元素传递（argv 数组），km 不拼接 shell 字符串；用户要 shell 语义需显式 `sh -c '...'`。

## 输出

- 帮助、版本、doctor 报告 → stdout（这些是产品输出）。
- km 诊断、进度、错误 → stderr。工具 stdout/stderr 分别透传，前后不附加任何 km 内容。
- 常规 Ctrl-C 目标：工具进程返回 130（最终语义以 P2 实现与复测为准）。

## 错误标识

`KM_RUNTIME_MISSING`（无 docker CLI / 容器内命令缺失）、`KM_RUNTIME_OFFLINE`（引擎不可达）、`KM_ENDPOINT_REMOTE`（远程 endpoint，v0.1 拒绝）、`KM_PROJECT_MISSING`、`KM_CONFIG_INVALID`、`KM_STATE_INVALID`、`KM_CONTAINER_CONFLICT`（名称/标签/挂载任一不符，不接管）、`KM_NOT_IMPLEMENTED`、`KM_USAGE`。

## 项目身份

- `.km.json`（可共享，声明式）：`schema_version`（当前仅 1）、`image`（必填）、`name`/`platform`（可选）。未知字段即报错并指出字段名；km 不执行其中任何内容。
- `.km/state.json`（本机，勿提交）：`project_id`（随机 `p`+10 hex）、容器名 `km-<project_id>`、docker context/endpoint 身份、镜像内容标识、创建时间。
- 归属认定 = 容器 ID + `km.project` 标签 + `/workspace` 挂载源三者同时匹配，缺一按 `KM_CONTAINER_CONFLICT` 处理。

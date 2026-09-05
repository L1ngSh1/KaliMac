# Kali-Mac 架构短记（P0 后）

目标不变：Mac 终端输入 `km 工具 参数`，在当前项目的 Kali 容器执行；文件留在 Mac，输出与退出状态回原终端。

```text
Mac 终端
  └─ km（Go, 本仓库）
      ├─ internal/cli        分派/帮助/版本/doctor；工具 argv 原样透传
      ├─ internal/project    .km.json 校验；.km/ 本机状态与身份
      ├─ internal/runtime    docker CLI 封装（Executor 可注入，测试用 fake）
      └─ internal/terminal   （P2 引入：交互/信号/终端恢复，按 P0 结论实现）
```

## P0 改变/确认的设计决策

1. **信号路径（ADR-001）**：实验证实杀死 `docker exec` 客户端不传播任何信号到容器内任务；只有 PTY 中键入的 Ctrl-C（0x03 字节）可靠到达任务。因此 P2 的非交互工具执行必须由 km 进程主动做会话级清理，方案选择见 docs/phase-0.md「对 P2 的直接约束」。
2. **容器主进程**：必须处理 SIGTERM（避免 stop 恒等 10s 宽限期）。
3. **镜像**：裸 `kalilinux/kali-rolling` 无候选工具；P2 起本地构建精选镜像（apt 源可达性已实测）。本机拉取走镜像站 `docker.1ms.run`（Docker Hub 直连超时的网络观察）。
4. **归属认定**：容器 ID + `km.project` 标签 + `/workspace` 挂载源三重匹配（doctor 已实现该检查，P2 所有变更操作复用同一判定）。
5. **运行时层保持单实现**：只有 docker CLI 一个后端；Executor 接口仅为可测试性存在，不做成插件框架。

## 错误模型

工具退出码原样返回；km 基础设施错误一律携带 `KM_*` 稳定标识（见 docs/cli-contract.md），避免与工具退出码数值重叠混淆。

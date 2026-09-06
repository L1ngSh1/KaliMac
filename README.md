# Kali-Mac（km）

Mac 上精简、可靠的 Kali CLI 入口：在终端输入 `km 工具 参数`，在当前项目的 Kali 容器执行；项目文件留在 Mac，工具输出与退出状态回到原终端。

状态：**P0 技术验证 + P1 最小 CLI 骨架已完成（含 review 修复，0.1.1-p1）**。`init / run / shell / stop` 属于 P2，当前调用会明确报 `KM_NOT_IMPLEMENTED`（不会伪装成功）。doctor 会先解析有效 Docker endpoint（非本地引擎直接拒绝且不发引擎查询），并按记录的容器 ID、标签、挂载与镜像内容核验项目归属。

## 依赖

- macOS（首个验证平台：Apple Silicon）
- Go 1.25+（构建）
- 本机 Docker Desktop（P1 的 doctor 会检测；help/version 不需要）

## 快速开始

```bash
make build          # 产出 ./bin/km
./bin/km --help
./bin/km --version  # 不依赖 Docker
./bin/km doctor     # 只读检查平台/Docker/项目配置/容器归属/镜像
```

在任意目录运行 `./bin/km doctor` 会从当前目录向上查找 `.km.json` 项目配置并核对本机状态与容器归属。

## 文档

- [docs/phase-0.md](docs/phase-0.md) — P0 实验记录：argv/stdio/退出码/挂载/信号（核心风险：docker exec 客户端死亡不传播信号，已实验证实）
- [docs/cli-contract.md](docs/cli-contract.md) — CLI 行为合同与错误标识
- [docs/architecture.md](docs/architecture.md) — 架构短记与设计决策
- [docs/verification.md](docs/verification.md) — 本轮验证命令与结果

## 开发

```bash
make test   # go test ./...
make vet    # go vet ./...
make all    # vet + test + build
```

单测不需要 Docker（runtime 层 executor 可注入）。P0 实验集可重跑：`bash tests/p0/run-p0.sh`（自动创建唯一标签的实验容器、生成证据到 `tests/p0/evidence/`、结束后清理并核对）。

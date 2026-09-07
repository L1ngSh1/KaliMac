# Kali-Mac（km）

Mac 上精简、可靠的 Kali CLI 入口：在终端输入 `km 工具 参数`，在当前项目的 Kali 容器执行；项目文件留在 Mac，工具输出与退出状态回到原终端。

状态：**P2-C 交互版（0.3.0-p2）**。`init → run → shell → stop → 恢复` 闭环可用：非交互执行（会话级取消、退出码透传）+ 交互 bash（真实终端接管，作业控制/Ctrl-C/窗口尺寸跟随）。执行走最小会话内核（唯一会话身份 + 容器内侧进程组清理，SIGINT 目标退出码 130）；doctor 先解析有效 Docker endpoint（非本地引擎直接拒绝且不发引擎查询），并按记录的容器 ID、标签、挂载与镜像内容核验项目归属。

## 依赖

- macOS（首个验证平台：Apple Silicon）
- Go 1.25+（构建）
- 本机 Docker Desktop（P1 的 doctor 会检测；help/version 不需要）

## 快速开始

**第一次使用？先看 [km 使用指南](docs/user-guide.md)**：从构建、练习项目到日常命令，区分 Mac 终端与容器 shell，并提供报错排查和当前已知问题。

```bash
make build                     # 产出 ./bin/km
KM="$PWD/bin/km"               # 固定绝对路径（进入其他目录后 ./bin/km 不再可达）
"$KM" --version                # 不依赖 Docker

# （推荐）构建本地精选镜像并让项目使用它
docker build -t kali-mac-min:0.2 images/kali

cd /path/to/你的项目
echo '{"schema_version":1,"image":"kali-mac-min:0.2"}' > .km.json
"$KM" init                     # 建立项目环境（幂等；未写 .km.json 时使用默认镜像）
echo 'print("hi")' > t.py
"$KM" run -- python3 t.py      # 或 "$KM" python3 t.py
"$KM" shell                    # 交互 bash（真实终端接管；Ctrl-C/作业控制可用）
# 在容器内输入 exit 回到 Mac 后，再执行以下命令
"$KM" stop                     # 停止（容器与数据保留，幂等）
"$KM" doctor                   # 只读检查平台/Docker/项目/容器归属/镜像/会话
```

工具在项目容器内执行；项目文件经 bind mount 双向可见（容器内 /workspace），工具输出与退出状态回到原终端。镜像构建证据见 tests/evidence/。

## 文档

- [docs/user-guide.md](docs/user-guide.md) — 面向使用者：首次配置、命令速查、文件共享、退出与排错
- [docs/adr-004-session-execution.md](docs/adr-004-session-execution.md) — 会话执行与取消方案（P2-A，含取消/失联语义）
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

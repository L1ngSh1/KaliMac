# Kali-Mac（km）

Mac 上精简、可靠的 Kali CLI 入口：在终端输入 `km 工具 参数`，在当前项目的 Kali 容器执行；项目文件留在 Mac，工具输出与退出状态回到原终端。

状态：**会话恢复版（0.4.0-p3 候选）**。`init → run → shell → stop → 恢复` 闭环可用：非交互执行（会话级取消、退出码透传）+ 交互 bash（真实终端接管，作业控制/Ctrl-C/窗口尺寸跟随）。执行走最小会话内核（唯一会话身份 + 容器内侧进程组清理，SIGINT 目标退出码 130）；doctor 先解析有效 Docker endpoint（非本地引擎直接拒绝且不发引擎查询），并按记录的容器 ID、标签、挂载与镜像内容核验项目归属。

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
"$KM" version --verbose        # 提交、工作区标记与目标架构

# （推荐）构建本地精选镜像并让项目使用它
docker build -t kali-mac-min:0.2 images/kali

cd /path/to/你的项目
"$KM" init --image kali-mac-min:0.2  # 建立项目环境并写入配置（幂等）
echo 'print("hi")' > t.py
"$KM" run -- python3 t.py      # 或 "$KM" python3 t.py
"$KM" shell                    # 交互 bash（真实终端接管；Ctrl-C/作业控制可用）
# 在容器内输入 exit 回到 Mac 后，再执行以下命令
"$KM" stop                     # 停止（容器与数据保留，幂等）
"$KM" doctor                   # 只读检查平台/Docker/项目/容器归属/镜像/会话
```

工具在项目容器内执行；项目文件经 bind mount 双向可见（容器内 /workspace），工具输出与退出状态回到原终端。镜像构建证据见 tests/evidence/。

## 安装 / 打包（本地）

```bash
make build
DESTDIR=/tmp/stage PREFIX=/opt/km-test scripts/install.sh   # 临时目录试装
PREFIX=$HOME/.local scripts/install.sh                      # 用户级安装
PREFIX=$HOME/.local scripts/uninstall.sh                    # 卸载（只删清单内文件）
scripts/package.sh                                          # 分架构安装包 + SHA256SUMS → dist/
```

安装为清单式（`$PREFIX/share/km/manifest.txt`），卸载只删除清单内文件，不改 PATH 与 shell 配置。
发布包解压后可直接运行包内 `install.sh`；它只安装同目录的已验收二进制，并把构建身份保存为
`$PREFIX/share/km/BUILD-INFO`。打包过程不查询 Docker；若需关联已核验镜像内容 ID，显式设置
`KM_IMAGE_CONTENT_ID=sha256:… scripts/package.sh`。
平台边界：**darwin/arm64 为实测平台**（本机全量验证）；darwin/amd64 为交叉编译产物，未在真实硬件
做过端到端测试；Linux 未测试（CI integration 作业首跑后另记）。镜像构建与来源见 `images/kali/`
与 `tests/evidence/kali-mac-min-0.2/build-evidence.txt`。候选版本号、tag 与发布命令均为待审步骤，
不会自动执行。

## 文档

- [docs/user-guide.md](docs/user-guide.md) — 面向使用者：首次配置、命令速查、文件共享、退出与排错
- [docs/adr-004-session-execution.md](docs/adr-004-session-execution.md) — 会话执行与取消方案（P2-A，含取消/失联语义）
- [docs/phase-0.md](docs/phase-0.md) — P0 实验记录：argv/stdio/退出码/挂载/信号（核心风险：docker exec 客户端死亡不传播信号，已实验证实）
- [docs/cli-contract.md](docs/cli-contract.md) — CLI 行为合同与错误标识
- [docs/architecture.md](docs/architecture.md) — 架构短记与设计决策
- [docs/verification.md](docs/verification.md) — 本轮验证命令与结果

## 开发

```bash
make test          # go test -count=1 ./...
make vet           # go vet ./...
make fmt-check     # gofmt 检查（make fmt 写入）
make integration   # 真实集成套件（需本机 Docker 引擎与最小镜像）
make all           # fmt-check + vet + test + build
```

单测不需要 Docker（runtime 层 executor 可注入）。真实集成套件按完整容器 ID 登记终检，
区分「残留 / 无法核实」，引擎不可达时显式 `P2-INTEGRATION-SKIP`。P0 实验集可重跑：
`bash tests/p0/run-p0.sh`（自动创建唯一标签的实验容器、生成证据到 `tests/p0/evidence/`、
结束后清理并核对）。性能基线：`tests/perf/perf-baseline.sh`（单调时钟、逐样本退出码检查、
配对 `docker exec` 对照）。CI 工作流见 `.github/workflows/ci.yml`（verify + 独立 integration
作业；首次 push 前远端不会运行）。

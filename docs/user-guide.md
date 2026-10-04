# km 使用指南

适用版本：`0.4.0-rc.1`（候选）。先读这份说明，开发设计与实验细节再看 ADR。

## 1. 先理解：km 到底是什么？

**km 是你在 Mac 上使用项目专属 Kali 容器的命令入口，不是一台完整 Kali 虚拟机。**

```text
Mac 项目文件夹  ← 双向共享 →  容器里的 /workspace
Mac 终端输入 km python3 …  →  容器执行 python3 → 输出回到 Mac 终端
```

- Go 只用于构建 km；Docker Desktop 提供运行环境。
- 项目有各自的容器。同一项目当前只运行一个 km 工具任务或交互 shell。
- km 不会自动安装你输入的任意工具；工具必须存在于项目镜像或容器中。
- 当前精选镜像包含 `python3`、`curl`、`jq`、`file`、`openssl`、`nmap`，不是完整 Kali 工具集。

## 2. 第一次准备（在 Mac 终端）

先启动 Docker Desktop。安装包用户只需 macOS 与本机 Docker Desktop；从源码构建时再准备 Go 1.25+。
Apple Silicon 选择 `darwin-arm64`；Intel Mac 选择 `darwin-amd64`（交叉编译与校验和已覆盖，实机验收待补）。
安装包不包含 Kali 镜像，附带 `images/kali/Dockerfile`；第一次构建需联网。

从 [本版 Release](https://github.com/L1ngSh1/KaliMac/releases/tag/v0.4.0-rc.1) 下载对应压缩包与
`SHA256SUMS`，放在同一个下载目录。下面以 Apple Silicon 为例：

```bash
shasum -a 256 km-0.4.0-rc.1-darwin-arm64.tar.gz
grep 'km-0.4.0-rc.1-darwin-arm64.tar.gz' SHA256SUMS
# 两行的 SHA256 必须一致，再解包；Intel 用户将 arm64 换为 amd64
tar -xzf km-0.4.0-rc.1-darwin-arm64.tar.gz
cd km-0.4.0-rc.1-darwin-arm64
```

只有一个架构的文件时按上述两行核对；下载了两个架构时可用 `shasum -a 256 -c SHA256SUMS` 一次核对。

**路径 A（拿到安装包的新用户）**：解压对应架构的压缩包，在解压目录安装；不需要开发仓库：

```bash
PREFIX="$HOME/.local" ./install.sh
export PATH="$HOME/.local/bin:$PATH"
KM="$HOME/.local/bin/km"
"$KM" version --verbose
docker info --format '{{.ServerVersion}}'
# 仍在解压目录；已有本地精选镜像时可跳过这一行
docker build -t kali-mac-min:0.2 images/kali
```

成功标志：版本是 `0.4.0-rc.1`，`commit` 与 Release 的源码提交相同，`worktree` 为 `clean`；
Docker 输出服务端版本，镜像构建正常结束。安装脚本不替你修改 shell 配置；新终端需重新配置 PATH 或设置 `KM`。

**路径 B（在开发仓库内）**：从仓库根目录执行：

```bash
make build
KM="$PWD/bin/km"
"$KM" --version
docker info --format '{{.ServerVersion}}'
docker build -t kali-mac-min:0.2 images/kali
```

`KM` 保存可执行文件的绝对路径，这样切换目录后也能调用。首次镜像构建需要下载依赖；本地已有该镜像时可跳过构建。CLI 版本 `0.4.0-rc.1` 和镜像标签 `0.2` 是两套版本号。

### 为什么下面写的是 `"$KM"`，不是 `km`？

`make build` 只生成仓库中的 `bin/km`，并未把命令安装进 PATH。

- 当前终端：按上面的方式设置 `KM`，照抄本指南即可。
- 新开终端：重新设置 `KM` 为仓库里 `bin/km` 的绝对路径。
- 想直接输入 `km`：在仓库根目录执行 `export PATH="$PWD/bin:$PATH"`，对当前终端生效。确认好仓库绝对路径后，可把对应的 PATH 配置加入自己的 shell 配置文件。

后文的 `"$KM" shell` 与配置好 PATH 后的 `km shell` 是同一条命令。

## 3. 第一次试用：创建一个独立练习项目

仍在刚才的 **Mac 终端**，执行：

```bash
mkdir -p "$HOME/Workspace"
DEMO=$(mktemp -d "$HOME/Workspace/km 初次体验.XXXXXX")
cd "$DEMO"
"$KM" init --image kali-mac-min:0.2
"$KM" doctor
echo "$DEMO"
```

这里创建的是全新练习目录，配置不会覆盖你的现有项目。记下 `echo "$DEMO"` 显示的位置，下次回到这里即可继续用。

`init` 会建立项目配置、本机状态和容器；同一个项目重复执行会复用已有环境。这里通过参数选择精选镜像，不需要手写 JSON；省略参数时默认使用的 Kali 基础镜像不等于精选工具镜像。

### 跑一条命令

```bash
"$KM" python3 --version
"$KM" run -- python3 -c 'print("hello from Kali")'
"$KM" run -- pwd
```

最后一条在练习项目根目录应输出 `/workspace`。短形式 `km TOOL …` 与长形式 `km run -- TOOL …` 等价；长形式的 `--` 是必需的。

### 看状态和工具

```bash
"$KM" status
"$KM" tools
"$KM" env list
```

先完成上一小节的命令，再检查：`status` 显示 `running_idle`，`tools` 列出六项 AVAILABLE，
`env list` 中当前容器角色为 CURRENT。`tools` 的 AVAILABLE 只代表 PATH 中存在该程序。
首次 `run` 前的 doctor 可能提示“会话脚本未安装”；首次执行后再次检查即可。

### 验证文件共享

```bash
printf 'print("hello from a Mac file")\n' > hello.py
"$KM" python3 hello.py
"$KM" python3 -c 'from pathlib import Path; Path("result.txt").write_text("made in Kali\n")'
cat result.txt
```

成功标志：Python 输出 `hello from a Mac file`，Mac 的 `cat result.txt` 输出 `made in Kali`。

`hello.py` 在 Mac 创建、在 Kali 执行；`result.txt` 在 Kali 创建、在 Mac 直接可读。**容器内对 `/workspace` 文件的修改和删除，也会影响 Mac 项目文件。**

## 4. 想连续操作：进入 shell

在 **Mac 终端**单独运行：

```bash
"$KM" shell
```

看到 `KM_SHELL> ` 后，你已经进入 **容器里的 bash**。这时直接输入工具名，不要再加 `km`：

```bash
pwd
ls
python3 hello.py
exit
```

`exit` 后才回到 Mac 终端。不要把后面的 Mac 命令提前一起粘贴进容器。

想知道容器里有哪些常用工具？回到 Mac 终端运行 `"$KM" tools`：它列出精选六项
（python3、curl、jq、file、openssl、nmap）的 AVAILABLE（含路径）或 MISSING。清单只是
精选集合，不代表容器全部软件；AVAILABLE 表示可从 PATH 定位，不保证版本或执行结果。
缺失时的可持续做法是维护镜像 Dockerfile（`images/kali`）并重建；在容器里临时安装
不会随容器删除保留，不构成可复现配置。

| 操作 | 含义 |
|---|---|
| `Ctrl-C` | 中断当前前台命令，通常仍留在 shell 中 |
| `exit` | 退出交互 shell；容器继续运行 |
| 空提示符处 `Ctrl-D` | 退出 shell |
| `exit 7` | shell 退出，并让 km 返回退出码 7 |
| Mac 上的 `km stop` | 停止整个项目容器，不删除它 |

当前 shell 使用 `bash --noprofile --norc`，不会读取你的 Mac zsh 配置，也不会加载 bash 启动文件。`km shell` 需要真正的终端，不能用 `echo … | km shell` 代替脚本执行。

## 5. 每天最常用的五步

以下均在 **Mac 终端**：

```bash
cd "$DEMO"                   # 或你实际的项目目录；新终端请填真实路径
"$KM" doctor                 # 有疑问先检查；正常使用无需每次都跑
"$KM" python3 hello.py       # 单条命令，用完直接回到 Mac
"$KM" stop                   # 当前没有 km 任务时，停止项目容器
"$KM" status                 # 应显示 container_stopped
"$KM" python3 hello.py       # 下次执行会自动启动已停止的容器
```

连续操作就用上一节的 `shell → exit`，退出后再执行 `stop`。项目首次需要 `init`，不是每次运行都需要；目前也不需要单独的 `km start` 命令。

成功标志：停止后状态为 `container_stopped`，再次执行脚本仍输出原来的内容；`result.txt` 仍在。
结束练习时再运行 `"$KM" stop` 即可，下次回到打印出的练习目录继续使用。

### 主动体验会话取消（两个 Mac 终端）

终端 A 在练习项目中运行 `"$KM" run -- sleep 120`。终端 B 设置相同的 `KM`，
`cd` 到同一个练习目录，然后分步执行：

```bash
"$KM" sessions
# 从上一条输出完整复制 s 开头的会话 ID；下行是模板，先替换占位符
"$KM" cancel <完整会话ID>
"$KM" run -- printf 'recovered\n'
```

成功标志：会话从 ACTIVE 消失，终端 A 返回（TERM 通常为退出码 143），终端 B 输出 `recovered`。
没有必要为了体验恢复去强杀终端。若真的遇到异常退出，仍按 sessions → 核对任务 → cancel 的顺序处理。
查询成功但 ID 已不在列表时，cancel 会报告 `KM_SESSION_UNKNOWN`，不要把它理解成刚刚取消成功。

### 管道和重定向在哪执行？

```bash
# > 由 Mac 的 shell 处理：输出保存到 Mac 当前目录
"$KM" python3 hello.py > output.txt

# 管道放进引号里交给容器中的 sh 执行
"$KM" run -- sh -c 'printf "a\nb\n" | wc -l'
```

工具参数中的路径按容器环境解释。项目根目录映射为 `/workspace`；从项目子目录调用 km 时，工作目录也映射到对应子目录。Mac 上项目外的绝对路径不会自动变成容器可访问路径。

## 6. 文件、配置和工具装在哪里？

| 内容 | 位置与保留方式 |
|---|---|
| 源码、结果、笔记 | 放在项目目录，即容器 `/workspace`；Mac 上直接可见 |
| `.km.json` | 声明使用哪个镜像；可以跟项目一起提交 Git |
| `.km/` | 当前机器的容器身份、锁等状态；在项目 `.gitignore` 中加入 `.km/` |
| 容器中 `/workspace` 外的文件 | 不在项目共享目录；stop/start 保留，容器被删除后不保留 |
| 额外安装的工具 | 临时装进容器只随该容器保存；要可复现，应维护镜像 Dockerfile |

精选镜像不是所有 Kali 工具的全集。先用 `km shell` 后的 `command -v 工具名` 检查。修改镜像、搬动项目目录或切换 Docker 引擎都涉及项目身份，不建议把删除 `.km/` 当成通用修复方法。

## 7. 更换项目镜像：env switch / rollback / recover

`km tools` 报告缺工具时，自己准备一个新镜像（`docker build`/`docker pull` 到本地引擎），然后显式切换：

```bash
"$KM" env list                                  # 只看：哪些是当前环境/回退目标/保留容器，谁能删
"$KM" env switch --image my-kali:v2 --dry-run   # 只读预览：当前/目标镜像、平台与全部影响
"$KM" env switch --image my-kali:v2             # 交互终端会要求输入 yes 确认；脚本中须加 --yes
"$KM" env rollback --dry-run                    # 新环境不合适？先预览回退
"$KM" env rollback                              # 回到上一代（只保留一代）
"$KM" env remove <完整容器ID> --dry-run          # 删除一个确认不要的保留容器（先预览）
"$KM" env remove <完整容器ID> --yes              # 真正删除（可写层永久丢失，不可逆）
"$KM" env recover --dry-run                     # 中断后恢复未完成事务（少见，按提示使用）
```

约定：

- 只切换**本地引擎上已存在**的同平台镜像（不自动拉取/构建/装工具）；目标按镜像内容 ID 锁定，构建期标签漂移不会静默换目标；同内容不同标签是明确 no-op。
- 切换 = 新建候选容器 → 停止并保留旧容器；旧容器的可写层（容器里临时装的东西）不迁移。
- **环境回退不会撤销新环境运行期间对项目文件的改动**（`/workspace` 就是 Mac 目录）。回退槽位只有一代：成功回退后再次 rollback 会明确提示无可回退记录；被换下的容器保留并登记在 `.km/env/retained.json`（不入 Git）。`km env list` 会标明每个容器的角色与是否可删除；`km env remove <完整容器ID>` 可以删除一个已停止的保留容器——只删这一个容器，可写层内容永久丢失、项目文件与镜像不受影响；当前环境、回退目标和别的项目的容器永远拒绝删除。
- 有未完成任务（`KM_SESSION_ACTIVE`/`KM_SESSION_UNKNOWN`）、身份冲突或平台不一致时拒绝切换；切换被强杀中断后，`run/stop/init/switch/rollback` 都会以 `KM_TRANSACTION_PENDING` 阻断，按提示先 `km env recover --dry-run` 再 `km env recover` 恢复（恢复方向由冻结规则决定，重复调用幂等）。
- `--dry-run` 永远只读（不取锁、不写状态、不触碰容器）；确认只替代交互输入，不绕过任何检查。

## 8. 报错时怎么办？

先在 Mac 的项目目录运行 `"$KM" doctor`，阅读各检查项；doctor 完成检查时可以返回 0，因此不能只看它的退出码判断环境健康。

| 提示 | 下一步 |
|---|---|
| `command not found: km` | 尚未配置 PATH；使用已设置绝对路径的 `"$KM"` |
| `KM_PROJECT_MISSING` | 回到项目目录；新项目先 `init` |
| `KM_RUNTIME_MISSING` / `KM_RUNTIME_OFFLINE` | 检查 Docker CLI、Docker Desktop 是否运行及引擎是否可达 |
| `KM_NOT_TTY` | 在真正的 Mac 终端直接运行 shell；脚本改用 `run --` |
| `KM_PROJECT_BUSY` | 同项目另一个 km 任务或 shell 正在运行；等待结束，或在原终端中断任务 / 退出 shell |
| `KM_SESSION_ACTIVE` | 容器仍有登记的旧会话；先核对任务，确认要结束后用 `km sessions` 查看会话，再 `km cancel <id>` 显式清理（id 会话完整复制），保持 Docker 引擎与项目一致 |
| `KM_SESSION_UNKNOWN` | 会话查询失败或结果异常；检查 Docker 与 doctor 输出，不要通过删除登记目录绕过检查 |
| `KM_IMAGE_DRIFT` / `KM_RUNTIME_MISMATCH` / `KM_CONTAINER_CONFLICT` | 镜像、引擎或容器身份变化；保留配置和状态，先核对变动原因 |
| `KM_TRANSACTION_PENDING` | 存在未完成的环境事务（可能由强杀中断）；按提示 `km env recover --dry-run` 查看方案后执行恢复 |
| `KM_NO_PREVIOUS` | 回退槽位为空：只有成功执行过 `km env switch` 才有上一代可回退 |
| `KM_PLATFORM_MISMATCH` | 目标镜像与当前容器实际平台不一致；本版本不支持跨架构切换 |

## 9. 当前版本的已知问题

上一轮 review 确认的两个问题已在本版修复并有回归覆盖：后台管道作业退出清理（含停止态、忽略 TERM 的作业，按 SID 域 TERM→KILL 清理）；外部 SIGTERM/SIGHUP 的终端恢复时序（快照改在启动客户端之前获取，恢复完成后才返回 143）。若终端回显异常，仍可输入 `stty sane` 后按回车。

仍然成立的行为约定：

- 宿主进程被强杀（SIGKILL/关终端）时，容器内的 shell 可能继续运行；后续命令会被 `KM_SESSION_ACTIVE` 保护性阻断——它不是应该删除的“缓存”。恢复入口：**`km sessions` 查看会话，`km cancel <id>` 显式取消**（`<id>` 从 sessions 输出完整复制；取消只作用于当前项目，查询成功但 ID 不在列表时会明确报错）。旧的 `docker exec <容器> /tmp/km-bin/km-ctl cancel <会话ID>` 保留为高级排障手段，普通恢复不再需要。
- 被 `km cancel` 外部取消的运行中任务，其客户端退出码为工具真实状态（TERM=143）；Ctrl-C 的 130 语义不变。
- 主动 `setsid … &` 脱离会话的进程不属于 km 的清理范围，会一直存活（可观测、可手动清理）。
- `km shell` 每次启动会向容器安装/刷新一次会话脚本，热调用比裸 `docker exec` 慢约 160ms（2026-09 基线：km p50 203–207ms vs exec 45ms，见 tests/perf/evidence/ 与 CHANGELOG）。
- `init` 后、首次 `run`/`shell` 之前运行 `doctor`，会话检查会显示一条预期中的警告「会话脚本未安装」（此时容器内还没有会话脚本可查）；首次 `run` 或 `shell` 之后该检查自动变为 OK。

## 10. 完成首次上手后

本版包含执行、交互 shell、状态/工具查询、会话恢复和环境生命周期管理。先把上述练习跑通，再在
真实项目使用；首次体验不需要清理 CURRENT 或手动删除 `.km/`。

- 发布说明：[v0.4.0-rc.1](releases/v0.4.0-rc.1.md)。
- 安装包入口与卸载：[安装包快速开始](package-quickstart.md)。卸载只删除 km 安装清单内文件，项目与容器仍保留。
- 后续小步工程计划：[下一阶段计划](next-iteration-plan.md)。
- 开发验证：[验证记录](verification.md)；命令细节：[CLI 合同](cli-contract.md)。

当前仅发布 macOS 二进制；Linux/amd64 是 CI 回归平台，Intel macOS 实机状态单独记录。

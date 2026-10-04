# km 0.4.0-rc.1 安装包快速开始

macOS + 已启动的本机 Docker Desktop。安装预编译包不需要 Go。
Apple Silicon 使用 darwin-arm64；Intel 使用 darwin-amd64（实机验收待补）。

## 1. 校验与安装

解包前，在下载目录核对压缩包 SHA256 与同版 `SHA256SUMS` 对应行一致。
下载了两份压缩包时可执行 `shasum -a 256 -c SHA256SUMS`；只下载一份时用
`shasum -a 256 <压缩包文件名>` 核对对应行。

进入解压目录，执行：

```bash
PREFIX="$HOME/.local" ./install.sh
export PATH="$HOME/.local/bin:$PATH"
KM="$HOME/.local/bin/km"
"$KM" version --verbose
docker info --format '{{.ServerVersion}}'
```

成功标志：版本为 `0.4.0-rc.1`、commit 与 Release 的源码 SHA 一致、worktree 为 clean。
安装程序只使用同目录的二进制；构建身份位于 `$PREFIX/share/km/BUILD-INFO`，
清单位于 `$PREFIX/share/km/manifest.txt`。保留解压目录以便卸载。

## 2. 准备镜像与练习项目

包内没有预构建镜像，但包含精选镜像 Dockerfile。先在解压目录构建（需联网；已有该镜像时可跳过）：

```bash
docker build -t kali-mac-min:0.2 images/kali
mkdir -p "$HOME/Workspace"
DEMO=$(mktemp -d "$HOME/Workspace/km 初次体验.XXXXXX")
cd "$DEMO"
"$KM" init --image kali-mac-min:0.2
"$KM" run -- python3 -c 'print("hello from Kali")'
"$KM" status
"$KM" tools
"$KM" env list
echo "$DEMO"
```

成功标志：输出 hello from Kali；status 为 running_idle；tools 中六项 AVAILABLE；
env list 有 CURRENT。记下项目目录，新终端回到此处并重新设置 KM 即可继续。

## 3. 文件共享与交互

```bash
printf 'print("hello from a Mac file")\n' > hello.py
"$KM" python3 hello.py
"$KM" python3 -c 'from pathlib import Path; Path("result.txt").write_text("made in Kali\n")'
cat result.txt
```

Mac 文件在容器执行，容器生成的结果在 Mac 可见。`/workspace` 的修改与删除直接作用于项目文件。

单独运行 `"$KM" shell`，出现 `KM_SHELL> ` 后再输入：

```bash
pwd
python3 hello.py
exit
```

exit 回到 Mac 后再运行：

```bash
"$KM" stop
"$KM" status
"$KM" python3 hello.py
"$KM" stop
```

停止后为 container_stopped；下次执行自动启动，项目文件保留。
`km <管理命令> --help` 查看帮助；异常退出使用 `km sessions` 查看后再显式 `km cancel <完整会话ID>`。
更换环境用 `km env switch --image <本地镜像> --dry-run` 先预览；环境回退不回退共享文件。

完整操作手册：https://github.com/L1ngSh1/KaliMac/blob/v0.4.0-rc.1/docs/user-guide.md

## 4. 卸载

回到解压目录：

```bash
PREFIX="$HOME/.local" ./uninstall.sh
```

只删除安装清单内文件；项目、配置、容器与镜像仍保留。结束日常使用只需 km stop。

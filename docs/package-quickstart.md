# km 安装包快速开始

## 安装

在解压目录执行：

```bash
PREFIX="$HOME/.local" ./install.sh
export PATH="$HOME/.local/bin:$PATH"
km version --verbose
km --help
```

安装程序只使用同目录的包内二进制。构建身份保存在
`$PREFIX/share/km/BUILD-INFO`，安装文件清单保存在
`$PREFIX/share/km/manifest.txt`。

## 准备镜像与初始化

安装包不内置已构建的容器镜像，但附带可审查的精选镜像 Dockerfile。先在
解压目录构建镜像，再初始化项目：

```bash
docker build -t kali-mac-min:0.2 images/kali
cd /path/to/project
km init --image kali-mac-min:0.2
km status
km tools
km run -- python3 --version
km shell
km stop
```

使用 `km <命令> --help` 查看管理命令说明。工具自己的帮助会原样透传，
例如 `km python3 --help`。异常中断后运行 `km sessions`，再按输出使用
`km cancel <完整会话ID>` 恢复。

## 卸载

仍在解压目录执行：

```bash
PREFIX="$HOME/.local" ./uninstall.sh
```

卸载只删除安装清单内文件，不删除用户项目。

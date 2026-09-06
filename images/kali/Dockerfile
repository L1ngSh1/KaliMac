# km 最小精选镜像（v0.2，非交互最小可用版）
#
# 基础镜像：Docker Hub 官方 kalilinux/kali-rolling（经本机可用镜像站拉取；
# 实际内容身份以构建后 `docker image inspect` 的 ID 与 RepoDigests 为准，
# 构建脚本会把证据写入 tests/evidence/kali-mac-min-*/，不虚构 digest）。
#
# 内容：会话执行内核依赖（procps/setsid/coreutils）+ 6 个候选工具的最小集合。
# 普通执行只复用本镜像，不临时 apt 安装。
FROM docker.1ms.run/kalilinux/kali-rolling:latest

# 包源：构建时显式使用 USTC 公开 Kali 镜像（默认 http.kali.org 经本机代理
# 间歇 502；此处仅配置本镜像自身的 sources，不触碰用户全局 Docker/apt 配置），
# 并记录于构建证据。基础镜像使用 deb822 格式的 kali.sources，直接改写该文件。
# Acquire::Retries 缓解瞬时网关错误。
RUN printf 'Types: deb\nURIs: http://mirrors.ustc.edu.cn/kali/\nSuites: kali-rolling\nComponents: main contrib non-free non-free-firmware\nSigned-By: /usr/share/keyrings/kali-archive-keyring.gpg\n' \
      > /etc/apt/sources.list.d/kali.sources \
 && apt-get -o Acquire::Retries=5 update \
 && apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
      procps \
      util-linux \
      coreutils \
      python3 \
      curl \
      jq \
      file \
      openssl \
      nmap \
 && rm -rf /var/lib/apt/lists/*

# km 会话脚本由 km 在执行前通过 docker cp 引导安装（/tmp/km-bin），
# 不打进镜像层：保证镜像只包含可审计的包集合。

# 本轮验证记录（P0＋P1，2026-09-06）

## 环境检查（只读）

| 项 | 值 |
|---|---|
| 平台 | macOS 26.6.2, arm64 |
| Go | go1.26.6 darwin/arm64 |
| Docker CLI | 29.6.1 (`/usr/local/bin/docker`) |
| 引擎 | Docker Desktop 29.6.1, linux/aarch64（本轮启动；启动前离线） |
| 当前 context | desktop-linux（本地 unix socket；DOCKER_HOST 未设置） |
| 网络 | Docker Hub 直连超时；镜像站 docker.1ms.run 可用；Kali apt 源可达 |

## Go 验证（PASS）

```text
$ gofmt -l .                 → 无输出（已格式化）
$ go vet ./...               → PASS，无告警
$ go test ./...              → ok  kalimac/internal/cli      (0.7s)
                               ok  kalimac/internal/project (cached)
                               ok  kalimac/internal/runtime (0.8s)
$ go build -o ./bin/km ./cmd/km → PASS
$ ./bin/km --version         → km 0.1.0-p1
$ ./bin/km --help            → 帮助文本（不触发 docker 调用，由单测 failIfRun 断言）
```

单测覆盖：复杂 argv（空/空格/中文/引号/前导 `-`）、`--help` 留给工具、`run --` 解析与重名、未实现命令显式报错、配置损坏（坏 JSON/未知版本/未知字段/缺 image/类型错）、状态损坏、doctor 八种场景（无 CLI/引擎离线/远程 endpoint/无项目/坏配置/坏状态/健康栈/标签冲突/容器缺失/镜像缺失）、doctor 只读性快照断言。

## 真实 Docker 验证

| 项 | 结果 |
|---|---|
| E1–E9 基础语义（argv/stdio/退出码/路径/挂载/持久） | PASS（证据见 docs/phase-0.md） |
| E10–E13 信号实验 | 完成并给出结论；非 PTY 与外部杀客户端场景容器内残留（这正是要验证的风险） |
| E14 stop 兜底 | PASS（10.1s 宽限期观察记录） |
| K1 Kali 基线 | PASS（无预装工具；apt 源可达） |
| km doctor 真实集成（临时项目+真实容器+标签/挂载核验） | PASS：8 通过 0 警告 0 失败 |
| T01 无 Docker 时 help/version | 真实停机场景 SKIPPED（不停止用户运行中的 Docker Desktop）；由单测 failIfRun 保证代码路径不触碰 Docker |

mock/fake 与真实容器证据分开报告，未混用。

## 资源清理

本轮创建的容器（km-p0-20260906a-c1、km-p0-20260906a-k1、km-p1doctor2026）与 /tmp 临时项目在轮末删除并核对（见汇报）。保留：`docker.1ms.run/kalilinux/kali-rolling` 与 busybox 两个镜像（P2 直接复用，避免重新下载）；未执行任何全局 prune；用户既有容器与数据未触碰。

## 遗留风险

1. 信号清理的 P2 方案未选定（三个候选见 phase-0.md），这是 P2 主要工作量。
2. `docker stop` 10s 宽限期问题需容器主进程可捕获 SIGTERM 才能消除。
3. 无 Linux CI；macOS 为唯一验证平台（符合 v0.1 范围）。
4. PTY 下窗口尺寸变化（T22）未测，留 P2 shell 实现。

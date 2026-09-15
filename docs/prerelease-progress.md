# 预发布验收进度（prerelease）

- 计划来源：用户《KaliMac 下一阶段计划》（2026-09-16 会话）
- 基线：`d7f74c9`（goal-1789353851 的 7 个提交，全部本地未推送）
- 证据目录：`tests/evidence/prerelease-c1/`（阶段一）
- 执行约定：push / 正式安装 / 打 tag / 发布均为独立确认步骤；
  验收记录标注「通过 / 失败 / 未执行」，不沿用历史结果。

## 阶段一：远端 CI 首跑验收 — 进行中

### 预检（通过）

| 项 | 结果 |
| --- | --- |
| 远端 | `origin = https://github.com/L1ngSh1/KaliMac.git` 已配置 |
| go.mod vs workflow | go.mod 要求 `go 1.25`，workflow `1.25.x`，匹配，无需调整 |
| 国内镜像站引用（CI 触达范围） | 4 处属实：Dockerfile `FROM`、Dockerfile apt 源（USTC）、`harness_test.go` toolImage、`matrix_test.go` 漂移注入 busybox（fresh runner 上会 `t.Fatal`） |
| 不受 CI 影响 | `tests/p0/*` 两处（已参数化、CI 不跑 P0）；docs 历史记录 |

### 批次 1：镜像引用可覆盖化（默认值不变，本地行为零变化）

- `images/kali/Dockerfile`：`ARG KALI_BASE`（默认 1ms）、`ARG KALI_APT_MIRROR`（默认 USTC），
  RUN 用 ARG 拼源；CI 用官方 `kalilinux/kali-rolling:latest` + `http://kali.download/kali/`。
- `harness_test.go`：`toolImage` 改 var，读 `KM_TEST_TOOL_IMAGE`（默认现值）。
- `matrix_test.go`：漂移镜像改读 `KM_TEST_DRIFT_IMAGE`（默认现值）。
- `ci.yml` integration 作业：预拉官方 kali-rolling + busybox；build 带 CI 参数；
  test step 设两个环境变量指官方引用。

本地验证：

- [x] gofmt 空、vet（含 integration tag）零告警、单测全绿、workflow YAML 解析通过
- [x] Docker 引擎启动（本轮 `open -a Docker`）
- [x] 官方镜像本地拉取：busybox:stable 直连成功；kali-rolling 直连首次 EOF、重试成功
      （`official-pull.txt`）
- [x] CI 同款参数构建模拟：https CDN 在**本机代理**下 apt update 即失败（3 次）；
      http 变体走到 51 包中的 50 个后遇瞬时 502——**ARG 管道已验证**（构建确实使用
      指定源），官方 CDN 端到端受本地代理限制未完成，留待 CI 首跑验证
      （`ci-sim-build.txt`）
- [x] Dockerfile 修改后默认参数构建端到端成功（`kali-mac-min:default-sim`，验证后已删；
      **未触碰 `kali-mac-min:0.2`**，避免用户真实项目镜像身份漂移）
- [x] **CI 同款环境变量本地彩排**：`KM_TEST_TOOL_IMAGE=kalilinux/kali-rolling:latest
      KM_TEST_DRIFT_IMAGE=busybox:stable` 全套件 PASS 133.2s、零 SKIP、零残留
      （`suite-cienv-local.txt`）
- [x] 批次已提交：`b4ada0a`（Dockerfile/harness/matrix/ci.yml/进度文档/证据）

### 待办（阶段一剩余）

- [ ] 推送（**独立确认步骤：已询问用户，未获回答，未推送**）。
      **推送范围如实披露**：本地领先 `origin/master`（`3de51b6`，P2-C 验收证据轮）
      共 **18 个提交**——review 加固轮 5 个、审计轮 4 个、goal-1789353851 共 7 个、
      阶段一批次 2 个。首次 CI 将基于这批完整历史运行。
- [ ] 首次 CI 观察：verify + integration 两作业结果、SKIP 行扫描、
      P2B-CLEANUP 标记、失败可定位性
- [ ] 完成标准核对：必需检查全过、无意外跳过、日志可定位、真实集成后零残留
- [ ] CI 运行证据目录回填并提交（本轮套件运行产生的
      `tests/evidence/p2c-shellproto-1789491527/` 一并入库）

## 阶段二：安装包与新用户路径验收 — 未开始

前置改动计划：`scripts/install.sh` 支持从 dist 产物安装（KM_BIN 覆盖，模式同 PERF_KM_BIN）。

## 阶段三：真实项目试用 — 未开始

## 阶段四：确定下一轮主题 — 未开始

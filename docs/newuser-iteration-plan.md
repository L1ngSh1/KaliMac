# KaliMac 新用户闭环与状态可信性迭代（夜间任务书·执行版）

> 状态：执行中。任务书来源：用户 2026-09-17 凌晨建议。约束：不加 GUI/多项目/插件、
> 不自动装工具、不自动取消/删除/重建用户容器、不自动推送/打 tag/发布、不宣称 amd64
> 实机验收。08:00 后不加功能。候选版本：**0.4.0-p3 滚动吸收本轮变更**（候选未发布，
> 不做版本号churn）。

## S0 基线核对（全部属实）

| 事实 | 位置 |
| --- | --- |
| `km init` 不接受任何参数 | internal/cli/init.go:55 |
| 默认镜像 = `kalilinux/kali-rolling:latest`（非精选） | internal/project/config.go:33 |
| 配置声明 platform（新配置显式写 linux/arm64；LoadConfig 对缺失字段隐式补默认），但 CreateContainer 的 `docker run` 不传 `--platform` | config.go:89-91、docker.go:291-303、init.go:108 |
| package.sh 版本取自可能过期的 `bin/km --version` | scripts/package.sh:11 |

## 包 A 合同（冻结）：初始化参数与配置兑现

### 参数与优先级

| 场景 | 行为 |
| --- | --- |
| 新项目（无 .km.json）+ 无参数 | 配置写入 `DefaultImage` + **host-native 平台**（`linux/<GOARCH>`，km 二进制为本机构建）；创建/拉取均携带该平台 |
| 新项目 + `--image <ref>` | 配置写入该 ref；ref 前缀 `kali-mac-min:` → 结果消息标「精选工具镜像」，否则附「可能不含精选工具集」提示 |
| 新项目 + `--platform <p>` | 配置写入该值并兑现到创建/拉取 |
| 已有配置 + `--image` 相同 | 放行（幂等） |
| 已有配置 + `--image` 不同 | `KM_CONFIG_INVALID` 冲突退出 1：不覆盖配置、不重建容器；指引「编辑 .km.json 后重试」 |
| 已有配置**未声明** platform（旧配置）+ `--platform` | 一律 `KM_CONFIG_INVALID`：声明缺失时不接受参数声明（防伪冒/防意外重建）；指引显式编辑 .km.json |
| 已有配置**已声明** platform + `--platform` 相同/不同 | 相同放行；不同冲突报错（同上，不静默改配置） |
| 已有配置（无参数） | 现状幂等语义不变 |

### 兑现与兼容

- `CreateContainer` 增加 `Platform`：非空时 `docker run --platform <p>`；**空 = 不传**（native）。
- `PullImage` 增加 platform 参数：声明平台时 `docker pull --platform <p> <ref>`。
- **旧配置兼容**：LoadConfig 新增 `PlatformDeclared`（未序列化）——字段缺失时不隐式补默认参与
  创建（保持 native 历史行为），`Platform` 字段仍填充默认值供展示（doctor 行为不变）。
  已存在的旧项目：复用/重建均 native，与容器当初创建方式一致。
- init 结果消息（三种结局：全新/复用/接管）统一扩展：镜像 ref、是否精选、声明平台
  （未声明 → `平台=未声明(本机架构)`）。

### 验收清单

新项目参数写入/无参默认、`--image` 精选与非精选消息、已有配置 image 冲突、已有配置
platform 声明缺失拒绝 `--platform`、声明冲突与一致、旧配置复用 argv 无 `--platform`、
新配置创建 argv 含 `--platform`、声明平台缺失镜像时 pull argv 含 `--platform`、镜像
拉取失败环境保留（既有回归）。

## 包 B 合同（冻结）：`km status` 与 `--json`

状态枚举（`state` 字段，机器可比较）与退出码：

| state | 判定 | exit |
| --- | --- | --- |
| `uninitialized` | wd 向上无 .km.json（家目录硬边界同 FindConfig） | 0 |
| `config_incomplete` | 有 .km.json、无 .km/state.json | 0 |
| `container_missing` | 记录容器 ID 不存在（被外部删除） | 0（提示 km init 恢复） |
| `container_stopped` | 容器存在且 state != running | 0 |
| `running_idle` | running 且无 ACTIVE 会话（STALE 单列说明） | 0 |
| `running_session_active` | 有 ACTIVE 会话（**中性表述**，不称异常遗留——可能是正在使用的 shell） | 0 |
| `identity_conflict` | 引擎漂移/容器身份不符/同名重建 | 1 |
| `unknown` | 查询失败、输出不可解析、配置损坏 | 1（error{code,msg} 必填） |

- 只读：不启动容器、不引导脚本、不清扫、不写任何文件（集成前后对照验证）。
- 人类输出与 JSON 由同一内部结构渲染（一致性由构造保证）。JSON 顶层：
  `{"schema_version":1,"state":…,"project":{root,image,platform,platform_declared,container_id}|null, "container":{"state","name"}|null,"sessions":{"active":[],"stale":[]}|null,"advice":…,"error":{code,msg}|null}`。
- 退出码：0=状态判定完成（含前五种）；1=unknown/identity_conflict；2=用法（不接受
  位置参数，仅 `--json`）。
- 「查询失败不显示为无会话」「活跃会话中性表述」为硬性要求。

## 包 C 合同（冻结）：交付一致性

- 版本来源改为 `go run ./cmd/km --version`（当前源码，与 bin/km 新旧无关）。
- metadata 增加 `worktree: clean|dirty(<n> paths)`（git status --porcelain 计数），
  避免 HEAD SHA 暗示产物来自干净提交。
- 任意 cwd 可调用（REPO 自定位，回归断言）。
- 验收：伪造过期 bin/km（输出假版本）→ 打包仍标注当前源码版本与正确产物名。

## 执行结果（2026-09-17 收口）

| 工作包 | 状态 | 关键产物/证据 |
| --- | --- | --- |
| 包 A 初始化参数与配置兑现 | **passed** | `--image`/`--platform` 落盘+兑现（create/pull argv 断言）；已有配置冲突不覆盖不重建；旧配置 native 兼容；结果消息含镜像/精选/平台（init_flags_test.go 10 项） |
| 包 B km status/--json | **passed** | 8 状态枚举全覆盖+只读性（.km 逐字节）+人类/JSON 一致+查询失败不显示为无会话（status_test.go 8 项） |
| 包 C 交付一致性 | **passed** | 版本源=当前源码（伪造旧 bin/km 验证不受影响）；worktree 脏记录；任意 cwd 可调用；CI 增打包一致性冒烟步 |
| 独立审查 agent | **FAIL→已修复** | P1×3（status 同名重建分类、PullImage 实参反序、复用/接管消息零断言）+P2×6 全部处置 |
| 验证 agent | **FAIL→已修复** | F2 证据分层定稿采纳；空转测试与伪覆盖逐项补强 |
| 安装版实走 | **passed+发现修复** | 安装版全链通过；**新发现并修复**：脚本缺失时 docker exec 实际返回 126（非 127），status/sessions/doctor 三处分类已修，新 init 后首个 status 现为 running_idle+说明 |
| 全量回归 | passed | gofmt/vet（含 integration tag）/单测/race/build 全绿；集成 147.6s 零 SKIP 零残留 |

夜间过程自纠：两次测试资源泄漏（失败用例先于登记、验收脚本清理顺序）均被最终
残留核对发现并按完整 ID 清理，防护（登记位置前移）已固化进测试。

未验证/待审：amd64 实机验收（持续保留）；推送、tag、正式发布待用户指令。

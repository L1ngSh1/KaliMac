# ADR：项目环境切换与单代回退的事务设计（冻结稿）

> 状态：**已冻结**（P0 产出）。本文是 `docs/environment-switch-plan.md` 的设计合同，
> 实现不得偏离；如需变更须先修订本文并在 progress 文档记录。
> 基线：分支 `codex/environment-switch`，基于 `1d3eaab`（含 tool-discovery 全部工作）。

## 1. 命令合同（冻结）

```bash
km env switch --image <ref> [--dry-run] [--yes]
km env rollback [--dry-run] [--yes]
km env recover [--dry-run] [--yes]
```

- `env` 为管理命令：加入 CLI 分派与 `managementCommands`；重名工具仍可用
  `km run -- env ...` 调用（长形式带 `--` 分隔符，绕过管理分派）。
- `switch` 必须且只能提供一个非空 `--image`；重复、缺失、空值、未知参数 → 退出码 2。
- `rollback`/`recover` 不接受位置参数与 `--image`；未知参数 → 退出码 2。
- `--dry-run`：只读预检与影响预览。不取项目锁、不写任何文件（含锁/状态/事务/日志）、
  不创建/启动/停止/删除容器、不安装容器内脚本、不拉取镜像、不询问确认。
  会话状态在 dry-run 中标注为"执行时核验"。允许执行的预检 → 退出码 0；
  拒绝 → 退出码 1 并给出稳定错误分类与下一步。
- 确认：交互 stdin（terminal）默认展示影响并要求键入 `y`/`yes`（大小写不敏感）确认；
  其他输入视为拒绝。非交互调用必须显式 `--yes`，否则在变更前返回退出码 2。
  **冻结：用户拒绝确认 → 退出码 1，`KM_CANCELED`，明确输出"未执行任何变更"。**
- `--yes` 只替代确认交互，不绕过任何门禁（会话、身份、平台、漂移、事务互斥）。
- 预览不是授权令牌：真实执行在取锁后重新核验全部条件。

### 返回值表（冻结）

| 情况 | 退出码 | 稳定分类 |
| --- | --- | --- |
| 预检通过 / 操作成功 / no-op / recover 无需恢复 | 0 | — |
| 镜像缺失（本地不存在，不拉取） | 1 | `KM_NOT_FOUND` |
| 平台冲突 | 1 | `KM_PLATFORM_MISMATCH`（新增） |
| 身份冲突（容器/名称/标签/挂载） | 1 | `KM_CONTAINER_CONFLICT` |
| 会话活跃 / 会话状态未知 | 1 | `KM_SESSION_ACTIVE` / `KM_SESSION_UNKNOWN` |
| 无上一代可回退 | 1 | `KM_NO_PREVIOUS`（新增） |
| 存在未完成事务（switch/rollback 阻断；run/init/stop 同样阻断） | 1 | `KM_TRANSACTION_PENDING`（新增） |
| 参数错误 / 非交互缺确认 | 2 | `KM_USAGE` |
| 用户拒绝确认 | 1 | `KM_CANCELED` |
| 取消、超时、引擎故障 | 1 | 沿用既有 `KM_CANCELED`/`KM_TIMEOUT`/`KM_RUNTIME_OFFLINE` 等 |

新增错误码集中定义在 `internal/runtime/errors.go`（与既有 19 码同处），
并登记进 `docs/cli-contract.md` 错误标识节：`KM_TRANSACTION_PENDING`、
`KM_NO_PREVIOUS`、`KM_PLATFORM_MISMATCH`。

## 2. 数据布局与 schema（冻结）

```text
.km/
  state.json                 state_version 1|2；v2 增加 env 块
  env/
    transaction.json         未完成事务（存在即意味着待恢复）
    previous.json            最近一次成功 switch 的上一代快照（回退槽位）
    retained.json            有意保留容器的资源账本（只增不删，除人工清理）
```

- 三个 env 记录文件各自携带 `env_version`（当前 1）。未知版本：拒绝读、拒绝写、
  拒绝执行任何 env 命令，原始文件保持原样（`KM_STATE_INVALID`）。
- 所有写入沿用 temp file + rename 原子写。多文件写入**不是**原子事务；
  一致性由事务记录 + 恢复规则保证（见 §4）。
- 状态内容不含凭据、不记录用户环境变量、不复制项目文件。

### state.json v2（`.km.json` 不变）

```json
"env": { "env_version": 1, "generation": <int >= 0> }
```

- generation 0 = 原始代（init 创建）；第一次 switch 成功后当前代为 1，此后单调递增。
- 仅 env 功能首次落地时把 state_version 从 1 升到 2；**此后该项目恒为 v2**（见 §3）。
- v1 项目继续被本构建完全支持（不批量迁移）；`env switch` 的 PREPARED 步
  把它升级为 v2（见 §4 写入顺序）。

### transaction.json（`env_version: 1`）

```json
{
  "env_version": 1,
  "op_id": "e<16hex>",
  "kind": "switch|rollback",
  "stage": "PREPARED|CANDIDATE_CREATED|CANDIDATE_VERIFIED|OLD_STOPPED|COMMIT_INTENT|CURRENT_COMMITTED",
  "old":   { "container_id", "container_name", "image_id", "was_running", "generation" },
  "new":   { "container_id", "container_name", "image_id", "was_running", "generation" },
  "config_backup": { "sha256", "data_base64" },
  "state_backup":  { "sha256", "data_base64" },
  "probes": [ { "container_id", "removed" } ],
  "target_image_ref": "（switch）用户引用",
  "prev_image_ref":   "（rollback 目标代的配置引用）",
  "created_at": "...", "updated_at": "..."
}
```

`old/new` 两份快照在 PREPARED 时即完整落盘；恢复只依赖 transaction.json，
不依赖 previous.json（previous.json 是提交后的工件）。

### previous.json（`env_version: 1`）

```json
{
  "env_version": 1,
  "generation", "container_id", "container_name", "image_id",
  "image_ref": "切换前 .km.json 的 image 字段",
  "was_running", "op_id", "switched_at"
}
```

### retained.json（`env_version: 1`）

```json
{
  "env_version": 1,
  "containers": [
    { "container_id", "container_name", "image_id", "generation",
      "reason": "superseded-by-switch|rolled-back|probe-cleanup-failed",
      "op_id", "retained_at" }
  ]
}
```

按 `container_id` 去重追加；本功能不提供删除接口，文档给出核验完整 ID 后
`docker rm` 的人工清理方法。

## 3. 旧二进制兼容策略（冻结，设计门槛）

- **机制**：state_version 升到 2。旧构建的 `LoadState` 只接受 1，遇 2 直接
  `StateError`（KM_STATE_INVALID 通道）——旧二进制对该项目的所有命令都会
  在接触 Docker 之前明确拒绝，不会误用中间态。
- **升版时机**：`switch`/`rollback` 的 PREPARED 步（首个 Docker 变更之前）。
  写入顺序冻结为：先写 transaction.json，再升 state.json。任一时刻崩溃：
  - (transaction 已写, state 仍 v1)：新二进制按 pending 阻断 run/init/stop；
    旧二进制可运行，但此刻尚未发生任何容器变更，无危害。
  - (state 已 v2)：旧二进制整体拒绝该项目。
  即**任意崩溃窗口内，旧二进制要么无害、要么被拒**。
- **采纳后不降版**：rollback 完成后仍保持 v2（retained/previous 记录属于
  新语义）。最低支持版本：具备 schema 2 能力的 km（即本构建起）。
  旧二进制看到的错误为 `.km/state.json: 不支持的状态版本 2`。
- 证据：用基线提交 `1d3eaab` 构建的旧二进制实测 `km status`/`km stop` 拒绝
  v2 状态并留档（不依赖 Docker；见 tests/evidence/environment-switch/）。

## 4. 状态转换、写入点与恢复规则（冻结）

```text
PREPARED → CANDIDATE_CREATED → CANDIDATE_VERIFIED → OLD_STOPPED
         → COMMIT_INTENT → CURRENT_COMMITTED →（移除 transaction.json）成功
```

**收敛方向（唯一裁决规则）**：stage < COMMIT_INTENT → 收敛到**前态**
（原环境）；stage ≥ COMMIT_INTENT → 收敛到**新态**。recover 不做其他选择。

各阶段失败/强杀后的恢复（recover 幂等、可重复调用）：

| stage | 崩溃时 Docker 侧可能的实际状态 | recover 动作 |
| --- | --- | --- |
| PREPARED | 无本事务资源（防御性按 km.op 标签复核） | 无资源则恢复文件（state 备份回写、移除 transaction.json）；发现 op 标签资源则按 CANDIDATE_* 处理 |
| CANDIDATE_CREATED / CANDIDATE_VERIFIED | 候选容器存在（可能未被记录 ID）；旧容器未动 | 按 km.op 标签发现并核验候选（名称/项目标签/镜像内容/挂载一致才动它）→ 删除候选；旧容器按 was_running 校验；恢复文件；移除 transaction.json |
| OLD_STOPPED | 旧容器已停止；候选存在 | 删除候选 → 恢复旧容器运行状态（was_running=true 则 start）；启动失败 → **保留事务**并输出诊断（文件不回写），可重复 recover 重试；成功 → 恢复文件、移除 transaction.json |
| COMMIT_INTENT | 旧容器停止；候选存在；文件可能已改/未改（任意中间组合）；回退槽位可能仍是更早一代（轮换未完成） | 收敛新态：核验候选身份 → 幂等地写 config.image + state v2（事务内快照为准，先核对外部编辑）→ 写 previous.json（旧槽位先完成轮换转入 retained）→ FINALIZE |
| CURRENT_COMMITTED | 文件已一致指向新代 | 核验候选运行、上一代停止且与 previous.json 一致 → FINALIZE |

- 恢复中任何**资源身份无法确认**（候选/旧容器消失、镜像内容不符、挂载变化、
  按标签发现多个候选）→ 停止写操作，保留事务，输出完整容器 ID、阶段与
  原/目标关系，供人工核对；**不把查询错误当资源不存在**。
- **外部编辑检测**：提交写 config/state 前重读文件并与 `config_backup`/
  `state_backup` 哈希比对；不一致 → 拒绝覆盖，保留事务，输出冲突诊断。
- FINALIZE（两类事务共用）：核验不变量（§6）→ 移除 transaction.json → 成功。
  FINALIZE 本身幂等（无 transaction.json 即无事可做）。

### switch 提交序列（写点冻结）

1. CANDIDATE_VERIFIED 后，停止旧容器；成功后写 stage=OLD_STOPPED。
2. 目标引用复验：`ImageID(target_ref)` 必须仍等于已验证内容 ID；漂移 → 拒绝提交
   并按提交点前规则收敛回前态（不重打标签、不静默跟随新内容）。
3. 外部编辑检测：config/state 与事务备份哈希（或语义等价）不符 → 拒绝提交并
   收敛回前态，外部修改保持原样（写点处的比对保留为最后防线）。
4. **写 stage=COMMIT_INTENT（不可回退点）**。
5. 幂等写 previous.json（旧代快照 + 切换前 config.image 引用；旧槽位先转入 retained）。
6. 重读并哈希比对 `.km.json` → 原子写（仅 image 字段变更，name/platform/
   schema_version/platform_declared 语义保持）。
7. 同样比对后原子写 state.json（v2；container 指向候选，generation=旧+1）。
8. 写 stage=CURRENT_COMMITTED。
9. FINALIZE。

### rollback 提交序列（写点冻结）

1. 核验 previous.json 与两个容器实态后，停止当前代容器；写 OLD_STOPPED。
2. 上一代引用复验（同上：漂移拒绝并收敛回前态）。
3. 外部编辑检测（同上，不符拒绝并收敛回前态）。
4. **写 stage=COMMIT_INTENT**。
5. 启动上一代容器（仅当其 was_running=true）。
6. 哈希比对后原子写 `.km.json`（image 恢复为 prev_image_ref）。
7. 哈希比对后原子写 state.json（container 指向上一代，generation=prev.generation）。
8. 当前代容器按 container_id 追加进 retained.json（reason=rolled-back）；
   移除 previous.json（回退槽位已消费）。
9. 写 stage=CURRENT_COMMITTED；FINALIZE。

## 5. 门禁与锁（冻结）

顺序（真实执行；预检为同一集合的只读子集，不取锁）：

1. `loadProjectStack`：config + state 可读且 schema 受支持（v1 允许，switch 升版）。
2. `resolveEngine`：拒绝远程 endpoint；`verifyProjectEngine`：与 state 记录
   endpoint 一致；此后整个事务用同一 `*runtime.Docker`（EndpointOverride 固定，
   不再重新解析）。
3. `AcquireLock`（项目锁，覆盖整个事务；与 run/init/stop 互斥；状态查询不取锁）。
   取锁后**重新核验** 4–9（预览不作数）。
4. 无未完成事务（transaction.json 可解析且不存在）→ 否则 KM_TRANSACTION_PENDING。
   `run`/`init`/`stop` 在取锁后同样检查 pending 并阻断；`sessions`/`cancel`
   保持只读/定向，不受事务锁影响（显式取消能力不回退）。
5. 当前容器归属核验（完整 ID、名称、km.project 标签、/workspace 挂载源）。
6. 当前容器状态 ∈ {running, exited}；paused/restarting/dead/created → 拒绝
   （KM_CONTAINER_CONFLICT，说明性消息），不自动 unpause。
7. 会话门禁：running → 先 Bootstrap 安装 km-ctl（与 `km run` 同一幂等动作）后查询
   ——**缺脚本不直接证明无任务，必须安装后核实**（ACTIVE → KM_SESSION_ACTIVE；
   查询失败/不可解析 → KM_SESSION_UNKNOWN；STALE 清扫）。Bootstrap 是拒绝场景
   唯一允许的残留（惰性脚本文件，不产生进程/代际/挂载变化，已记录为合同例外）。
   **exited 容器内无进程，不可能存在活跃 km 会话，免查询**（记入预览说明）。
   dry-run 与确认前预览不安装、不查询（零副作用）；会话核验只发生在用户确认后的
   执行路径与停止旧容器前的复验。
8. switch：目标镜像本地存在（`docker image inspect` 实际元数据；不拉取）→
   `ImageOSArch` 得目标实际平台；当前容器实际镜像（`res.Image`）的平台必须与
   目标一致 → 否则 KM_PLATFORM_MISMATCH。rollback：previous 记录完整、两容器
   均实存、身份/平台/镜像内容与记录一致，且 `prev_image_ref` 本地仍解析到
   `prev.image_id`（缺失或漂移 → 拒绝，提示恢复正确引用；不静默改写配置）。
9. retained/previous 可解析且归属一致；switch 的回退槽位轮换影响在预览列明。

- **switch 与镜像漂移门禁的关系（冻结）**：`verifyStackForMutation` 的
  cfg.Image↔state 漂移检查对 switch 放行——switch 本身就是漂移的唯一显式
  出口；容器实际镜像（res.Image↔state.container.image_id）仍校验。
  rollback/其他命令不放松。`run.go` 的"重新 init"提示文案同步更新为
  "km env switch 或重新 init"。
- no-op（冻结）：目标内容 ID == 当前容器实际镜像内容 ID 且**全部门禁（含槽位/
  retained 可解析与归属）通过**——no-op 判定后置于门禁，损坏记录下不得 exit 0
  → 退出码 0，明确"目标与当前一致，未创建新代、未消费回退槽位"，
  不触碰任何容器与文件。不同标签指向同一内容 ID 同样 no-op（按内容判定）。
- 候选创建的平台参数：仅当 `.km.json` 显式声明 platform 时传 `--platform`
  （沿用 init 语义；未声明不兑现隐式值）。

## 6. 候选、探测与不变量（冻结）

- **只读探测容器**：`km-probe-<op_id>`，来自目标镜像，工作区以
  **`:ro`** 挂载，`sh` 执行有界（10s）依赖检查（km 会话内核所需命令存在性，
  严格解析，失败→UNKNOWN→拒绝切换；不强制精选六项工具）。探测容器用后
  立即删除；删除失败 → 记入 retained（reason=probe-cleanup-failed），
  不阻塞提交但计入账本。探测**不**执行项目自定义脚本、不扫描/修改项目文件。
- **正式候选容器**：`km-<pid>-g<N>`，标签 `km.project`、`km.owner=km`、
  `km.schema=1`、`km.op=<op_id>`、`km.gen=<N>`；`/workspace` 正式读写挂载；
  `--init`、`sleep infinity`；创建即运行。候选只做 inspect 级验证
  （完整 ID/名称/标签/挂载/镜像内容），**不**在候选内 exec。
  两类资源都写入 transaction（probes 数组 / new 快照）。
- **不变量（验收与 FINALIZE 共同依据）**：
  1. 账本恒等式：实际项目容器集合 = 当前代 + 上一代（若有）+ retained 清单
     + 处于未完成事务中的资源；额外出现即泄漏。
  2. transaction.json 存在 ⇒ state_version=2 且 run/init/stop 阻断。
  3. 提交后：config.image == new 代用户引用；state.container 与实存容器
     完整一致；previous.json 与旧容器一致且该容器处于停止状态。
  4. rollback 后：previous.json 不存在；当前代容器 = 原上一代；
     被撤下的容器在 retained 中。
  5. 任何成功路径结束时不存在本事务的 probe 容器（已删除或已入账本）。
  6. 项目根、项目 ID、平台声明、endpoint 记录在整个事务中不变。

## 7. 回退记录轮换（冻结）

- switch 成功：旧容器进 previous.json；若已有 previous.json → 其容器按
  container_id 转入 retained（reason=superseded-by-switch），预览必须列明。
- rollback 成功：previous.json 移除（消费）；被撤容器入 retained
  （reason=rolled-back）。
- 再次 rollback：无 previous.json → KM_NO_PREVIOUS，退出码 1，无任何变更。
  不在两代之间意外来回。
- retained 只记账不承诺接口。

## 8. 显示与帮助（冻结）

- `km status`：显示当前代（generation）、是否有未完成事务（pending 指向
  `km env recover`）、上一代是否存在；中间态不得渲染为正常。
- `km doctor`：项目节显示 generation 与 env 记录健康（无法解析/未知版本 →
  失败项）；账本与实际容器集合比对结果计入诊断。
- `km --help` 增加 env 行；`km help env` 输出子命令说明；
  `docs/cli-contract.md` 增加 env 行与新增错误码。
- 预览与成功输出固定包含一句："环境回退不会撤销新环境运行期间对项目文件的
  改动；容器可写层不迁移。"

## 9. 测试与证据（冻结）

- fake/单测层：每个持久化写点与 Docker 操作边界注入失败；覆盖 §4 恢复表全部
  stage（含 CANDIDATE_VERIFIED、rollback PREPARED、带槽位的 COMMIT_INTENT）；
  "操作成功但响应丢失"（创建后未收到 ID → 按 km.op 标签发现）；外部编辑冲突
  （提交前拒绝 + recover 侧拒绝/恢复后收敛）；I/O 故障（阶段写点只读目录 →
  前态收敛；SavePrevious 写点 → 事务保留、权限恢复后幂等收敛；损坏 JSON/
  未知版本）；拒绝场景断言 Docker 调用日志只读、无文件变更；init 被事务阻断、
  sessions 不被阻断。
- 真实集成层（Docker 可用时；不可用记 NOT-RUN）：两个本地构建的确定性镜像
  （内容差异用固定文件/命令证明，不依赖外网）；A→B→rollback→rollback、
  no-op、文件边界、可写层边界、残留账本恒等式核验。
- 旧二进制实测：用基线提交 `1d3eaab` 构建旧 km，对 v2 状态项目执行
  `km status`，留档拒绝输出（无需 Docker）。
- 证据目录：`tests/evidence/environment-switch/<run-id>/`。

## 10. 环境资源清理（km env remove）合同（冻结，v1 2026-10-01）

> 计划原文：docs/environment-inventory-plan.md。本节冻结其实现合同；
> list 为只读独立交付点，remove 为不可逆定向删除。

### 10.1 旧二进制门禁（§6.1 的可验证方案）

- **remove 事务复用 `transaction.json`，`kind="remove"`**。旧构建的
  `LoadTransaction` 只接受 kind ∈ {switch, rollback}，遇 remove 在 kind 校验处
  fail-closed：run/init/stop（pending 检查）、switch/rollback、recover 全部以
  KM_STATE_INVALID 拒绝，不会忽略清理操作并行变更，也不会把删除事务按切换事务
  解释。无 schema 升版；v1/v2 项目的既有行为不变；**list 不要求升级本机状态**。
- 证据：用前序基线（c9f297b）构建的旧二进制对 kind=remove 事务夹具实测
  `status/run` 拒绝（与 v2 状态实测同法，留档 evidence）。

### 10.2 记录布局与阶段

- 事务字段复用：`old`/`new` 快照均填目标容器快照；新增
  `ledger_backup`（前态 retained.json 字节+哈希）、`protected`（PREPARED 时
  保护集合摘要）、`remove_reason`（retained 条目原因）。旧二进制因 kind 拒绝，
  未知字段无影响。
- 阶段（写入点冻结）：
  `PREPARED → REMOVE_REQUESTED → ABSENCE_CONFIRMED → LEDGER_UPDATED →（清除事务）`
  | 阶段 | 动作 | 中断后 |
  | --- | --- | --- |
  | PREPARED | 持久化目标快照、保护集合、账本备份、操作 ID | 未发 rm；重核验 |
  | REMOVE_REQUESTED | rm 调用前写意图，再发普通 rm | inspect 原完整 ID 定实态；不合格不重试 |
  | ABSENCE_CONFIRMED | 目标明确不存在 | 只做账本收尾，不恢复容器 |
  | LEDGER_UPDATED | 原子移除目标条目（其他条目原样保留） | 核验后清除事务 |
- **恢复语义（与 switch/rollback 的本质区别）**：永不重建容器。收敛 = 完成核验
  与账本收尾；目标仍在且全部删除资格成立时，可在显式 recover 确认后重试普通
  rm；资格丧失（running/身份/保护变化）→ 停止写操作、保留事务、输出诊断。
- **账本更新为外科手术式**：解析当前 retained.json（不可解析/未知版本拒绝），
  仅移除目标条目，其余条目（含外部编辑结果）原样保留；绝不回写备份快照。
  `ledger_backup` 用于诊断与"已被外部编辑"提示，不作为覆盖源。

### 10.3 删除资格（全部满足才可删）

1. 记录可解析（config/state/previous/retained，schema 受支持）。
2. endpoint 固定且与项目记录一致。
3. 目标为**本项目 retained 的精确完整 ID**（64 hex；不接受名称/短 ID/多目标），
   且与 CURRENT/PREVIOUS/事务引用集合无重叠（即使同时错误出现在 retained 中，
   保护优先）。
4. 扩展 inspect 身份全匹配：完整 ID、名称、km.project 标签、镜像内容、
   /workspace 挂载源；`probe-cleanup-failed` 条目须额外核验 km-probe-* 命名与
   **只读挂载**，核验不了即拒绝（不放宽为仅标签匹配）。
5. 实际状态 = exited；running/paused/restarting/created/dead 一律拒绝。
6. 无未完成事务（含删除操作本身）。

### 10.4 Docker 删除方式

- 新增 `runtime.RemoveContainerGraceful`：`docker rm <完整ID>`，**无 -f、无 -v**；
  既有带 `-f` 的 `RemoveContainer` 仅限 init 回滚/探测清理，不用于本功能。
- rm 返回 0 仍须 inspect 确认不存在；错误或响应丢失 → 重新核验实态：
  明确不存在 → 收尾；仍存在且资格成立 → 事务保留于 REMOVE_REQUESTED（内联不
  重试，由显式 recover 重试）；仍存在但资格丧失或未知 → 停止写操作并诊断。
- 外部进程在检查后重启目标 → 普通 rm 失败，不升级为强制删除。

### 10.5 错误码与退出码

- 不新增错误码：保护/身份/状态拒绝 → `KM_CONTAINER_CONFLICT`（消息含具体原因）；
  未知目标（不在本项目 retained 且无待收尾记录）→ `KM_NOT_FOUND`；其余复用
  KM_TRANSACTION_PENDING / KM_STATE_INVALID / KM_RESOURCE_UNKNOWN / KM_TIMEOUT。
- 退出码遵循计划 §5 表：list 缺失条目 0；list 冲突/损坏/待恢复 1；remove
  用法错误 2；拒绝确认 = KM_CANCELED 1。

### 10.6 list 只读性（冻结）

- 不取锁、不写任何文件、不安装脚本、不清扫；枚举用 `--no-trunc` 完整 ID 后逐个
  inspect；角色 CURRENT/PREVIOUS/RETAINED/TRANSACTION/UNTRACKED；显示态
  MISSING/UNKNOWN/CONFLICT 与实际状态分开；MISSING 不触发任何清理。
- exit 0 仅当：记录可解析、无未完成事务、无角色冲突且查询完整；否则 exit 1
  并展示可确认部分。

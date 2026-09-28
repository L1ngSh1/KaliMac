package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runEnvSwitch implements `km env switch --image <ref> [--dry-run] [--yes]`.
// 合同（冻结）：docs/adr-environment-transactions.md §1/§4/§5。
func runEnvSwitch(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	f, perr := parseEnvFlags(rest, true)
	if perr != nil {
		return usageError(stderr, "km env switch: %v", perr)
	}
	if f.image == "" {
		return usageError(stderr, "km env switch 需要 --image <镜像引用>")
	}
	if !requireInteractiveConsent(f, stderr) {
		return ExitUsage
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	root, cfg, st, err := loadProjectStack(wd)
	if err != nil {
		return envError(stderr, err)
	}

	plan, err := planSwitch(ctx, dk, root, cfg, st, f.image, false, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if f.dryRun {
		renderSwitchPreview(stdout, plan)
		return ExitOK
	}

	lock, err := project.AcquireLock(root)
	if err != nil {
		return envError(stderr, err) // KM_PROJECT_BUSY
	}
	defer lock.Release()
	if lock.BrokeStale() {
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）；这不代表容器内任务已结束\n")
	}

	// 取锁后重读配置与状态（独立审查 P1-1：状态派生事实必须基于锁内新鲜读取；
	// 并发完成的 switch 会把状态升到 v2，旧对象会覆盖它）——预览不是授权令牌
	cfg, st, err = reloadProjectFiles(root)
	if err != nil {
		return envError(stderr, err)
	}
	plan, err = planSwitch(ctx, dk, root, cfg, st, f.image, false, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if plan.noOp {
		fmt.Fprintf(stdout, "km env switch: 目标镜像内容与当前一致（%s），no-op：未创建新代、未消费回退槽位。\n", shortID(plan.targetID))
		return ExitOK
	}
	renderSwitchPreview(stdout, plan)
	if confirmed, code := confirmEnvAction(stdout, stderr, f.yes); !confirmed {
		return code
	}

	// 确认后第三次核验（关键条件变化 → 放弃执行）
	recheck, err := planSwitch(ctx, dk, root, cfg, st, f.image, true, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if !switchPlanStillValid(plan, recheck) {
		return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "确认后环境条件已变化（当前容器/目标镜像/回退槽位），未执行任何变更；请重新运行预览"})
	}

	if err := executeSwitch(ctx, dk, root, recheck, stderr); err != nil {
		return envError(stderr, err)
	}
	fmt.Fprintf(stdout, "km env switch: 已切换到第 %s（容器 %s，镜像内容 %s）；上一代容器 %s 已停止并保留。\n",
		generationDisplay(recheck.nextGen), recheck.candName, shortID(recheck.targetID), recheck.res.Name)
	fmt.Fprintf(stdout, "%s\n", boundaryNote)
	fmt.Fprintf(stdout, "回退：km env rollback --dry-run 预览后执行。\n")
	return ExitOK
}

// switchPlanStillValid 比较确认前后的两次规划：当前容器、目标内容、no-op
// 判定与回退槽位必须完全一致，否则放弃执行。
func switchPlanStillValid(a, b *switchPlan) bool {
	if a.res.ID != b.res.ID || a.targetID != b.targetID || a.noOp != b.noOp {
		return false
	}
	if (a.prev == nil) != (b.prev == nil) {
		return false
	}
	if a.prev != nil && b.prev != nil && a.prev.ContainerID != b.prev.ContainerID {
		return false
	}
	return true
}

// switchPlan 是一次 switch 的完整只读规划：门禁结果 + 影响预览 + 执行参数。
type switchPlan struct {
	root       string
	cfg        *project.Config
	st         *project.State
	ep         runtime.EndpointInfo
	res        runtime.InspectResult
	targetRef  string
	targetID   string
	targetPlat string
	curPlat    string
	curGen     int
	nextGen    int
	candName   string
	probeName  string
	prev       *envtxn.Previous
	noOp       bool
}

// planSwitch 执行 switch 的全部只读门禁并收集影响。sessionCheck=false 时
// 跳过会话门禁（dry-run/确认前预览零副作用，不安装脚本）；真实执行在确认后
// 以 sessionCheck=true 重跑本函数。
func planSwitch(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State, targetRef string, sessionCheck bool, stderr io.Writer) (*switchPlan, error) {
	// 未完成事务阻断（switch/rollback 共用）
	txn, err := loadOpenTransaction(root)
	if err != nil {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: ".km/env/transaction.json 无法解析，无法确认项目是否处于事务中间态", Err: err}
	}
	if txn != nil {
		return nil, errTxnPending(txn)
	}
	ep, res, err := verifyStackForQuery(ctx, dk, root, st)
	if err != nil {
		return nil, err
	}
	// 当前容器状态门禁：仅支持 running/exited；其余状态独立拒绝，不自动恢复
	switch res.State {
	case "running", "exited":
	default:
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("当前容器处于不支持的状态 %q（仅支持 running/exited；不自动 unpause/强制处理）；请先 km doctor 复核", res.State)}
	}
	curGen := 0
	if st.Env != nil {
		curGen = st.Env.Generation
	}
	p := &switchPlan{
		root: root, cfg: cfg, st: st, ep: ep, res: res,
		targetRef: targetRef, curGen: curGen, nextGen: curGen + 1,
		candName:  fmt.Sprintf("km-%s-g%d", st.ProjectID, curGen+1),
		probeName: "", // op id 在执行期生成；预览用固定前缀说明
	}

	// 目标镜像必须已在本地；使用实际元数据确认内容与平台，不拉取
	targetID, ok, err := dk.ImageID(ctx, targetRef)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &runtime.Error{Code: runtime.CodeNotFound,
			Msg: fmt.Sprintf("镜像 %s 不在本地引擎；km env switch 不拉取镜像，请先自行准备（docker pull/build）后重试", targetRef)}
	}
	p.targetID = targetID
	targetPlat, err := dk.ImageOSArch(ctx, targetID)
	if err != nil {
		return nil, err
	}
	p.targetPlat = targetPlat
	curPlat, err := dk.ImageOSArch(ctx, res.Image)
	if err != nil {
		return nil, err
	}
	p.curPlat = curPlat
	if targetPlat != curPlat {
		return nil, &runtime.Error{Code: runtime.CodePlatformMismatch,
			Msg: fmt.Sprintf("目标镜像平台 %s 与当前容器实际平台 %s 不一致；本版本只支持同平台切换（不跨架构迁移）", targetPlat, curPlat)}
	}

	// 会话门禁（sessionCheck 控制是否安装脚本并查询）；exited 容器免查询
	if err := envSessionGate(ctx, ep, res, sessionCheck, stderr); err != nil {
		return nil, err
	}

	// 容器实际镜像内容与本机状态记录一致（ADR §5：switch 不放松此校验）
	if st.Container.ImageID != "" && res.Image != st.Container.ImageID {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("容器实际镜像 %s… 与本机状态记录 %s… 不一致；请 km doctor 复核", shortID(res.Image), shortID(st.Container.ImageID))}
	}

	// 回退槽位与 retained 账本现状（ADR §5 门禁 9：可解析且归属一致；
	// 轮换影响需在预览列明）
	prev, err := envtxn.LoadPrevious(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "回退槽位记录无法解析；不覆盖、不猜测", Err: err}
	}
	if prev != nil {
		prevRes, exists, err := dk.InspectContainer(ctx, prev.ContainerID)
		if err != nil {
			return nil, err
		}
		if !exists || prevRes.Name != prev.ContainerName || prevRes.ProjectID != st.ProjectID {
			return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: fmt.Sprintf("回退槽位记录的上一代容器（%s，ID %s）已不存在或身份不符，资源账本不一致；请人工核验后清理 .km/env/ 记录再切换", prev.ContainerName, shortID(prev.ContainerID))}
		}
	}
	p.prev = prev
	if _, rerr := envtxn.LoadRetained(root); rerr != nil && !os.IsNotExist(rerr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "retained 账本无法解析；不覆盖、不猜测", Err: rerr}
	}

	// no-op 判定后置于全部门禁（ADR §5：目标内容一致**且其余条件全部通过**）
	if targetID == res.Image {
		p.noOp = true
	}
	return p, nil
}

func renderSwitchPreview(w io.Writer, p *switchPlan) {
	fmt.Fprintf(w, "km env switch 预览（只读，未做任何变更）\n")
	fmt.Fprintf(w, "  项目根: %s\n", p.root)
	fmt.Fprintf(w, "  项目 ID: %s\n", p.st.ProjectID)
	fmt.Fprintf(w, "  引擎: %s（来源 %s）\n", p.ep.Endpoint, p.ep.Source)
	fmt.Fprintf(w, "  当前代: %s（容器 %s，ID %s，状态 %s，镜像内容 %s，平台 %s）\n",
		generationDisplay(p.curGen), p.res.Name, shortID(p.res.ID), p.res.State, shortID(p.res.Image), p.curPlat)
	fmt.Fprintf(w, "  目标镜像: %s（内容 %s，平台 %s）\n", p.targetRef, shortID(p.targetID), p.targetPlat)
	fmt.Fprintf(w, "  计划:\n")
	fmt.Fprintf(w, "    - 会话门禁: 执行时在当前容器安装 km-ctl 并核验（ACTIVE/未知将拒绝；dry-run 不安装不查询）\n")
	fmt.Fprintf(w, "    - 创建只读探测容器（验证会话运行依赖后删除；不执行项目脚本，工作区只读挂载）\n")
	fmt.Fprintf(w, "    - 新建候选容器 %s（内容 %s，成为第 %s 并保持运行）\n", p.candName, shortID(p.targetID), generationDisplay(p.nextGen))
	fmt.Fprintf(w, "    - 停止当前容器 %s 并保留（停止会结束其中非 km 管理的进程）\n", p.res.Name)
	if p.prev != nil {
		fmt.Fprintf(w, "    - 回退槽位轮换：现有上一代（第 %s，容器 %s，ID %s）转入 retained 清单\n",
			generationDisplay(p.prev.Generation), p.prev.ContainerName, shortID(p.prev.ContainerID))
	} else {
		fmt.Fprintf(w, "    - 回退槽位: 空 → 切换后可用 km env rollback 回退上一代\n")
	}
	fmt.Fprintf(w, "  边界: %s\n", boundaryNote)
	fmt.Fprintf(w, "  结果: 允许执行\n")
}

// executeSwitch 执行切换事务。PREPARED 起每一步失败都会尝试收敛回前态；
// COMMIT_INTENT 之后的失败保留事务并指向 recover（ADR §4）。
func executeSwitch(ctx context.Context, dk *runtime.Docker, root string, plan *switchPlan, stderr io.Writer) error {
	opID, err := envtxn.NewOpID()
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "生成操作 ID 失败", Err: err}
	}
	plan.probeName = "km-probe-" + opID
	cfgRaw, err := os.ReadFile(filepath.Join(root, project.ConfigFileName))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "读取配置失败", Err: err}
	}
	stRaw, err := os.ReadFile(project.StatePath(root))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "读取状态失败", Err: err}
	}
	txn := &envtxn.Transaction{
		OpID: opID, Kind: envtxn.KindSwitch, Stage: envtxn.StagePrepared,
		Old: envtxn.Snapshot{
			ContainerID: plan.res.ID, ContainerName: plan.res.Name,
			ImageID: plan.res.Image, WasRunning: plan.res.State == "running",
			Generation: plan.curGen,
		},
		New: envtxn.Snapshot{
			ContainerName: plan.candName, ImageID: plan.targetID,
			WasRunning: true, Generation: plan.nextGen,
		},
		ConfigBackup:   envtxn.NewFileBackup(cfgRaw),
		StateBackup:    envtxn.NewFileBackup(stRaw),
		TargetImageRef: plan.targetRef,
	}

	stage := envtxn.StagePrepared
	fail := func(err error) error {
		if envtxn.AtOrAfterCommitIntent(stage) {
			return withRecoverHint(err)
		}
		if cerr := convergeSwitchPreCommit(ctx, dk, root, txn, plan.st, stderr); cerr != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: "清理未完成，事务保留；运行 km env recover --dry-run 查看",
				Err: errors.Join(err, cerr)}
		}
		return err
	}

	// PREPARED：先事务后升版（ADR §3 冻结顺序）
	if err := envtxn.SaveTransaction(root, txn); err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "事务写入失败；未做任何变更", Err: err}
	}
	if err := bumpStateToEnv(root, plan.st, plan.curGen); err != nil {
		return fail(&runtime.Error{Code: runtime.CodeStateInvalid, Msg: "状态升版失败", Err: err})
	}

	// 只读探测（先探测后建候选，ADR §6）
	probeID, err := createProbeContainer(ctx, dk, root, plan, txn, stderr)
	if err != nil {
		return fail(err)
	}
	txn.Probes = append(txn.Probes, envtxn.ProbeRecord{ContainerID: probeID})
	if err := envtxn.SaveTransaction(root, txn); err != nil {
		return fail(err)
	}
	ctl := newSessionController(plan.ep.Endpoint)
	probeCmd := append([]string{"/bin/sh", "-c", envProbeSh, "sh"}, envProbeDeps...)
	pOut, pErrStr, pExit, perr := ctl.ExecCapture(ctx, probeID, probeCmd)
	if perr != nil {
		return fail(&runtime.Error{Code: runtime.CodeRuntimeMissing, Msg: "目标镜像探测失败（引擎故障或超时）", Err: perr})
	}
	if pExit != 0 {
		return fail(&runtime.Error{Code: runtime.CodeRuntimeMissing,
			Msg: fmt.Sprintf("目标镜像缺少可用的 /bin/sh 或探测异常（exit=%d, stderr: %s）", pExit, relTrim(pErrStr))})
	}
	missing, ok := parseEnvProbe(pOut, envProbeDeps)
	if !ok {
		return fail(&runtime.Error{Code: runtime.CodeToolsProtocol,
			Msg: fmt.Sprintf("依赖探测输出协议异常（%q）", relTrim(pOut))})
	}
	if len(missing) > 0 {
		return fail(&runtime.Error{Code: runtime.CodeRuntimeMissing,
			Msg: fmt.Sprintf("目标镜像缺少 km 会话运行依赖: %v；请更换镜像或补齐后重试", missing)})
	}
	removeProbeContainer(ctx, dk, root, txn, stderr)

	// 候选创建（唯一代际名 + 本轮标签；创建结果未知时按 op 标签核验，不重复创建）
	stage = envtxn.StageCandidateCreated
	candID, err := createCandidateContainer(ctx, dk, root, plan, txn, stderr)
	if err != nil {
		return fail(err)
	}
	txn.New.ContainerID = candID
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 候选身份核验（inspect 级；不在候选内执行任何命令）
	stage = envtxn.StageCandidateVerified
	if err := verifyCandidate(ctx, dk, root, plan, txn); err != nil {
		return fail(err)
	}
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 停止旧容器前的会话复验（冻结：立即重查）
	if err := envSessionGate(ctx, plan.ep, plan.res, true, stderr); err != nil {
		return fail(err)
	}

	// OLD_STOPPED
	stage = envtxn.StageOldStopped
	if err := dk.StopContainer(ctx, plan.res.ID); err != nil {
		return fail(err)
	}
	oldRes, exists, err := dk.InspectContainer(ctx, plan.res.ID)
	if err != nil {
		return fail(err)
	}
	if !exists || oldRes.State != "exited" {
		return fail(&runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "旧容器停止后状态未确认（exited）"})
	}
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 提交前复验（ADR §6 冻结）：目标引用必须仍指向已验证的内容 ID；
	// 漂移则拒绝提交并收敛回前态——不重打标签、不静默跟随新内容。
	if curID, ok, err := dk.ImageID(ctx, plan.targetRef); err != nil {
		return fail(err)
	} else if !ok || curID != plan.targetID {
		return fail(&runtime.Error{Code: runtime.CodeImageDrift,
			Msg: fmt.Sprintf("目标引用 %s 在切换过程中漂移（当前内容 %s，已验证 %s）；拒绝提交，正在恢复原环境", plan.targetRef, shortID(curID), shortID(plan.targetID))})
	}

	// 提交前外部编辑检测（提前到不可回退点之前：失败可完整收敛前态，
	// 不覆盖外部修改；写点处的哈希比对保留为最后防线）
	if err := verifyFilesUnchangedSincePrepared(root, txn); err != nil {
		return fail(err)
	}

	// COMMIT_INTENT：不可回退点
	stage = envtxn.StageCommitIntent
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 提交：previous（含轮换）→ config → state
	if err := rotatePrevious(ctx, dk, root, plan, txn); err != nil {
		return fail(err)
	}
	if err := writeSwitchCommitFiles(root, txn); err != nil {
		return fail(err)
	}
	stage = envtxn.StageCurrentCommitted
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// FINALIZE：不变量核验后清除事务
	if err := finalizeSwitch(ctx, dk, root, txn, stderr); err != nil {
		return withRecoverHint(err)
	}
	return nil
}

// verifyFilesUnchangedSincePrepared 在不可回退点之前检测事务外的文件修改。
// 与备份哈希一致，或语义等价（我们自己的 v2 升版 / 等价改写）即放行；
// 否则拒绝——不覆盖外部修改。
func verifyFilesUnchangedSincePrepared(root string, txn *envtxn.Transaction) error {
	raw, err := os.ReadFile(filepath.Join(root, project.ConfigFileName))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交前读取配置失败", Err: err}
	}
	if !txn.ConfigBackup.Matches(raw) {
		cur, perr := project.LoadConfig(filepath.Join(root, project.ConfigFileName))
		var backup project.Config
		bBytes, berr := txn.ConfigBackup.Bytes()
		if perr != nil || berr != nil || json.Unmarshal(bBytes, &backup) != nil ||
			cur.SchemaVersion != backup.SchemaVersion || cur.Name != backup.Name ||
			cur.Image != backup.Image || cur.Platform != backup.Platform {
			return &runtime.Error{Code: runtime.CodeConfigInvalid,
				Msg: "检测到事务外的 .km.json 修改（与事务备份不一致），拒绝提交；正在恢复原环境，外部修改保持原样"}
		}
	}
	stRaw, err := os.ReadFile(project.StatePath(root))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交前读取状态失败", Err: err}
	}
	if !txn.StateBackup.Matches(stRaw) {
		cur, perr := project.LoadState(root)
		var backup project.State
		bBytes, berr := txn.StateBackup.Bytes()
		if perr != nil || berr != nil || json.Unmarshal(bBytes, &backup) != nil || !stateSemanticallyEqual(cur, &backup) {
			return &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: "检测到事务外的 .km/state.json 修改（与事务备份不一致），拒绝提交；正在恢复原环境，外部修改保持原样"}
		}
	}
	return nil
}

func setStage(root string, txn *envtxn.Transaction, stage string) error {
	txn.Stage = stage
	return envtxn.SaveTransaction(root, txn)
}

// bumpStateToEnv 在首个容器变更前把状态升为 v2（采纳 env 功能）。v1→v2
// 携带当前代号；已是 v2 则保持（generation 由提交步更新）。
func bumpStateToEnv(root string, st *project.State, generation int) error {
	if st.StateVersion == project.SupportedStateVersionEnv && st.Env != nil {
		return nil
	}
	up := *st
	up.StateVersion = project.SupportedStateVersionEnv
	up.Env = &project.EnvState{EnvVersion: project.EnvRecordVersion, Generation: generation}
	return project.SaveState(root, &up)
}

// recoverCreateByOpLabel 在创建命令失败（可能 daemon 已创建但客户端未收到
// 结果）后按精确 op 标签核验；要求名称、归属标签、挂载与镜像内容全部匹配，
// 且绝不发起第二次创建。
func recoverCreateByOpLabel(ctx context.Context, dk *runtime.Docker, root string, st *project.State, txn *envtxn.Transaction, name, imageID string) (string, bool, error) {
	sums, err := dk.FindContainersByLabel(ctx, runtime.OpLabel, txn.OpID)
	if err != nil {
		return "", false, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("容器创建结果未知（name=%s），且按 op 标签核验失败；不重复创建，可运行 km env recover 核实", name), Err: err}
	}
	var found []string
	for _, s := range sums {
		if s.Name == name {
			found = append(found, s.ID)
		}
	}
	if len(found) == 0 {
		return "", false, nil
	}
	if len(found) > 1 {
		return "", false, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("按 op 标签发现 %d 个同名候选，身份歧义，不接管", len(found))}
	}
	res, exists, err := dk.InspectContainer(ctx, found[0])
	if err != nil {
		return "", false, err
	}
	if !exists || res.Name != name || res.ProjectID != st.ProjectID ||
		filepath.Clean(res.MountSource) != filepath.Clean(root) || res.Image != imageID {
		return "", false, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("创建失败后按 op 标签发现资源但身份不匹配（name=%s image=%s），不接管", name, shortID(res.Image))}
	}
	return res.ID, true, nil
}

func createProbeContainer(ctx context.Context, dk *runtime.Docker, root string, plan *switchPlan, txn *envtxn.Transaction, stderr io.Writer) (string, error) {
	id, err := dk.CreateContainer(ctx, runtime.ContainerCreateOpts{
		Name: plan.probeName, ProjectID: plan.st.ProjectID, Image: plan.targetID,
		ProjectDir: root, WorkspaceReadOnly: true,
		ExtraLabels: []string{
			runtime.OpLabel + "=" + txn.OpID,
			runtime.RoleLabel + "=probe",
		},
	})
	if err == nil {
		return id, nil
	}
	recID, recovered, rerr := recoverCreateByOpLabel(ctx, dk, root, plan.st, txn, plan.probeName, plan.targetID)
	if rerr != nil {
		return "", rerr
	}
	if !recovered {
		return "", &runtime.Error{Code: runtime.CodeRuntimeOffline,
			Msg: fmt.Sprintf("探测容器创建失败（name=%s）", plan.probeName), Err: err}
	}
	fmt.Fprintf(stderr, "km: 探测容器创建命令失败后核实到已创建资源，正在登记（%s）…\n", shortID(recID))
	return recID, nil
}

func createCandidateContainer(ctx context.Context, dk *runtime.Docker, root string, plan *switchPlan, txn *envtxn.Transaction, stderr io.Writer) (string, error) {
	id, err := dk.CreateContainer(ctx, runtime.ContainerCreateOpts{
		Name: plan.candName, ProjectID: plan.st.ProjectID, Image: plan.targetID,
		ProjectDir: root, Platform: createPlatform(plan.cfg),
		ExtraLabels: []string{
			runtime.OpLabel + "=" + txn.OpID,
			runtime.GenLabel + "=" + strconv.Itoa(plan.nextGen),
			runtime.RoleLabel + "=candidate",
		},
	})
	if err == nil {
		return id, nil
	}
	recID, recovered, rerr := recoverCreateByOpLabel(ctx, dk, root, plan.st, txn, plan.candName, plan.targetID)
	if rerr != nil {
		return "", rerr
	}
	if !recovered {
		return "", &runtime.Error{Code: runtime.CodeRuntimeOffline,
			Msg: fmt.Sprintf("候选容器创建失败（name=%s）；原环境未受影响", plan.candName), Err: err}
	}
	fmt.Fprintf(stderr, "km: 候选容器创建命令失败后核实到已创建资源，正在登记（%s）…\n", shortID(recID))
	return recID, nil
}

// verifyCandidate 对正式候选做 inspect 级身份验证：名称、归属标签、挂载与
// 镜像内容必须与计划一致（不能只验证另一个探测容器）。
func verifyCandidate(ctx context.Context, dk *runtime.Docker, root string, plan *switchPlan, txn *envtxn.Transaction) error {
	res, exists, err := dk.InspectContainer(ctx, txn.New.ContainerID)
	if err != nil {
		return err
	}
	if !exists {
		return &runtime.Error{Code: runtime.CodeNotFound,
			Msg: fmt.Sprintf("候选容器（ID %s）创建后消失；原环境未受影响", shortID(txn.New.ContainerID))}
	}
	if res.Name != plan.candName || res.ProjectID != plan.st.ProjectID ||
		filepath.Clean(res.MountSource) != filepath.Clean(root) || res.Image != plan.targetID {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("候选容器身份不符（name=%s project=%s mount=%s image=%s）；原环境未受影响",
				res.Name, res.ProjectID, res.MountSource, shortID(res.Image))}
	}
	return nil
}

// removeProbeContainer 删除探测容器；删除失败计入 retained 账本
// （probe-cleanup-failed），不阻塞提交。
func removeProbeContainer(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, stderr io.Writer) {
	for i := range txn.Probes {
		p := &txn.Probes[i]
		if p.Removed {
			continue
		}
		if err := dk.RemoveContainer(ctx, p.ContainerID); err != nil && !runtime.IsNotFound(err) {
			fmt.Fprintf(stderr, "km: 探测容器 %s 删除未确认（%v）；计入 retained 账本\n", shortID(p.ContainerID), err)
			if _, aerr := envtxn.AppendRetained(root, envtxn.RetainedEntry{
				ContainerID: p.ContainerID, ContainerName: "km-probe-" + txn.OpID,
				Reason: envtxn.ReasonProbeCleanupFailed, OpID: txn.OpID,
			}); aerr != nil {
				fmt.Fprintf(stderr, "km: retained 账本写入失败（%v）；请人工记录探测容器 %s\n", aerr, p.ContainerID)
			}
			continue
		}
		p.Removed = true
	}
	_ = envtxn.SaveTransaction(root, txn)
}

// rotatePrevious 在提交段写入回退槽位：已有槽位先转入 retained（轮换），
// 再写入本次 switch 的上一代快照。
func rotatePrevious(ctx context.Context, dk *runtime.Docker, root string, plan *switchPlan, txn *envtxn.Transaction) error {
	if envtxn.PreviousExists(root) {
		oldPrev, err := envtxn.LoadPrevious(root)
		if err != nil {
			return err
		}
		res, exists, err := dk.InspectContainer(ctx, oldPrev.ContainerID)
		if err != nil {
			return err
		}
		if !exists || res.Name != oldPrev.ContainerName || res.ProjectID != plan.st.ProjectID {
			return &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: fmt.Sprintf("回退槽位中的上一代容器（%s，ID %s）已不存在或身份不符，账本不一致；请人工核验后清理 .km/env/ 记录", oldPrev.ContainerName, shortID(oldPrev.ContainerID))}
		}
		if _, err := envtxn.AppendRetained(root, envtxn.RetainedEntry{
			ContainerID: oldPrev.ContainerID, ContainerName: oldPrev.ContainerName,
			ImageID: oldPrev.ImageID, Generation: oldPrev.Generation,
			Reason: envtxn.ReasonSupersededBySwitch, OpID: txn.OpID,
		}); err != nil {
			return err
		}
	}
	return envtxn.SavePrevious(root, &envtxn.Previous{
		Generation:    txn.Old.Generation,
		ContainerID:   txn.Old.ContainerID,
		ContainerName: txn.Old.ContainerName,
		ImageID:       txn.Old.ImageID,
		ImageRef:      plan.cfg.Image,
		WasRunning:    txn.Old.WasRunning,
		OpID:          txn.OpID,
		SwitchedAt:    nowUTC(),
	})
}

// writeSwitchCommitFiles 提交段写入 config 与 state：写前重读并比对事务
// 备份，检测事务外的外部修改；已应用的写入幂等跳过。
func writeSwitchCommitFiles(root string, txn *envtxn.Transaction) error {
	cfgPath := filepath.Join(root, project.ConfigFileName)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时读取配置失败", Err: err}
	}
	cfg, err := project.LoadConfig(cfgPath)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时配置无法解析", Err: err}
	}
	switch {
	case cfg.Image == txn.TargetImageRef:
		// 已应用（幂等恢复）
	case txn.ConfigBackup.Matches(raw):
		newCfg := *cfg
		newCfg.Image = txn.TargetImageRef
		if err := project.WriteConfig(cfgPath, &newCfg); err != nil {
			return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时写入配置失败", Err: err}
		}
	default:
		return &runtime.Error{Code: runtime.CodeConfigInvalid,
			Msg: fmt.Sprintf("检测到事务外的 .km.json 修改（与事务备份不一致），不覆盖外部修改；请人工核对后运行 km env recover")}
	}

	stPath := project.StatePath(root)
	stRaw, err := os.ReadFile(stPath)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交时读取状态失败", Err: err}
	}
	cur, err := project.LoadState(root)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交时状态无法解析", Err: err}
	}
	if cur.StateVersion == project.SupportedStateVersionEnv && cur.Env != nil &&
		cur.Container.ID == txn.New.ContainerID {
		return nil // 已应用
	}
	if !txn.StateBackup.Matches(stRaw) {
		// 允许“我们自己的 PREPARED 升版”：语义等价于备份即视为未修改；
		// 其余差异都是事务外的外部修改，拒绝覆盖。
		backupBytes, berr := txn.StateBackup.Bytes()
		var backup project.State
		if berr != nil || json.Unmarshal(backupBytes, &backup) != nil || !stateSemanticallyEqual(cur, &backup) {
			return &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: "检测到事务外的 .km/state.json 修改（与事务备份不一致），不覆盖外部修改；请人工核对后运行 km env recover"}
		}
	}
	newSt := *cur
	newSt.StateVersion = project.SupportedStateVersionEnv
	newSt.Env = &project.EnvState{EnvVersion: project.EnvRecordVersion, Generation: txn.New.Generation}
	newSt.Container.ID = txn.New.ContainerID
	newSt.Container.Name = txn.New.ContainerName
	newSt.Container.ImageID = txn.New.ImageID
	if err := project.SaveState(root, &newSt); err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交时写入状态失败", Err: err}
	}
	return nil
}

// finalizeSwitch 验证最终不变量（候选运行、上一代停止、槽位一致）后清除
// 事务；失败保留事务并指向 recover。
func finalizeSwitch(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, stderr io.Writer) error {
	res, exists, err := dk.InspectContainer(ctx, txn.New.ContainerID)
	if err != nil {
		return err
	}
	if !exists || res.Name != txn.New.ContainerName || res.Image != txn.New.ImageID {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("最终核验失败：当前代容器（%s）不存在或身份不符", shortID(txn.New.ContainerID))}
	}
	if res.State != "running" {
		if err := dk.StartContainer(ctx, txn.New.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: "最终核验：当前代容器启动失败", Err: err}
		}
	}
	oldRes, exists, err := dk.InspectContainer(ctx, txn.Old.ContainerID)
	if err != nil {
		return err
	}
	if !exists {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("最终核验失败：上一代容器（%s）不存在，回退槽位将失效；请人工核验", shortID(txn.Old.ContainerID))}
	}
	if oldRes.State == "running" {
		fmt.Fprintf(stderr, "km: 上一代容器意外处于运行状态，正在停止（保持“上一代停止保留”不变量）…\n")
		if err := dk.StopContainer(ctx, txn.Old.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "上一代容器停止失败", Err: err}
		}
	}
	prev, err := envtxn.LoadPrevious(root)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "最终核验失败：回退槽位不可读", Err: err}
	}
	if prev.ContainerID != txn.Old.ContainerID {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "最终核验失败：回退槽位与事务记录不一致"}
	}
	return envtxn.ClearTransaction(root)
}

func relTrim(s string) string {
	out := s
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return relTrimSpace(out)
}

func relTrimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

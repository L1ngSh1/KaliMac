package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runEnvRollback implements `km env rollback [--dry-run] [--yes]`：
// 单代回退，只能使用项目自己的已验证上一代记录；成功后消费回退槽位，
// 被撤下的容器进入 retained 账本（ADR §4/§7）。
func runEnvRollback(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	f, perr := parseEnvFlags(rest, false)
	if perr != nil {
		return usageError(stderr, "km env rollback: %v", perr)
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

	plan, err := planRollback(ctx, dk, root, cfg, st, false, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if f.dryRun {
		renderRollbackPreview(stdout, plan)
		return ExitOK
	}

	lock, err := project.AcquireLock(root)
	if err != nil {
		return envError(stderr, err)
	}
	defer lock.Release()
	if lock.BrokeStale() {
		fmt.Fprintf(stderr, "km: 清理了遗留锁（持有人进程已退出）；这不代表容器内任务已结束\n")
	}

	// 取锁后重读配置与状态（独立审查 P1-1：锁内新鲜读取）
	cfg, st, err = reloadProjectFiles(root)
	if err != nil {
		return envError(stderr, err)
	}
	plan, err = planRollback(ctx, dk, root, cfg, st, false, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	renderRollbackPreview(stdout, plan)
	if confirmed, code := confirmEnvAction(stdout, stderr, f.yes); !confirmed {
		return code
	}
	cfg, st, err = reloadProjectFiles(root)
	if err != nil {
		return envError(stderr, err)
	}
	recheck, err := planRollback(ctx, dk, root, cfg, st, true, stderr)
	if err != nil {
		return envError(stderr, err)
	}
	if !rollbackPlanStillValid(plan, recheck) {
		return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "确认后环境条件已变化（当前容器/上一代容器/槽位），未执行任何变更；请重新运行预览"})
	}

	if err := executeRollback(ctx, dk, root, recheck, stderr); err != nil {
		return envError(stderr, err)
	}
	fmt.Fprintf(stdout, "km env rollback: 已回退到第 %s（容器 %s，镜像引用 %s）；被撤下的容器 %s 已停止并计入 retained 清单。\n",
		generationDisplay(recheck.prev.Generation), recheck.prev.ContainerName, recheck.prev.ImageRef, recheck.res.Name)
	fmt.Fprintf(stdout, "%s\n", boundaryNote)
	fmt.Fprintf(stdout, "回退槽位已消费；再次 rollback 将提示无可回退记录。\n")
	return ExitOK
}

func rollbackPlanStillValid(a, b *rollbackPlan) bool {
	return a.res.ID == b.res.ID && a.prev.ContainerID == b.prev.ContainerID &&
		a.prev.ImageID == b.prev.ImageID
}

type rollbackPlan struct {
	root     string
	cfg      *project.Config
	st       *project.State
	ep       runtime.EndpointInfo
	res      runtime.InspectResult
	prev     *envtxn.Previous
	prevRes  runtime.InspectResult
	curPlat  string
	prevPlat string
}

// planRollback 执行 rollback 的全部只读门禁并收集影响（ADR §5.8/§8）。
func planRollback(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State, sessionCheck bool, stderr io.Writer) (*rollbackPlan, error) {
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
	// 当前容器状态门禁：仅支持 running/exited；其余状态独立拒绝
	switch res.State {
	case "running", "exited":
	default:
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("当前容器处于不支持的状态 %q（仅支持 running/exited；不自动 unpause/强制处理）；请先 km doctor 复核", res.State)}
	}
	// 容器实际镜像内容与本机状态记录一致（ADR §5：rollback 不放松此校验）
	if st.Container.ImageID != "" && res.Image != st.Container.ImageID {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("容器实际镜像 %s… 与本机状态记录 %s… 不一致；请 km doctor 复核", shortID(res.Image), shortID(st.Container.ImageID))}
	}
	// retained 账本可解析（ADR §5 门禁 9；提交段要向它追加，损坏必须提前拒绝）
	if _, rerr := envtxn.LoadRetained(root); rerr != nil && !os.IsNotExist(rerr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "retained 账本无法解析；不覆盖、不猜测", Err: rerr}
	}
	p := &rollbackPlan{root: root, cfg: cfg, st: st, ep: ep, res: res}

	// rollback 要求项目已采纳 env 功能（previous.json 存在必然经历过 v2 升版）
	if st.StateVersion != project.SupportedStateVersionEnv || st.Env == nil {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "本机状态未采纳环境切换记录（state_version 1），与回退槽位要求不一致；请人工核验 .km/"}
	}
	prev, err := envtxn.LoadPrevious(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &runtime.Error{Code: runtime.CodeNoPrevious,
				Msg: "没有可回退的上一代记录（回退槽位为空）；只有成功执行过 km env switch 才会保留上一代，当前环境未做任何修改"}
		}
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "回退槽位记录无法解析；不覆盖、不猜测", Err: err}
	}
	p.prev = prev

	// 上一代容器必须实存且身份与记录一致（不凭名称接管替代资源）
	prevRes, exists, err := dk.InspectContainer(ctx, prev.ContainerID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &runtime.Error{Code: runtime.CodeNotFound,
			Msg: fmt.Sprintf("上一代容器（%s，ID %s）不存在（可能被外部删除）；当前环境未做任何修改，请人工核验后清理 .km/env/ 记录", prev.ContainerName, shortID(prev.ContainerID))}
	}
	if prevRes.Name != prev.ContainerName || prevRes.ProjectID != st.ProjectID ||
		prevRes.Image != prev.ImageID {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("上一代容器身份与记录不符（name=%s project=%s image=%s）；不接管，当前环境未做任何修改",
				prevRes.Name, prevRes.ProjectID, shortID(prevRes.Image))}
	}
	p.prevRes = prevRes

	// 上一代配置引用仍须解析到记录的内容 ID（引用漂移在变更前明确拒绝）
	refID, ok, err := dk.ImageID(ctx, prev.ImageRef)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &runtime.Error{Code: runtime.CodeImageDrift,
			Msg: fmt.Sprintf("上一代镜像引用 %s 已不在本地，无法确认其内容仍为 %s；请恢复该引用（docker tag/build）后重试，或人工处理 .km/env/ 记录", prev.ImageRef, shortID(prev.ImageID))}
	}
	if refID != prev.ImageID {
		return nil, &runtime.Error{Code: runtime.CodeImageDrift,
			Msg: fmt.Sprintf("上一代镜像引用 %s 当前内容 %s 与记录 %s 不一致（引用漂移）；请恢复正确引用后重试", prev.ImageRef, shortID(refID), shortID(prev.ImageID))}
	}

	// 平台一致（基于实际镜像元数据）
	curPlat, err := dk.ImageOSArch(ctx, res.Image)
	if err != nil {
		return nil, err
	}
	prevPlat, err := dk.ImageOSArch(ctx, prev.ImageID)
	if err != nil {
		return nil, err
	}
	p.curPlat, p.prevPlat = curPlat, prevPlat
	if curPlat != prevPlat {
		return nil, &runtime.Error{Code: runtime.CodePlatformMismatch,
			Msg: fmt.Sprintf("当前容器实际平台 %s 与上一代实际平台 %s 不一致；本版本不支持跨平台回退", curPlat, prevPlat)}
	}

	// 当前代无活跃任务（sessionCheck=false 时跳过；exited 容器内无进程，免查询）
	if err := envSessionGate(ctx, ep, res, sessionCheck, stderr); err != nil {
		return nil, err
	}
	return p, nil
}

func renderRollbackPreview(w io.Writer, p *rollbackPlan) {
	fmt.Fprintf(w, "km env rollback 预览（只读，未做任何变更）\n")
	fmt.Fprintf(w, "  项目根: %s\n", p.root)
	fmt.Fprintf(w, "  项目 ID: %s\n", p.st.ProjectID)
	fmt.Fprintf(w, "  引擎: %s（来源 %s）\n", p.ep.Endpoint, p.ep.Source)
	fmt.Fprintf(w, "  当前代: 第 %s（容器 %s，ID %s，状态 %s，镜像内容 %s，平台 %s）\n",
		generationDisplay(currentGeneration(p.st)), p.res.Name, shortID(p.res.ID), p.res.State, shortID(p.res.Image), p.curPlat)
	fmt.Fprintf(w, "  回退目标: 第 %s（容器 %s，ID %s，镜像引用 %s，内容 %s，平台 %s，切换前状态 %s）\n",
		generationDisplay(p.prev.Generation), p.prev.ContainerName, shortID(p.prev.ContainerID),
		p.prev.ImageRef, shortID(p.prev.ImageID), p.prevPlat, runningDisplay(p.prev.WasRunning))
	fmt.Fprintf(w, "  计划:\n")
	fmt.Fprintf(w, "    - 会话门禁: 执行时在当前容器安装 km-ctl 并核验（ACTIVE/未知将拒绝；dry-run 不安装不查询）\n")
	fmt.Fprintf(w, "    - 停止当前容器 %s（停止会结束其中非 km 管理的进程）\n", p.res.Name)
	fmt.Fprintf(w, "    - 激活上一代容器 %s（恢复其切换前的 %s 状态）\n", p.prev.ContainerName, runningDisplay(p.prev.WasRunning))
	fmt.Fprintf(w, "    - .km.json 的 image 恢复为 %s；本机状态指向上一代\n", p.prev.ImageRef)
	fmt.Fprintf(w, "    - 当前容器计入 retained 清单；回退槽位消费（再次 rollback 将无可回退记录）\n")
	fmt.Fprintf(w, "  边界: %s\n", boundaryNote)
	fmt.Fprintf(w, "  结果: 允许执行\n")
}

func currentGeneration(st *project.State) int {
	if st.Env != nil {
		return st.Env.Generation
	}
	return 0
}

func runningDisplay(wasRunning bool) string {
	if wasRunning {
		return "running"
	}
	return "exited"
}

// executeRollback 执行回退事务。阶段与失败收敛规则与 switch 对称（ADR §4）。
func executeRollback(ctx context.Context, dk *runtime.Docker, root string, plan *rollbackPlan, stderr io.Writer) error {
	opID, err := envtxn.NewOpID()
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "生成操作 ID 失败", Err: err}
	}
	cfgRaw, err := os.ReadFile(root + "/" + project.ConfigFileName)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "读取配置失败", Err: err}
	}
	stRaw, err := os.ReadFile(project.StatePath(root))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "读取状态失败", Err: err}
	}
	txn := &envtxn.Transaction{
		OpID: opID, Kind: envtxn.KindRollback, Stage: envtxn.StagePrepared,
		Old: envtxn.Snapshot{
			ContainerID: plan.res.ID, ContainerName: plan.res.Name,
			ImageID: plan.res.Image, WasRunning: plan.res.State == "running",
			Generation: currentGeneration(plan.st),
		},
		New: envtxn.Snapshot{
			ContainerID:   plan.prev.ContainerID,
			ContainerName: plan.prev.ContainerName,
			ImageID:       plan.prev.ImageID,
			WasRunning:    plan.prev.WasRunning,
			Generation:    plan.prev.Generation,
		},
		ConfigBackup: cfgBackup(cfgRaw),
		StateBackup:  envtxn.NewFileBackup(stRaw),
		PrevImageRef: plan.prev.ImageRef,
	}

	stage := envtxn.StagePrepared
	fail := func(err error) error {
		if envtxn.AtOrAfterCommitIntent(stage) {
			return withRecoverHint(err)
		}
		if cerr := convergeRollbackPreCommit(ctx, dk, root, txn, plan.st, stderr); cerr != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: "清理未完成，事务保留；运行 km env recover --dry-run 查看",
				Err: errJoin(err, cerr)}
		}
		return err
	}

	if err := envtxn.SaveTransaction(root, txn); err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "事务写入失败；未做任何变更", Err: err}
	}

	// 停止当前代（回退不创建任何容器）
	stage = envtxn.StageOldStopped
	if err := dk.StopContainer(ctx, plan.res.ID); err != nil {
		return fail(err)
	}
	oldRes, exists, err := dk.InspectContainer(ctx, plan.res.ID)
	if err != nil {
		return fail(err)
	}
	if !exists || oldRes.State != "exited" {
		return fail(&runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "当前代容器停止后状态未确认"})
	}
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 提交前复验：上一代引用必须仍解析到记录内容 ID（ADR §6）；漂移拒绝提交
	if curID, ok, err := dk.ImageID(ctx, plan.prev.ImageRef); err != nil {
		return fail(err)
	} else if !ok || curID != plan.prev.ImageID {
		return fail(&runtime.Error{Code: runtime.CodeImageDrift,
			Msg: fmt.Sprintf("上一代引用 %s 在回退过程中漂移（当前内容 %s，记录 %s）；拒绝提交，正在恢复原环境", plan.prev.ImageRef, shortID(curID), shortID(plan.prev.ImageID))})
	}

	// 提交前外部编辑检测（同 switch：提前到不可回退点之前）
	if err := verifyFilesUnchangedSincePrepared(root, txn); err != nil {
		return fail(err)
	}

	// COMMIT_INTENT：不可回退点
	stage = envtxn.StageCommitIntent
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	// 激活上一代（按其切换前状态语义）
	if plan.prev.WasRunning {
		if err := dk.StartContainer(ctx, plan.prev.ContainerID); err != nil {
			return fail(err)
		}
	}
	if err := writeRollbackCommitFiles(root, txn); err != nil {
		return fail(err)
	}
	// 账本轮换：当前代入 retained，消费回退槽位
	if _, err := envtxn.AppendRetained(root, envtxn.RetainedEntry{
		ContainerID:   txn.Old.ContainerID,
		ContainerName: txn.Old.ContainerName,
		ImageID:       txn.Old.ImageID,
		Generation:    txn.Old.Generation,
		Reason:        envtxn.ReasonRolledBack,
		OpID:          txn.OpID,
	}); err != nil {
		return fail(err)
	}
	if err := envtxn.RemovePrevious(root); err != nil {
		return fail(err)
	}
	stage = envtxn.StageCurrentCommitted
	if err := setStage(root, txn, stage); err != nil {
		return fail(err)
	}

	if err := finalizeRollback(ctx, dk, root, txn, stderr); err != nil {
		return withRecoverHint(err)
	}
	return nil
}

func cfgBackup(raw []byte) envtxn.FileBackup { return envtxn.NewFileBackup(raw) }

func errJoin(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%v；清理错误: %v", a, b)
}

// writeRollbackCommitFiles 恢复 config.image 为上一代引用并把状态指向上一代。
// 写前重读并比对备份；已应用则幂等跳过（与 switch 对称）。
func writeRollbackCommitFiles(root string, txn *envtxn.Transaction) error {
	cfgPath := root + "/" + project.ConfigFileName
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时读取配置失败", Err: err}
	}
	cfg, err := project.LoadConfig(cfgPath)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时配置无法解析", Err: err}
	}
	switch {
	case cfg.Image == txn.PrevImageRef:
	case txn.ConfigBackup.Matches(raw):
		newCfg := *cfg
		newCfg.Image = txn.PrevImageRef
		if err := project.WriteConfig(cfgPath, &newCfg); err != nil {
			return &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "提交时写入配置失败", Err: err}
		}
	default:
		return &runtime.Error{Code: runtime.CodeConfigInvalid,
			Msg: "检测到事务外的 .km.json 修改（与事务备份不一致），不覆盖外部修改；请人工核对后运行 km env recover"}
	}

	stRaw, err := os.ReadFile(project.StatePath(root))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交时读取状态失败", Err: err}
	}
	cur, err := project.LoadState(root)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "提交时状态无法解析", Err: err}
	}
	if cur.StateVersion == project.SupportedStateVersionEnv && cur.Env != nil &&
		cur.Container.ID == txn.New.ContainerID && cur.Env.Generation == txn.New.Generation {
		return nil
	}
	if !txn.StateBackup.Matches(stRaw) {
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "检测到事务外的 .km/state.json 修改（与事务备份不一致），不覆盖外部修改；请人工核对后运行 km env recover"}
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

// finalizeRollback 验证最终不变量：当前代 = 上一代容器；被撤容器已停止并
// 在 retained 中；previous.json 已消费。
func finalizeRollback(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, stderr io.Writer) error {
	res, exists, err := dk.InspectContainer(ctx, txn.New.ContainerID)
	if err != nil {
		return err
	}
	if !exists || res.Name != txn.New.ContainerName || res.Image != txn.New.ImageID {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("最终核验失败：回退目标容器（%s）不存在或身份不符", shortID(txn.New.ContainerID))}
	}
	if txn.New.WasRunning && res.State != "running" {
		if err := dk.StartContainer(ctx, txn.New.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "回退目标容器启动失败", Err: err}
		}
	}
	oldRes, exists, err := dk.InspectContainer(ctx, txn.Old.ContainerID)
	if err != nil {
		return err
	}
	if exists && oldRes.State == "running" {
		fmt.Fprintf(stderr, "km: 被撤下的容器意外处于运行状态，正在停止…\n")
		if err := dk.StopContainer(ctx, txn.Old.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "被撤下的容器停止失败", Err: err}
		}
	}
	r, err := envtxn.LoadRetained(root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	found := false
	if r != nil {
		for _, e := range r.Containers {
			if e.ContainerID == txn.Old.ContainerID {
				found = true
			}
		}
	}
	if !found {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "最终核验失败：被撤下的容器未计入 retained 账本"}
	}
	if envtxn.PreviousExists(root) {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "最终核验失败：回退槽位未被消费"}
	}
	return envtxn.ClearTransaction(root)
}

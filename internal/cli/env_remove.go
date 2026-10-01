package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// fullIDPattern 是 docker 完整容器 ID（64 hex）；remove 只接受完整 ID，
// 不接受名称、短 ID、多目标或通配符（ADR §10.3）。
var fullIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// runEnvRemove implements `km env remove <完整容器ID> [--dry-run] [--yes]`：
// 仅允许删除本项目 retained 账本内、身份完全匹配且 exited 的容器；
// 普通 docker rm（无 -f/-v）；容器可写层删除不可逆（ADR §10）。
func runEnvRemove(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	f, targetID, perr := parseEnvRemoveArgs(rest)
	if perr != nil {
		return usageError(stderr, "km env remove: %v", perr)
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

	plan, err := planRemove(ctx, dk, root, cfg, st, targetID)
	if err != nil {
		return envError(stderr, err)
	}
	if f.dryRun {
		renderRemovePreview(stdout, plan)
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

	// 取锁后重读记录并重新预检（预览不是授权令牌；ADR §4.1）
	cfg, st, err = reloadProjectFiles(root)
	if err != nil {
		return envError(stderr, err)
	}
	plan, err = planRemove(ctx, dk, root, cfg, st, targetID)
	if err != nil {
		return envError(stderr, err)
	}
	renderRemovePreview(stdout, plan)
	if confirmed, code := confirmEnvAction(stdout, stderr, f.yes); !confirmed {
		return code
	}
	// 确认后锁内重读全部记录（独立审计 P1-2：等待确认期间 state/账本可能被
	// 外部修改，沿用确认前对象会绕过保护）
	cfg, st, err = reloadProjectFiles(root)
	if err != nil {
		return envError(stderr, err)
	}
	recheck, err := planRemove(ctx, dk, root, cfg, st, targetID)
	if err != nil {
		return envError(stderr, err)
	}
	if !removePlanStillValid(plan, recheck) {
		return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "确认后目标、角色或记录已变化，未执行任何操作；请重新运行预览"})
	}

	if err := executeRemove(ctx, dk, root, recheck, stdout, stderr); err != nil {
		return envError(stderr, err)
	}
	return ExitOK
}

// parseEnvRemoveArgs 解析唯一目标（完整 ID）与标志；名称、短 ID、多目标、
// 未知选项一律用法错误（退出码 2）。
func parseEnvRemoveArgs(rest []string) (*envFlags, string, error) {
	f := &envFlags{}
	var target string
	for i := 0; i < len(rest); i++ {
		cur := rest[i]
		switch {
		case cur == "--dry-run":
			if f.dryRun {
				return nil, "", fmt.Errorf("--dry-run 重复")
			}
			f.dryRun = true
		case cur == "--yes":
			if f.yes {
				return nil, "", fmt.Errorf("--yes 重复")
			}
			f.yes = true
		case cur == "--image" || len(cur) > 2 && cur[:2] == "--" && cur != "--":
			return nil, "", fmt.Errorf("未知参数 %q（remove 仅接受一个完整容器 ID 与 --dry-run/--yes）", cur)
		default:
			if target != "" {
				return nil, "", fmt.Errorf("一次只能指定一个目标；不接受多个 ID")
			}
			target = cur
		}
	}
	if target == "" {
		return nil, "", fmt.Errorf("需要一个完整容器 ID（64 位十六进制；km env list 查看）")
	}
	if !fullIDPattern.MatchString(target) {
		return nil, "", fmt.Errorf("目标 %q 不是完整容器 ID（64 位十六进制）；不接受名称或短 ID", target)
	}
	return f, target, nil
}

// removePlan 是一次 remove 的只读规划：门禁结果 + 影响预览。
type removePlan struct {
	root       string
	st         *project.State
	targetID   string
	entry      *envtxn.RetainedEntry
	absent     bool // 目标容器已不存在（失效记录清理路径）
	state      string
	imageID    string
	probeEntry bool
	protected  []string // PREPARED 时的保护集合摘要
}

// planRemove 执行 remove 的全部门禁（只读）。
func planRemove(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State, targetID string) (*removePlan, error) {
	// 未完成事务（含进行中的删除）阻断
	txn, err := loadOpenTransaction(root)
	if err != nil {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: ".km/env/transaction.json 无法解析，无法确认项目是否处于事务中间态", Err: err}
	}
	if txn != nil {
		return nil, errTxnPending(txn)
	}

	// 引擎固定 + 引擎匹配 + 当前容器身份（CURRENT 是保护集合成员）
	if _, _, err := verifyStackForQuery(ctx, dk, root, st); err != nil {
		return nil, err
	}
	return verifyRemoveTarget(ctx, dk, root, st, targetID, nil)
}

// verifyRemoveTarget 是所有删除入口（普通 remove 与 recover 收尾）共用的
// 完整资格核验（独立审计 P1-1：recover 不得绕过保护）。基于调用方传入的
// 新鲜记录判定：保护集合（state/previous/事务引用）→ retained 精确匹配 →
// 扩展 inspect 身份全匹配（与 list 共用 envIdentityProblems）→ 状态 exited。
// selfTxn 非 nil 表示恢复自身事务：仅豁免其自身引用，其余保护仍然生效。
func verifyRemoveTarget(ctx context.Context, dk *runtime.Docker, root string, st *project.State, targetID string, selfTxn *envtxn.Transaction) (*removePlan, error) {
	p := &removePlan{root: root, st: st, targetID: targetID}

	// 保护集合先行（ADR §7）：当前环境/回退目标/事务引用始终受保护，
	// 即便同时错误出现在 retained 中；此检查不依赖账本存在
	protected := map[string]bool{}
	if st.Container.ID != "" {
		protected[st.Container.ID] = true
	}
	prev, perr := envtxn.LoadPrevious(root)
	if perr != nil && !os.IsNotExist(perr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "回退槽位无法解析；不覆盖、不猜测", Err: perr}
	}
	if prev != nil {
		protected[prev.ContainerID] = true
	}
	txn, terr := envtxn.LoadTransaction(root)
	if terr != nil && !os.IsNotExist(terr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "事务记录无法解析；不覆盖、不猜测", Err: terr}
	}
	if txn != nil {
		self := selfTxn != nil && txn.OpID == selfTxn.OpID
		if !self {
			protected[txn.Old.ContainerID] = true
			protected[txn.New.ContainerID] = true
		}
		for _, pr := range txn.Probes {
			protected[pr.ContainerID] = true
		}
	}
	p.protected = mapKeys(protected)
	if protected[targetID] {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: "目标受保护（当前环境/回退目标/事务引用不可删除）；即使它同时出现在 retained 账本中也始终受保护"}
	}

	// retained 精确匹配（ADR §10.3.3）
	ret, rerr := envtxn.LoadRetained(root)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, &runtime.Error{Code: runtime.CodeNotFound,
				Msg: "本项目没有 retained 账本（无已保留容器）；remove 只删除账本内条目，不会清理任意记录或容器"}
		}
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "retained 账本无法解析；不覆盖、不猜测", Err: rerr}
	}

	var entry *envtxn.RetainedEntry
	for i := range ret.Containers {
		if ret.Containers[i].ContainerID == targetID {
			e := ret.Containers[i]
			entry = &e
			break
		}
	}
	if entry == nil {
		return nil, &runtime.Error{Code: runtime.CodeNotFound,
			Msg: fmt.Sprintf("目标 %s 不在本项目的 retained 账本中；本命令不会清理任意记录，也不会删除未知目标", shortID(targetID))}
	}
	p.entry = entry
	p.probeEntry = entry.Reason == envtxn.ReasonProbeCleanupFailed

	// 目标实态（完整 ID 扩展 inspect）
	ext, exists, ierr := dk.InspectContainerExtended(ctx, targetID)
	if ierr != nil {
		return nil, ierr
	}
	if !exists {
		// 失效记录清理路径：仅移除账本条目，不执行容器删除
		p.absent = true
		return p, nil
	}
	p.state = ext.State
	p.imageID = ext.Image

	// 身份全匹配（与 list 共用同一套判定；缺失挂载不得视为匹配）
	if problems := envIdentityProblems(ext, st, root, entry.ContainerName, entry.ImageID, probeEntryOrNil(p.probeEntry, entry)); len(problems) > 0 {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: "目标身份与 retained 记录不符，不删除: " + strings.Join(problems, "；")}
	}

	// 状态门禁：仅 exited（第一版对 running/paused/restarting/created/dead 一律拒绝）
	if ext.State != "exited" {
		return nil, &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("目标状态为 %s；第一版仅允许删除 exited 的保留容器（不自动 stop/unpause/kill）", ext.State)}
	}
	return p, nil
}

func probeEntryOrNil(probe bool, entry *envtxn.RetainedEntry) *envtxn.RetainedEntry {
	if probe {
		return entry
	}
	return nil
}

func renderRemovePreview(w io.Writer, p *removePlan) {
	fmt.Fprintf(w, "km env remove 预览（只读，未做任何变更）\n")
	fmt.Fprintf(w, "  目标容器: %s\n", p.targetID)
	fmt.Fprintf(w, "  记录角色: RETAINED（原因 %s）\n", p.entry.Reason)
	if p.absent {
		fmt.Fprintf(w, "  实际状态: 容器已不存在（MISSING）\n")
		fmt.Fprintf(w, "  计划: 仅移除这条失效账本记录；不执行任何容器删除\n")
	} else {
		fmt.Fprintf(w, "  实际状态: %s（镜像内容 %s）\n", p.state, shortID(p.imageID))
		fmt.Fprintf(w, "  计划: 普通 docker rm（无 -f/-v）→ 确认不存在 → 从 retained 账本移除本条目\n")
	}
	fmt.Fprintf(w, "  保护检查: 当前环境/回退目标/事务资源不受影响；其他账本条目原样保留\n")
	fmt.Fprintf(w, "  边界: 容器可写层内文件将永久丢失；不会删除共享项目文件，也不会删除镜像\n")
	if p.absent {
		fmt.Fprintf(w, "  结果: 允许执行（仅清理失效记录）\n")
	} else {
		fmt.Fprintf(w, "  结果: 允许执行\n")
	}
}

// removePlanStillValid 比较确认前后的两次规划：身份/保护/记录快照必须完全
// 一致（独立审计 P1-2：只比 ID/state/reason 会漏掉记录被外部改写的情况）。
func removePlanStillValid(a, b *removePlan) bool {
	if a.targetID != b.targetID || a.absent != b.absent || a.state != b.state || a.imageID != b.imageID {
		return false
	}
	if (a.entry == nil) != (b.entry == nil) {
		return false
	}
	if a.entry != nil && b.entry != nil {
		if a.entry.ContainerID != b.entry.ContainerID ||
			a.entry.ContainerName != b.entry.ContainerName ||
			a.entry.ImageID != b.entry.ImageID ||
			a.entry.Generation != b.entry.Generation ||
			a.entry.Reason != b.entry.Reason {
			return false
		}
	}
	if a.probeEntry != b.probeEntry {
		return false
	}
	return true
}

// executeRemove 执行删除事务（ADR §10.2 冻结写点）。任何一步失败都保留事务
// 并输出真实阶段；journal 写失败时绝不发 Docker 删除。
func executeRemove(ctx context.Context, dk *runtime.Docker, root string, plan *removePlan, stdout, stderr io.Writer) error {
	opID, err := envtxn.NewOpID()
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "生成操作 ID 失败", Err: err}
	}
	retRaw, err := os.ReadFile(envtxn.RetainedPath(root))
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "读取 retained 账本失败", Err: err}
	}
	snap := envtxn.Snapshot{
		ContainerID:   plan.targetID,
		ContainerName: plan.entry.ContainerName,
		ImageID:       plan.entry.ImageID,
		Generation:    plan.entry.Generation,
	}
	txn := &envtxn.Transaction{
		OpID: opID, Kind: envtxn.KindRemove, Stage: envtxn.StagePrepared,
		Old: snap, New: snap,
		LedgerBackup: envtxn.NewFileBackup(retRaw),
		Protected:    plan.protected,
		RemoveReason: plan.entry.Reason,
	}
	// PREPARED：journal 先行；写失败不得发 Docker 删除
	if err := envtxn.SaveTransaction(root, txn); err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "清理事务写入失败；未执行任何删除", Err: err}
	}

	if plan.absent {
		// 失效记录：明确不存在核验已由 planRemove 完成
		if err := setStage(root, txn, envtxn.StageAbsenceConfirmed); err != nil {
			return withRecoverHint(err)
		}
		if err := finishRemoveLedger(root, txn); err != nil {
			return withRecoverHint(err)
		}
		fmt.Fprintf(stdout, "km env remove: 目标 %s 已不存在（此前被外部删除）；仅清理失效账本记录，未执行容器删除。\n", plan.targetID)
		return nil
	}

	// REMOVE_REQUESTED：调用 Docker 前写意图
	if err := setStage(root, txn, envtxn.StageRemoveRequested); err != nil {
		return withRecoverHint(err)
	}
	if err := dk.RemoveContainerGraceful(ctx, plan.targetID); err != nil {
		// 结果可能已生效：inspect 原完整 ID 定实态，不盲目重试或删同名容器
		ext, exists, ierr := dk.InspectContainerExtended(ctx, plan.targetID)
		if ierr != nil {
			return withRecoverHint(&runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: "rm 失败且实态核验也失败，结果未知", Err: errJoin(err, ierr)})
		}
		if exists {
			if removeEligible(root, plan, &ext) {
				return withRecoverHint(&runtime.Error{Code: runtime.CodeResourceUnknown,
					Msg: fmt.Sprintf("rm 失败（%v）且容器仍存在、删除资格未变；事务保留于 REMOVE_REQUESTED，可运行 km env recover 确认后重试", err)})
			}
			return withRecoverHint(&runtime.Error{Code: runtime.CodeContainerConflict,
				Msg: fmt.Sprintf("rm 失败且目标状态/资格已变化（state=%s）；停止写操作，事务保留", extStateOrUnknown(&ext, exists))})
		}
		// rm 报错但容器明确不存在：结果已生效，按成功路径收尾
		fmt.Fprintf(stderr, "km: rm 命令报错但目标已确认不存在；按已删除收尾（%v）\n", err)
	}

	// ABSENCE_CONFIRMED：目标在固定引擎上明确不存在
	if err := verifyAbsence(ctx, dk, plan.targetID); err != nil {
		return withRecoverHint(err)
	}
	if err := setStage(root, txn, envtxn.StageAbsenceConfirmed); err != nil {
		return withRecoverHint(err)
	}
	if err := finishRemoveLedger(root, txn); err != nil {
		// 删除已发生、账本未收尾：不返回完整成功
		return withRecoverHint(err)
	}
	fmt.Fprintf(stdout, "km env remove: 已删除容器 %s（RETAINED，原因 %s）；retained 账本已更新。\n", plan.targetID, plan.entry.Reason)
	return nil
}

// verifyAbsence 断言目标在固定引擎上明确不存在；查询失败不等于不存在。
func verifyAbsence(ctx context.Context, dk *runtime.Docker, targetID string) error {
	_, exists, err := dk.InspectContainerExtended(ctx, targetID)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "删除后核验失败（引擎故障），结果未知；事务保留", Err: err}
	}
	if exists {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "rm 返回成功但目标仍存在，结果不一致；事务保留，请运行 km env recover 核实"}
	}
	return nil
}

// finishRemoveLedger 外科手术式账本收尾：解析当前账本，仅移除目标条目，
// 其余条目（含外部编辑结果）原样保留；已移除则幂等跳过。
func finishRemoveLedger(root string, txn *envtxn.Transaction) error {
	ret, err := envtxn.LoadRetained(root)
	if err != nil {
		if os.IsNotExist(err) {
			return &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: "retained 账本缺失，无法收尾；请人工核验 .km/env/"}
		}
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "retained 账本无法解析（可能与事务前状态不一致），不覆盖外部修改；请人工核验", Err: err}
	}
	kept := ret.Containers[:0:0]
	removed := false
	for _, e := range ret.Containers {
		if e.ContainerID == txn.New.ContainerID {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		// 已被移除（幂等恢复）
		return envtxn.ClearTransaction(root)
	}
	ret.Containers = kept
	if err := envtxn.SaveRetained(root, ret); err != nil {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "容器已删除，但 retained 账本写入失败：记录待收尾；事务保留，可运行 km env recover 完成", Err: err}
	}
	if err := setStage(root, txn, envtxn.StageLedgerUpdated); err != nil {
		return err
	}
	return envtxn.ClearTransaction(root)
}

// removeEligible 供 rm 失败后的内联复核：资格未变才允许保留事务等待 recover 重试。
func removeEligible(root string, plan *removePlan, ext *runtime.InspectExtended) bool {
	if ext.State != "exited" {
		return false
	}
	if ext.Name != plan.entry.ContainerName || ext.Image != plan.entry.ImageID {
		return false
	}
	if filepath.Clean(ext.MountSource) != filepath.Clean(root) {
		return false
	}
	return true
}

func extStateOrUnknown(ext *runtime.InspectExtended, exists bool) string {
	if !exists {
		return "MISSING"
	}
	return ext.State
}

// convergeRemove 是 recover 的 remove 分派（ADR §10.2）：永不重建容器。
// 锁内重读最新记录并执行与普通 remove 完全相同的资格核验（独立审计 P1-1）：
// 目标被手工记为 CURRENT/PREVIOUS、身份被篡改、记录与确认快照不一致 → 一律拒绝；
// 目标缺失 → 仅账本收尾；目标仍在且资格成立 → 确认后重试普通 rm。
func convergeRemove(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) (string, error) {
	// 锁内重读最新 config/state（不信任 recover 入口处的旧对象）
	_, stFresh, err := reloadProjectFiles(root)
	if err != nil {
		return "", err
	}
	target := txn.New.ContainerID

	switch txn.Stage {
	case envtxn.StageLedgerUpdated:
		// 账本已更新：确认缺失并清除事务
		if err := verifyAbsence(ctx, dk, target); err != nil {
			return "", err
		}
		return "remove 收尾完成（账本已更新）", envtxn.ClearTransaction(root)
	case envtxn.StageAbsenceConfirmed:
		// 删除已确认生效：仅完成账本收尾
		if err := verifyAbsence(ctx, dk, target); err != nil {
			return "", err
		}
		if err := finishRemoveLedger(root, txn); err != nil {
			return "", err
		}
		return "remove 收尾完成（仅账本收尾，容器不可恢复）", nil
	}

	// PREPARED / REMOVE_REQUESTED：与普通 remove 相同的完整资格核验
	plan, err := verifyRemoveTarget(ctx, dk, root, stFresh, target, txn)
	if err != nil {
		return "", err // 保护/身份/资格问题 → 拒绝，事务保留
	}
	// 与最初确认的快照比对：retained 记录被外部改名/换镜像时拒绝
	if plan.entry.ContainerName != txn.New.ContainerName ||
		plan.entry.ImageID != txn.New.ImageID ||
		plan.entry.Generation != txn.New.Generation {
		return "", &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("retained 记录与确认快照不一致（确认时 %s/%s…/gen%d，现 %s/%s…/gen%d）；停止写操作，事务保留",
				txn.New.ContainerName, shortID(txn.New.ImageID), txn.New.Generation,
				plan.entry.ContainerName, shortID(plan.entry.ImageID), plan.entry.Generation)}
	}

	if plan.absent {
		// 目标已明确不存在：仅账本收尾
		if err := setStage(root, txn, envtxn.StageAbsenceConfirmed); err != nil {
			return "", err
		}
		if err := finishRemoveLedger(root, txn); err != nil {
			return "", err
		}
		return "remove 收尾完成（目标已不存在，仅账本收尾）", nil
	}

	// 目标仍在且资格成立：确认后重试不可逆删除（普通 rm）
	if err := dk.RemoveContainerGraceful(ctx, target); err != nil {
		return "", &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "重试普通 rm 失败；事务保留", Err: err}
	}
	if err := verifyAbsence(ctx, dk, target); err != nil {
		return "", err
	}
	if err := setStage(root, txn, envtxn.StageAbsenceConfirmed); err != nil {
		return "", err
	}
	if err := finishRemoveLedger(root, txn); err != nil {
		return "", err
	}
	return "remove 收尾完成（目标已删除，账本已更新）", nil
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runEnvRecover implements `km env recover [--dry-run] [--yes]`：恢复未完成
// 事务。收敛方向冻结（ADR §4）：stage < COMMIT_INTENT → 前态；
// stage ≥ COMMIT_INTENT → 新态。recover 幂等、可重复调用。
func runEnvRecover(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	f, perr := parseEnvFlags(rest, false)
	if perr != nil {
		return usageError(stderr, "km env recover: %v", perr)
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	root, _, st, err := loadProjectStack(wd)
	if err != nil {
		return envError(stderr, err)
	}

	txn, err := loadOpenTransaction(root)
	if err != nil {
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "事务记录无法解析，无法自动恢复；原始文件已保留，请人工核对 .km/env/transaction.json 与下方诊断", Err: err})
	}
	if txn == nil {
		fmt.Fprintf(stdout, "km env recover: 无未完成事务，无需恢复。\n")
		return ExitOK
	}
	// 存在待恢复事务时，非交互执行必须显式 --yes（ADR §1：变更前检查）
	if !f.dryRun && !f.yes && !envStdinIsTTY() {
		return usageError(stderr, "非交互环境执行恢复必须显式 --yes（--dry-run 可只读预览）；本次未执行任何变更")
	}

	renderRecoverPlan(stdout, root, txn, st)
	if f.dryRun {
		fmt.Fprintf(stdout, "  结果: 以上为恢复方案（dry-run 未做任何变更）\n")
		return ExitOK
	}

	lock, err := project.AcquireLock(root)
	if err != nil {
		return envError(stderr, err)
	}
	defer lock.Release()

	// 取锁后重读事务（可能与锁前不同）；与用户确认的对象不一致则放弃
	confirmedOp, confirmedStage := txn.OpID, txn.Stage
	txn, err = loadOpenTransaction(root)
	if err != nil {
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "事务记录无法解析，无法自动恢复", Err: err})
	}
	if txn == nil {
		fmt.Fprintf(stdout, "km env recover: 无未完成事务，无需恢复。\n")
		return ExitOK
	}
	if txn.OpID != confirmedOp || txn.Stage != confirmedStage {
		return envError(stderr, &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: "确认后事务已变化（op 或 stage 不同），未执行任何恢复；请重新运行 km env recover --dry-run"})
	}
	if !f.dryRun && !f.yes {
		if confirmed, code := confirmEnvAction(stdout, stderr, false); !confirmed {
			return code
		}
	}

	// 恢复只需要引擎一致（事务期间容器状态可能任意）；
	// 容器级事实由收敛过程按事务记录交叉核验。
	ep, err := resolveEngine(ctx, dk)
	if err != nil {
		return envError(stderr, err)
	}
	if err := verifyProjectEngine(st, ep.Endpoint); err != nil {
		return envError(stderr, err)
	}

	diag, err := convergeTxn(ctx, dk, root, txn, st, stderr)
	if err != nil {
		return envError(stderr, withRecoverHint(err))
	}
	fmt.Fprintf(stdout, "km env recover: 恢复完成（%s）。\n%s\n", diag, boundaryNote)
	return ExitOK
}

// renderRecoverPlan 输出实际状态、计划恢复方向与风险；不修文件或容器。
func renderRecoverPlan(w io.Writer, root string, txn *envtxn.Transaction, st *project.State) string {
	after := envtxn.AtOrAfterCommitIntent(txn.Stage)
	direction := "前态（恢复原环境）"
	if after {
		direction = "新态（完成提交）"
	}
	fmt.Fprintf(w, "km env recover 恢复方案（事务 %s）\n", txn.OpID)
	fmt.Fprintf(w, "  项目根: %s\n", root)
	fmt.Fprintf(w, "  事务: kind=%s stage=%s op=%s\n", txn.Kind, txn.Stage, txn.OpID)
	fmt.Fprintf(w, "  原环境: 第 %s（容器 %s，ID %s，镜像内容 %s，切换前状态 %s）\n",
		generationDisplay(txn.Old.Generation), txn.Old.ContainerName, shortID(txn.Old.ContainerID),
		shortID(txn.Old.ImageID), runningDisplay(txn.Old.WasRunning))
	if txn.Kind == envtxn.KindSwitch {
		fmt.Fprintf(w, "  目标环境: 第 %s（候选 %s，镜像内容 %s，引用 %s）\n",
			generationDisplay(txn.New.Generation), txn.New.ContainerName, shortID(txn.New.ImageID), txn.TargetImageRef)
	} else {
		fmt.Fprintf(w, "  目标环境: 第 %s（容器 %s，ID %s，镜像引用 %s）\n",
			generationDisplay(txn.New.Generation), txn.New.ContainerName, shortID(txn.New.ContainerID), txn.PrevImageRef)
	}
	fmt.Fprintf(w, "  恢复方向: %s\n", direction)
	if after {
		fmt.Fprintf(w, "  风险: 已过提交点，恢复将完成新代提交（当前容器/配置可能已指向新代）\n")
	} else {
		fmt.Fprintf(w, "  风险: 提交点前，恢复将删除本事务创建的容器并还原文件（原环境不受影响）\n")
	}
	fmt.Fprintf(w, "  探测容器登记: %d 个（未删除者将被清理或计入账本）\n", len(txn.Probes))
	return direction
}

// convergeTxn 按冻结规则把未完成事务收敛到前态或新态。
func convergeTxn(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) (string, error) {
	after := envtxn.AtOrAfterCommitIntent(txn.Stage)
	if txn.Kind == envtxn.KindSwitch {
		if after {
			return "switch 收敛到新态", convergeSwitchPostCommit(ctx, dk, root, txn, st, stderr)
		}
		return "switch 收敛到前态", convergeSwitchPreCommit(ctx, dk, root, txn, st, stderr)
	}
	if after {
		return "rollback 收敛到新态", convergeRollbackPostCommit(ctx, dk, root, txn, st, stderr)
	}
	return "rollback 收敛到前态", convergeRollbackPreCommit(ctx, dk, root, txn, st, stderr)
}

// ---- 共享收敛原语 ---------------------------------------------------------

// discoverOpResources 按 km.op 标签发现本事务创建的资源。查询失败不等于
// 无资源：错误必须上抛并保留事务。
func discoverOpResources(ctx context.Context, dk *runtime.Docker, opID string) ([]runtime.ContainerSummary, error) {
	return dk.FindContainersByLabel(ctx, runtime.OpLabel, opID)
}

// removeOpResources 删除本事务创建的候选/探测容器。每个资源先 inspect 并
// 核验归属（项目标签、挂载），身份不明时停止写操作。
func removeOpResources(ctx context.Context, dk *runtime.Docker, root string, st *project.State, txn *envtxn.Transaction, stderr io.Writer) error {
	sums, err := discoverOpResources(ctx, dk, txn.OpID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range sums {
		seen[s.ID] = true
		if err := removeOpResourceChecked(ctx, dk, root, st, s.ID, txn); err != nil {
			return err
		}
	}
	// 记录过完整 ID 但未带标签的兜底核验（外部改标签等罕见情形）
	for _, id := range []string{txn.New.ContainerID} {
		if id == "" || seen[id] {
			continue
		}
		res, exists, err := dk.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if res.ProjectID != st.ProjectID || filepath.Clean(res.MountSource) != filepath.Clean(root) {
			return &runtime.Error{Code: runtime.CodeContainerConflict,
				Msg: fmt.Sprintf("事务记录的容器（%s）归属不符，不删除；请人工核验", shortID(id))}
		}
		if txn.Kind == envtxn.KindRollback || res.Name == txn.New.ContainerName || res.Name == "km-probe-"+txn.OpID {
			if err := dk.RemoveContainer(ctx, id); err != nil && !runtime.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func removeOpResourceChecked(ctx context.Context, dk *runtime.Docker, root string, st *project.State, id string, txn *envtxn.Transaction) error {
	res, exists, err := dk.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if res.ProjectID != st.ProjectID || filepath.Clean(res.MountSource) != filepath.Clean(root) {
		return &runtime.Error{Code: runtime.CodeContainerConflict,
			Msg: fmt.Sprintf("op 标签资源（%s，name=%s）归属不符，不删除；请人工核验", shortID(id), res.Name)}
	}
	// 名称与镜像内容必须与本事务已知的资源（候选/探测）一致才删除
	// （ADR §4：名称/项目标签/镜像内容/挂载一致才动它）
	known := (res.Name == txn.New.ContainerName || res.Name == "km-probe-"+txn.OpID) &&
		res.Image == txn.New.ImageID
	if txn.Kind == envtxn.KindSwitch && known {
		if err := dk.RemoveContainer(ctx, id); err != nil && !runtime.IsNotFound(err) {
			return err
		}
		return nil
	}
	return &runtime.Error{Code: runtime.CodeContainerConflict,
		Msg: fmt.Sprintf("op 标签资源（%s，name=%s）不属于本事务已知资源，不删除；请人工核验", shortID(id), res.Name)}
}

// ensureSnapshotState 把快照容器恢复到记录的运行状态。容器消失或启动失败
// 必须报错保留事务；外部多启动的容器只告警不强制停止。
func ensureSnapshotState(ctx context.Context, dk *runtime.Docker, snap envtxn.Snapshot, what string) error {
	res, exists, err := dk.InspectContainer(ctx, snap.ContainerID)
	if err != nil {
		return err
	}
	if !exists {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("%s（%s，ID %s）不存在（可能被外部删除）；不猜测，事务保留", what, snap.ContainerName, shortID(snap.ContainerID))}
	}
	switch {
	case snap.WasRunning && res.State != "running":
		if err := dk.StartContainer(ctx, snap.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: fmt.Sprintf("%s启动失败；事务保留", what), Err: err}
		}
		res, exists, err = dk.InspectContainer(ctx, snap.ContainerID)
		if err != nil || !exists || res.State != "running" {
			return &runtime.Error{Code: runtime.CodeResourceUnknown,
				Msg: fmt.Sprintf("%s启动后状态未确认；事务保留", what)}
		}
	case !snap.WasRunning && res.State == "running":
		fmt.Fprintf(os.Stderr, "km: %s在事务期间被外部启动；保持现状，不强停（请人工确认）\n", what)
	}
	return nil
}

// restoreStateBackup 前态收敛时还原 state.json。当前内容与备份一致 → 跳过；
// 与备份语义等价（仅差我们自己的 v2 升版）→ 还原；无法证明 → 拒绝覆盖。
func restoreStateBackup(root string, txn *envtxn.Transaction) error {
	raw, err := os.ReadFile(project.StatePath(root))
	if os.IsNotExist(err) {
		return envtxn.ClearTransaction(root)
	}
	if err != nil {
		return err
	}
	if txn.StateBackup.Matches(raw) {
		return nil
	}
	var cur project.State
	if json.Unmarshal(raw, &cur) != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "检测到事务外的 .km/state.json 修改（无法解析），不覆盖；请人工核对"}
	}
	var backup project.State
	backupBytes, err := txn.StateBackup.Bytes()
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "事务状态备份损坏", Err: err}
	}
	if json.Unmarshal(backupBytes, &backup) != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "事务状态备份损坏（解码失败）"}
	}
	if !stateSemanticallyEqual(&cur, &backup) {
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "检测到事务外的 .km/state.json 修改（与事务备份不一致），不覆盖外部修改；请人工核对"}
	}
	return writeStateRaw(root, backupBytes)
}

// stateSemanticallyEqual 比较 env 升版之外的身份数据是否一致。
func stateSemanticallyEqual(a, b *project.State) bool {
	return a.ProjectID == b.ProjectID &&
		a.Container.ID == b.Container.ID &&
		a.Container.Name == b.Container.Name &&
		a.Container.ImageID == b.Container.ImageID &&
		a.Runtime.Endpoint == b.Runtime.Endpoint &&
		a.Runtime.Context == b.Runtime.Context
}

func writeStateRaw(root string, data []byte) error {
	dir := filepath.Join(root, project.StateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "state.json.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), project.StatePath(root))
}

// ---- switch 收敛 ----------------------------------------------------------

// convergeSwitchPreCommit 提交点前收敛：删除本事务候选/探测容器、恢复旧
// 容器运行状态、还原 state、清除事务。任何一步失败保留事务。
func convergeSwitchPreCommit(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) error {
	if err := removeOpResources(ctx, dk, root, st, txn, stderr); err != nil {
		return err
	}
	if err := ensureSnapshotState(ctx, dk, txn.Old, "旧容器"); err != nil {
		return err
	}
	if err := restoreStateBackup(root, txn); err != nil {
		return err
	}
	return envtxn.ClearTransaction(root)
}

// convergeSwitchPostCommit 提交点后收敛到新态：核验候选 → 幂等补齐
// previous/config/state → 清理探测残留 → 核验上一代停止 → 清除事务。
func convergeSwitchPostCommit(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) error {
	// 候选身份：不存在/不符一律停止写操作（配置可能已指向它）
	res, exists, err := dk.InspectContainer(ctx, txn.New.ContainerID)
	if err != nil {
		return err
	}
	if !exists || res.Name != txn.New.ContainerName || res.Image != txn.New.ImageID {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("候选容器（%s，ID %s）不存在或身份不符，无法收敛到新态；请人工核验", txn.New.ContainerName, shortID(txn.New.ContainerID))}
	}

	// 探测容器残留清理（失败计入账本，不阻塞）
	removeProbeContainer(ctx, dk, root, txn, stderr)

	// 幂等补齐提交写入
	if err := rotatePreviousOnRecover(ctx, dk, root, txn, st); err != nil {
		return err
	}
	if err := writeSwitchCommitFiles(root, txn); err != nil {
		return err
	}

	// 上一代保持停止
	oldRes, exists, err := dk.InspectContainer(ctx, txn.Old.ContainerID)
	if err != nil {
		return err
	}
	if !exists {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("上一代容器（%s）不存在，回退槽位将失效；请人工核验后清理 .km/env/", shortID(txn.Old.ContainerID))}
	}
	if oldRes.State == "running" {
		if err := dk.StopContainer(ctx, txn.Old.ContainerID); err != nil {
			return err
		}
	}

	// 候选保持运行
	if res.State != "running" {
		if err := dk.StartContainer(ctx, txn.New.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "候选容器启动失败；事务保留", Err: err}
		}
	}
	return envtxn.ClearTransaction(root)
}

// rotatePreviousOnRecover 恢复路径的槽位补齐：已一致则跳过；不存在则写入；
// 仍持有更早一代（提交点崩溃窗口内轮换未完成）则**完成轮换**——与提交路径
// 同一套核验（容器实存 + 归属一致才转入 retained），保证二次及以后的 switch
// 在 COMMIT_INTENT 崩溃后 recover 仍可收敛新态（独立审查 P0-1 修复）。
func rotatePreviousOnRecover(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State) error {
	if envtxn.PreviousExists(root) {
		prev, err := envtxn.LoadPrevious(root)
		if err != nil {
			return err
		}
		if prev.ContainerID == txn.Old.ContainerID {
			return nil
		}
		res, exists, ierr := dk.InspectContainer(ctx, prev.ContainerID)
		if ierr != nil {
			return ierr
		}
		if !exists || res.Name != prev.ContainerName || res.ProjectID != st.ProjectID {
			return &runtime.Error{Code: runtime.CodeStateInvalid,
				Msg: fmt.Sprintf("回退槽位中的上一代容器（%s，ID %s）已不存在或身份不符，账本不一致；请人工核验后清理 .km/env/ 记录", prev.ContainerName, shortID(prev.ContainerID))}
		}
		if _, err := envtxn.AppendRetained(root, envtxn.RetainedEntry{
			ContainerID:   prev.ContainerID,
			ContainerName: prev.ContainerName,
			ImageID:       prev.ImageID,
			Generation:    prev.Generation,
			Reason:        envtxn.ReasonSupersededBySwitch,
			OpID:          txn.OpID,
		}); err != nil {
			return err
		}
	}
	return envtxn.SavePrevious(root, &envtxn.Previous{
		Generation:    txn.Old.Generation,
		ContainerID:   txn.Old.ContainerID,
		ContainerName: txn.Old.ContainerName,
		ImageID:       txn.Old.ImageID,
		ImageRef:      prevConfigImageRef(root, txn),
		WasRunning:    txn.Old.WasRunning,
		OpID:          txn.OpID,
		SwitchedAt:    nowUTC(),
	})
}

// prevConfigImageRef 推导切换前的 config.image：优先从事务配置备份解析。
func prevConfigImageRef(root string, txn *envtxn.Transaction) string {
	if raw, err := txn.ConfigBackup.Bytes(); err == nil {
		var cfg project.Config
		if json.Unmarshal(raw, &cfg) == nil && cfg.Image != "" {
			return cfg.Image
		}
	}
	return txn.TargetImageRef
}

// ---- rollback 收敛 --------------------------------------------------------

// convergeRollbackPreCommit 提交点前收敛：回退不创建资源，只需恢复当前代
// 容器的运行状态并清除事务。
func convergeRollbackPreCommit(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) error {
	if err := ensureSnapshotState(ctx, dk, txn.Old, "当前代容器"); err != nil {
		return err
	}
	if err := restoreStateBackup(root, txn); err != nil {
		return err
	}
	return envtxn.ClearTransaction(root)
}

// convergeRollbackPostCommit 提交点后收敛到新态（上一代成为当前代）：
// 激活上一代 → 幂等补齐 config/state/retained → 消费槽位 → 清除事务。
func convergeRollbackPostCommit(ctx context.Context, dk *runtime.Docker, root string, txn *envtxn.Transaction, st *project.State, stderr io.Writer) error {
	res, exists, err := dk.InspectContainer(ctx, txn.New.ContainerID)
	if err != nil {
		return err
	}
	if !exists || res.Name != txn.New.ContainerName || res.Image != txn.New.ImageID {
		return &runtime.Error{Code: runtime.CodeResourceUnknown,
			Msg: fmt.Sprintf("回退目标容器（%s，ID %s）不存在或身份不符，无法收敛；请人工核验", txn.New.ContainerName, shortID(txn.New.ContainerID))}
	}
	if txn.New.WasRunning && res.State != "running" {
		if err := dk.StartContainer(ctx, txn.New.ContainerID); err != nil {
			return &runtime.Error{Code: runtime.CodeResourceUnknown, Msg: "回退目标容器启动失败；事务保留", Err: err}
		}
	}
	if err := writeRollbackCommitFiles(root, txn); err != nil {
		return err
	}
	if _, err := envtxn.AppendRetained(root, envtxn.RetainedEntry{
		ContainerID:   txn.Old.ContainerID,
		ContainerName: txn.Old.ContainerName,
		ImageID:       txn.Old.ImageID,
		Generation:    txn.Old.Generation,
		Reason:        envtxn.ReasonRolledBack,
		OpID:          txn.OpID,
	}); err != nil {
		return err
	}
	if err := envtxn.RemovePrevious(root); err != nil {
		return err
	}
	// 被撤下的容器保持停止
	oldRes, exists, err := dk.InspectContainer(ctx, txn.Old.ContainerID)
	if err != nil {
		return err
	}
	if exists && oldRes.State == "running" {
		if err := dk.StopContainer(ctx, txn.Old.ContainerID); err != nil {
			return err
		}
	}
	return envtxn.ClearTransaction(root)
}

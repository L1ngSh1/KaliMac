package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// ---------- 独立审查修复的回归测试 ----------

// P0-1 回归：第二次 switch 在 COMMIT_INTENT 崩溃（回退槽位仍持有更早一代）时，
// recover 必须完成槽位轮换并收敛新态，而不是永远拒绝。
func TestEnvRecoverCrashAtCommitIntentWithSlot(t *testing.T) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	cfgB := `{"schema_version":1,"name":"demo","image":"` + envImgBRef + `","platform":"linux/arm64"}`
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g2", state: "running", project: envProjID, image: envImgAID, mount: resolveDir(t, dir), opID: "e0000000000000003", role: "candidate"})
	f.conts[gen1ID].state = "exited"
	txn := &envtxn.Transaction{
		OpID: "e0000000000000003", Kind: envtxn.KindSwitch, Stage: envtxn.StageCommitIntent,
		Old:            envtxn.Snapshot{ContainerID: gen1ID, ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID, WasRunning: true, Generation: 1},
		New:            envtxn.Snapshot{ContainerID: candID, ContainerName: "km-" + envProjID + "-g2", ImageID: envImgAID, WasRunning: true, Generation: 2},
		ConfigBackup:   envtxn.NewFileBackup([]byte(cfgB)),
		StateBackup:    envtxn.NewFileBackup(readStateBytes(t, dir)),
		TargetImageRef: envImgARef,
	}
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}

	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("recover 应完成轮换并收敛新态: code=%d err=%s", code, errb)
	}
	// gen0 槽位轮换进 retained
	r, err := envtxn.LoadRetained(dir)
	if err != nil || len(r.Containers) != 1 || r.Containers[0].ContainerID != envOldID ||
		r.Containers[0].Reason != envtxn.ReasonSupersededBySwitch {
		t.Fatalf("gen0 应转入 retained: %+v err=%v", r, err)
	}
	// 槽位更新为 gen1
	prev, err := envtxn.LoadPrevious(dir)
	if err != nil || prev.ContainerID != gen1ID || prev.Generation != 1 || prev.ImageRef != envImgBRef {
		t.Fatalf("槽位应为 gen1: %+v err=%v", prev, err)
	}
	// 文件收敛新态
	cfg, err := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if err != nil || cfg.Image != envImgARef {
		t.Fatalf("config: %+v err=%v", cfg, err)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != candID || st.Env.Generation != 2 {
		t.Fatalf("state: %+v", st)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	// 账本恒等式：3 个容器（当前代 + 上一代 + retained）
	if len(f.conts) != 3 {
		t.Fatalf("实际容器应为 3: %d", len(f.conts))
	}
}

// P1-3：事务中的外部 .km.json 修改 → 提交前拒绝，不覆盖外部修改，
// 收敛回前态（原容器恢复运行、候选删除、事务清除）。
func TestEnvSwitchExternalEditConflict(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	edited := `{"schema_version":1,"name":"demo","image":"img-c:3"}`
	f.onStopHook = func() {
		if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeConfigInvalid) || !strings.Contains(errb, "拒绝提交") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	// 外部修改保持原样
	raw, _ := os.ReadFile(filepath.Join(dir, project.ConfigFileName))
	if string(raw) != edited {
		t.Fatalf("外部修改不应被覆盖: %s", raw)
	}
	// 收敛回前态
	if f.conts[envOldID].state != "running" || len(f.conts) != 1 {
		t.Fatalf("原环境应恢复: state=%s conts=%d", f.conts[envOldID].state, len(f.conts))
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

// P1-3：提交点崩溃 + 外部修改 config → recover 拒绝覆盖并保留事务；
// 恢复外部修改原状后 recover 完成收敛。
func TestEnvRecoverExternalEditConflict(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000004", role: "candidate"})
	f.conts[envOldID].state = "exited"
	cfgA := envConfigA()
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCommitIntent, candID, 1)
	edited := `{"schema_version":1,"name":"tampered","image":"img-x:9"}`
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = cfgA
	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeConfigInvalid) || !strings.Contains(errb, "不覆盖") {
		t.Fatalf("recover 应拒绝覆盖外部修改: code=%d err=%s", code, errb)
	}
	if !envtxn.TransactionExists(dir) {
		t.Fatal("事务必须保留")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, project.ConfigFileName)); string(raw) != edited {
		t.Fatal("外部修改不应被覆盖")
	}
	// 用户恢复外部修改原状后，recover 完成收敛
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(envConfigA()), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb = runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("恢复后应收敛: code=%d err=%s", code, errb)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != candID || st.Env.Generation != 1 {
		t.Fatalf("新态: %+v", st.Container)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

// P2-4：阶段写点 I/O 故障（.km/env 只读）→ 提交点前的失败完整收敛前态：
// 事务清除、候选删除、旧容器恢复运行、状态还原。
func TestEnvSwitchIOFailureAtStageWriteConvergesPreCommit(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	f.onStopHook = func() {
		if err := os.Chmod(envtxn.Dir(dir), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(envtxn.Dir(dir), 0o755) })
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv {
		t.Fatalf("写失败应报错: code=%d err=%s", code, errb)
	}
	// 目录只读时连事务清除都会失败：清理未完成 → 事务保留并给出诊断
	if !envtxn.TransactionExists(dir) {
		t.Fatal("清理未完成时事务必须保留")
	}
	if f.conts[envOldID].state != "running" && f.conts[envOldID].state != "exited" {
		t.Fatalf("旧容器状态异常: %s", f.conts[envOldID].state)
	}
	// 权限恢复后 recover 完成前态收敛
	if err := os.Chmod(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errb = runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	if f.conts[envOldID].state != "running" || len(f.conts) != 1 {
		t.Fatalf("原环境应恢复: state=%s conts=%d", f.conts[envOldID].state, len(f.conts))
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

// P2-4：COMMIT_INTENT 写点（SavePrevious）I/O 故障 → 事务保留并诊断；
// 权限恢复后 recover 幂等完成收敛。
func TestEnvRecoverIOFailureAtSavePreviousThenCompletes(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000006", role: "candidate"})
	f.conts[envOldID].state = "exited"
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCommitIntent, candID, 1)
	if err := os.Chmod(envtxn.Dir(dir), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(envtxn.Dir(dir), 0o755) })
	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitEnv {
		t.Fatalf("SavePrevious 写失败应报错: code=%d err=%s", code, errb)
	}
	if !envtxn.TransactionExists(dir) {
		t.Fatal("提交点后失败：事务必须保留")
	}
	if err := os.Chmod(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errb = runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ImageID != envImgBID || st.Env.Generation != 1 {
		t.Fatalf("新态: %+v", st.Container)
	}
	prev, err := envtxn.LoadPrevious(dir)
	if err != nil || prev.ContainerID != envOldID {
		t.Fatalf("槽位: %+v err=%v", prev, err)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

// P2-4：init 在未完成事务下阻断。
func TestInitBlockedByPendingTxn(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
	code, _, errb := runInit(t, f.docker(), dir)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["create"] != 0 {
		t.Fatal("阻断时不应创建容器")
	}
}

// P2-4：未完成事务不阻断只读恢复入口 sessions（ADR §5.4）。
func TestSessionsUnblockedDuringPendingTxn(t *testing.T) {
	f, dir, sess := setupEnvReady(t)
	writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	code := runSessionsCommand(context.Background(), nil, &out, &errb, f.docker())
	if code != ExitOK {
		t.Fatalf("sessions 不应被事务阻断: code=%d err=%s", code, errb.String())
	}
	if sess.sessionsCalls < 1 {
		t.Fatal("应执行会话查询")
	}
}

// P2-4：CANDIDATE_VERIFIED 阶段崩溃恢复（恢复表全阶段覆盖补齐）。
func TestEnvRecoverCrashAtCandidateVerified(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCandidateVerified, candID, 1)
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if _, ok := f.conts[candID]; ok {
		t.Fatal("候选应删除")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

// P2-4：rollback PREPARED 阶段崩溃恢复（未发生任何变更 → 仅清除事务）。
func TestEnvRecoverRollbackCrashAtPrepared(t *testing.T) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	txn := &envtxn.Transaction{
		OpID: "e0000000000000005", Kind: envtxn.KindRollback, Stage: envtxn.StagePrepared,
		Old:          envtxn.Snapshot{ContainerID: gen1ID, ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID, WasRunning: true, Generation: 1},
		New:          envtxn.Snapshot{ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID, WasRunning: true, Generation: 0},
		ConfigBackup: envtxn.NewFileBackup([]byte(`{"schema_version":1,"name":"demo","image":"` + envImgBRef + `","platform":"linux/arm64"}`)),
		StateBackup:  envtxn.NewFileBackup(readStateBytes(t, dir)),
		PrevImageRef: envImgARef,
	}
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	if !envtxn.PreviousExists(dir) {
		t.Fatal("槽位应保留")
	}
	if f.conts[gen1ID].state != "running" {
		t.Fatal("当前代不应被触碰")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态不应被改动")
	}
}

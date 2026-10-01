package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

func bytesEqual(a, b []byte) bool { return bytes.Equal(a, b) }

func unmarshalJSONFile(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// ---------- P3：真实 switch ----------

func TestEnvSwitchHappyPath(t *testing.T) {
	f, dir, sess := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "已切换") || !strings.Contains(out, "rollback") {
		t.Fatalf("输出缺少结果与回退指引:\n%s", out)
	}
	// 本机状态：v2、指向新代
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.StateVersion != project.SupportedStateVersionEnv || st.Env.Generation != 1 {
		t.Fatalf("应为 v2 第 1 代: %+v", st.Env)
	}
	candName := "km-" + envProjID + "-g1"
	candID := f.names[candName]
	if candID == "" || st.Container.ID != candID || st.Container.Name != candName || st.Container.ImageID != envImgBID {
		t.Fatalf("状态应指向新代: %+v", st.Container)
	}
	// 配置：仅 image 变更，name/platform 保留
	cfg, err := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if err != nil || cfg.Image != envImgBRef || cfg.Name != "demo" || !cfg.PlatformDeclared {
		t.Fatalf("配置: %+v err=%v", cfg, err)
	}
	// 回退槽位：旧容器快照
	prev, err := envtxn.LoadPrevious(dir)
	if err != nil || prev.ContainerID != envOldID || prev.ImageRef != envImgARef || prev.Generation != 0 || !prev.WasRunning {
		t.Fatalf("previous: %+v err=%v", prev, err)
	}
	// Docker 侧：旧容器停止保留，候选运行，探测已删除
	if f.conts[envOldID].state != "exited" {
		t.Fatal("旧容器应停止并保留")
	}
	if f.conts[candID].state != "running" {
		t.Fatal("候选容器应运行")
	}
	if f.hooks["create"] != 2 || f.hooks["stop"] != 1 || f.hooks["rm"] != 1 {
		t.Fatalf("调用次数: %+v", f.hooks)
	}
	if f.lastImageArg != envImgBID {
		t.Fatalf("候选必须按内容 ID 创建: %s", f.lastImageArg)
	}
	if f.lastMount != resolveDir(t, dir)+":/workspace" {
		t.Fatalf("候选挂载: %s", f.lastMount)
	}
	// 账本：无 retained（首次切换），无事务
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应已清除")
	}
	if envtxn.PreviousExists(dir) && prev == nil {
		t.Fatal("unreachable")
	}
	if _, err := envtxn.LoadRetained(dir); !os.IsNotExist(err) {
		t.Fatalf("首次切换不应有 retained: %v", err)
	}
	_ = sess
}

// 相同内容（不同标签）→ no-op：无新代、无停止、不消费槽位。
func TestEnvSwitchNoOp(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.imgs["alias-a:latest"] = envImgAID // 同一内容的不同标签
	prev, _ := os.Getwd()
	_ = prev
	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", "alias-a:latest", "--yes")
	if code != ExitOK || !strings.Contains(out, "no-op") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if f.hooks["create"] != 0 || f.hooks["stop"] != 0 || f.hooks["rm"] != 0 {
		t.Fatalf("no-op 不应触碰容器: %+v", f.hooks)
	}
	if envtxn.PreviousExists(dir) {
		t.Fatal("no-op 不应写入回退槽位")
	}
	st, _ := project.LoadState(dir)
	if st.StateVersion != project.SupportedStateVersion {
		t.Fatal("no-op 不应升版状态")
	}
}

// 探测缺依赖 → 拒绝；候选不创建；探测容器删除；事务收敛回前态。
func TestEnvSwitchProbeMissingDepRefused(t *testing.T) {
	f, dir, sess := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	sess.missingDeps = []string{"setsid"}
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeRuntimeMissing) || !strings.Contains(errb, "setsid") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["create"] != 1 || f.hooks["rm"] != 1 {
		t.Fatalf("只应创建并删除探测容器: %+v", f.hooks)
	}
	if envtxn.TransactionExists(dir) || envtxn.PreviousExists(dir) {
		t.Fatal("事务应收敛清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态文件应还原为 v1 原字节")
	}
	if _, err := os.Stat(filepath.Join(dir, project.ConfigFileName)); err != nil {
		t.Fatal(err)
	}
}

// “Docker 已创建但客户端没拿到结果”：按 op 标签核验登记，不重复创建。
func TestEnvSwitchCreateResponseLost(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	f.createLostRsp = true
	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if f.hooks["create"] != 2 {
		t.Fatalf("不确定创建不得重试: %+v", f.hooks)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ImageID != envImgBID || st.Env.Generation != 1 {
		t.Fatalf("应登记候选: %+v", st.Container)
	}
}

// 停止旧容器失败（提交点前）→ 收敛回前态：候选删除、原容器保留、文件还原。
func TestEnvSwitchStopFailureConverges(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	f.failStopNext = true
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.conts[envOldID].state != "running" {
		t.Fatal("旧容器应保持运行")
	}
	if len(f.conts) != 1 {
		t.Fatalf("候选与探测应删除，实际: %d", len(f.conts))
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应收敛清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态文件应还原")
	}
}

// ---------- P2：各阶段崩溃恢复 ----------

// envV2BumpedState 构造 PREPARED 之后的升版状态（与 v1 身份一致 + env 块）。
func envV2BumpedState(gen int) string {
	base := strings.Replace(envV1State(), `{"state_version":1`, `{"state_version":2,"env":{"env_version":1,"generation":`+fmt.Sprint(gen)+`}`, 1)
	return base
}

func writeTxnFixture(t *testing.T, dir, kind, stage string, newID string, newGen int) *envtxn.Transaction {
	t.Helper()
	txn := &envtxn.Transaction{
		OpID: "e0000000000000002", Kind: kind, Stage: stage,
		Old:            envtxn.Snapshot{ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID, WasRunning: true, Generation: 0},
		New:            envtxn.Snapshot{ContainerID: newID, ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID, WasRunning: true, Generation: newGen},
		ConfigBackup:   envtxn.NewFileBackup([]byte(envConfigA())),
		StateBackup:    envtxn.NewFileBackup(readStateBytes(t, dir)),
		TargetImageRef: envImgBRef,
	}
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}
	return txn
}

func TestEnvRecoverCrashAtPrepared(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	v1Bytes := readStateBytes(t, dir)                          // 升版前的原始 v1 字节
	envWriteProject(t, dir, envConfigA(), envV2BumpedState(0)) // 升版已发生
	txn := writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StagePrepared, "", 1)
	txn.StateBackup = envtxn.NewFileBackup(v1Bytes) // 事务备份 = 升版前状态
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}
	stateBefore := readStateBytes(t, dir)
	_ = stateBefore

	code, out, errb := runEnv(t, f.docker(), dir, "recover", "--dry-run")
	if code != ExitOK || !strings.Contains(out, "前态") {
		t.Fatalf("dry-run: code=%d out=%s err=%s", code, out, errb)
	}
	code, out, errb = runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原为 v1 原字节（而非停留在升版态）")
	}
	var chk map[string]any
	if err := unmarshalJSONFile(after, &chk); err != nil || chk["state_version"].(float64) != 1 {
		t.Fatalf("应还原为 v1: %s %v", after, err)
	}
	// 幂等：再次 recover 无需恢复
	code, out, _ = runEnv(t, f.docker(), dir, "recover")
	if code != ExitOK || !strings.Contains(out, "无需恢复") {
		t.Fatalf("二次恢复: code=%d out=%s", code, out)
	}
}

func TestEnvRecoverCrashAtCandidateCreated(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCandidateCreated, candID, 1)
	stateBefore := readStateBytes(t, dir)

	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if _, ok := f.conts[candID]; ok {
		t.Fatal("候选容器应被删除")
	}
	if f.conts[envOldID].state != "running" {
		t.Fatal("旧容器应保持运行")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

func TestEnvRecoverCrashAtOldStopped(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
	f.conts[envOldID].state = "exited" // 已被停止
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageOldStopped, candID, 1)
	stateBefore := readStateBytes(t, dir)

	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.conts[envOldID].state != "running" {
		t.Fatal("旧容器应按 was_running 恢复运行")
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

func TestEnvRecoverCrashAtCommitIntent(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
	f.conts[envOldID].state = "exited"
	envWriteProject(t, dir, envConfigA(), envV2BumpedState(0)) // 状态仍是升版前态内容
	writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCommitIntent, candID, 1)

	code, out, errb := runEnv(t, f.docker(), dir, "recover", "--dry-run")
	if code != ExitOK || !strings.Contains(out, "新态") {
		t.Fatalf("dry-run: code=%d out=%s err=%s", code, out, errb)
	}
	code, _, errb = runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	// 新态：config/state 指向候选，槽位指向旧容器，候选运行，旧容器停止
	cfg, err := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if err != nil || cfg.Image != envImgBRef {
		t.Fatalf("config: %+v err=%v", cfg, err)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != candID || st.Env.Generation != 1 {
		t.Fatalf("state: %+v", st)
	}
	prev, err := envtxn.LoadPrevious(dir)
	if err != nil || prev.ContainerID != envOldID || prev.ImageRef != envImgARef {
		t.Fatalf("previous: %+v err=%v", prev, err)
	}
	if f.conts[candID].state != "running" || f.conts[envOldID].state != "exited" {
		t.Fatal("候选应运行、旧容器应停止")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

func TestEnvRecoverCrashAtCurrentCommitted(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
	f.conts[envOldID].state = "exited"
	// 文件已一致指向新代
	envWriteProject(t, dir, `{"schema_version":1,"name":"demo","image":"`+envImgBRef+`","platform":"linux/arm64"}`, "")
	txn := writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCurrentCommitted, candID, 1)
	stRaw := fmt.Sprintf(`{"state_version":2,"project_id":%q,"container":{"id":%q,"name":"km-%s-g1","image_id":%q},"runtime":{"context":"desktop-linux","endpoint":%q},"created_at":"2026-01-01T00:00:00Z","env":{"env_version":1,"generation":1}}`, envProjID, candID, envProjID, envImgBID, sdEndpoint)
	envWriteProject(t, dir, `{"schema_version":1,"name":"demo","image":"`+envImgBRef+`","platform":"linux/arm64"}`, stRaw)
	_ = txn

	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != candID {
		t.Fatalf("新态应保持: %+v", st.Container)
	}
}

func TestEnvRecoverRollbackPreAndPostCommit(t *testing.T) {
	t.Run("提交点前恢复当前代", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		prevID := f.addContainer(envContainer{name: "km-" + envProjID + "-g0x", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir)})
		savePrevFixture(t, dir, prevID, "km-"+envProjID+"-g0x", 0)
		f.conts[envOldID].state = "exited" // 已停止
		txn := writeTxnFixture(t, dir, envtxn.KindRollback, envtxn.StageOldStopped, prevID, 0)
		txn.New.ImageID = envImgAID
		txn.New.ContainerName = "km-" + envProjID + "-g0x"
		txn.New.WasRunning = false
		txn.PrevImageRef = envImgARef
		if err := envtxn.SaveTransaction(dir, txn); err != nil {
			t.Fatal(err)
		}
		stateBefore := readStateBytes(t, dir)
		code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
		if code != ExitOK {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		if f.conts[envOldID].state != "running" {
			t.Fatal("当前代容器应恢复运行")
		}
		if !envtxn.PreviousExists(dir) {
			t.Fatal("回退槽位应保留")
		}
		if envtxn.TransactionExists(dir) {
			t.Fatal("事务应清除")
		}
		after, _ := os.ReadFile(project.StatePath(dir))
		if !bytesEqual(stateBefore, after) {
			t.Fatal("状态应还原")
		}
	})
	t.Run("提交点后完成回退", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		prevID := f.addContainer(envContainer{name: "km-" + envProjID + "-g0x", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir)})
		savePrevFixture(t, dir, prevID, "km-"+envProjID+"-g0x", 0)
		f.conts[envOldID].state = "exited"
		txn := writeTxnFixture(t, dir, envtxn.KindRollback, envtxn.StageCommitIntent, prevID, 0)
		txn.New.ImageID = envImgAID
		txn.New.ContainerName = "km-" + envProjID + "-g0x"
		txn.New.WasRunning = false
		txn.PrevImageRef = envImgARef
		if err := envtxn.SaveTransaction(dir, txn); err != nil {
			t.Fatal(err)
		}
		stateBefore := readStateBytes(t, dir)
		_ = stateBefore
		code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
		if code != ExitOK {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		st, _ := project.LoadState(dir)
		if st.Container.ID != prevID || st.Env.Generation != 0 {
			t.Fatalf("状态应指向上一代: %+v", st.Container)
		}
		if envtxn.PreviousExists(dir) {
			t.Fatal("回退槽位应消费")
		}
		r, err := envtxn.LoadRetained(dir)
		if err != nil || len(r.Containers) != 1 || r.Containers[0].ContainerID != envOldID {
			t.Fatalf("retained 应记录被撤容器: %+v err=%v", r, err)
		}
		if envtxn.TransactionExists(dir) {
			t.Fatal("事务应清除")
		}
	})
}

// 资源身份无法确认 → 停止写操作，保留事务（ADR §4）。
func TestEnvRecoverRefusesWhenIdentityUnresolvable(t *testing.T) {
	t.Run("提交点后候选消失", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		f.addImage(envImgBRef, envImgBID)
		f.conts[envOldID].state = "exited"
		candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
		writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageCommitIntent, candID, 1)
		delete(f.conts, candID) // 外部删除候选
		stateBefore := readStateBytes(t, dir)
		code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeResourceUnknown) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		if !envtxn.TransactionExists(dir) {
			t.Fatal("事务必须保留")
		}
		after, _ := os.ReadFile(project.StatePath(dir))
		if !bytesEqual(stateBefore, after) {
			t.Fatal("不应改写文件")
		}
	})
	t.Run("提交点前旧容器消失", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		candID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: resolveDir(t, dir), opID: "e0000000000000002", role: "candidate"})
		f.conts[envOldID].state = "exited"
		writeTxnFixture(t, dir, envtxn.KindSwitch, envtxn.StageOldStopped, candID, 1)
		delete(f.conts, envOldID) // 外部删除旧容器
		code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeResourceUnknown) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		if !envtxn.TransactionExists(dir) {
			t.Fatal("事务必须保留")
		}
	})
}

// ---------- P4：switch→switch→rollback→rollback ----------

func TestEnvFullCycleSwitchSwitchRollbackRollback(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)

	// 共享文件：验证“环境回退不回滚项目文件”
	marker := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(marker, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// switch A→B（第 1 代）
	if code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes"); code != ExitOK {
		t.Fatalf("switch1: %d %s", code, errb)
	}
	gen1ID := f.names["km-"+envProjID+"-g1"]

	// 修改共享文件（新环境运行期间）
	if err := os.WriteFile(marker, []byte("v2-edited-in-B"), 0o644); err != nil {
		t.Fatal(err)
	}

	// switch B→A（第 2 代；gen0 转入 retained 轮换）
	if code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgARef, "--yes"); code != ExitOK {
		t.Fatalf("switch2: %d %s", code, errb)
	}
	r, err := envtxn.LoadRetained(dir)
	if err != nil || len(r.Containers) != 1 || r.Containers[0].ContainerID != envOldID ||
		r.Containers[0].Reason != envtxn.ReasonSupersededBySwitch {
		t.Fatalf("gen0 应转入 retained: %+v err=%v", r, err)
	}
	gen2ID := f.names["km-"+envProjID+"-g2"]
	prev, _ := envtxn.LoadPrevious(dir)
	if prev.ContainerID != gen1ID || prev.Generation != 1 || prev.ImageRef != envImgBRef {
		t.Fatalf("槽位应为 gen1: %+v", prev)
	}

	// 共享文件改动仍在
	if b, _ := os.ReadFile(marker); string(b) != "v2-edited-in-B" {
		t.Fatal("切换不应回滚项目文件")
	}

	// rollback：回到 gen1（B 容器），gen2 入 retained，槽位消费
	if code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes"); code != ExitOK {
		t.Fatalf("rollback1: %d %s", code, errb)
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != gen1ID || st.Env.Generation != 1 {
		t.Fatalf("应回退到第 1 代: %+v", st.Container)
	}
	cfg, _ := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if cfg.Image != envImgBRef {
		t.Fatalf("config 应恢复为 %s: %+v", envImgBRef, cfg)
	}
	if f.conts[gen1ID].state != "running" {
		t.Fatal("回退目标容器应按 was_running 恢复运行")
	}
	if f.conts[gen2ID].state != "exited" {
		t.Fatal("被撤下的容器应停止保留")
	}
	if envtxn.PreviousExists(dir) {
		t.Fatal("槽位应已消费")
	}
	r, _ = envtxn.LoadRetained(dir)
	if len(r.Containers) != 2 {
		t.Fatalf("retained 应有 gen0+gen2: %+v", r)
	}
	// 文件改动在回退后仍然保留
	if b, _ := os.ReadFile(marker); string(b) != "v2-edited-in-B" {
		t.Fatal("回退不应回滚项目文件")
	}

	// 再次 rollback：无可回退记录，环境不再变动
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeNoPrevious) {
		t.Fatalf("二次 rollback: code=%d err=%s", code, errb)
	}
	if f.hooks["stop"] != 3 { // switch1 + switch2 + rollback1 各停止一次
		t.Fatalf("stop 次数: %d", f.hooks["stop"])
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("无可回退时不应修改状态")
	}
}

package cli

import (
	"os"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// ---------- S2/S3：km env remove 测试 ----------

func envRemoveRun(t *testing.T, f *envFake, dir string, args ...string) (int, string, string) {
	t.Helper()
	return runEnv(t, f.docker(), dir, append([]string{"remove"}, args...)...)
}

// makeRetainedFixture：post-switch 项目（gen1 当前，gen0 在槽位），并把 gen0
// 移入 retained（模拟"槽位被更高代替换后"的典型 retained 场景）。
func makeRetainedFixture(t *testing.T) (*envFake, string, string) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	if err := envtxn.RemovePrevious(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
		ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID,
		Generation: 0, Reason: envtxn.ReasonSupersededBySwitch, OpID: "e00000000000000aa",
	}); err != nil {
		t.Fatal(err)
	}
	f.conts[envOldID].state = "exited"
	return f, dir, gen1ID
}

// 合法路径：exited retained → 普通 rm（无 -f/-v）→ 账本仅移除目标条目。
func TestEnvRemoveHappyPath(t *testing.T) {
	f, dir, gen1ID := makeRetainedFixture(t)
	code, out, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "已删除容器") || !strings.Contains(out, "superseded-by-switch") {
		t.Fatalf("输出应含删除结果与原因:\n%s", out)
	}
	// 普通 rm argv 断言由 runtime 层测试覆盖；这里断言调用次数与容器消失
	if f.hooks["rm"] != 1 || f.hooks["stop"] != 0 {
		t.Fatalf("调用次数: %+v", f.hooks)
	}
	if _, ok := f.conts[envOldID]; ok {
		t.Fatal("目标容器应已删除")
	}
	if _, ok := f.conts[gen1ID]; !ok {
		t.Fatal("其他容器不得被触碰")
	}
	// 账本：目标条目移除
	ret, err := envtxn.LoadRetained(dir)
	if err != nil || len(ret.Containers) != 0 {
		t.Fatalf("账本应仅剩 0 条: %+v err=%v", ret, err)
	}
	// 事务清除；状态/槽位不受影响
	if envtxn.TransactionExists(dir) || envtxn.PreviousExists(dir) {
		t.Fatal("事务应清除且槽位不受影响")
	}
	st, _ := project.LoadState(dir)
	if st.Container.ID != gen1ID {
		t.Fatalf("当前代不受影响: %+v", st.Container)
	}
}

// dry-run：零 docker 变更、零写入。
func TestEnvRemoveDryRun(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	stateBefore := readStateBytes(t, dir)
	code, out, errb := envRemoveRun(t, f, dir, envOldID, "--dry-run")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	for _, want := range []string{"普通 docker rm", "永久丢失", "不会删除共享项目文件，也不会删除镜像", "允许执行"} {
		if !strings.Contains(out, want) {
			t.Fatalf("预览缺少 %q:\n%s", want, out)
		}
	}
	noContainerMutations(t, f, dir)
	if envtxn.TransactionExists(dir) {
		t.Fatal("dry-run 不得写事务")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("dry-run 不得改状态")
	}
	ret, _ := envtxn.LoadRetained(dir)
	if len(ret.Containers) != 1 {
		t.Fatal("dry-run 不得改账本")
	}
}

// 保护矩阵：当前代/回退目标/事务资源一律拒绝且零 docker 变更。
func TestEnvRemoveProtectedTargets(t *testing.T) {
	t.Run("当前代", func(t *testing.T) {
		f, dir, gen1ID := makeRetainedFixture(t)
		if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
			ContainerID: gen1ID, ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID,
			Generation: 1, Reason: envtxn.ReasonRolledBack, OpID: "e00000000000000bb",
		}); err != nil {
			t.Fatal(err)
		}
		f.conts[gen1ID].state = "exited" // 即使恰好 exited，受保护仍然拒绝
		code, _, errb := envRemoveRun(t, f, dir, gen1ID, "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) || !strings.Contains(errb, "受保护") {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noContainerMutations(t, f, dir)
	})
	t.Run("回退目标", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t) // gen0 在槽位
		code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
		if code != ExitEnv || !strings.Contains(errb, "受保护") {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noContainerMutations(t, f, dir)
	})
	t.Run("事务资源", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		writeEnvTxnFixture(t, dir, envtxn.StagePrepared) // 引用 envOldID
		code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
		// 事务存在本身先阻断（KM_TRANSACTION_PENDING）
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
	})
}

// 跨项目：B 的真实 retained ID 不能从 A 删除，B 原样。
func TestEnvRemoveCrossProjectRefused(t *testing.T) {
	fA, dirA, _ := setupEnvReady(t)
	// B 项目的 retained 容器使用独立 ID（envOldID 恰是 A 的当前代，属保护集合）
	fB, dirB, _, _ := setupEnvPostSwitch(t)
	fB.addImage(envImgARef, envImgAID)
	crossID := fB.addContainer(envContainer{
		id: strings.Repeat("e", 64), name: "km-bproject", state: "exited",
		project: envProjID, image: envImgAID, mount: resolveDir(t, dirB),
	})
	if err := envtxn.RemovePrevious(dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := envtxn.AppendRetained(dirB, envtxn.RetainedEntry{
		ContainerID: crossID, ContainerName: "km-bproject", ImageID: envImgAID,
		Generation: 1, Reason: envtxn.ReasonRolledBack, OpID: "e00000000000000cc",
	}); err != nil {
		t.Fatal(err)
	}
	code, _, errb := envRemoveRun(t, fA, dirA, crossID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeNotFound) {
		t.Fatalf("跨项目 ID 应按未知目标拒绝: code=%d err=%s", code, errb)
	}
	if fA.hooks["rm"] != 0 {
		t.Fatal("A 不得触碰 B 的容器")
	}
	if _, ok := fB.conts[crossID]; !ok {
		t.Fatal("B 的容器应原样保留")
	}
	retB, gerr := envtxn.LoadRetained(dirB)
	if gerr != nil || len(retB.Containers) != 1 || retB.Containers[0].ContainerID != crossID {
		t.Fatalf("B 的账本应原样: %+v err=%v", retB, gerr)
	}
}

func TestEnvRemoveRunningRefused(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.conts[envOldID].state = "running"
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) || !strings.Contains(errb, "running") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["stop"] != 0 || f.hooks["rm"] != 0 {
		t.Fatal("不得自动 stop 或 rm")
	}
}

// 身份不符：镜像被外部替换 → 拒绝。
func TestEnvRemoveIdentityMismatchRefused(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.conts[envOldID].image = envImgBID
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	noContainerMutations(t, f, dir)
}

// 用法：短 ID/名称/多目标/未知选项 → 退出码 2，零变更。
func TestEnvRemoveUsageErrors(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	cases := [][]string{
		{"aaaa11112222", "--yes"},     // 短 ID
		{"km-" + envProjID, "--yes"},  // 名称
		{envOldID, envOldID, "--yes"}, // 多目标
		{envOldID, "--force"},         // 未知选项
		{envOldID, "extra"},           // 多余位置参数
		{},                            // 缺目标
	}
	for _, args := range cases {
		code, _, errb := envRemoveRun(t, f, dir, args...)
		if code != ExitUsage || !strings.Contains(errb, runtime.CodeUsage) {
			t.Fatalf("%v: code=%d err=%s", args, code, errb)
		}
	}
	noContainerMutations(t, f, dir)
}

// 非交互缺 --yes → 用法错误；拒绝确认 → KM_CANCELED 退出 1，零变更。
func TestEnvRemoveConfirmation(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	oldTTY := envStdinIsTTY
	envStdinIsTTY = func() bool { return false }
	defer func() { envStdinIsTTY = oldTTY }()
	code, _, errb := envRemoveRun(t, f, dir, envOldID)
	if code != ExitUsage || !strings.Contains(errb, "--yes") {
		t.Fatalf("非交互缺 --yes: code=%d err=%s", code, errb)
	}
	noContainerMutations(t, f, dir)

	f2, dir2, _ := makeRetainedFixture(t)
	envStdinIsTTY = func() bool { return true }
	envStdin = strings.NewReader("no\n")
	code, _, errb = envRemoveRun(t, f2, dir2, envOldID)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeCanceled) {
		t.Fatalf("拒绝确认: code=%d err=%s", code, errb)
	}
	noContainerMutations(t, f2, dir2)
}

// 失效记录：目标被外部删除 → 仅清理记录，不执行容器删除；
// 同名新容器存在时不得误删。
func TestEnvRemoveStaleRecord(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	delete(f.conts, envOldID) // 外部删除
	// 同名新容器（不同 ID、镜像也不符）
	f.addContainer(envContainer{name: "km-" + envProjID, state: "exited", project: envProjID, image: envImgBID, mount: resolveDir(t, dir)})
	code, out, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitOK || !strings.Contains(out, "仅清理失效账本记录") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if f.hooks["rm"] != 0 {
		t.Fatal("失效记录清理不得执行容器删除")
	}
	ret, err := envtxn.LoadRetained(dir)
	if err != nil || len(ret.Containers) != 0 {
		t.Fatalf("失效条目应移除: %+v err=%v", ret, err)
	}
	// 同名新容器不受影响
	for _, c := range f.conts {
		if c.name == "km-"+envProjID {
			return
		}
	}
	t.Fatal("同名新容器不得被删除")
}

// 响应丢失：rm 在 daemon 侧成功但客户端收到错误 → 复核实态后按成功收尾。
func TestEnvRemoveResponseLost(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.rmLostResponse = true // rm 删除容器但返回错误
	code, out, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitOK {
		t.Fatalf("响应丢失应按实态收尾: code=%d out=%s err=%s", code, out, errb)
	}
	if f.hooks["rm"] != 1 {
		t.Fatalf("不得重复删除: %+v", f.hooks)
	}
	ret, _ := envtxn.LoadRetained(dir)
	if len(ret.Containers) != 0 {
		t.Fatal("账本应收尾")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

// rm 失败且容器仍在（资格未变）→ 事务保留于 REMOVE_REQUESTED；
// 显式 recover 重试成功。
func TestEnvRemoveRmFailureThenRecoverRetries(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.failRmNext = true
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeResourceUnknown) {
		t.Fatalf("rm 失败应非零且保留事务: code=%d err=%s", code, errb)
	}
	txn, err := envtxn.LoadTransaction(dir)
	if err != nil || txn.Kind != envtxn.KindRemove || txn.Stage != envtxn.StageRemoveRequested {
		t.Fatalf("事务应保留于 REMOVE_REQUESTED: %+v err=%v", txn, err)
	}
	if _, ok := f.conts[envOldID]; !ok {
		t.Fatal("容器应仍在")
	}
	// recover 确认后重试（第二次 rm 成功）
	if code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes"); code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	if _, ok := f.conts[envOldID]; ok {
		t.Fatal("recover 应完成删除")
	}
	ret, _ := envtxn.LoadRetained(dir)
	if len(ret.Containers) != 0 {
		t.Fatal("账本应收尾")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

// rm 失败且目标变为运行中 → recover 停止写操作，事务保留，不强制删除。
func TestEnvRemoveRecoverRefusesWhenEligibilityLost(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.failRmNext = true
	if code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes"); code != ExitEnv {
		t.Fatalf("rm 失败: code=%d err=%s", code, errb)
	}
	// 外部把目标启动了
	f.conts[envOldID].state = "running"
	code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) || !strings.Contains(errb, "仅允许删除 exited") {
		t.Fatalf("recover 应拒绝: code=%d err=%s", code, errb)
	}
	if !envtxn.TransactionExists(dir) {
		t.Fatal("事务必须保留")
	}
	if f.conts[envOldID].state != "running" {
		t.Fatal("不得强制删除运行中的容器")
	}
}

// journal 写失败 → 不得发 Docker 删除（ADR §6：写 journal 失败不删）。
func TestEnvRemoveJournalWriteFailureNoDelete(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	if err := os.Chmod(envtxn.Dir(dir), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(envtxn.Dir(dir), 0o755) })
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, "未执行任何删除") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["rm"] != 0 {
		t.Fatal("journal 写失败时不得发 rm")
	}
	if _, ok := f.conts[envOldID]; !ok {
		t.Fatal("容器应原样保留")
	}
}

// 删除成功但账本写入失败 → 报告"容器已删除，记录待收尾"，
// 事务保留；权限恢复后 recover 完成收尾。
func TestEnvRemoveLedgerWriteFailure(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.onRmHook = func() {
		if err := os.Chmod(envtxn.Dir(dir), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(envtxn.Dir(dir), 0o755) })
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv {
		t.Fatalf("删除后账本写失败应非零: code=%d err=%s", code, errb)
	}
	if _, ok := f.conts[envOldID]; ok {
		t.Fatal("容器已被删除（不可逆）")
	}
	if !envtxn.TransactionExists(dir) {
		t.Fatal("事务必须保留")
	}
	// 权限恢复后 recover 完成账本收尾
	if err := os.Chmod(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runEnv(t, f.docker(), dir, "recover", "--yes"); code != ExitOK {
		t.Fatalf("recover: code=%d err=%s", code, errb)
	}
	ret, _ := envtxn.LoadRetained(dir)
	if len(ret.Containers) != 0 {
		t.Fatal("账本应收尾")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应清除")
	}
}

// 账本被外部编辑（新增条目）后收尾：仅移除目标条目，外部编辑保留。
func TestEnvRemovePreservesExternalLedgerEdits(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.onRmHook = func() {
		// rm 之后、账本收尾之前，外部追加一条无关条目
		_, _ = envtxn.AppendRetained(dir, envtxn.RetainedEntry{
			ContainerID: strings.Repeat("d", 64), ContainerName: "km-external", ImageID: envImgBID,
			Generation: 0, Reason: envtxn.ReasonRolledBack, OpID: "e00000000000000dd",
		})
	}
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	ret, err := envtxn.LoadRetained(dir)
	if err != nil || len(ret.Containers) != 1 || ret.Containers[0].ContainerID != strings.Repeat("d", 64) {
		t.Fatalf("外部编辑的条目必须保留: %+v err=%v", ret, err)
	}
}

// 完成后再次删除同一 ID → 按未知目标处理（幂等恢复 ≠ 任意未知 ID 可删）。
func TestEnvRemoveTwiceIsUnknownTarget(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	if code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes"); code != ExitOK {
		t.Fatalf("首次删除: %d %s", code, errb)
	}
	code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeNotFound) {
		t.Fatalf("二次删除应按未知目标拒绝: code=%d err=%s", code, errb)
	}
}

// remove 期间 pending 事务阻断 switch（旧客户端门禁由事务 kind=remove 的
// fail-closed 校验保证，另见真实旧二进制实测）。
func TestEnvRemovePendingBlocksSwitch(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.failRmNext = true
	if code, _, _ := envRemoveRun(t, f, dir, envOldID, "--yes"); code != ExitEnv {
		t.Fatal("前置：rm 失败制造 pending remove")
	}
	f.addImage(envImgBRef, envImgBID)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
		t.Fatalf("pending remove 应阻断 switch: code=%d err=%s", code, errb)
	}
}

// finishRemoveLedger 的"记录待收尾"分支：账本写入失败时的确切错误语义。
func TestFinishRemoveLedgerWriteFailureMessage(t *testing.T) {
	dir := t.TempDir()
	// 夹具：retained 账本含目标条目
	if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
		ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID,
		Generation: 0, Reason: envtxn.ReasonRolledBack, OpID: "e00000000000000ee",
	}); err != nil {
		t.Fatal(err)
	}
	txn := &envtxn.Transaction{
		OpID: "e00000000000000ee", Kind: envtxn.KindRemove, Stage: envtxn.StageAbsenceConfirmed,
		Old:          envtxn.Snapshot{ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID, Generation: 0},
		New:          envtxn.Snapshot{ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID, Generation: 0},
		LedgerBackup: envtxn.NewFileBackup([]byte(`{"env_version":1,"containers":[]}`)),
		RemoveReason: envtxn.ReasonRolledBack,
	}
	if err := os.Chmod(envtxn.Dir(dir), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(envtxn.Dir(dir), 0o755) })
	err := finishRemoveLedger(dir, txn)
	if err == nil || !strings.Contains(err.Error(), "记录待收尾") {
		t.Fatalf("应报告记录待收尾: %v", err)
	}
	// 权限恢复后幂等完成
	if err := os.Chmod(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := finishRemoveLedger(dir, txn); err != nil {
		t.Fatalf("恢复后收尾: %v", err)
	}
	ret, gerr := envtxn.LoadRetained(dir)
	if gerr != nil || len(ret.Containers) != 0 {
		t.Fatalf("目标条目应移除: %+v err=%v", ret, gerr)
	}
}

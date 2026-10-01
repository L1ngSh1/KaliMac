package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// ---------- S1：km env list 负向与只读性测试 ----------

func envListRun(t *testing.T, f *envFake, dir string) (int, string, string) {
	t.Helper()
	return runEnv(t, f.docker(), dir, "list")
}

// v1 项目：仅显示当前容器，不触发迁移、不虚构记录、零写入。
func TestEnvListV1Project(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	stateBefore := readStateBytes(t, dir)
	code, out, errb := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "CURRENT") || !strings.Contains(out, "km-"+envProjID) {
		t.Fatalf("应显示 CURRENT:\n%s", out)
	}
	if strings.Contains(out, "RETAINED") || strings.Contains(out, "PREVIOUS") {
		t.Fatalf("v1 项目不应虚构角色:\n%s", out)
	}
	if _, err := os.Stat(envtxn.Dir(dir)); !os.IsNotExist(err) {
		t.Fatalf("list 不得写入 .km/env: %v", err)
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("list 不得改动状态文件")
	}
	noContainerMutations(t, f, dir)
}

// 多角色：切换后 CURRENT(gen1) + PREVIOUS(gen0)；回退后 CURRENT + RETAINED 可删除。
func TestEnvListRolesAfterSwitchAndRollback(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	if code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes"); code != ExitOK {
		t.Fatalf("switch: %d %s", code, errb)
	}
	code, out, _ := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("list: code=%d", code)
	}
	if !strings.Contains(out, "CURRENT") || !strings.Contains(out, "km-"+envProjID+"-g1") {
		t.Fatalf("应显示当前代:\n%s", out)
	}
	if !strings.Contains(out, "PREVIOUS") || !strings.Contains(out, "受保护") {
		t.Fatalf("PREVIOUS 应受保护:\n%s", out)
	}
	// 回退后：gen1 容器进入 retained（exited）→ REMOVABLE=是
	if code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes"); code != ExitOK {
		t.Fatalf("rollback: %d %s", code, errb)
	}
	code, out, _ = envListRun(t, f, dir)
	if code != ExitOK || !strings.Contains(out, "RETAINED") || !strings.Contains(out, "rolled-back") {
		t.Fatalf("应显示 RETAINED:\n%s", out)
	}
	if !strings.Contains(out, "km env remove <完整ID>）") {
		t.Fatalf("exited retained 应标记可删除:\n%s", out)
	}
	if strings.Contains(out, "发现账本外") {
		t.Fatalf("list 不应出现账本外语录（那是 doctor 文案）:\n%s", out)
	}
}

// MISSING：retained 条目指向已删除容器 → 显示 MISSING + 失效记录提示，
// 退出码 0，且绝不顺手清理（零 docker 变更）。
func TestEnvListMissingRetainedEntry(t *testing.T) {
	f, dir, _, _ := setupEnvPostSwitch(t)
	// gen0（retained 槽位指向它）被外部删除……
	// setupEnvPostSwitch: previous=envOldID（gen0），当前 gen1
	if err := envtxn.RemovePrevious(dir); err != nil {
		t.Fatal(err)
	}
	// 重新放置槽位指向的容器到 retained 账本（模拟真实 retained 条目）
	if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
		ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID,
		Generation: 0, Reason: envtxn.ReasonSupersededBySwitch, OpID: "e00000000000000aa",
	}); err != nil {
		t.Fatal(err)
	}
	delete(f.conts, envOldID) // 外部删除

	code, out, errb := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("MISSING 条目应 exit 0: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "MISSING") || !strings.Contains(out, "失效记录") {
		t.Fatalf("应显示 MISSING 与失效记录提示:\n%s", out)
	}
	noContainerMutations(t, f, dir)
}

// 枚举失败（ps 不可达）：exit 1、标注异常，且不宣称任何资源可删除；
// 已知容器的逐项 inspect 成功时行状态仍如实显示。
func TestEnvListEnumerationFailure(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	d := &runtime.Docker{Exec: &fakeExecutor{respond: func(args []string) ([]byte, []byte, error) {
		if args[1] == "ps" {
			return envFail("Cannot connect to the Docker daemon", 1)
		}
		return f.docker().Exec.(*fakeExecutor).respond(args)
	}}}
	code, out, errb := runEnv(t, d, dir, "list")
	if code != ExitEnv {
		t.Fatalf("枚举失败应 exit 1: code=%d err=%s", code, errb)
	}
	if strings.Contains(out, "是（km env remove") {
		t.Fatalf("枚举失败不得宣称可删除:\n%s", out)
	}
	if !strings.Contains(out, "记录或查询存在异常") {
		t.Fatalf("应标注异常:\n%s", out)
	}
}

// 逐项查询失败（inspect 不可达）≠ MISSING：行显示 UNKNOWN，exit 1。
func TestEnvListInspectFailureIsUnknown(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	d := &runtime.Docker{Exec: &fakeExecutor{respond: func(args []string) ([]byte, []byte, error) {
		if args[1] == "container" && args[2] == "inspect" {
			return envFail("Cannot connect to the Docker daemon", 1)
		}
		return f.docker().Exec.(*fakeExecutor).respond(args)
	}}}
	code, out, errb := runEnv(t, d, dir, "list")
	if code != ExitEnv || !strings.Contains(out, "UNKNOWN") {
		t.Fatalf("查询失败应显示 UNKNOWN 且 exit 1: code=%d out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "MISSING") {
		t.Fatalf("查询失败不得显示为 MISSING:\n%s", out)
	}
}

// 未完成事务 → TRANSACTION 行 + 退出码 1（不把中间态当健康总览）。
func TestEnvListPendingTxnExit1(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
	code, out, errb := envListRun(t, f, dir)
	if code != ExitEnv {
		t.Fatalf("待恢复事务应 exit 1: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "TRANSACTION") || !strings.Contains(out, "km env recover") {
		t.Fatalf("应显示事务资源与恢复入口:\n%s", out)
	}
	noContainerMutations(t, f, dir)
}

// 损坏账本 → exit 1，原样保留。
func TestEnvListDamagedRetained(t *testing.T) {
	f, dir, _, _ := setupEnvPostSwitch(t)
	if err := os.MkdirAll(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envtxn.RetainedPath(dir), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb := envListRun(t, f, dir)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeStateInvalid) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	after, _ := os.ReadFile(envtxn.RetainedPath(dir))
	if string(after) != "{broken" {
		t.Fatal("损坏文件必须原样保留")
	}
}

// 角色冲突：同一 ID 同时出现在 previous 与 retained → CONFLICT，保护优先。
func TestEnvListRoleConflictProtected(t *testing.T) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
		ContainerID: gen1ID, ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID,
		Generation: 1, Reason: envtxn.ReasonRolledBack, OpID: "e00000000000000bb",
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := envListRun(t, f, dir)
	if code != ExitEnv {
		t.Fatalf("角色冲突应 exit 1: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "CONFLICT") || !strings.Contains(out, "保护优先") {
		t.Fatalf("应显示 CONFLICT 与保护说明:\n%s", out)
	}
	if strings.Contains(out, "是（km env remove") {
		t.Fatalf("冲突资源不得标记可删除:\n%s", out)
	}
	_ = gen1ID
}

// 身份不符（外部篡改镜像/名称）→ CONFLICT。
func TestEnvListIdentityConflict(t *testing.T) {
	f, dir, _, _ := setupEnvPostSwitch(t)
	f.conts[envOldID].name = "km-renamed"
	code, out, errb := envListRun(t, f, dir)
	if code != ExitEnv || !strings.Contains(out, "CONFLICT") {
		t.Fatalf("code=%d err=%s out=%s", code, errb, out)
	}
}

// UNTRACKED：带项目标签但不在记录中 → 仅报告，不可删除，exit 0。
func TestEnvListUntrackedReported(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addContainer(envContainer{name: "km-" + envProjID + "-mystery", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir)})
	code, out, errb := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("UNTRACKED 不应导致失败: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "UNTRACKED") || !strings.Contains(out, "仅报告") {
		t.Fatalf("应显示 UNTRACKED 行:\n%s", out)
	}
}

// probe-cleanup-failed 条目：只读挂载核验（可删除性判定依赖角色核验）。
func TestEnvListProbeEntryReadonlyCheck(t *testing.T) {
	f, dir, _, _ := setupEnvPostSwitch(t)
	probeID := f.addContainer(envContainer{name: "km-probe-e00000000000000cc", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir), opID: "e00000000000000cc", role: "probe"})
	if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
		ContainerID: probeID, ContainerName: "km-probe-e00000000000000cc", ImageID: envImgAID,
		Generation: 0, Reason: envtxn.ReasonProbeCleanupFailed, OpID: "e00000000000000cc",
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "probe-cleanup-failed") {
		t.Fatalf("应显示探测条目原因:\n%s", out)
	}
	_ = filepath.Join
}

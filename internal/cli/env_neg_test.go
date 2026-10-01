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

// ---------- P1：参数与确认（退出码 2） ----------

func TestEnvSwitchArgErrors(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	cases := []struct {
		name string
		args []string
	}{
		{"缺 --image", []string{"switch"}},
		{"--image 空值", []string{"switch", "--image", ""}},
		{"--image 空赋值", []string{"switch", "--image="}},
		{"--image 重复", []string{"switch", "--image", "x:1", "--image", "y:2"}},
		{"未知参数", []string{"switch", "--image", "x:1", "--bogus"}},
		{"位置参数", []string{"switch", "extra"}},
		{"dry-run 重复", []string{"switch", "--image", "x:1", "--dry-run", "--dry-run"}},
		{"rollback 带 --image", []string{"rollback", "--image", "x:1"}},
		{"rollback 带位置参数", []string{"rollback", "abc"}},
		{"recover 带未知参数", []string{"recover", "--bogus"}},
		{"无子命令", nil},
		{"未知子命令", []string{"frobnicate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			args = append(args, tc.args...)
			code, _, errb := runEnv(t, f.docker(), dir, args...)
			if code != ExitUsage || !strings.Contains(errb, runtime.CodeUsage) {
				t.Fatalf("应退出码 2: code=%d err=%s", code, errb)
			}
			noMutations(t, f, dir, nil)
		})
	}
}

// 非交互环境执行变更必须显式 --yes，在变更前返回用法错误（ADR §1）。
func TestEnvNonInteractiveRequiresYes(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	oldTTY := envStdinIsTTY
	envStdinIsTTY = func() bool { return false }
	defer func() { envStdinIsTTY = oldTTY }()

	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef)
	if code != ExitUsage || !strings.Contains(errb, "--yes") {
		t.Fatalf("非交互缺 --yes 应为用法错误: code=%d err=%s", code, errb)
	}
	noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))

	// --dry-run 不需要 --yes
	f.addImage(envImgBRef, envImgBID)
	code, out, _ := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--dry-run")
	if code != ExitOK || !strings.Contains(out, "预览") {
		t.Fatalf("dry-run 应允许非交互: code=%d out=%s", code, out)
	}
}

// 用户拒绝确认：冻结为退出码 1 + KM_CANCELED，明确未执行。
func TestEnvDeclinedConfirmation(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	oldTTY, oldStdin := envStdinIsTTY, envStdin
	envStdinIsTTY = func() bool { return true }
	envStdin = strings.NewReader("no\n")
	defer func() { envStdinIsTTY, envStdin = oldTTY, oldStdin }()

	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeCanceled) || !strings.Contains(errb, "未执行任何变更") {
		t.Fatalf("拒绝确认: code=%d err=%s", code, errb)
	}
	noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))
}

func TestEnvConfirmedViaPrompt(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	oldTTY, oldStdin := envStdinIsTTY, envStdin
	envStdinIsTTY = func() bool { return true }
	envStdin = strings.NewReader("yes\n")
	defer func() { envStdinIsTTY, envStdin = oldTTY, oldStdin }()

	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef)
	if code != ExitOK {
		t.Fatalf("确认后应成功: code=%d out=%s err=%s", code, out, errb)
	}
	st, err := project.LoadState(dir)
	if err != nil || st.Container.ImageID != envImgBID {
		t.Fatalf("切换未生效: %v %+v", err, st.Container)
	}
}

// ---------- P1：dry-run 只读预览 ----------

func TestEnvSwitchDryRunNoWrites(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	stateBefore := readStateBytes(t, dir)

	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--dry-run")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	for _, want := range []string{"项目根", "项目 ID", "当前代", "目标镜像", "回退槽位", "允许执行", "不会撤销"} {
		if !strings.Contains(out, want) {
			t.Fatalf("预览缺少 %q:\n%s", want, out)
		}
	}
	noMutations(t, f, dir, stateBefore)
}

// 预览必须列明回退槽位轮换（ADR §7）。
func TestEnvSwitchDryRunListsRotation(t *testing.T) {
	f, dir, sess := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	// 预置回退槽位（gen0 的旧容器 X）
	prevID := f.addContainer(envContainer{name: "km-" + envProjID + "-prev", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir)})
	if err := envtxn.SavePrevious(dir, &envtxn.Previous{Generation: 0, ContainerID: prevID, ContainerName: "km-" + envProjID + "-prev", ImageID: envImgAID, ImageRef: envImgARef, WasRunning: true, OpID: "e0000000000000001"}); err != nil {
		t.Fatal(err)
	}
	_ = sess
	code, out, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--dry-run")
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "retained") || !strings.Contains(out, "轮换") {
		t.Fatalf("预览应列明槽位轮换:\n%s", out)
	}
}

// ---------- P1：拒绝场景（稳定错误分类 + 零变更） ----------

func TestEnvSwitchRefusals(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, f *envFake, dir string, sess *envSessCtl)
		args     []string
		wantCode string
	}{
		{"镜像缺失", func(t *testing.T, f *envFake, dir string, sess *envSessCtl) {}, []string{"switch", "--image", "nope:9", "--yes"}, runtime.CodeNotFound},
		{"平台冲突", func(t *testing.T, f *envFake, dir string, sess *envSessCtl) {
			f.addImage(envImgBRef, envImgBID)
			f.plats[envImgBID] = "linux/amd64-if-different"
			f.plats[envImgBID] = "linux/" + goruntimeGOARCHOther()
		}, []string{"switch", "--image", envImgBRef, "--yes"}, runtime.CodePlatformMismatch},
		{"身份冲突", func(t *testing.T, f *envFake, dir string, sess *envSessCtl) {
			f.conts[envOldID].name = "km-taken"
		}, []string{"switch", "--image", envImgBRef, "--yes"}, runtime.CodeContainerConflict},
		{"会话活跃", func(t *testing.T, f *envFake, dir string, sess *envSessCtl) {
			f.addImage(envImgBRef, envImgBID)
			sess.sessionsOut = "ACTIVE sabc123\n"
		}, []string{"switch", "--image", envImgBRef, "--yes"}, runtime.CodeSessionActive},
		{"会话未知", func(t *testing.T, f *envFake, dir string, sess *envSessCtl) {
			f.addImage(envImgBRef, envImgBID)
			sess.sessionsErr = errTestBoom
		}, []string{"switch", "--image", envImgBRef, "--yes"}, runtime.CodeSessionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, dir, sess := setupEnvReady(t)
			tc.mutate(t, f, dir, sess)
			stateBefore := readStateBytes(t, dir)
			code, _, errb := runEnv(t, f.docker(), dir, tc.args...)
			if code != ExitEnv || !strings.Contains(errb, tc.wantCode) {
				t.Fatalf("code=%d err=%s（期望 %s）", code, errb, tc.wantCode)
			}
			noMutations(t, f, dir, stateBefore)
		})
	}
}

func goruntimeGOARCHOther() string { return "s390x" }

var errTestBoom = &testBoom{}

type testBoom struct{}

func (e *testBoom) Error() string { return "boom" }

// 未完成事务阻断 switch；既有事务保持原样。
func TestEnvSwitchPendingTxnRefused(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["create"] != 0 || f.hooks["stop"] != 0 {
		t.Fatal("阻断时不应触碰容器")
	}
	if !envtxn.TransactionExists(dir) {
		t.Fatal("既有事务应保持")
	}
}

// ---------- P1：rollback 拒绝场景 ----------

func TestEnvRollbackRefusals(t *testing.T) {
	t.Run("无回退记录", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		if err := os.Remove(envtxn.PreviousPath(dir)); err != nil {
			t.Fatal(err)
		}
		stateBefore := readStateBytes(t, dir)
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeNoPrevious) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutations(t, f, dir, stateBefore)
	})
	t.Run("上一代容器被外部删除", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		delete(f.conts, envOldID)
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeNotFound) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))
	})
	t.Run("引用漂移", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		f.imgs[envImgARef] = envImgBID // img-a:1 现在指向别的内容
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeImageDrift) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))
	})
	t.Run("上一代身份不符", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		f.conts[envOldID].image = envImgBID
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))
	})
	t.Run("平台不一致", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		f.plats[envImgAID] = "linux/s390x"
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodePlatformMismatch) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutationsKeepEnv(t, f, dir, readStateBytes(t, dir))
	})
	t.Run("未完成事务", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
	})
}

// v1 状态但存在 previous.json：记录组合不一致，拒绝。
func TestEnvRollbackV1StateRefused(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	prevID := f.addContainer(envContainer{name: "km-p-g0", state: "exited", project: envProjID, image: envImgAID, mount: resolveDir(t, dir)})
	savePrevFixture(t, dir, prevID, "km-p-g0", 0)
	code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeStateInvalid) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

// ---------- 事务记录损坏：一律拒绝且保留原始证据 ----------

func TestEnvDamagedTxnRecordBlocksAll(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	if err := os.MkdirAll(envtxn.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not-json"
	if err := os.WriteFile(envtxn.TransactionPath(dir), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"switch", "--image", envImgBRef, "--dry-run"},
		{"rollback", "--dry-run"},
		{"recover", "--dry-run"},
	} {
		code, _, errb := runEnv(t, f.docker(), dir, args...)
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeStateInvalid) {
			t.Fatalf("%v: code=%d err=%s", args, code, errb)
		}
	}
	// run 同样阻断
	code, errb := runTool(t, f.docker(), dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeStateInvalid) {
		t.Fatalf("run 应被损坏事务阻断: code=%d err=%s", code, errb)
	}
	raw, _ := os.ReadFile(envtxn.TransactionPath(dir))
	if string(raw) != broken {
		t.Fatal("损坏的事务文件必须原样保留")
	}
}

// ---------- run/init/stop 的事务阻断（ADR §5.4） ----------

func TestMutationsBlockedByPendingTxn(t *testing.T) {
	t.Run("run 阻断", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		writeEnvTxnFixture(t, dir, envtxn.StageOldStopped)
		code, errb := runTool(t, f.docker(), dir, "nmap", "-h")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		if f.hooks["start"] != 0 || f.hooks["stop"] != 0 {
			t.Fatal("阻断时不应触碰容器")
		}
	})
	t.Run("stop 阻断", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
		code, _, errb := runStop(t, f.docker(), dir)
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeTransactionPending) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		if f.hooks["stop"] != 0 {
			t.Fatal("阻断时不应停止容器")
		}
	})
}

// recover 无事务 → 明确无需恢复，退出码 0。
func TestEnvRecoverNoTxn(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	code, out, errb := runEnv(t, f.docker(), dir, "recover")
	if code != ExitOK || !strings.Contains(out, "无需恢复") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	_ = filepath.Join
}

// ---------- 夹具辅助 ----------

func savePrevFixture(t *testing.T, dir, containerID, name string, gen int) {
	t.Helper()
	if err := envtxn.SavePrevious(dir, &envtxn.Previous{
		Generation: gen, ContainerID: containerID, ContainerName: name,
		ImageID: envImgAID, ImageRef: envImgARef, WasRunning: true, OpID: "e0000000000000001",
	}); err != nil {
		t.Fatal(err)
	}
}

// writeEnvTxnFixture 写入一个最小可解析的未完成事务。
func writeEnvTxnFixture(t *testing.T, dir, stage string) {
	t.Helper()
	txn := &envtxn.Transaction{
		OpID: "e0000000000000001", Kind: envtxn.KindSwitch, Stage: stage,
		Old:          envtxn.Snapshot{ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID, WasRunning: true, Generation: 0},
		New:          envtxn.Snapshot{ContainerID: "", ContainerName: "km-" + envProjID + "-g1", ImageID: envImgBID, WasRunning: true, Generation: 1},
		ConfigBackup: envtxn.NewFileBackup([]byte(envConfigA())),
		StateBackup:  envtxn.NewFileBackup(readStateBytes(t, dir)),
	}
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}
}

// ---------- 容器状态门禁与提交时漂移拒绝 ----------

// 仅支持 running/exited；paused 等独立拒绝（不自动 unpause）。
func TestEnvPausedContainerRefused(t *testing.T) {
	t.Run("switch", func(t *testing.T) {
		f, dir, _ := setupEnvReady(t)
		f.addImage(envImgBRef, envImgBID)
		f.conts[envOldID].state = "paused"
		stateBefore := readStateBytes(t, dir)
		code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) || !strings.Contains(errb, "paused") {
			t.Fatalf("code=%d err=%s", code, errb)
		}
		noMutations(t, f, dir, stateBefore)
	})
	t.Run("rollback", func(t *testing.T) {
		f, dir, _, _ := setupEnvPostSwitch(t)
		f.conts[f.names["km-"+envProjID+"-g1"]].state = "paused"
		code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
		if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) {
			t.Fatalf("code=%d err=%s", code, errb)
		}
	})
}

// 目标标签在切换过程中漂移 → 提交前拒绝，收敛回前态（不静默跟随新内容）。
func TestEnvSwitchRefDriftAtCommitRefused(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	f.onStopRetagRef = envImgBRef
	f.onStopRetagTo = "sha256:" + strings.Repeat("9", 64)
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeImageDrift) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.conts[envOldID].state != "running" {
		t.Fatal("原环境应恢复运行")
	}
	if len(f.conts) != 1 {
		t.Fatalf("候选与探测应删除: %d", len(f.conts))
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应收敛清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

// 回退过程中上一代引用漂移 → 提交前拒绝，恢复当前代。
func TestEnvRollbackRefDriftAtCommitRefused(t *testing.T) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	f.onStopRetagRef = envImgARef
	f.onStopRetagTo = "sha256:" + strings.Repeat("9", 64)
	stateBefore := readStateBytes(t, dir)
	code, _, errb := runEnv(t, f.docker(), dir, "rollback", "--yes")
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeImageDrift) {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.conts[gen1ID].state != "running" {
		t.Fatal("当前代应恢复运行")
	}
	if !envtxn.PreviousExists(dir) {
		t.Fatal("回退槽位应保留")
	}
	if envtxn.TransactionExists(dir) {
		t.Fatal("事务应收敛清除")
	}
	after, _ := os.ReadFile(project.StatePath(dir))
	if !bytesEqual(stateBefore, after) {
		t.Fatal("状态应还原")
	}
}

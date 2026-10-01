package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

func auditSetCurrent(t *testing.T, dir, id, name, image string, gen int) {
	t.Helper()
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Container.ID, st.Container.Name, st.Container.ImageID = id, name, image
	st.Env.Generation = gen
	if err := project.SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRecoverRechecksAllDeletionGuards(t *testing.T) {
	for _, scenario := range []string{"current", "previous", "project-label", "mount-mode", "snapshot-identity"} {
		t.Run(scenario, func(t *testing.T) {
			f, dir, _ := makeRetainedFixture(t)
			f.failRmNext = true
			if code, _, errb := envRemoveRun(t, f, dir, envOldID, "--yes"); code != ExitEnv {
				t.Fatalf("setup did not leave pending remove: %d %s", code, errb)
			}
			switch scenario {
			case "current":
				auditSetCurrent(t, dir, envOldID, "km-"+envProjID, envImgAID, 0)
			case "previous":
				if err := envtxn.SavePrevious(dir, &envtxn.Previous{
					ContainerID: envOldID, ContainerName: "km-" + envProjID,
					ImageID: envImgAID, ImageRef: envImgARef, OpID: "e00000000000000aa",
				}); err != nil {
					t.Fatal(err)
				}
			case "project-label":
				f.conts[envOldID].project = "pother-project"
			case "mount-mode":
				f.conts[envOldID].role = "probe" // fake emits read-only /workspace
			case "snapshot-identity":
				f.conts[envOldID].name = "km-renamed-after-confirmation"
				ret, err := envtxn.LoadRetained(dir)
				if err != nil {
					t.Fatal(err)
				}
				ret.Containers[0].ContainerName = f.conts[envOldID].name
				if err := envtxn.SaveRetained(dir, ret); err != nil {
					t.Fatal(err)
				}
			}
			callsBefore := f.hooks["rm"]
			code, out, errb := runEnv(t, f.docker(), dir, "recover", "--yes")
			_, exists := f.conts[envOldID]
			t.Logf("scenario=%s rc=%d extra_rm=%d target_exists=%v journal_exists=%v stdout=%q stderr=%q",
				scenario, code, f.hooks["rm"]-callsBefore, exists, envtxn.TransactionExists(dir), out, errb)
			if code != ExitEnv || !exists || f.hooks["rm"] != callsBefore || !envtxn.TransactionExists(dir) {
				t.Errorf("changed eligibility must refuse without deleting target or clearing journal")
			}
		})
	}
}

type auditConfirmReader struct {
	edit func()
	done bool
}

func (r *auditConfirmReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	r.edit()
	return copy(p, "yes\n"), nil
}

func TestAuditRemoveReloadsStateAfterConfirmation(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	oldTTY, oldInput := envStdinIsTTY, envStdin
	t.Cleanup(func() { envStdinIsTTY, envStdin = oldTTY, oldInput })
	envStdinIsTTY = func() bool { return true }
	envStdin = &auditConfirmReader{edit: func() {
		auditSetCurrent(t, dir, envOldID, "km-"+envProjID, envImgAID, 0)
	}}
	code, out, errb := envRemoveRun(t, f, dir, envOldID)
	_, exists := f.conts[envOldID]
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rc=%d rm=%d current_is_target=%v target_exists=%v stdout=%q stderr=%q",
		code, f.hooks["rm"], st.Container.ID == envOldID, exists, out, errb)
	if code != ExitEnv || !exists || f.hooks["rm"] != 0 {
		t.Errorf("confirmation-time state change must abort instead of deleting new CURRENT")
	}
}

func TestAuditListUntrackedInspectErrorIsUnknown(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	id := f.addContainer(envContainer{
		name: "km-untracked", state: "exited", project: envProjID,
		image: envImgAID, mount: resolveDir(t, dir),
	})
	d := &runtime.Docker{Exec: &fakeExecutor{respond: func(args []string) ([]byte, []byte, error) {
		if args[1] == "container" && args[2] == "inspect" && args[len(args)-1] == id {
			return envFail("Cannot connect to the Docker daemon", 1)
		}
		return f.respond(args)
	}}}
	code, out, errb := runEnv(t, d, dir, "list")
	t.Logf("rc=%d stdout=%q stderr=%q", code, out, errb)
	if code != ExitEnv || !strings.Contains(out, "UNKNOWN") {
		t.Errorf("untracked inspect failure must produce UNKNOWN and nonzero exit")
	}
}

func TestAuditListRejectsMissingMount(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.conts[envOldID].mount = ""
	code, out, errb := envListRun(t, f, dir)
	removeCode, _, removeErr := envRemoveRun(t, f, dir, envOldID, "--dry-run")
	t.Logf("list_rc=%d remove_rc=%d stdout=%q stderr=%q remove_stderr=%q", code, removeCode, out, errb, removeErr)
	if code != ExitEnv || !strings.Contains(out, "CONFLICT") || strings.Contains(out, "是（km env remove") {
		t.Errorf("missing /workspace must not be advertised as healthy and removable")
	}
}

func TestAuditListDetectsSnapshotChange(t *testing.T) {
	f, dir, _, gen1ID := setupEnvPostSwitch(t)
	nextID := strings.Repeat("c", 64)
	nextImage := "sha256:" + strings.Repeat("3", 64)
	changed := false
	d := &runtime.Docker{Exec: &fakeExecutor{respond: func(args []string) ([]byte, []byte, error) {
		if args[1] == "ps" && !changed {
			changed = true
			// A->B snapshot was read. Another command completes B->C before ps.
			f.addImage("img-c:3", nextImage)
			f.addContainer(envContainer{id: nextID, name: "km-" + envProjID + "-g2", state: "running",
				project: envProjID, image: nextImage, mount: resolveDir(t, dir)})
			f.conts[gen1ID].state = "exited"
			auditSetCurrent(t, dir, nextID, "km-"+envProjID+"-g2", nextImage, 2)
			cfgPath := dir + "/" + project.ConfigFileName
			cfgRaw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, []byte(strings.ReplaceAll(string(cfgRaw), envImgBRef, "img-c:3")), 0644); err != nil {
				t.Fatal(err)
			}
			if err := envtxn.SavePrevious(dir, &envtxn.Previous{
				ContainerID: gen1ID, ContainerName: "km-" + envProjID + "-g1",
				ImageID: envImgBID, ImageRef: envImgBRef, Generation: 1, OpID: "e00000000000000aa",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := envtxn.AppendRetained(dir, envtxn.RetainedEntry{
				ContainerID: envOldID, ContainerName: "km-" + envProjID, ImageID: envImgAID,
				Reason: envtxn.ReasonSupersededBySwitch, OpID: "e00000000000000aa",
			}); err != nil {
				t.Fatal(err)
			}
		}
		return f.respond(args)
	}}}
	code, out, errb := runEnv(t, d, dir, "list")
	t.Logf("rc=%d stdout=%q stderr=%q", code, out, errb)
	if code == ExitOK {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "UNTRACKED") && strings.Contains(line, nextID) {
				t.Errorf("mixed-generation snapshot was returned as healthy; new CURRENT became UNTRACKED")
			}
		}
	}
}

func TestAuditRecoverPreviewStatesDeleteNotRollback(t *testing.T) {
	f, dir, _ := makeRetainedFixture(t)
	f.failRmNext = true
	if code, _, _ := envRemoveRun(t, f, dir, envOldID, "--yes"); code != ExitEnv {
		t.Fatal("setup")
	}
	txn, err := envtxn.LoadTransaction(dir)
	if err != nil {
		t.Fatal(err)
	}
	txn.Stage = envtxn.StagePrepared
	if err := envtxn.SaveTransaction(dir, txn); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(envtxn.TransactionPath(dir))
	code, out, errb := runEnv(t, f.docker(), dir, "recover", "--dry-run")
	after, _ := os.ReadFile(envtxn.TransactionPath(dir))
	t.Logf("rc=%d stdout=%q stderr=%q", code, out, errb)
	if string(before) != string(after) {
		t.Fatal("dry-run changed journal")
	}
	if strings.Contains(out, "恢复原环境") || strings.Contains(out, "还原文件") || !strings.Contains(out, "重试普通") {
		t.Errorf("remove recovery preview must disclose retrying irreversible deletion, not rollback promises")
	}
}

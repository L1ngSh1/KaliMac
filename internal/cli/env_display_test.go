package cli

import (
	"bytes"
	"context"
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

// ---------- status / doctor 的代际与事务展示（ADR §8） ----------

func runStatusCmd(t *testing.T, d *runtime.Docker, dir string, jsonMode bool) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	args := []string{}
	if jsonMode {
		args = append(args, "--json")
	}
	code := runStatusCommand(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

// 中间态不得报为正常：未完成事务 → 独立状态 + 退出码 1 + 指向 recover。
func TestStatusShowsPendingTxn(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	writeEnvTxnFixture(t, dir, envtxn.StageCommitIntent)
	code, out, errb := runStatusCmd(t, f.docker(), dir, true)
	if code != ExitEnv {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	var ps struct {
		State string `json:"state"`
		Env   *struct {
			Generation  int `json:"generation"`
			Transaction *struct {
				OpID  string `json:"op_id"`
				Kind  string `json:"kind"`
				Stage string `json:"stage"`
			} `json:"transaction"`
		} `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if ps.State != statusEnvTxnPending || ps.Env == nil || ps.Env.Transaction == nil {
		t.Fatalf("应报告 env_transaction_pending: %s", out)
	}
	if ps.Env.Transaction.Stage != envtxn.StageCommitIntent {
		t.Fatalf("stage: %+v", ps.Env)
	}
}

func TestStatusShowsGeneration(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	if code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes"); code != ExitOK {
		t.Fatalf("switch: %d %s", code, errb)
	}
	code, out, _ := runStatusCmd(t, f.docker(), dir, false)
	if code != ExitOK || !strings.Contains(out, "第 1 代") {
		t.Fatalf("status 应显示当前代: code=%d out=%s", code, out)
	}
	_, outJSON, _ := runStatusCmd(t, f.docker(), dir, true)
	if !strings.Contains(outJSON, `"generation": 1`) {
		t.Fatalf("json 应含 generation: %s", outJSON)
	}
}

func TestDoctorShowsEnvRecords(t *testing.T) {
	f, dir, _, _ := setupEnvPostSwitch(t)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	var out bytes.Buffer
	code := RunDoctor(context.Background(), dir, &out, f.docker())
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	got := out.String()
	for _, want := range []string{"环境记录", "当前代: 第 1", "回退槽位: 第 0", "资源账本与实际容器一致"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor 缺少 %q:\n%s", want, got)
		}
	}
}

// 存在未完成事务时 doctor 给出失败项与恢复入口。
func TestDoctorShowsPendingTxn(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	writeEnvTxnFixture(t, dir, envtxn.StagePrepared)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	var out bytes.Buffer
	RunDoctor(context.Background(), dir, &out, f.docker())
	got := out.String()
	if !strings.Contains(got, "KM_TRANSACTION_PENDING") || !strings.Contains(got, "km env recover") {
		t.Fatalf("doctor 应报告未完成事务:\n%s", got)
	}
}

// ---------- 分派与帮助 ----------

func TestEnvDispatchAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"env", "--help"}, &out, &errb); code != ExitOK || !strings.Contains(out.String(), "km env") {
		t.Fatalf("km env --help: code=%d out=%s err=%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run(context.Background(), []string{"help", "env"}, &out, &errb); code != ExitOK || !strings.Contains(out.String(), "rollback") {
		t.Fatalf("km help env: code=%d out=%s", code, out.String())
	}
	out.Reset()
	if code := Run(context.Background(), []string{"--help"}, &out, &errb); code != ExitOK || !strings.Contains(out.String(), "km env switch|rollback|recover") {
		t.Fatalf("总帮助应含 env 行:\n%s", out.String())
	}
}

// ---------- 并发与多项目（测试矩阵） ----------

// switch 与 run/stop 互斥：他人持锁 → KM_PROJECT_BUSY。
func TestEnvSwitchBusyLockRefused(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	f.addImage(envImgBRef, envImgBID)
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.LockPath(dir), []byte(fmt.Sprintf("pid=%d\ncreated_at=now\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runEnv(t, f.docker(), dir, "switch", "--image", envImgBRef, "--yes")
	if code != ExitEnv || !strings.Contains(errb, "KM_PROJECT_BUSY") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if f.hooks["create"] != 0 {
		t.Fatal("持锁期间不应创建容器")
	}
}

// 多项目隔离：项目 A 持锁不影响项目 B 的切换。
func TestEnvSwitchProjectIsolation(t *testing.T) {
	fA, dirA, _ := setupEnvReady(t)
	fB, dirB, _ := setupEnvReady(t)
	fB.addImage(envImgBRef, envImgBID)
	// A 持锁
	if err := os.MkdirAll(filepath.Join(dirA, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.LockPath(dirA), []byte(fmt.Sprintf("pid=%d\ncreated_at=now\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	// B 正常切换
	if code, _, errb := runEnv(t, fB.docker(), dirB, "switch", "--image", envImgBRef, "--yes"); code != ExitOK {
		t.Fatalf("B 应不受 A 影响: %d %s", code, errb)
	}
	if fA.hooks["create"] != 0 {
		t.Fatal("A 的资源不应被 B 触碰")
	}
	if fB.conts[envOldID].state != "exited" {
		t.Fatal("B 的旧容器应停止")
	}
	_ = fA
}

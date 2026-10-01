//go:build integration

// S4：环境资源查看与显式清理的真实 Docker 演练。
// A→B→C 三代构造：list 角色断言 → C/B 删除拒绝 → A 删除成功 →
// 可写层金丝雀随删除消失、项目文件金丝雀保留、镜像未删除 →
// C 正常执行、仍能 rollback B；中断场景（真实 kill -9）与安装版另测。
package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envBuildThirdImage(t *testing.T) string {
	t.Helper()
	envTestImage(t, "km-envtest-c:local", "env-c")
	return "km-envtest-c:local"
}

// 不可逆边界：容器可写层金丝雀随删除消失；项目文件金丝雀保留；镜像不删。
func TestEnvRemoveRealDrillABC(t *testing.T) {
	dir := envInitProject(t)
	imgC := envBuildThirdImage(t)

	// 可写层金丝雀：趁 A（gen0）还在运行时写入其可写层（/workspace 之外，
	// 不在共享目录；删除容器后必然消失，且不会出现在任何其他容器）
	PID := envProjectID(t, dir)
	aName := "km-" + PID
	if out, code := dexec(t, aName, "/bin/sh", "-c", "printf canary > /opt/canary-in-A"); code != 0 {
		t.Fatalf("写入可写层金丝雀失败: %s", out)
	}

	// A(gen0) → switch B(gen1, 槽位 A) → switch C(gen2, 槽位 B, retained A)
	if out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", envImgBTag, "--yes"); code != 0 {
		t.Fatalf("switch B: %s %s", out, errb)
	}
	if out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", imgC, "--yes"); code != 0 {
		t.Fatalf("switch C: %s %s", out, errb)
	}

	// 三个容器：C 当前 + B 槽位 + A retained
	if got := len(envProjectContainers(t, dir)); got != 3 {
		t.Fatalf("应有 3 个容器（当前+槽位+retained）: %d", got)
	}

	// 项目文件金丝雀
	if err := os.WriteFile(filepath.Join(dir, "keep-me.txt"), []byte("project-file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// list：C 当前、B 槽位、A retained 且可删除
	out, errb, code := kmRun(t, dir, nil, "env", "list")
	if code != 0 {
		t.Fatalf("list: code=%d err=%s", code, errb)
	}
	for _, want := range []string{"CURRENT", "PREVIOUS", "RETAINED", "superseded-by-switch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list 缺少 %q:\n%s", want, out)
		}
	}
	aFullID := containerFullIDByName(t, aName)

	// C/B 删除拒绝（保护），零变更
	_, errb, code = kmRun(t, dir, nil, "env", "remove", containerFullIDByName(t, "km-"+PID+"-g2"), "--yes")
	if code != 1 || !strings.Contains(errb, "受保护") {
		t.Fatalf("C 应受保护拒绝: code=%d err=%s", code, errb)
	}
	_, errb, code = kmRun(t, dir, nil, "env", "remove", containerFullIDByName(t, "km-"+PID+"-g1"), "--yes")
	if code != 1 || !strings.Contains(errb, "受保护") {
		t.Fatalf("B 应受保护拒绝: code=%d err=%s", code, errb)
	}

	// A 预览无变更 → 确认删除
	if out, errb, code := kmRun(t, dir, nil, "env", "remove", aFullID, "--dry-run"); code != 0 || !strings.Contains(out, "允许执行") {
		t.Fatalf("A 预览: code=%d out=%s err=%s", code, out, errb)
	}
	if out, errb, code := kmRun(t, dir, nil, "env", "remove", aFullID, "--yes"); code != 0 {
		t.Fatalf("A 删除: code=%d out=%s err=%s", code, out, errb)
	}

	// A 消失（金丝雀随容器消失）；B/C 保留；项目文件金丝雀仍在；镜像未删
	if _, missing := envContainerState(t, aName); !missing {
		t.Fatal("A 容器应已删除")
	}
	if _, missing := envContainerState(t, "km-"+PID+"-g1"); missing {
		t.Fatal("B 不得被删除")
	}
	if _, missing := envContainerState(t, "km-"+PID+"-g2"); missing {
		t.Fatal("C 不得被删除")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "keep-me.txt")); err != nil || string(b) != "project-file" {
		t.Fatal("项目文件金丝雀应保留")
	}
	if code := exitCodeOf("image", "inspect", envImgATag); code != 0 {
		t.Fatal("镜像不得被删除")
	}

	// 账本：A 条目移除，实际容器 = 2
	ret, err := os.ReadFile(filepath.Join(dir, ".km", "env", "retained.json"))
	if err != nil || strings.Contains(string(ret), aFullID) {
		t.Fatalf("A 条目应移除: %v %s", err, ret)
	}
	if got := len(envProjectContainers(t, dir)); got != 2 {
		t.Fatalf("删除后应剩 2 个容器: %d", got)
	}

	// C 正常执行；仍能 rollback B；回退后当前为 B
	if out, errb, code := kmRun(t, dir, nil, "run", "--", "cat", envMarkerPath); code != 0 || !strings.Contains(out, "env-c") {
		t.Fatalf("C 执行: code=%d out=%s err=%s", code, out, errb)
	}
	if out, errb, code := kmRun(t, dir, nil, "env", "rollback", "--yes"); code != 0 {
		t.Fatalf("rollback B: %s %s", out, errb)
	}
	if out, errb, code := kmRun(t, dir, nil, "run", "--", "cat", envMarkerPath); code != 0 || !strings.Contains(out, "env-b") {
		t.Fatalf("回退后应为 B: code=%d out=%s err=%s", code, out, errb)
	}
}

// containerFullIDByName 从 docker inspect 取完整 ID。
func containerFullIDByName(t *testing.T, name string) string {
	t.Helper()
	out, code := runCapture(t, nil, "container", "inspect", "--format", "{{.Id}}", name)
	if code != 0 {
		t.Fatalf("inspect %s 失败: %s", name, out)
	}
	return strings.TrimSpace(out)
}

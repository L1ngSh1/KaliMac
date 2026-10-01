//go:build integration

// 环境切换与单代回退的真实 Docker 集成测试（P5）。
// 资源边界：只用本轮 init/switch 创建的项目容器（按 km.project=<project_id>
// 标签清理与复核）；两个测试镜像本地构建，内容差异用固定 marker 文件证明。
// Docker 不可达时 TestMain 已整体跳过；单测试仍按 dockerUp 双重保护。
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	envImgATag    = "km-envtest-a:local"
	envImgBTag    = "km-envtest-b:local"
	envMarkerPath = "/opt/km-env/marker"
)

// envTestImage 确定性构建测试镜像：差异只在 marker 文件内容。
// 已存在则复用（缓存友好）；构建有据可查。
func envTestImage(t *testing.T, tag, marker string) {
	t.Helper()
	if !dockerUp(t) {
		t.Skip("Docker 引擎不可达")
	}
	if code := exitCodeOf("image", "inspect", tag); code == 0 {
		return
	}
	dir := t.TempDir()
	df := fmt.Sprintf("FROM %s\nRUN mkdir -p /opt/km-env && printf '%s\\n' > %s\n", minImageRef, marker, envMarkerPath)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	b := exec.Command("docker", "build", "-t", tag, dir)
	out, err := b.CombinedOutput()
	if err != nil {
		t.Fatalf("构建 %s 失败: %v\n%s", tag, err, out)
	}
}

// envRegisterProjectCleanup 登记项目全部容器（当前代/上一代/retained/探测）
// 的清理与终检：按 km.project 标签列出 → rm -f → 复核为空（查询失败记无法核实）。
func envRegisterProjectCleanup(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".km", "state.json"))
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	var st struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("状态解析失败: %v", err)
	}
	lbl := "km.project=" + st.ProjectID
	t.Cleanup(func() {
		out, code := runCapture(t, nil, "ps", "-a", "--filter", "label="+lbl, "-q")
		if code != 0 {
			t.Errorf("ENV-CLEANUP-UNVERIFIED: docker ps 失败: %s", out)
			return
		}
		for _, id := range strings.Fields(out) {
			if err := exec.Command("docker", "rm", "-f", id).Run(); err != nil {
				t.Logf("ENV-CLEANUP-WARN: 移除 %s 失败: %v", id, err)
			}
		}
		out2, code2 := runCapture(t, nil, "ps", "-a", "--filter", "label="+lbl, "-q")
		if code2 != 0 || strings.TrimSpace(out2) != "" {
			t.Errorf("ENV-CLEANUP-FAIL: 项目 %s 存在残留: %s", st.ProjectID, out2)
		}
	})
}

func envInitProject(t *testing.T) string {
	t.Helper()
	envTestImage(t, envImgATag, "env-a")
	envTestImage(t, envImgBTag, "env-b")
	dir := t.TempDir()
	out, errb, code := kmRun(t, dir, nil, "init", "--image", envImgATag)
	if code != 0 {
		t.Fatalf("init 失败: %s %s", out, errb)
	}
	envRegisterProjectCleanup(t, dir)
	return dir
}

func envStateOf(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".km", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func envProjectID(t *testing.T, dir string) string {
	t.Helper()
	return envStateOf(t, dir)["project_id"].(string)
}

// envContainerState 按名称查询容器状态；notFound=true 表示不存在。
func envContainerState(t *testing.T, name string) (state string, notFound bool) {
	t.Helper()
	out, code := runCapture(t, nil, "container", "inspect", "--format", "{{.State.Status}}", name)
	if code != 0 {
		return "", true
	}
	return strings.TrimSpace(out), false
}

func envProjectContainers(t *testing.T, dir string) []string {
	t.Helper()
	lbl := "km.project=" + envProjectID(t, dir)
	out, code := runCapture(t, nil, "ps", "-a", "--filter", "label="+lbl, "--format", "{{.ID}} {{.Names}} {{.State}}")
	if code != 0 {
		t.Fatalf("ps 失败: %s", out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// 正常切换 + 回退全链路：新环境实际执行、身份一致、账本恒等式、文件边界、
// 槽位消费与二次回退拒绝。
func TestEnvSwitchRealHappyRollback(t *testing.T) {
	dir := envInitProject(t)

	// dry-run：退出 0 且无任何记录落盘
	out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", envImgBTag, "--dry-run")
	if code != 0 || !strings.Contains(out, "允许执行") {
		t.Fatalf("dry-run: code=%d out=%s err=%s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".km", "env")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应写 .km/env: %v", err)
	}

	// 切换 A→B
	out, errb, code = kmRun(t, dir, nil, "env", "switch", "--image", envImgBTag, "--yes")
	if code != 0 {
		t.Fatalf("switch: code=%d out=%s err=%s", code, out, errb)
	}
	st := envStateOf(t, dir)
	if st["state_version"].(float64) != 2 || st["env"].(map[string]any)["generation"].(float64) != 1 {
		t.Fatalf("应为 v2 第 1 代: %v", st)
	}
	// 新命令实际在 B 中执行（marker 证明内容身份）
	out, errb, code = kmRun(t, dir, nil, "run", "--", "cat", envMarkerPath)
	if code != 0 || !strings.Contains(out, "env-b") {
		t.Fatalf("B 环境执行: code=%d out=%s err=%s", code, out, errb)
	}
	// 旧容器停止且保留
	if s, missing := envContainerState(t, "km-"+envProjectID(t, dir)); missing || s != "exited" {
		t.Fatalf("旧容器应停止保留: state=%q missing=%v", s, missing)
	}
	// 回退槽位与账本
	rawPrev, err := os.ReadFile(filepath.Join(dir, ".km", "env", "previous.json"))
	if err != nil {
		t.Fatalf("previous.json 缺失: %v", err)
	}
	if !strings.Contains(string(rawPrev), envImgATag) {
		t.Fatalf("previous 应记录旧引用: %s", rawPrev)
	}

	// 共享文件改动（新环境运行期间）不因回退消失
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("edited-in-B"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 回退 B→A
	out, errb, code = kmRun(t, dir, nil, "env", "rollback", "--yes")
	if code != 0 {
		t.Fatalf("rollback: code=%d out=%s err=%s", code, out, errb)
	}
	out, errb, code = kmRun(t, dir, nil, "run", "--", "cat", envMarkerPath)
	if code != 0 || !strings.Contains(out, "env-a") {
		t.Fatalf("A 环境执行: code=%d out=%s err=%s", code, out, errb)
	}
	st = envStateOf(t, dir)
	if st["env"].(map[string]any)["generation"].(float64) != 0 {
		t.Fatalf("应回到第 0 代: %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "note.txt")); string(b) != "edited-in-B" {
		t.Fatal("回退不应回滚项目文件")
	}
	if _, err := os.Stat(filepath.Join(dir, ".km", "env", "previous.json")); !os.IsNotExist(err) {
		t.Fatal("回退槽位应已消费")
	}
	// B 容器保留在 retained
	rawRet, err := os.ReadFile(filepath.Join(dir, ".km", "env", "retained.json"))
	if err != nil || !strings.Contains(string(rawRet), "rolled-back") {
		t.Fatalf("retained 应记录被撤容器: %v %s", err, rawRet)
	}

	// 二次回退：KM_NO_PREVIOUS，环境不变
	_, errb, code = kmRun(t, dir, nil, "env", "rollback", "--yes")
	if code != 1 || !strings.Contains(errb, "KM_NO_PREVIOUS") {
		t.Fatalf("二次回退: code=%d err=%s", code, errb)
	}

	// 账本恒等式：实际容器 = 当前代 + retained（2 个）
	conts := envProjectContainers(t, dir)
	if len(conts) != 2 {
		t.Fatalf("实际容器应恰为 2（当前代+retained）: %v", conts)
	}
}

// no-op：同一内容的不同标签不产生任何变更。
func TestEnvSwitchRealNoOp(t *testing.T) {
	dir := envInitProject(t)
	alias := "km-envtest-a-alias:local"
	if out, code := runCapture(t, nil, "tag", envImgATag, alias); code != 0 {
		t.Fatalf("tag 失败: %s", out)
	}
	before := envProjectContainers(t, dir)
	out, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", alias, "--yes")
	if code != 0 || !strings.Contains(out, "no-op") {
		t.Fatalf("no-op: code=%d out=%s err=%s", code, out, errb)
	}
	st := envStateOf(t, dir)
	if _, ok := st["env"]; ok {
		t.Fatalf("no-op 不应升版状态: %v", st)
	}
	after := envProjectContainers(t, dir)
	if len(before) != len(after) {
		t.Fatalf("no-op 不应新增容器: %v → %v", before, after)
	}
}

// 本地镜像缺失：拒绝且不创建任何资源。
func TestEnvSwitchRealMissingImage(t *testing.T) {
	dir := envInitProject(t)
	before := envProjectContainers(t, dir)
	_, errb, code := kmRun(t, dir, nil, "env", "switch", "--image", "km-envtest-missing:xyz", "--yes")
	if code != 1 || !strings.Contains(errb, "KM_NOT_FOUND") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".km", "env")); !os.IsNotExist(err) {
		t.Fatal("不应产生事务记录")
	}
	after := envProjectContainers(t, dir)
	if len(before) != len(after) {
		t.Fatalf("不应创建容器: %v → %v", before, after)
	}
}

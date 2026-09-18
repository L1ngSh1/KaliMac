//go:build integration

// 新用户闭环迭代（init 参数/平台兑现、km status）真实集成验收。
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// init --image 精选镜像 → 配置落盘、容器创建、status 为 running_idle（人类/JSON 一致）、
// status 只读性（.km 前后逐字节一致）、短命令可用。
func TestNewUserInitFlagsAndStatus(t *testing.T) {
	dir := newP2BProject(t)
	// 该用例验证「新项目 + 显式参数」：删掉夹具预写的旧格式配置
	if err := os.Remove(filepath.Join(dir, ".km.json")); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := kmRun(t, dir, nil, "init", "--image", "kali-mac-min:0.2"); code != 0 {
		t.Fatalf("init --image: %s", errb)
	}
	// 登记必须在 init 成功后立即执行（M1 教训）：后续任何断言失败都不能泄漏容器
	registerProjectCleanup(t, dir)
	cfgRaw, err := os.ReadFile(filepath.Join(dir, ".km.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Image    string `json:"image"`
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "kali-mac-min:0.2" || !strings.HasPrefix(cfg.Platform, "linux/") {
		t.Fatalf("配置应记录参数镜像与平台: %s", cfgRaw)
	}

	if _, _, c := kmRun(t, dir, nil, "run", "--", "/bin/true"); c != 0 {
		t.Fatal("run 失败")
	}

	// 人类输出
	out, _, code := kmRun(t, dir, nil, "status")
	if code != 0 || !strings.Contains(out, "running_idle") || !strings.Contains(out, "kali-mac-min:0.2") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	// JSON 输出同一状态
	jOut, _, jCode := kmRun(t, dir, nil, "status", "--json")
	if jCode != 0 {
		t.Fatalf("status --json: %d", jCode)
	}
	var ps struct {
		SchemaVersion int    `json:"schema_version"`
		State         string `json:"state"`
		Project       *struct {
			Image            string `json:"image"`
			PlatformDeclared bool   `json:"platform_declared"`
		} `json:"project"`
		Sessions *struct {
			Active []string `json:"active"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(jOut), &ps); err != nil {
		t.Fatalf("JSON: %v\n%s", err, jOut)
	}
	if ps.SchemaVersion != 1 || ps.State != "running_idle" || ps.Project == nil ||
		ps.Project.Image != "kali-mac-min:0.2" || !ps.Project.PlatformDeclared || ps.Sessions == nil {
		t.Fatalf("JSON 状态不符: %s", jOut)
	}
	// 平台兑现端到端：声明平台的架构 == 实际镜像架构
	archRaw, aerr := exec.Command("docker", "image", "inspect", "--format",
		"{{.Architecture}}", cfg.Image).Output()
	if aerr != nil {
		t.Fatalf("镜像架构查询失败: %v", aerr)
	}
	arch := strings.TrimSpace(string(archRaw))
	if !strings.HasSuffix(cfg.Platform, arch) {
		t.Fatalf("声明平台 %s 与镜像架构 %s 不一致", cfg.Platform, arch)
	}

	// 只读性：status 前后 .km 逐字节一致
	snap := func() map[string]string {
		m := map[string]string{}
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				b, _ := os.ReadFile(p)
				m[p] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("快照失败: %v", err)
		}
		return m
	}
	before := snap()
	if _, _, c := kmRun(t, dir, nil, "status"); c != 0 {
		t.Fatal("status 失败")
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatalf(".km 文件数变化: %d->%d", len(before), len(after))
	}
	for p, c := range before {
		if after[p] != c {
			t.Fatalf("只读性破坏: %s", p)
		}
	}
}

// 旧配置兼容（真实容器）：platform 字段缺失 → 复用/重建保持 native，
// --platform 参数被拒绝且不影响已有容器。
func TestNewUserLegacyConfigCompatReal(t *testing.T) {
	dir := newP2BProject(t)
	// newP2BProject 的配置没有 platform 字段（旧格式）
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	if _, errb, code := kmRun(t, dir, nil, "init", "--platform", "linux/arm64"); code != 1 {
		t.Fatalf("旧配置 + --platform 应拒绝: code=%d err=%s", code, errb)
	}
	// 拒绝后环境仍可用
	if out, _, code := kmRun(t, dir, nil, "run", "--", "/bin/echo", "STILL_OK"); code != 0 || !strings.Contains(out, "STILL_OK") {
		t.Fatalf("拒绝后环境应保留: %s", out)
	}
}

// 反例2 集成版（审查 P1）：配置声明平台修改后，init 必须拒绝复用平台不一致的
// 旧容器（不静默复用也不自动重建），且容器保持原状。
func TestNewUserPlatformChangeRejectedReal(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	id := projectContainerID(t, dir)

	// 修改配置声明平台（模拟用户编辑 .km.json）：声明取本机架构之外的平台，
	// 保证与实际镜像架构（本机构建）必然不一致
	otherPlat := "linux/amd64"
	if goruntime.GOARCH == "amd64" {
		otherPlat = "linux/arm64"
	}
	cfgPath := filepath.Join(dir, ".km.json")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"schema_version":1,"image":"kali-mac-min:0.2","platform":%q}`, otherPlat)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := kmRun(t, dir, nil, "init", "--platform", otherPlat); code != 1 || !strings.Contains(errb, "KM_CONTAINER_CONFLICT") {
		t.Fatalf("平台不一致的复用应拒绝: code=%d err=%s", code, errb)
	}
	// 容器未变：同 ID 仍运行
	if got := containerState(t, id); got != "running" {
		t.Fatalf("容器应保持原状: %s", got)
	}
}

// 反例4 集成版（审查 P2）：paused 容器 → status 报 container_paused（独立状态），
// 不报 stopped、不推断无会话。
func TestNewUserStatusPausedReal(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	id := registerProjectCleanup(t, dir)
	if _, _, c := kmRun(t, dir, nil, "run", "--", "/bin/true"); c != 0 {
		t.Fatal("run 失败")
	}
	if out, err := exec.Command("docker", "pause", id).CombinedOutput(); err != nil {
		t.Fatalf("pause: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "unpause", id).Run() })
	out, _, code := kmRun(t, dir, nil, "status")
	if code != 0 || !strings.Contains(out, "container_paused") || strings.Contains(out, "container_stopped") {
		t.Fatalf("paused 状态: code=%d out=%q", code, out)
	}
}

// km tools 真实集成：精选镜像六项全 AVAILABLE；只读性；长任务持锁时仍可查询。
func TestNewUserToolsReal(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init", "--image", "kali-mac-min:0.2"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	if _, _, c := kmRun(t, dir, nil, "run", "--", "/bin/true"); c != 0 {
		t.Fatal("首次 run（安装脚本）失败")
	}

	out, errb, code := kmRun(t, dir, nil, "tools")
	if code != 0 {
		t.Fatalf("tools: code=%d out=%q err=%q", code, out, errb)
	}
	for _, tl := range []string{"python3", "curl", "jq", "file", "openssl", "nmap"} {
		if !strings.Contains(out, "AVAILABLE "+tl) {
			t.Fatalf("精选镜像应含 %s:\n%s", tl, out)
		}
	}
	if !strings.Contains(out, "/usr/bin/") && !strings.Contains(out, "/usr/local/bin/") {
		t.Fatalf("AVAILABLE 应附解析路径:\n%s", out)
	}

	// 并发：长任务运行中 tools 仍可用（不持执行锁）
	cmd := kmRunAsync(t, dir, "run", "--", "/bin/sh", "-c", "sleep 30")
	waitSessionDir(t, projectContainerID(t, dir))
	out2, _, code2 := kmRun(t, dir, nil, "tools")
	if code2 != 0 || !strings.Contains(out2, "AVAILABLE python3") {
		t.Fatalf("并发 tools: code=%d out=%q", code2, out2)
	}
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()

	// 只读性：.km 前后逐字节一致
	snap := func() map[string]string {
		m := map[string]string{}
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				b, _ := os.ReadFile(p)
				m[p] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("快照失败: %v", err)
		}
		return m
	}
	before := snap()
	if _, _, c := kmRun(t, dir, nil, "tools"); c != 0 {
		t.Fatal("tools 失败")
	}
	after := snap()
	// 双向 diff：文件集合与内容都不得变化（新增也不允许）
	for p, c := range before {
		if after[p] != c {
			t.Fatalf("只读性破坏(修改/删除): %s", p)
		}
	}
	for p := range after {
		if _, existed := before[p]; !existed {
			t.Fatalf("只读性破坏(新增文件): %s", p)
		}
	}
	// 容器状态保持 running
	id := projectContainerID(t, dir)
	if got := containerState(t, id); got != "running" {
		t.Fatalf("容器状态变化: %s", got)
	}
}

// 停止容器 → tools 明示状态退出 1；不自动启动。
func TestNewUserToolsStoppedReal(t *testing.T) {
	dir := newP2BProject(t)
	if _, errb, code := kmRun(t, dir, nil, "init"); code != 0 {
		t.Fatalf("init: %s", errb)
	}
	registerProjectCleanup(t, dir)
	if _, _, c := kmRun(t, dir, nil, "run", "--", "/bin/true"); c != 0 {
		t.Fatal("run 失败")
	}
	if _, errb, c := kmRun(t, dir, nil, "stop"); c != 0 {
		t.Fatalf("stop: %s", errb)
	}
	id := projectContainerID(t, dir)
	_, errb, code := kmRun(t, dir, nil, "tools")
	if code != 1 || !strings.Contains(errb, "已停止") {
		t.Fatalf("停止状态: code=%d err=%q", code, errb)
	}
	if got := containerState(t, id); got != "exited" {
		t.Fatalf("tools 不得启动容器: %s", got)
	}
}

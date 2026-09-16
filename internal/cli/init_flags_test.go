package cli

// 包 A（init --image/--platform 与配置兑现）的单元验收。
// 合同：docs/newuser-iteration-plan.md「包 A 合同（冻结）」。

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runInitArgs 与 runInit 相同，但允许传显式参数。
func runInitArgs(t *testing.T, d *runtime.Docker, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb strings.Builder
	code := runInitCommand(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

// hasPlatformArg 断言 docker run/pull 调用里 --platform 的出现与取值。
func platformArgOf(calls []fakeCall, sub string) (string, bool) {
	val := ""
	found := false
	for _, c := range calls {
		if len(c.args) < 2 || c.args[1] != sub {
			continue
		}
		for i, a := range c.args {
			if a == "--platform" && i+1 < len(c.args) {
				val = c.args[i+1]
				found = true
			}
		}
	}
	return val, found
}

func writeConfigFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitFlagsNewProjectDefaults(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, out, errb := runInitArgs(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "环境就绪") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
	want := "linux/" + goruntime.GOARCH
	if v, ok := platformArgOf(execOf(d).calls, "run"); !ok || v != want {
		t.Fatalf("新项目应兑现 host-native 平台 %s: got %q found=%v", want, v, ok)
	}
	cfg, err := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PlatformDeclared || cfg.Platform != want || cfg.Image != project.DefaultImage {
		t.Fatalf("配置应显式声明 host-native 平台与默认镜像: %+v", cfg)
	}
	if !strings.Contains(out, want) || !strings.Contains(out, "非精选镜像") {
		t.Fatalf("结果消息应含平台与非精选提示: %q", out)
	}
}

func TestInitFlagsImageCuratedAndNonCurated(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, out, _ := runInitArgs(t, d, dir, "--image", "kali-mac-min:0.2")
	if code != ExitOK || !strings.Contains(out, "精选工具镜像") {
		t.Fatalf("精选镜像应有标记: code=%d out=%q", code, out)
	}
	cfg, _ := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if cfg.Image != "kali-mac-min:0.2" {
		t.Fatalf("配置应写入参数镜像: %+v", cfg)
	}

	dir2 := t.TempDir()
	d2 := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	code2, out2, _ := runInitArgs(t, d2, dir2, "--image", "foo:1")
	if code2 != ExitOK || !strings.Contains(out2, "非精选镜像") {
		t.Fatalf("非精选镜像应提示: code=%d out=%q", code2, out2)
	}
}

func TestInitFlagsPlatformNewProject(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, _, errb := runInitArgs(t, d, dir, "--platform", "linux/amd64", "--image", "img:2")
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errb)
	}
	if v, ok := platformArgOf(execOf(d).calls, "run"); !ok || v != "linux/amd64" {
		t.Fatalf("创建应携带 --platform linux/amd64: %q %v", v, ok)
	}
	cfg, _ := project.LoadConfig(filepath.Join(dir, project.ConfigFileName))
	if cfg.Platform != "linux/amd64" || !cfg.PlatformDeclared || cfg.Image != "img:2" {
		t.Fatalf("配置应记录参数: %+v", cfg)
	}
}

func TestInitFlagsConflictOnExistingConfig(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"img:1","platform":"linux/arm64"}`)
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))

	// --image 不一致 → 冲突，不创建、不覆盖
	code, _, errb := runInitArgs(t, d, dir, "--image", "other:1")
	if code != ExitEnv || !strings.Contains(errb, "KM_CONFIG_INVALID") || !strings.Contains(errb, "other:1") {
		t.Fatalf("image 冲突: code=%d err=%q", code, errb)
	}
	if hooks["create"] != 0 {
		t.Fatal("冲突时不得创建容器")
	}
	b, _ := os.ReadFile(filepath.Join(dir, project.ConfigFileName))
	if !strings.Contains(string(b), `"img:1"`) {
		t.Fatalf("配置不得被覆盖: %s", b)
	}

	// --platform 不一致 → 冲突
	code, _, errb = runInitArgs(t, d, dir, "--platform", "linux/amd64")
	if code != ExitEnv || !strings.Contains(errb, "KM_CONFIG_INVALID") {
		t.Fatalf("platform 冲突: code=%d err=%q", code, errb)
	}
	// 一致 → 放行（复用/创建按配置继续）
	code, _, errb = runInitArgs(t, d, dir, "--image", "img:1", "--platform", "linux/arm64")
	if code != ExitOK {
		t.Fatalf("一致参数应放行: code=%d err=%q", code, errb)
	}
}

// 旧配置兼容：platform 字段缺失 → 创建保持 native（argv 无 --platform）；
// --platform 一律拒绝（声明缺失不接受参数声明）。
func TestInitFlagsLegacyConfigCompat(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"img:1"}`)
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	code, _, errb := runInitArgs(t, d, dir)
	if code != ExitOK {
		t.Fatalf("旧配置 init: code=%d err=%q", code, errb)
	}
	if v, found := platformArgOf(execOf(d).calls, "run"); found {
		t.Fatalf("未声明平台的旧配置应保持 native（argv 无 --platform）: got %q", v)
	}
	// 回填容器表使第二次 init 走「复用现有环境」路径（真实场景：记录容器存在）
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	containers[st.Container.ID] = inspectLine(st.Container.ID, st.Container.Name, "running", st.ProjectID, sdImageID, resolveDir(t, dir))
	code2, out2, errb2 := runInitArgs(t, d, dir)
	if !strings.Contains(out2, "复用现有环境") || !strings.Contains(out2, "平台=未声明(本机架构)") {
		t.Fatalf("复用消息应含镜像与未声明平台标注: code=%d out=%q err=%q", code2, out2, errb2)
	}

	// --platform 对声明缺失的旧配置一律拒绝（即使值与默认相同）
	dir2 := t.TempDir()
	writeConfigFile(t, dir2, `{"schema_version":1,"image":"img:1"}`)
	d2 := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, _, errb = runInitArgs(t, d2, dir2, "--platform", "linux/arm64")
	if code != ExitEnv || !strings.Contains(errb, "KM_CONFIG_INVALID") || !strings.Contains(errb, "未声明") {
		t.Fatalf("旧配置 + --platform 应拒绝: code=%d err=%q", code, errb)
	}
}

// 接管路径（记录容器消失、同名容器匹配）的消息同样扩展镜像与平台标注。
func TestInitFlagsTakeoverMessageIncludesImageAndPlatform(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"img:1"}`)
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	if code, _, errb := runInitArgs(t, d, dir); code != ExitOK {
		t.Fatalf("首次 init: %s", errb)
	}
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 记录的容器消失；同名容器被「重建」但标签/挂载匹配 → 允许接管
	name := st.Container.Name
	containers[name] = inspectLine(sdContID, name, "running", st.ProjectID, sdImageID, resolveDir(t, dir))
	code, out, errb := runInitArgs(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "接管匹配的同名容器") ||
		!strings.Contains(out, "image=img:1") || !strings.Contains(out, "平台=未声明(本机架构)") {
		t.Fatalf("接管消息应含镜像与平台标注: code=%d out=%q err=%q", code, out, errb)
	}
}

// 冲突消息包含「编辑 .km.json」指引（合同文案）。
func TestInitFlagsConflictMessageGuidance(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"img:1","platform":"linux/arm64"}`)
	d := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	_, _, errb := runInitArgs(t, d, dir, "--image", "other:1")
	if !strings.Contains(errb, "编辑 .km.json") {
		t.Fatalf("冲突消息应含编辑指引: %q", errb)
	}
}

// 新项目 + --platform 兑现到拉取（缺失镜像场景）。
func TestInitFlagsPlatformHonoredOnPullNewProject(t *testing.T) {
	hooks := map[string]int{"pull": 0}
	dir := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, _, errb := runInitArgs(t, d, dir, "--image", "missing:1", "--platform", "linux/amd64")
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errb)
	}
	if v, ok := platformArgOf(execOf(d).calls, "pull"); !ok || v != "linux/amd64" {
		t.Fatalf("新项目拉取应携带声明平台: %q %v", v, ok)
	}
}

// 声明平台兑现到拉取：缺失镜像 + 已声明 platform → pull argv 含 --platform。
func TestInitFlagsPlatformHonoredOnPull(t *testing.T) {
	hooks := map[string]int{"pull": 0}
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"schema_version":1,"image":"missing:1","platform":"linux/arm64"}`)
	d := mkDocker(dockerResponder(t, map[string]string{}, hooks))
	code, _, errb := runInitArgs(t, d, dir)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errb)
	}
	if hooks["pull"] != 1 {
		t.Fatal("应拉取一次")
	}
	if v, ok := platformArgOf(execOf(d).calls, "pull"); !ok || v != "linux/arm64" {
		t.Fatalf("拉取应携带声明平台: %q %v", v, ok)
	}
}

func TestInitFlagsUsageErrors(t *testing.T) {
	dir := t.TempDir()
	d := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	for _, args := range [][]string{
		{"--image"},              // 缺值
		{"--platform"},           // 缺值
		{"--bogus"},              // 未知参数
		{"--image", "a b"},       // 空白
		{"--platform", "a", "x"}, // 多余位置参数
	} {
		code, _, errb := runInitArgs(t, d, dir, args...)
		if code != ExitUsage || !strings.Contains(errb, "KM_USAGE") {
			t.Fatalf("%v 应 usage: code=%d err=%q", args, code, errb)
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// ---------- fake docker（脚本化响应，记录调用） ----------

type fakeCall struct {
	args []string
}

type scriptedDocker struct {
	calls []fakeCall
	// respond 返回 stdout 与退出码（退出码非 0 时包装为 RunError）
	respond func(args []string) (string, int)
}

func (s *scriptedDocker) LookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func (s *scriptedDocker) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	full := append([]string{name}, args...)
	s.calls = append(s.calls, fakeCall{args: full})
	stdout, code := s.respond(full)
	if code != 0 {
		return []byte(stdout), []byte(stdout), &runtime.RunError{
			Err:      fmt.Errorf("exit status %d", code),
			Stderr:   []byte(stdout),
			ExitCode: code,
		}
	}
	return []byte(stdout), nil, nil
}

func (s *scriptedDocker) count(prefix string) int {
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(strings.Join(c.args, " "), prefix) {
			n++
		}
	}
	return n
}

const (
	sdImageID  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sdContID   = "aaaa1111222233334444555566667777888899990000aaaabbbbccccddddeeee"
	sdEndpoint = "unix:///Users/x/.docker/run/docker.sock"
)

// dockerResponder 覆盖 init/run/stop 流程所需的 docker 子命令。
// containers: ref -> inspect 行（6 字段）；missing 传入 "NOSUCH"。
func dockerResponder(t *testing.T, containers map[string]string, hooks map[string]int) func([]string) (string, int) {
	t.Helper()
	return func(args []string) (string, int) {
		switch {
		case args[1] == "context" && args[2] == "show":
			return "desktop-linux", 0
		case args[1] == "context" && args[2] == "inspect":
			return sdEndpoint, 0
		case args[1] == "image" && args[2] == "inspect" && strings.Contains(strings.Join(args, " "), "{{.Os}}/{{.Architecture}}"):
			// fake 宿主镜像架构 = host-native（与 init 写入的声明一致，平台无关）
			return "linux/" + goruntime.GOARCH, 0
		case args[1] == "image" && args[2] == "inspect":
			ref := args[len(args)-1]
			if ref == "missing:1" && hooks["pull"] == 0 {
				return "Error response from daemon: No such image: missing:1", 1
			}
			return sdImageID, 0
		case args[1] == "container" && args[2] == "inspect":
			ref := args[len(args)-1]
			if line, ok := containers[ref]; ok {
				return line, 0
			}
			return "Error response from daemon: No such container: " + ref, 1
		case args[1] == "run" && args[2] == "-d":
			hooks["create"]++
			return sdContID, 0
		case args[1] == "start":
			hooks["start"]++
			return "", 0
		case args[1] == "stop":
			hooks["stop"]++
			return "", 0
		case args[1] == "pull":
			hooks["pull"]++
			return "", 0
		case args[1] == "version":
			return "29.6.1|29.6.1|linux|aarch64", 0
		default:
			t.Fatalf("fake: 未预期的 docker 调用 %v", args)
			return "", 1
		}
	}
}

func inspectLine(id, name, state, label, image, mount string) string {
	return id + "|/" + name + "|" + state + "|" + label + "|" + image + "|" + mount
}

// resolveDir 解析 macOS 的 /tmp → /private/tmp 符号链接，
// 使夹具挂载源与产品经 os.Getwd() 看到的路径一致。
func resolveDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mkDocker(resp func([]string) (string, int)) *runtime.Docker {
	return &runtime.Docker{Exec: &scriptedDocker{respond: resp}}
}

func execOf(d *runtime.Docker) *scriptedDocker { return d.Exec.(*scriptedDocker) }

func runInit(t *testing.T, d *runtime.Docker, dir string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	code := runInitCommand(context.Background(), nil, &out, &errb, d)
	return code, out.String(), errb.String()
}

func validImgConfig() string {
	return `{"schema_version":1,"image":"img:1"}`
}

// ---------- init ----------

func TestInitFreshProject(t *testing.T) {
	hooks := map[string]int{}
	containers := map[string]string{} // 同名容器不存在
	d := mkDocker(dockerResponder(t, containers, hooks))
	dir := t.TempDir()

	code, out, errb := runInit(t, d, dir)
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "环境就绪") {
		t.Fatalf("out=%q", out)
	}
	if hooks["create"] != 1 {
		t.Fatalf("应创建一个容器: %d", hooks["create"])
	}
	// 配置与状态落盘
	if _, err := os.Stat(filepath.Join(dir, project.ConfigFileName)); err != nil {
		t.Fatalf(".km.json 缺失: %v", err)
	}
	st, err := project.LoadState(dir)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Container.ID != sdContID || st.Container.ImageID != sdImageID || st.Runtime.Endpoint != sdEndpoint {
		t.Fatalf("身份记录不完整: %+v", st)
	}
	if st.Container.Name != runtime.ContainerName(st.ProjectID) {
		t.Fatalf("容器名: %q", st.Container.Name)
	}
}

func TestInitIdempotentReuses(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	if code, _, errb := runInit(t, d, dir); code != ExitOK {
		t.Fatalf("首次 init: code=%d err=%s", code, errb)
	}
	st1, _ := project.LoadState(dir)
	// 第二次：容器按记录 ID 存在且匹配
	containers[sdContID] = inspectLine(sdContID, st1.Container.Name, "running", st1.ProjectID, sdImageID, resolveDir(t, dir))
	code, out, errb := runInit(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "复用") {
		t.Fatalf("code=%d out=%q err=%s", code, out, errb)
	}
	st2, _ := project.LoadState(dir)
	if st1.Container.ID != st2.Container.ID {
		t.Fatal("幂等 init 应复用同一容器")
	}
	if hooks["create"] != 1 {
		t.Fatalf("不应重复创建容器: %d", hooks["create"])
	}
}

func TestInitStoppedContainerStartedOnReuse(t *testing.T) {
	hooks := map[string]int{"start": 0}
	dir := t.TempDir()
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	runInit(t, d, dir)
	st, _ := project.LoadState(dir)
	containers[sdContID] = inspectLine(sdContID, st.Container.Name, "exited", st.ProjectID, sdImageID, resolveDir(t, dir))
	code, _, _ := runInit(t, d, dir)
	if code != ExitOK || hooks["start"] != 1 {
		t.Fatalf("停止的容器应在复用时启动: code=%d start=%d", code, hooks["start"])
	}
}

func TestInitPullsMissingImageOnce(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	// 预写配置指向缺失镜像
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(`{"schema_version":1,"image":"missing:1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	containers := map[string]string{}
	d := mkDocker(dockerResponder(t, containers, hooks))
	code, _, errb := runInit(t, d, dir)
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if hooks["pull"] != 1 {
		t.Fatalf("缺失镜像应触发一次拉取: %d", hooks["pull"])
	}
}

func TestInitNameConflictRejected(t *testing.T) {
	hooks := map[string]int{}
	dir := t.TempDir()
	// 同名容器已存在但归属他人
	d := mkDocker(func(args []string) (string, int) {
		switch {
		case args[1] == "context" && args[2] == "show":
			return "desktop-linux", 0
		case args[1] == "context" && args[2] == "inspect":
			return sdEndpoint, 0
		case args[1] == "image" && args[2] == "inspect":
			return sdImageID, 0
		case args[1] == "container" && args[2] == "inspect":
			// 名称查询返回他人容器；ID 查询不存在
			if len(args[len(args)-1]) == 64 && !strings.HasPrefix(args[len(args)-1], "km-") {
				return "Error: No such container", 1
			}
			return inspectLine(sdContID, "km-taken", "running", "pOTHER", sdImageID, "/other"), 0
		default:
			t.Fatalf("未预期调用 %v", args)
			return "", 1
		}
	})
	code, _, errb := runInit(t, d, dir)
	if code != ExitEnv || !strings.Contains(errb, "KM_CONTAINER_CONFLICT") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if hooks["create"] != 0 {
		t.Fatal("冲突时不应创建容器")
	}
	if project.StateExists(dir) {
		t.Fatal("冲突时不应写入状态")
	}
}

// docker run 是有副作用操作：客户端报错不代表 daemon 没有完成创建。
// 反例要求按唯一名称核实并登记完整 ID，且同一次 init 不得再次 create。
func TestInitCreateFailureRecoversCreatedContainer(t *testing.T) {
	hooks := map[string]int{}
	containers := map[string]string{}
	dir := t.TempDir()
	root := resolveDir(t, dir)
	base := dockerResponder(t, containers, hooks)
	d := mkDocker(func(args []string) (string, int) {
		if args[1] == "run" && args[2] == "-d" {
			hooks["create"]++
			var name, projectID string
			for i := range args {
				if args[i] == "--name" {
					name = args[i+1]
				}
				if args[i] == "--label" && strings.HasPrefix(args[i+1], runtime.ProjectLabel+"=") {
					projectID = strings.TrimPrefix(args[i+1], runtime.ProjectLabel+"=")
				}
			}
			containers[name] = inspectLine(sdContID, name, "running", projectID, sdImageID, root)
			return "client timed out after daemon accepted create", 1
		}
		return base(args)
	})

	code, out, errb := runInit(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "环境就绪") || !strings.Contains(errb, "核实到已创建容器") {
		t.Fatalf("应核实并登记已创建资源: code=%d out=%q err=%q", code, out, errb)
	}
	if hooks["create"] != 1 {
		t.Fatalf("不确定创建不得自动重试: create=%d", hooks["create"])
	}
	st, err := project.LoadState(dir)
	if err != nil || st.Container.ID != sdContID {
		t.Fatalf("应登记完整容器 ID: state=%+v err=%v", st, err)
	}
}

// 创建失败后的核验也失败时，状态只能是 unknown，不能报告不存在或成功。
func TestInitCreateFailureAndVerificationFailureIsUnknown(t *testing.T) {
	hooks := map[string]int{}
	inspectCount := 0
	dir := t.TempDir()
	base := dockerResponder(t, map[string]string{}, hooks)
	d := mkDocker(func(args []string) (string, int) {
		if args[1] == "container" && args[2] == "inspect" {
			inspectCount++
			if inspectCount == 1 { // 创建前：名称明确不存在
				return "Error response from daemon: No such container", 1
			}
			return "Cannot connect to the Docker daemon", 1
		}
		if args[1] == "run" && args[2] == "-d" {
			hooks["create"]++
			return "client timed out", 1
		}
		return base(args)
	})

	code, _, errb := runInit(t, d, dir)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeResourceUnknown) ||
		!strings.Contains(errb, "创建结果未知") || strings.Contains(errb, "零残留") {
		t.Fatalf("创建与核验双失败必须报告 unknown: code=%d err=%q", code, errb)
	}
	if hooks["create"] != 1 || project.StateExists(dir) {
		t.Fatalf("不得重试或伪造状态: create=%d state=%v", hooks["create"], project.StateExists(dir))
	}
}

// 创建报错后出现同名但身份不符的资源时必须拒绝接管。
func TestInitCreateFailureRecoveryRejectsIdentityMismatch(t *testing.T) {
	hooks := map[string]int{}
	containers := map[string]string{}
	dir := t.TempDir()
	base := dockerResponder(t, containers, hooks)
	d := mkDocker(func(args []string) (string, int) {
		if args[1] == "run" && args[2] == "-d" {
			hooks["create"]++
			var name string
			for i := range args {
				if args[i] == "--name" {
					name = args[i+1]
				}
			}
			containers[name] = inspectLine(sdContID, name, "running", "pOTHER", sdImageID, resolveDir(t, dir))
			return "client timed out", 1
		}
		return base(args)
	})

	code, _, errb := runInit(t, d, dir)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeContainerConflict) || project.StateExists(dir) {
		t.Fatalf("身份不符不得接管: code=%d err=%q state=%v", code, errb, project.StateExists(dir))
	}
	if hooks["create"] != 1 {
		t.Fatalf("不得重试 create: %d", hooks["create"])
	}
}

func stateWriteFailureFixture(t *testing.T, removeCode int) (int, string, map[string]int) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, project.StateDirName)
	if err := os.MkdirAll(filepath.Join(stateDir, "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := map[string]int{}
	d := mkDocker(func(args []string) (string, int) {
		switch {
		case args[1] == "container" && args[2] == "inspect":
			return "Error response from daemon: No such container", 1
		case args[1] == "run" && args[2] == "-d":
			hooks["create"]++
			return sdContID, 0
		case args[1] == "rm" && args[2] == "-f":
			hooks["remove"]++
			if removeCode != 0 {
				return "Cannot connect to the Docker daemon", removeCode
			}
			return sdContID, 0
		default:
			t.Fatalf("未预期调用 %v", args)
			return "", 1
		}
	})
	st := &project.State{StateVersion: project.SupportedStateVersion, ProjectID: "p1a2b3c4d5"}
	cfg := &project.Config{SchemaVersion: project.SupportedSchemaVersion, Image: "img:1"}
	var out, errb bytes.Buffer
	code := createContainerFor(context.Background(), d, &out, &errb, dir, cfg, st, sdEndpoint, sdImageID)
	return code, errb.String(), hooks
}

func TestInitStateWriteFailureConfirmsRollback(t *testing.T) {
	code, errb, hooks := stateWriteFailureFixture(t, 0)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeStateInvalid) || !strings.Contains(errb, "已回滚") {
		t.Fatalf("回滚成功应明确报告: code=%d err=%q", code, errb)
	}
	if hooks["create"] != 1 || hooks["remove"] != 1 {
		t.Fatalf("create/remove 次数错误: %+v", hooks)
	}
}

func TestInitStateWriteFailureAndRollbackFailureIsUnknown(t *testing.T) {
	code, errb, hooks := stateWriteFailureFixture(t, 1)
	if code != ExitEnv || !strings.Contains(errb, runtime.CodeResourceUnknown) ||
		!strings.Contains(errb, "回滚未确认") || !strings.Contains(errb, sdContID) {
		t.Fatalf("回滚失败必须报告 unknown 与完整 ID: code=%d err=%q", code, errb)
	}
	if hooks["create"] != 1 || hooks["remove"] != 1 {
		t.Fatalf("create/remove 次数错误: %+v", hooks)
	}
}

func TestInitParentProjectRejected(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, project.ConfigFileName), []byte(validImgConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(parent, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	d := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	code, _, errb := runInit(t, d, sub)
	if code != ExitEnv || !strings.Contains(errb, "KM_PROJECT_NESTED") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestInitEngineDriftRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(validImgConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	// 状态记录的 endpoint 与 fake 引擎不同
	os.WriteFile(project.StatePath(dir), []byte(
		`{"state_version":1,"project_id":"p1a2b3c4d5","container":{"id":"`+sdContID+`","name":"km-p1a2b3c4d5","image_id":"`+sdImageID+`"},"runtime":{"context":"other","endpoint":"unix:///other.sock"}}`), 0o644)
	d := mkDocker(dockerResponder(t, map[string]string{}, map[string]int{}))
	code, _, errb := runInit(t, d, dir)
	if code != ExitEnv || !strings.Contains(errb, "KM_RUNTIME_MISMATCH") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

// ---------- stop ----------

func runStop(t *testing.T, d *runtime.Docker, dir string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	code := runStopCommand(context.Background(), nil, &out, &errb, d)
	return code, out.String(), errb.String()
}

func setupReadyProject(t *testing.T, state string, containers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(validImgConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	full := fmt.Sprintf(`{"state_version":1,"project_id":"p1a2b3c4d5","container":{"id":%q,"name":"km-p1a2b3c4d5","image_id":%q},"runtime":{"context":"desktop-linux","endpoint":%q},"created_at":"2026-09-06T00:00:00Z"}`,
		sdContID, sdImageID, sdEndpoint)
	if state == "" {
		state = full
	}
	if err := os.WriteFile(project.StatePath(dir), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	containers[sdContID] = inspectLine(sdContID, "km-p1a2b3c4d5", "running", "p1a2b3c4d5", sdImageID, resolveDir(t, dir))
	return dir
}

func TestStopRunningContainer(t *testing.T) {
	hooks := map[string]int{}
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	d := mkDocker(dockerResponder(t, containers, hooks))
	code, out, errb := runStop(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "已停止") {
		t.Fatalf("code=%d out=%q err=%s", code, out, errb)
	}
	if hooks["stop"] != 1 {
		t.Fatalf("应调用一次 stop: %d", hooks["stop"])
	}
}

func TestStopIdempotentWhenAlreadyStopped(t *testing.T) {
	hooks := map[string]int{}
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	st, _ := project.LoadState(dir)
	containers[sdContID] = inspectLine(sdContID, st.Container.Name, "exited", st.ProjectID, sdImageID, resolveDir(t, dir))
	d := mkDocker(dockerResponder(t, containers, hooks))
	code, out, _ := runStop(t, d, dir)
	if code != ExitOK || !strings.Contains(out, "幂等") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if hooks["stop"] != 0 {
		t.Fatal("已停止的容器不应再调用 stop")
	}
}

// ---------- run（错误路径；正常路径走真实集成） ----------

func runTool(t *testing.T, d *runtime.Docker, dir string, argv ...string) (int, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	code := runToolCommand(context.Background(), argv[0], argv[1:], nil, &out, &errb, d)
	return code, errb.String()
}

func TestRunContainerMissingExplicit(t *testing.T) {
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	delete(containers, sdContID) // 记录的 ID 不存在（验证必须在 session 层之前失败）
	d := mkDocker(dockerResponder(t, containers, map[string]int{}))
	code, errb := runTool(t, d, dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, "KM_NOT_FOUND") || !strings.Contains(errb, "km init") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestRunImageDriftRejected(t *testing.T) {
	// 标签当前内容与记录不同
	other := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	d := mkDocker(func(args []string) (string, int) {
		switch {
		case args[1] == "context" && args[2] == "show":
			return "desktop-linux", 0
		case args[1] == "context" && args[2] == "inspect":
			return sdEndpoint, 0
		case args[1] == "image" && args[2] == "inspect":
			return other, 0
		case args[1] == "container" && args[2] == "inspect":
			return containers[args[len(args)-1]], 0
		default:
			t.Fatalf("未预期调用 %v", args)
			return "", 1
		}
	})
	code, errb := runTool(t, d, dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, "KM_IMAGE_DRIFT") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestRunBusyLockRejected(t *testing.T) {
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	// 预先放置活跃锁（当前进程 pid）
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(project.LockPath(dir), []byte(fmt.Sprintf("pid=%d\ncreated_at=now\n", os.Getpid())), 0o644)
	d := mkDocker(dockerResponder(t, containers, map[string]int{}))
	code, errb := runTool(t, d, dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, "KM_PROJECT_BUSY") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

func TestRunLegacyStateRefused(t *testing.T) {
	containers := map[string]string{}
	dir := setupReadyProject(t,
		`{"state_version":1,"project_id":"p1a2b3c4d5","container":{"name":"km-p1a2b3c4d5"},"runtime":{"context":"desktop-linux","endpoint":"`+sdEndpoint+`"}}`,
		containers)
	d := mkDocker(dockerResponder(t, containers, map[string]int{}))
	code, errb := runTool(t, d, dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, "KM_STATE_INVALID") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
}

// ---------- R2：宿主遗留锁 + 容器活跃会话 → 阻断新任务 ----------

// fakeSessionController：注入 newSessionController，记录调用并按脚本应答。
type fakeSessCtl struct {
	cpCalled      bool
	sweepCalled   bool
	sessionsCalls int
	sessionsOut   string
}

func (f *fakeSessCtl) controller() *session.DockerController {
	return &session.DockerController{
		RunFn: func(_ context.Context, _ []byte, args []string) (string, string, int, error) {
			switch {
			case args[0] == "cp":
				f.cpCalled = true
				return "", "", 0, nil
			case contains(args, "sessions"):
				f.sessionsCalls++
				return f.sessionsOut, "", 0, nil
			case contains(args, "sweep"):
				f.sweepCalled = true
				return "0", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func TestRunStaleLockActiveSessionBlocked(t *testing.T) {
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	// 遗留锁（死亡 pid）
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(project.LockPath(dir), []byte("pid=99999999\ntoken=x\n"), 0o644)
	d := mkDocker(dockerResponder(t, containers, map[string]int{}))
	fc := &fakeSessCtl{sessionsOut: "ACTIVE sabc123\n"}
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController { return fc.controller() }
	defer func() { newSessionController = old }()

	code, errb := runTool(t, d, dir, "nmap", "-h")
	if code != ExitEnv || !strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(errb, "sabc123") || !strings.Contains(errb, "cancel") {
		t.Fatalf("应给出会话身份与显式清理指引: %s", errb)
	}
	if !fc.cpCalled || fc.sessionsCalls < 1 {
		t.Fatalf("应先引导脚本再核验会话: cp=%v sessions=%d", fc.cpCalled, fc.sessionsCalls)
	}
}

func TestRunSweepsStaleSessionsNotBlocked(t *testing.T) {
	containers := map[string]string{}
	dir := setupReadyProject(t, "", containers)
	d := mkDocker(dockerResponder(t, containers, map[string]int{}))
	fc := &fakeSessCtl{sessionsOut: "STALE sold\n"}
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController { return fc.controller() }
	defer func() { newSessionController = old }()

	// STALE 不阻断；流程继续到真实会话执行（fake docker 下 exec 失败），错误不含 KM_SESSION_ACTIVE
	code, errb := runTool(t, d, dir, "nmap", "-h")
	if strings.Contains(errb, "KM_SESSION_ACTIVE") {
		t.Fatalf("遗留会话不应阻断: %s", errb)
	}
	if !fc.sweepCalled {
		t.Fatal("遗留会话应被清扫")
	}
	_ = code
}

// doctor 报告容器内会话状态。
func TestDoctorReportsActiveSessions(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	fc := &fakeSessCtl{sessionsOut: "ACTIVE sabc123\n"}
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController { return fc.controller() }
	defer func() { newSessionController = old }()
	// 不走 doctor() 辅助（它会覆盖会话控制器），直接注入后调用
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	var out bytes.Buffer
	code := RunDoctor(context.Background(), root, &out, &runtime.Docker{Exec: doctorFake(healthyContainerOut(resolveDir2(t, root)))})
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "容器内活跃会话") || !strings.Contains(out.String(), "sabc123") {
		t.Fatalf("doctor 应报告活跃会话:\n%s", out.String())
	}
}

func resolveDir2(t *testing.T, dir string) string {
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ---------- M0：会话查询失败/异常 → 阻断，且不执行工具 ----------

// startRecorder 实现 session.Starter，记录工具启动尝试。
type startRecorder struct {
	started [][]string
}

func (r *startRecorder) LookPath() (string, error) { return "/usr/bin/docker", nil }

func (r *startRecorder) Start(argv []string, _ io.Reader, _, _ io.Writer) (session.Proc, error) {
	r.started = append(r.started, argv)
	return nil, errors.New("blocked-by-test")
}

// fakeSessCtl 增加失败注入。
type fakeSessCtlFail struct {
	sessionsOut string
	failExit    int
	failErr     error
}

func (f *fakeSessCtlFail) controller() *session.DockerController {
	return &session.DockerController{
		RunFn: func(_ context.Context, _ []byte, args []string) (string, string, int, error) {
			switch {
			case args[0] == "cp":
				return "", "", 0, nil
			case contains(args, "sessions"):
				return f.sessionsOut, "diag-line", f.failExit, f.failErr
			case contains(args, "sweep"):
				return "0", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}
}

func TestSessionQueryFailureBlocksTool(t *testing.T) {
	cases := []struct {
		name     string
		sessions string
		exit     int
		runErr   error
		wantCode string
		proceed  bool
	}{
		{"查询执行失败", "", 0, errors.New("engine boom"), "KM_SESSION_UNKNOWN", false},
		{"查询退出码非0", "", 7, nil, "KM_SESSION_UNKNOWN", false},
		{"输出不可解析", "garbage-line\n", 0, nil, "KM_SESSION_UNKNOWN", false},
		{"空会话正常放行", "", 0, nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			containers := map[string]string{}
			dir := setupReadyProject(t, "", containers)
			d := mkDocker(dockerResponder(t, containers, map[string]int{}))
			sess := &fakeSessCtlFail{sessionsOut: tc.sessions, failExit: tc.exit, failErr: tc.runErr}
			rec := &startRecorder{}
			oldCtl, oldMgr := newSessionController, newSessionManager
			newSessionController = func(endpoint string) *session.DockerController { return sess.controller() }
			newSessionManager = func(endpoint string, diag io.Writer, ctl *session.DockerController) *session.Manager {
				return &session.Manager{Starter: rec, Controller: ctl, SkipBootstrap: true}
			}
			defer func() { newSessionController, newSessionManager = oldCtl, oldMgr }()

			code, errb := runTool(t, d, dir, "nmap", "-h")
			if tc.proceed {
				if len(rec.started) == 0 {
					t.Fatalf("空会话应放行并尝试启动工具, code=%d err=%s", code, errb)
				}
				return
			}
			if code != ExitEnv || !strings.Contains(errb, tc.wantCode) {
				t.Fatalf("code=%d err=%s", code, errb)
			}
			if len(rec.started) != 0 {
				t.Fatalf("失败分支不应启动工具: %v", rec.started)
			}
		})
	}
}

// doctor：会话查询失败/异常 → 检查失败（不当作无会话）。
func TestDoctorSessionQueryFailureReported(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	sess := &fakeSessCtlFail{sessionsOut: "garbage\n", failExit: 3}
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController { return sess.controller() }
	defer func() { newSessionController = old }()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	var out bytes.Buffer
	RunDoctor(context.Background(), root, &out, &runtime.Docker{Exec: doctorFake(healthyContainerOut(resolveDir2(t, root)))})
	if !strings.Contains(out.String(), "检查失败") {
		t.Fatalf("doctor 应报告会话检查失败:\n%s", out.String())
	}
}

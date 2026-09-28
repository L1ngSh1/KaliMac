package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// ---------- env 测试用结构化 docker fake ----------

var (
	envImgARef = "img-a:1"
	envImgBRef = "img-b:2"
	envImgAID  = "sha256:" + strings.Repeat("1", 64)
	envImgBID  = "sha256:" + strings.Repeat("2", 64)
	envOldID   = "aaaa1111222233334444555566667777888899990000aaaabbbbccccddddeeee"
	envProjID  = "p1a2b3c4d5"
)

type envContainer struct {
	id, name, state, project, image, mount string
	opID, role                             string
}

// fakeExecutor 把 envFake 的 respond 适配为 runtime.Executor。
type fakeExecutor struct {
	respond func(args []string) ([]byte, []byte, error)
}

func (e *fakeExecutor) LookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func (e *fakeExecutor) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	return e.respond(append([]string{name}, args...))
}

// envFake 是 env 流程的脚本化 docker：真实维护容器/镜像映射，可注入故障。
type envFake struct {
	t     *testing.T
	imgs  map[string]string
	plats map[string]string
	conts map[string]*envContainer
	names map[string]string
	seq   int

	hooks map[string]int

	failStopNext  bool // 下一次 stop 返回错误（不改变状态）
	createLostRsp bool // run -d 注册容器但客户端报错（响应丢失）

	// onStopRetagRef/To：stop 发生时把引用改指向别的内容（模拟切换/回退
	// 过程中的标签漂移，用于提交前复验测试）
	onStopRetagRef string
	onStopRetagTo  string
	// onStopHook：stop 发生时执行（模拟事务中的外部文件修改/IO 故障注入）
	onStopHook func()

	// 最近一次 run -d 的关键参数（断言创建细节用）
	lastMount    string
	lastPlatform string
	lastImageArg string
	lastGenLabel string
}

func newEnvFake(t *testing.T) *envFake {
	return &envFake{
		t:     t,
		imgs:  map[string]string{},
		plats: map[string]string{},
		conts: map[string]*envContainer{},
		names: map[string]string{},
		hooks: map[string]int{},
	}
}

func (f *envFake) addImage(ref, id string) {
	f.imgs[ref] = id
	f.plats[id] = "linux/" + goruntime.GOARCH
}

func (f *envFake) addContainer(c envContainer) string {
	f.seq++
	if c.id == "" {
		c.id = fmt.Sprintf("%064x", 0xb000+f.seq)
	}
	cc := c
	f.conts[cc.id] = &cc
	f.names[cc.name] = cc.id
	return cc.id
}

func (f *envFake) docker() *runtime.Docker {
	return &runtime.Docker{Exec: &fakeExecutor{respond: f.respond}}
}

func (f *envFake) resolveRef(ref string) (string, bool) {
	if id, ok := f.imgs[ref]; ok {
		return id, true
	}
	if _, ok := f.plats[ref]; ok {
		return ref, true // 直接按内容 ID 查询
	}
	return "", false
}

func envFail(stdout string, code int) ([]byte, []byte, error) {
	return []byte(stdout), []byte(stdout), &runtime.RunError{
		Err: fmt.Errorf("exit status %d", code), Stderr: []byte(stdout), ExitCode: code,
	}
}

func (f *envFake) respond(args []string) ([]byte, []byte, error) {
	full := args
	switch {
	case full[1] == "context" && full[2] == "show":
		return []byte("desktop-linux"), nil, nil
	case full[1] == "context" && full[2] == "inspect":
		return []byte(sdEndpoint), nil, nil
	case full[1] == "image" && full[2] == "inspect" && strings.Contains(strings.Join(full, " "), "{{.Os}}/{{.Architecture}}"):
		ref := full[len(full)-1]
		id, ok := f.resolveRef(ref)
		if !ok {
			return envFail("Error response from daemon: No such image: "+ref, 1)
		}
		return []byte(f.plats[id]), nil, nil
	case full[1] == "image" && full[2] == "inspect":
		ref := full[len(full)-1]
		id, ok := f.resolveRef(ref)
		if !ok {
			return envFail("Error response from daemon: No such image: "+ref, 1)
		}
		return []byte(id), nil, nil
	case full[1] == "container" && full[2] == "inspect":
		ref := full[len(full)-1]
		c := f.byRef(ref)
		if c == nil {
			return envFail("Error response from daemon: No such container: "+ref, 1)
		}
		line := fmt.Sprintf("%s|/%s|%s|%s|%s|%s", c.id, c.name, c.state, c.project, c.image, c.mount)
		return []byte(line), nil, nil
	case full[1] == "run" && full[2] == "-d":
		f.hooks["create"]++
		c := envContainer{state: "running", project: envProjID, mount: ""}
		valueOpts := map[string]bool{"--name": true, "--label": true, "-v": true, "--platform": true}
		image := ""
		for i := 3; i < len(full); i++ {
			arg := full[i]
			if valueOpts[arg] {
				i++
				switch arg {
				case "--name":
					c.name = full[i]
				case "--label":
					kv := strings.SplitN(full[i], "=", 2)
					switch kv[0] {
					case runtime.ProjectLabel:
						c.project = kv[1]
					case runtime.OpLabel:
						c.opID = kv[1]
					case runtime.GenLabel:
						f.lastGenLabel = kv[1]
					case runtime.RoleLabel:
						c.role = kv[1]
					}
				case "-v":
					f.lastMount = full[i]
					m := full[i]
					m = strings.TrimSuffix(m, ":ro")
					m = strings.TrimSuffix(m, ":/workspace")
					c.mount = m
				case "--platform":
					f.lastPlatform = full[i]
				}
				continue
			}
			if arg == "-d" || arg == "--init" || strings.HasPrefix(arg, "-") {
				continue
			}
			if image == "" {
				image = arg
			}
		}
		if c.name == "" || image == "" {
			f.t.Fatalf("fake: run -d 缺少 name/image: %v", full)
		}
		f.lastImageArg = image
		id, ok := f.resolveRef(image)
		if !ok {
			return envFail("Error response from daemon: No such image: "+image, 1)
		}
		c.image = id
		id0 := f.addContainer(c)
		if f.createLostRsp {
			return envFail("client timed out after daemon accepted create", 1)
		}
		return []byte(id0), nil, nil
	case full[1] == "start":
		f.hooks["start"]++
		ref := full[len(full)-1]
		c := f.byRef(ref)
		if c == nil {
			return envFail("Error response from daemon: No such container: "+ref, 1)
		}
		c.state = "running"
		return nil, nil, nil
	case full[1] == "stop":
		f.hooks["stop"]++
		ref := full[len(full)-1]
		c := f.byRef(ref)
		if c == nil {
			return envFail("Error response from daemon: No such container: "+ref, 1)
		}
		if f.failStopNext {
			f.failStopNext = false
			return envFail("Cannot connect to the Docker daemon", 1)
		}
		c.state = "exited"
		if f.onStopRetagRef != "" {
			f.imgs[f.onStopRetagRef] = f.onStopRetagTo
		}
		if f.onStopHook != nil {
			f.onStopHook()
		}
		return nil, nil, nil
	case full[1] == "rm" && full[2] == "-f":
		f.hooks["rm"]++
		ref := full[len(full)-1]
		c := f.byRef(ref)
		if c == nil {
			return envFail("Error response from daemon: No such container: "+ref, 1)
		}
		delete(f.conts, c.id)
		delete(f.names, c.name)
		return nil, nil, nil
	case full[1] == "version":
		return []byte("29.6.1|29.6.1|linux|aarch64"), nil, nil
	case strings.HasSuffix(full[0], "sw_vers"):
		return []byte("14.5.0"), nil, nil
	case full[1] == "ps" && full[2] == "-a":
		var key, val string
		for i := range full {
			if full[i] == "--filter" && i+1 < len(full) && strings.HasPrefix(full[i+1], "label=") {
				kv := strings.SplitN(strings.TrimPrefix(full[i+1], "label="), "=", 2)
				key, val = kv[0], kv[1]
			}
		}
		var out strings.Builder
		for _, c := range f.conts {
			match := false
			switch key {
			case runtime.OpLabel:
				match = c.opID == val
			case runtime.ProjectLabel:
				match = c.project == val
			}
			if match {
				fmt.Fprintf(&out, "%s %s %s\n", c.id, c.name, c.state)
			}
		}
		return []byte(out.String()), nil, nil
	default:
		f.t.Fatalf("envFake: 未预期的 docker 调用 %v", full)
		return nil, nil, nil
	}
}

func (f *envFake) byRef(ref string) *envContainer {
	if c, ok := f.conts[ref]; ok {
		return c
	}
	if id, ok := f.names[ref]; ok {
		return f.conts[id]
	}
	return nil
}

// envSessCtl 是会话控制器 fake：探测与查询按脚本应答。
type envSessCtl struct {
	missingDeps   []string
	probeErr      error
	probeExit     int
	sessionsOut   string
	sessionsExit  int
	sessionsErr   error
	sweepCalled   bool
	sessionsCalls int
	probeCalls    int
}

func (f *envSessCtl) controller() *session.DockerController {
	return &session.DockerController{
		RunFn: func(_ context.Context, _ []byte, args []string) (string, string, int, error) {
			switch {
			case args[0] == "cp":
				return "", "", 0, nil
			case args[0] == "exec" && len(args) > 3 && args[2] == "/bin/sh":
				f.probeCalls++
				if f.probeErr != nil {
					return "", "", -1, f.probeErr
				}
				var out strings.Builder
				for _, d := range args[6:] {
					if contains(f.missingDeps, d) {
						out.WriteString(d + "=MISSING\n")
					} else {
						out.WriteString(d + "=OK\n")
					}
				}
				return out.String(), "", f.probeExit, nil
			case args[0] == "exec" && len(args) > 3 && args[2] == session.CtlScriptPath && args[3] == "sessions":
				f.sessionsCalls++
				return f.sessionsOut, "", f.sessionsExit, f.sessionsErr
			case args[0] == "exec" && len(args) > 3 && args[2] == session.CtlScriptPath && args[3] == "sweep":
				f.sweepCalled = true
				return "0", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}
}

func installEnvSessCtl(f *envSessCtl) func() {
	old := newSessionController
	newSessionController = func(string) *session.DockerController { return f.controller() }
	return func() { newSessionController = old }
}

// ---------- 夹具与运行辅助 ----------

func envWriteProject(t *testing.T, dir, configJSON, stateJSON string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, project.ConfigFileName), []byte(configJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, project.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.StatePath(dir), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

func envV1State() string {
	return fmt.Sprintf(`{"state_version":1,"project_id":%q,"container":{"id":%q,"name":"km-%s","image_id":%q},"runtime":{"context":"desktop-linux","endpoint":%q},"created_at":"2026-01-01T00:00:00Z"}`,
		envProjID, envOldID, envProjID, envImgAID, sdEndpoint)
}

func envConfigA() string {
	return `{"schema_version":1,"name":"demo","image":"` + envImgARef + `","platform":"linux/` + goruntime.GOARCH + `"}`
}

// setupEnvReady 准备一个 v1 就绪项目：镜像 A 本地存在，容器 running。
func setupEnvReady(t *testing.T) (*envFake, string, *envSessCtl) {
	t.Helper()
	dir := t.TempDir()
	f := newEnvFake(t)
	f.addImage(envImgARef, envImgAID)
	root := resolveDir(t, dir)
	f.addContainer(envContainer{id: envOldID, name: "km-" + envProjID, state: "running", project: envProjID, image: envImgAID, mount: root})
	envWriteProject(t, dir, envConfigA(), envV1State())
	sess := &envSessCtl{}
	t.Cleanup(installEnvSessCtl(sess))
	return f, dir, sess
}

// setupEnvPostSwitch 准备一个“已完成一次 switch”的 v2 项目：
// 当前为第 1 代（容器 km-<pid>-g1，镜像 B，running），回退槽位指向第 0 代。
func setupEnvPostSwitch(t *testing.T) (*envFake, string, *envSessCtl, string) {
	t.Helper()
	dir := t.TempDir()
	f := newEnvFake(t)
	f.addImage(envImgARef, envImgAID)
	f.addImage(envImgBRef, envImgBID)
	root := resolveDir(t, dir)
	gen1ID := f.addContainer(envContainer{name: "km-" + envProjID + "-g1", state: "running", project: envProjID, image: envImgBID, mount: root})
	prevID := f.addContainer(envContainer{id: envOldID, name: "km-" + envProjID, state: "exited", project: envProjID, image: envImgAID, mount: root})
	envWriteProject(t, dir,
		`{"schema_version":1,"name":"demo","image":"`+envImgBRef+`","platform":"linux/`+goruntime.GOARCH+`"}`,
		fmt.Sprintf(`{"state_version":2,"project_id":%q,"container":{"id":%q,"name":"km-%s-g1","image_id":%q},"runtime":{"context":"desktop-linux","endpoint":%q},"created_at":"2026-01-01T00:00:00Z","env":{"env_version":1,"generation":1}}`,
			envProjID, gen1ID, envProjID, envImgBID, sdEndpoint))
	if err := envtxn.SavePrevious(dir, &envtxn.Previous{
		Generation: 0, ContainerID: prevID, ContainerName: "km-" + envProjID,
		ImageID: envImgAID, ImageRef: envImgARef, WasRunning: true, OpID: "e00000000000000aa",
	}); err != nil {
		t.Fatal(err)
	}
	sess := &envSessCtl{}
	t.Cleanup(installEnvSessCtl(sess))
	return f, dir, sess, gen1ID
}

func runEnv(t *testing.T, d *runtime.Docker, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	var out, errb bytes.Buffer
	code := runEnvCommand(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

// noMutations 断言拒绝场景没有发生容器或文件变更（P1 完成门槛）。
func noMutations(t *testing.T, f *envFake, dir string, stateBefore []byte) {
	t.Helper()
	noContainerMutations(t, f, dir)
	for _, p := range []string{envtxn.TransactionPath(dir), envtxn.PreviousPath(dir), envtxn.RetainedPath(dir)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 不应存在: %v", filepath.Base(p), err)
		}
	}
	if stateBefore != nil {
		after, err := os.ReadFile(project.StatePath(dir))
		if err != nil || !bytes.Equal(stateBefore, after) {
			t.Fatalf("状态文件被改动: %v", err)
		}
	}
}

// noMutationsKeepEnv 同 noMutations，但允许夹具预置的 env 记录存在。
func noMutationsKeepEnv(t *testing.T, f *envFake, dir string, stateBefore []byte) {
	t.Helper()
	noContainerMutations(t, f, dir)
	if _, err := os.Stat(envtxn.TransactionPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("事务不应存在: %v", err)
	}
	if stateBefore != nil {
		after, err := os.ReadFile(project.StatePath(dir))
		if err != nil || !bytes.Equal(stateBefore, after) {
			t.Fatalf("状态文件被改动: %v", err)
		}
	}
}

func noContainerMutations(t *testing.T, f *envFake, dir string) {
	t.Helper()
	for _, k := range []string{"create", "stop", "start", "rm"} {
		if f.hooks[k] != 0 {
			t.Fatalf("拒绝场景不应有 %s 调用: %d", k, f.hooks[k])
		}
	}
	if _, err := os.Stat(project.LockPath(dir)); !os.IsNotExist(err) {
		t.Fatal("锁文件不应残留")
	}
}

func readStateBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(project.StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kalimac/internal/project"
	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

const (
	fakeVersionOut  = "29.6.1|29.6.1|linux|aarch64"
	fakeContainerID = "c1abc123def4567890abcdef1234567890abcdef1234567890abcdef12345678"
	rebuiltID       = "9999aaaabbbbccccddddeeeeffff000011112222333344445555666677778888"
	fakeImageID     = "sha256:imgabc123"
	fakeProjectID   = "p1a2b3c4d5"
	localEndpoint   = "unix:///Users/x/.docker/run/docker.sock"
)

// doctorFake builds an executor answering the client-side and daemon calls
// doctor makes. containerOut is returned for container inspect; pass a
// special value to control that branch.
func doctorFake(containerOut string) *runtime.FakeExecutor {
	return &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				return []byte(containerOut), nil, nil
			case args[0] == "image":
				return []byte(fakeImageID), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
}

// containerLine builds an inspect output row: ID|Name|State|Label|Image|Mount.
func containerLine(id, name, state, label, image, mount string) string {
	return id + "|/" + name + "|" + state + "|" + label + "|" + image + "|" + mount
}

func healthyContainerOut(root string) string {
	return containerLine(fakeContainerID, "km-"+fakeProjectID, "running", fakeProjectID, fakeImageID, root)
}

func stateJSON(id, imageID, endpoint string) string {
	return fmt.Sprintf(`{"state_version":1,"project_id":%q,"container":{"id":%q,"name":"km-%s","image_id":%q},"runtime":{"context":"desktop-linux","endpoint":%q},"created_at":"2026-09-06T00:00:00Z"}`,
		fakeProjectID, id, fakeProjectID, imageID, endpoint)
}

func setupProject(t *testing.T, cfg, state string) string {
	t.Helper()
	root := t.TempDir()
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(root, project.ConfigFileName), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if state != "" {
		if err := os.MkdirAll(filepath.Join(root, project.StateDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(project.StatePath(root), []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func doctor(t *testing.T, dir string, fe *runtime.FakeExecutor) (int, string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	// 隔离会话检查：默认无会话脚本（各用例可再覆盖）
	old := newSessionController
	newSessionController = func(endpoint string) *session.DockerController {
		return &session.DockerController{RunFn: func(_ context.Context, _ []byte, _ []string) (string, string, int, error) {
			return "", "", 0, nil
		}}
	}
	t.Cleanup(func() { newSessionController = old })
	var out bytes.Buffer
	code := RunDoctor(context.Background(), dir, &out, &runtime.Docker{Exec: fe})
	return code, out.String()
}

const validConfig = `{"schema_version":1,"image":"img:1"}`

func TestDoctorNoDockerCLI(t *testing.T) {
	fe := &runtime.FakeExecutor{LookPathErr: errors.New("not found")}
	code, out := doctor(t, t.TempDir(), fe)
	if code != ExitOK {
		t.Fatalf("doctor 本身应完成, code=%d", code)
	}
	if !strings.Contains(out, runtime.CodeRuntimeMissing) {
		t.Fatalf("缺少 %s: %q", runtime.CodeRuntimeMissing, out)
	}
}

func TestDoctorEngineOffline(t *testing.T) {
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return nil, []byte("Cannot connect to the Docker daemon. Is the docker daemon running?"), runtime.RunErr("Cannot connect", 1)
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, t.TempDir(), fe)
	if !strings.Contains(out, runtime.CodeRuntimeOffline) {
		t.Fatalf("缺少 %s: %q", runtime.CodeRuntimeOffline, out)
	}
}

func TestDoctorRemoteHostSkipsDaemonQueries(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://fixture.invalid")
	var out bytes.Buffer
	code := RunDoctor(context.Background(), t.TempDir(), &out, &runtime.Docker{Exec: doctorFake("")})
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	s := out.String()
	if !strings.Contains(s, runtime.CodeEndpointRemote) || !strings.Contains(s, "跳过所有引擎查询") {
		t.Fatalf("缺少远程判定:\n%s", s)
	}
	if strings.Contains(s, "client=") {
		t.Fatalf("远程 endpoint 下不应有引擎查询结果:\n%s", s)
	}
}

// F1 回归：默认 context 是远程时，必须先判失败且不发任何 daemon 请求。
func TestDoctorRemoteContextNoDaemonCalls(t *testing.T) {
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("review-remote"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				if args[len(args)-1] != "review-remote" {
					t.Errorf("inspect 的 context 名不正确: %v", args)
				}
				return []byte("ssh://fixture.invalid"), nil, nil
			case args[0] == "-productVersion": // macVersion 的 sw_vers 调用
				return []byte("26.6.2"), nil, nil
			default:
				t.Errorf("远程 endpoint 下不应发起 daemon/资源查询: %v", args)
				return nil, nil, errors.New("forbidden daemon query")
			}
		},
	}
	_, out := doctor(t, t.TempDir(), fe)
	if !strings.Contains(out, runtime.CodeEndpointRemote) {
		t.Fatalf("缺少 %s:\n%s", runtime.CodeEndpointRemote, out)
	}
}

// F1 回归：DOCKER_CONTEXT 指向远程 context 时同样拦截。
func TestDoctorRemoteViaEnvContextNoDaemonCalls(t *testing.T) {
	// 不走 doctor() 辅助函数：它会把 DOCKER_HOST/DOCKER_CONTEXT 清空。
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "review-remote")
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "inspect":
				if args[len(args)-1] != "review-remote" {
					t.Errorf("应 inspect DOCKER_CONTEXT 指定的 context: %v", args)
				}
				return []byte("ssh://fixture.invalid"), nil, nil
			case args[0] == "-productVersion":
				return []byte("26.6.2"), nil, nil
			default:
				if args[0] == "context" && args[1] == "show" {
					t.Errorf("DOCKER_CONTEXT 已设置时不应调用 context show: %v", args)
				}
				t.Errorf("远程 endpoint 下不应发起 daemon/资源查询: %v", args)
				return nil, nil, errors.New("forbidden daemon query")
			}
		},
	}
	var out bytes.Buffer
	RunDoctor(context.Background(), t.TempDir(), &out, &runtime.Docker{Exec: fe})
	if !strings.Contains(out.String(), runtime.CodeEndpointRemote) {
		t.Fatalf("缺少 %s:\n%s", runtime.CodeEndpointRemote, out.String())
	}
}

// F1 回归：本机状态记录的 endpoint 与当前有效 endpoint 漂移 → 失败并跳过容器/镜像检查。
func TestDoctorStoredRuntimeDrift(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, "unix:///other/engine.sock"))
	calls := 0
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container", args[0] == "image", args[0] == "ps":
				t.Errorf("身份漂移后不应查询容器/镜像: %v", args)
				return nil, nil, errors.New("forbidden")
			default:
				calls++
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, runtime.CodeRuntimeMismatch) || !strings.Contains(out, "跳过容器与镜像检查") {
		t.Fatalf("缺少漂移判定:\n%s", out)
	}
	if strings.Contains(out, "标签归属") || strings.Contains(out, "镜像:") {
		t.Fatalf("漂移后不应有容器/镜像结论:\n%s", out)
	}
	_ = calls
}

// F1 回归：DOCKER_HOST 与 DOCKER_CONTEXT 冲突时提示 DOCKER_HOST 生效。
func TestDoctorEnvConflictWarning(t *testing.T) {
	// 不走 doctor() 辅助函数：它会把 DOCKER_HOST/DOCKER_CONTEXT 清空。
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	t.Setenv("DOCKER_CONTEXT", "some-context")
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch args[0] {
			case "version":
				return []byte(fakeVersionOut), nil, nil
			case "-productVersion": // macVersion 的 sw_vers 调用
				return []byte("26.6.2"), nil, nil
			default:
				t.Errorf("DOCKER_HOST 生效时不应有其他调用: %v", args)
				return nil, nil, errors.New("forbidden")
			}
		},
	}
	var out bytes.Buffer
	RunDoctor(context.Background(), t.TempDir(), &out, &runtime.Docker{Exec: fe})
	if !strings.Contains(out.String(), "DOCKER_HOST 生效") {
		t.Fatalf("缺少冲突提示:\n%s", out.String())
	}
}

func TestDoctorNoProject(t *testing.T) {
	_, out := doctor(t, t.TempDir(), doctorFake(""))
	if !strings.Contains(out, runtime.CodeProjectMissing) {
		t.Fatalf("缺少 %s: %q", runtime.CodeProjectMissing, out)
	}
}

func TestDoctorInvalidConfig(t *testing.T) {
	root := setupProject(t, `{"schema_version":2,"image":"x"}`, "")
	_, out := doctor(t, root, doctorFake(""))
	if !strings.Contains(out, runtime.CodeConfigInvalid) || !strings.Contains(out, "schema_version") {
		t.Fatalf("应指出 schema_version: %q", out)
	}
}

func TestDoctorCorruptState(t *testing.T) {
	root := setupProject(t, validConfig, "{broken")
	_, out := doctor(t, root, doctorFake(""))
	if !strings.Contains(out, runtime.CodeStateInvalid) {
		t.Fatalf("缺少 %s: %q", runtime.CodeStateInvalid, out)
	}
}

func TestDoctorHealthyStack(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	code, out := doctor(t, root, doctorFake(healthyContainerOut(root)))
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"有效 endpoint: " + localEndpoint,
		"client=29.6.1",
		"运行时身份与项目记录一致",
		"按记录 ID 查找",
		"标签归属",
		"匹配",
		"=> /workspace",
		"容器镜像内容与项目记录一致",
		"镜像: " + fakeImageID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "0 失败") {
		t.Fatalf("健康场景不应有失败项:\n%s", out)
	}
}

// F2 回归：容器被同名重建（记录 ID 已不存在，名字指向新容器）→ 冲突且不接管。
func TestDoctorContainerRebuiltSameName(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				ref := args[len(args)-1]
				if ref == fakeContainerID {
					return nil, []byte("Error response from daemon: No such container: " + ref), runtime.RunErr("No such container", 1)
				}
				// 按名字查询 → 返回重建后的新容器
				return []byte(containerLine(rebuiltID, "km-"+fakeProjectID, "running", fakeProjectID, fakeImageID, root)), nil, nil
			case args[0] == "image":
				return []byte(fakeImageID), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, runtime.CodeContainerConflict) || !strings.Contains(out, "同名重建") {
		t.Fatalf("应报告同名重建冲突:\n%s", out)
	}
	if !strings.Contains(out, "c1abc123def4") || !strings.Contains(out, "9999aaaabb") {
		t.Fatalf("应指出两个不同 ID:\n%s", out)
	}
}

// F2 回归：旧版状态没有容器 ID → 报不完整，且不按名称接管。
func TestDoctorOldStateWithoutContainerID(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON("", fakeImageID, localEndpoint))
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				t.Errorf("状态缺少容器 ID 时不应按名称查询容器: %v", args)
				return nil, nil, errors.New("forbidden")
			case args[0] == "image":
				return []byte(fakeImageID), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, "缺少容器 ID") || !strings.Contains(out, "无法核验容器归属") {
		t.Fatalf("应报告状态不完整:\n%s", out)
	}
}

func TestDoctorContainerLabelConflict(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	out2 := containerLine(fakeContainerID, "km-"+fakeProjectID, "running", "pOTHER", fakeImageID, "/somewhere/else")
	_, out := doctor(t, root, doctorFake(out2))
	if !strings.Contains(out, runtime.CodeContainerConflict) {
		t.Fatalf("缺少 %s:\n%s", runtime.CodeContainerConflict, out)
	}
	if !strings.Contains(out, "pOTHER") || !strings.Contains(out, "/somewhere/else") {
		t.Fatalf("应指出冲突细节:\n%s", out)
	}
}

func TestDoctorContainerMissing(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				return nil, []byte("Error response from daemon: No such container"), runtime.RunErr("No such container", 1)
			case args[0] == "image":
				return []byte(fakeImageID), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, "不存在") || !strings.Contains(out, "init") {
		t.Fatalf("应报告容器缺失并提示 init:\n%s", out)
	}
}

// F3 回归：镜像标签内容与项目记录漂移 → 警告；容器实际内容与记录一致。
func TestDoctorImageTagDrift(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	const newImage = "sha256:dddd4444eeee5555aaaaaaaaaaaabbbbccccddddeeeeffff00001111222233"
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				return []byte(containerLine(fakeContainerID, "km-"+fakeProjectID, "running", fakeProjectID, fakeImageID, root)), nil, nil
			case args[0] == "image":
				return []byte(newImage), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, "漂移") {
		t.Fatalf("应报告镜像标签漂移:\n%s", out)
	}
	if !strings.Contains(out, "容器镜像内容与项目记录一致") {
		t.Fatalf("容器内容与记录一致时不应报冲突:\n%s", out)
	}
}

// F3 回归：容器实际镜像与项目记录不一致 → 失败。
func TestDoctorContainerImageMismatch(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	const otherImage = "sha256:eeee5555ffffffff0000111111111111222233334444555566667777888899aa"
	out2 := containerLine(fakeContainerID, "km-"+fakeProjectID, "running", fakeProjectID, otherImage, root)
	_, out := doctor(t, root, doctorFake(out2))
	if !strings.Contains(out, "容器实际镜像") || !strings.Contains(out, runtime.CodeContainerConflict) {
		t.Fatalf("应报告容器镜像内容不一致:\n%s", out)
	}
}

func TestDoctorMissingImage(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch {
			case args[0] == "context" && args[1] == "show":
				return []byte("desktop-linux"), nil, nil
			case args[0] == "context" && args[1] == "inspect":
				return []byte(localEndpoint), nil, nil
			case args[0] == "version":
				return []byte(fakeVersionOut), nil, nil
			case args[0] == "container":
				return []byte(healthyContainerOut(root)), nil, nil
			case args[0] == "image":
				return nil, []byte("Error: No such image: img:1"), runtime.RunErr("No such image", 1)
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, "不在本地") {
		t.Fatalf("应报告镜像缺失:\n%s", out)
	}
}

// doctor 必须是只读的：整个检查过程不得写项目目录。
func TestDoctorDoesNotWrite(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	before := snapshot(t, root)
	doctor(t, root, doctorFake(healthyContainerOut(root)))
	after := snapshot(t, root)
	if before != after {
		t.Fatalf("doctor 改动了项目目录:\nbefore=%s\nafter=%s", before, after)
	}
}

func snapshot(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		sb.WriteString(path)
		if !info.IsDir() {
			raw, _ := os.ReadFile(path)
			sb.WriteString(":" + string(raw))
		}
		sb.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// F2 边界回归：inspect 返回的 ID 与记录不一致必须报冲突。
func TestDoctorInspectIDMismatch(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	// fake 按 ID 查询却返回另一个 ID（模拟异常实现/伪造响应）
	wrongID := containerLine(rebuiltID, "km-"+fakeProjectID, "running", fakeProjectID, fakeImageID, root)
	_, out := doctor(t, root, doctorFake(wrongID))
	if !strings.Contains(out, "inspect 返回的容器 ID 与记录不一致") {
		t.Fatalf("应报告 inspect ID 不一致:\n%s", out)
	}
	if !strings.Contains(out, runtime.CodeContainerConflict) {
		t.Fatalf("缺少 %s:\n%s", runtime.CodeContainerConflict, out)
	}
}

// 挂载路径规范化：结尾斜杠应视为同一目录。
func TestDoctorMountPathNormalized(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	out2 := containerLine(fakeContainerID, "km-"+fakeProjectID, "running", fakeProjectID, fakeImageID, root+"/")
	_, out := doctor(t, root, doctorFake(out2))
	if !strings.Contains(out, "=> /workspace") || strings.Contains(out, "挂载源为") {
		t.Fatalf("结尾斜杠的挂载源应规范化匹配:\n%s", out)
	}
}

// 回归（真实冒烟发现）：init 记录与容器挂载源都是符号链接解析后的路径
// （macOS 上 /tmp → /private/tmp、/var/folders → /private/var/folders），
// doctor 的项目根必须做同一规范化，否则健康项目被误判 KM_CONTAINER_CONFLICT。
func TestDoctorMountSourceCanonicalized(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	canon := project.CanonicalPath(root)
	if canon == root {
		t.Skipf("TempDir 已是规范化路径（%s），本用例无区分度", root)
	}
	// 容器实际挂载源是规范化路径（init 以 CanonicalPath 写入 docker -v）
	_, out := doctor(t, root, doctorFake(healthyContainerOut(canon)))
	if strings.Contains(out, "挂载源为") || !strings.Contains(out, "0 失败") {
		t.Fatalf("等价写法的挂载源不应误报冲突:\n%s", out)
	}
}

// 非法容器 ID 的状态应被 LoadState 拒绝（doctor 报状态损坏）。
func TestDoctorIllegalContainerIDState(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON("abc123", fakeImageID, localEndpoint))
	_, out := doctor(t, root, doctorFake(""))
	if !strings.Contains(out, runtime.CodeStateInvalid) || !strings.Contains(out, "container.id") {
		t.Fatalf("非法容器 ID 应判状态损坏:\n%s", out)
	}
}

// F2 回归：inspect 返回的容器名与记录不一致 → 冲突。
func TestDoctorContainerNameMismatch(t *testing.T) {
	root := setupProject(t, validConfig, stateJSON(fakeContainerID, fakeImageID, localEndpoint))
	out2 := containerLine(fakeContainerID, "km-someoneelse", "running", fakeProjectID, fakeImageID, root)
	_, out := doctor(t, root, doctorFake(out2))
	if !strings.Contains(out, `容器名称 "km-someoneelse" 与记录`) {
		t.Fatalf("应报告容器名称不一致:\n%s", out)
	}
}

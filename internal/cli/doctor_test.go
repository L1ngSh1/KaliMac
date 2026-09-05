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
)

const (
	fakeVersionOut  = "29.6.1|29.6.1|linux|aarch64"
	fakeContainerID = "sha256:c1abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890"
	fakeProjectID   = "p1a2b3c4d5"
)

// doctorFake builds an executor answering the docker calls doctor makes.
func doctorFake(containerOut string) *runtime.FakeExecutor {
	return &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch args[0] {
			case "version":
				return []byte(fakeVersionOut), nil, nil
			case "context":
				return []byte("desktop-linux"), nil, nil
			case "container":
				return []byte(containerOut), nil, nil
			case "image":
				return []byte("sha256:imgabc123"), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
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
	var out bytes.Buffer
	code := RunDoctor(context.Background(), dir, &out, &runtime.Docker{Exec: fe})
	return code, out.String()
}

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
			return nil, []byte("Cannot connect to the Docker daemon. Is the docker daemon running?"), runtime.RunErr("Cannot connect", 1)
		},
	}
	_, out := doctor(t, t.TempDir(), fe)
	if !strings.Contains(out, runtime.CodeRuntimeOffline) {
		t.Fatalf("缺少 %s: %q", runtime.CodeRuntimeOffline, out)
	}
}

func TestDoctorRemoteEndpointRejected(t *testing.T) {
	// 不走 doctor() 辅助函数：它会把 DOCKER_HOST 清空。
	t.Setenv("DOCKER_HOST", "tcp://remote-host:2375")
	var out bytes.Buffer
	code := RunDoctor(context.Background(), t.TempDir(), &out, &runtime.Docker{Exec: doctorFake("")})
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), runtime.CodeEndpointRemote) {
		t.Fatalf("缺少 %s: %q", runtime.CodeEndpointRemote, out.String())
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
	root := setupProject(t, `{"schema_version":1,"image":"img:1"}`, "{broken")
	_, out := doctor(t, root, doctorFake(""))
	if !strings.Contains(out, runtime.CodeStateInvalid) {
		t.Fatalf("缺少 %s: %q", runtime.CodeStateInvalid, out)
	}
}

func TestDoctorHealthyStack(t *testing.T) {
	root := setupProject(t,
		`{"schema_version":1,"image":"img:1"}`,
		fmt.Sprintf(`{"state_version":1,"project_id":%q,"container":{"name":"km-%s"},"runtime":{"context":"desktop-linux","endpoint":"unix:///x.sock"},"created_at":"2026-09-06T00:00:00Z"}`,
			fakeProjectID, fakeProjectID))
	container := fmt.Sprintf("%s|/km-%s|running|%s|%s", fakeContainerID, fakeProjectID, fakeProjectID, root)
	code, out := doctor(t, root, doctorFake(container))
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"client=29.6.1", "标签归属", "匹配", "挂载", "=> /workspace", "sha256:imgabc123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "0 失败") {
		t.Fatalf("健康场景不应有失败项:\n%s", out)
	}
}

func TestDoctorContainerLabelConflict(t *testing.T) {
	root := setupProject(t,
		`{"schema_version":1,"image":"img:1"}`,
		fmt.Sprintf(`{"state_version":1,"project_id":%q,"container":{"name":"km-%s"}}`, fakeProjectID, fakeProjectID))
	// 容器存在但标签属于别的项目，且挂载不对
	container := fmt.Sprintf("%s|/km-%s|running|pOTHER|/somewhere/else", fakeContainerID, fakeProjectID)
	_, out := doctor(t, root, doctorFake(container))
	if !strings.Contains(out, runtime.CodeContainerConflict) {
		t.Fatalf("缺少 %s:\n%s", runtime.CodeContainerConflict, out)
	}
	if !strings.Contains(out, "pOTHER") || !strings.Contains(out, "/somewhere/else") {
		t.Fatalf("应指出冲突细节:\n%s", out)
	}
}

func TestDoctorContainerMissing(t *testing.T) {
	root := setupProject(t,
		`{"schema_version":1,"image":"img:1"}`,
		fmt.Sprintf(`{"state_version":1,"project_id":%q,"container":{"name":"km-%s"}}`, fakeProjectID, fakeProjectID))
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch args[0] {
			case "version":
				return []byte(fakeVersionOut), nil, nil
			case "context":
				return []byte("desktop-linux"), nil, nil
			case "container":
				return nil, []byte("Error response from daemon: No such container: km-" + fakeProjectID), runtime.RunErr("No such container", 1)
			case "image":
				return []byte("sha256:imgabc123"), nil, nil
			default:
				return nil, nil, fmt.Errorf("fake: 未预期的调用 %v", args)
			}
		},
	}
	_, out := doctor(t, root, fe)
	if !strings.Contains(out, "不存在") {
		t.Fatalf("应报告容器缺失:\n%s", out)
	}
}

func TestDoctorMissingImage(t *testing.T) {
	root := setupProject(t, `{"schema_version":1,"image":"img:1"}`, "")
	fe := &runtime.FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			switch args[0] {
			case "version":
				return []byte(fakeVersionOut), nil, nil
			case "context":
				return []byte("desktop-linux"), nil, nil
			case "image":
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
	root := setupProject(t, `{"schema_version":1,"image":"img:1"}`, "")
	before := snapshot(t, root)
	doctor(t, root, doctorFake(""))
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

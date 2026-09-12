package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The fake docker must see exactly the argv km built, element by element —
// never a joined shell string.
func TestVersionOverFakeExecutorArgv(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			if strings.Join(args, " ") != "version --format {{.Client.Version}}|{{.Server.Version}}|{{.Server.Os}}|{{.Server.Arch}}" {
				t.Fatalf("docker argv = %q", strings.Join(args, " "))
			}
			return []byte("29.6.1|29.6.1|linux|aarch64"), nil, nil
		},
	}
	d := &Docker{Exec: fe}
	ver, err := d.Version(context.Background())
	if err != nil {
		t.Fatalf("Version 返回错误: %v", err)
	}
	if ver.Client != "29.6.1" || ver.Server != "29.6.1" || ver.ServerOS != "linux" || ver.ServerArc != "aarch64" {
		t.Fatalf("Version 字段不正确: %+v", ver)
	}
}

func TestVersionOfflineClassification(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			return nil, []byte("Cannot connect to the Docker daemon at unix:///Users/x/.docker/run/docker.sock. Is the docker daemon running?"), RunErr("Cannot connect to the Docker daemon", 1)
		},
	}
	d := &Docker{Exec: fe}
	_, err := d.Version(context.Background())
	if err == nil || !IsOffline(err) {
		t.Fatalf("期望 KM_RUNTIME_OFFLINE, got: %v", err)
	}
	var kmerr *Error
	if !errors.As(err, &kmerr) || kmerr.Code != CodeRuntimeOffline {
		t.Fatalf("错误未携带 KM_RUNTIME_OFFLINE: %v", err)
	}
}

func TestDockerCLIMissingClassification(t *testing.T) {
	fe := &FakeExecutor{LookPathErr: errors.New("exec: \"docker\": executable file not found in $PATH")}
	d := &Docker{Exec: fe}
	_, err := d.Version(context.Background())
	if err == nil || !IsMissing(err) {
		t.Fatalf("期望 KM_RUNTIME_MISSING, got: %v", err)
	}
}

func TestVersionMalformedOutput(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			return []byte("garbage-without-pipes"), nil, nil
		},
	}
	d := &Docker{Exec: fe}
	_, err := d.Version(context.Background())
	if err == nil {
		t.Fatal("畸形输出必须报错")
	}
}

func TestFindContainersByLabelParsing(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--filter label=km.project=p1a2b3c4d5") {
				t.Fatalf("缺少 label 过滤: %q", joined)
			}
			return []byte("abc123def456 km-p1a2b3c4d5 running\n\nxyz789 km-other exited\n"), nil, nil
		},
	}
	d := &Docker{Exec: fe}
	cs, err := d.FindContainersByLabel(context.Background(), ProjectLabel, "p1a2b3c4d5")
	if err != nil {
		t.Fatalf("FindContainersByLabel: %v", err)
	}
	if len(cs) != 2 || cs[0].ID != "abc123def456" || cs[0].State != "running" || cs[1].Name != "km-other" {
		t.Fatalf("解析结果不正确: %+v", cs)
	}
}

func TestInspectContainerNotFound(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			return nil, []byte("Error response from daemon: No such container: km-p1"), RunErr("Error response from daemon: No such container: km-p1", 1)
		},
	}
	d := &Docker{Exec: fe}
	res, exists, err := d.InspectContainer(context.Background(), "km-p1")
	if err != nil || exists {
		t.Fatalf("不存在的容器应返回 (zero, false, nil), got exists=%v err=%v", exists, err)
	}
	if res.State != "" {
		t.Fatalf("不存在时应返回零值: %+v", res)
	}
}

func TestInspectContainerFields(t *testing.T) {
	const out = "sha256:abc123def456789|/km-p1a2b3c4d5|running|p1a2b3c4d5|sha256:img123|/Users/me/我的 项目"
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			return []byte(out), nil, nil
		},
	}
	d := &Docker{Exec: fe}
	res, exists, err := d.InspectContainer(context.Background(), "km-p1a2b3c4d5")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if res.Name != "km-p1a2b3c4d5" || res.ProjectID != "p1a2b3c4d5" || res.State != "running" {
		t.Fatalf("字段解析不正确: %+v", res)
	}
	if res.Image != "sha256:img123" {
		t.Fatalf("容器实际镜像解析不正确: %q", res.Image)
	}
	if res.MountSource != "/Users/me/我的 项目" {
		t.Fatalf("挂载源解析不正确: %q", res.MountSource)
	}
}

func TestImageIDNotFound(t *testing.T) {
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			return nil, []byte("Error response from daemon: No such image: nope:latest"), RunErr("No such image", 1)
		},
	}
	d := &Docker{Exec: fe}
	id, ok, err := d.ImageID(context.Background(), "nope:latest")
	if err != nil || ok || id != "" {
		t.Fatalf("缺失镜像应返回 (\"\", false, nil), got %q %v %v", id, ok, err)
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName("p1a2b3c4d5"); got != "km-p1a2b3c4d5" {
		t.Fatalf("got %q", got)
	}
}

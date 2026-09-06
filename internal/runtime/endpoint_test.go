package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsLocalEndpoint(t *testing.T) {
	cases := []struct {
		ep    string
		local bool
	}{
		{"unix:///Users/x/.docker/run/docker.sock", true},
		{"unix:///var/run/docker.sock", true},
		{"npipe:////./pipe/docker_engine", true},
		{"tcp://127.0.0.1:2375", true},
		{"tcp://localhost:2375", true},
		{"tcp://[::1]:2375", true},
		{"ssh://fixture.invalid", false},
		{"tcp://192.168.1.10:2375", false},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		if got := IsLocalEndpoint(tc.ep); got != tc.local {
			t.Errorf("IsLocalEndpoint(%q) = %v, 期望 %v", tc.ep, got, tc.local)
		}
	}
}

// F1：DOCKER_HOST 环境变量最优先，且不得发起任何 docker CLI 调用。
func TestEffectiveEndpointHostEnvWins(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://fixture.invalid")
	t.Setenv("DOCKER_CONTEXT", "whatever")
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			t.Errorf("DOCKER_HOST 已设置时不应调用 docker CLI: %v", args)
			return nil, nil, errors.New("forbidden")
		},
	}
	ep, err := (&Docker{Exec: fe}).EffectiveEndpoint(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ep.Source != "DOCKER_HOST" || ep.Endpoint != "ssh://fixture.invalid" {
		t.Fatalf("ep=%+v", ep)
	}
}

// F1：DOCKER_CONTEXT 决定 inspect 哪个 context。
func TestEffectiveEndpointEnvContext(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "myctx")
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			if args[0] == "context" && args[1] == "inspect" && args[len(args)-1] == "myctx" {
				return []byte("unix:///run/docker.sock"), nil, nil
			}
			t.Errorf("未预期的调用 %v", args)
			return nil, nil, errors.New("forbidden")
		},
	}
	ep, err := (&Docker{Exec: fe}).EffectiveEndpoint(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ep.Source != "DOCKER_CONTEXT" || ep.Context != "myctx" || ep.Endpoint != "unix:///run/docker.sock" {
		t.Fatalf("ep=%+v", ep)
	}
}

func TestEffectiveEndpointCurrentContext(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			if args[0] == "context" && args[1] == "show" {
				return []byte("desktop-linux"), nil, nil
			}
			if args[0] == "context" && args[1] == "inspect" && args[len(args)-1] == "desktop-linux" {
				return []byte("unix:///Users/x/.docker/run/docker.sock"), nil, nil
			}
			t.Errorf("未预期的调用 %v", args)
			return nil, nil, errors.New("forbidden")
		},
	}
	ep, err := (&Docker{Exec: fe}).EffectiveEndpoint(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ep.Source != "context" || ep.Context != "desktop-linux" {
		t.Fatalf("ep=%+v", ep)
	}
	if !IsLocalEndpoint(ep.Endpoint) {
		t.Fatalf("endpoint 应为本地: %q", ep.Endpoint)
	}
}

func TestEffectiveEndpointEmptyEndpoint(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	fe := &FakeExecutor{
		Respond: func(name string, args []string) ([]byte, []byte, error) {
			if args[0] == "context" && args[1] == "show" {
				return []byte("broken"), nil, nil
			}
			if args[0] == "context" && args[1] == "inspect" {
				return []byte(""), nil, nil
			}
			t.Errorf("未预期的调用 %v", args)
			return nil, nil, errors.New("forbidden")
		},
	}
	_, err := (&Docker{Exec: fe}).EffectiveEndpoint(context.Background())
	if err == nil || !strings.Contains(err.Error(), "未定义 docker endpoint") {
		t.Fatalf("空 endpoint 应报错: %v", err)
	}
}

// F4：失去响应的管理查询必须超时，且错误可识别为 KM_TIMEOUT。
func TestDockerRunManagementTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slowdocker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Exec: CommandExecutor{}, DockerPath: script, ManagementTimeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := d.Version(context.Background())
	elapsed := time.Since(start)
	if err == nil || !IsTimeout(err) {
		t.Fatalf("期望 KM_TIMEOUT, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时应及时生效, 耗时 %v", elapsed)
	}
}

// F4：用户取消应返回 KM_CANCELED。
func TestDockerRunCanceled(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slowdocker")
	os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	d := &Docker{Exec: CommandExecutor{}, DockerPath: script, ManagementTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.Version(ctx)
	if err == nil || !IsCanceled(err) {
		t.Fatalf("期望 KM_CANCELED, got %v", err)
	}
}

// endpoint 固定应通过环境注入到子进程。
func TestWithEnvInjection(t *testing.T) {
	ce := CommandExecutor{ExtraEnv: []string{"DOCKER_HOST=unix:///tmp/x.sock"}}
	out, _, err := ce.Run(context.Background(), "/bin/sh", "-c", `printf %s "$DOCKER_HOST"`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if string(out) != "unix:///tmp/x.sock" {
		t.Fatalf("环境未注入: %q", out)
	}
}

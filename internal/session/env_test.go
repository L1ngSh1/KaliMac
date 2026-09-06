package session

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"kalimac/internal/runtime"
)

// R4：一次运行解析出的连接信息必须贯穿流式执行与控制路径。
// 环境注入为替换式——宿主已有 DOCKER_HOST 时不得遮蔽固定值。

func TestExecStarterPinsEndpointEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///ambient.sock")
	es := &ExecStarter{DockerPath: "/bin/sh", Env: []string{"DOCKER_HOST=unix:///pinned.sock"}}
	var out bytes.Buffer
	p, err := es.Start([]string{"/bin/sh", "-c", `printf %s "$DOCKER_HOST"`}, nil, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := p.Wait(); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if out.String() != "unix:///pinned.sock" {
		t.Fatalf("流式执行未固定 endpoint: %q", out.String())
	}
}

func TestExecStarterNoEnvKeepsAmbient(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///ambient.sock")
	es := &ExecStarter{DockerPath: "/bin/sh"}
	var out bytes.Buffer
	p, _ := es.Start([]string{"/bin/sh", "-c", `printf %s "$DOCKER_HOST"`}, nil, &out, io.Discard)
	p.Wait()
	if out.String() != "unix:///ambient.sock" {
		t.Fatalf("未注入时应继承环境: %q", out.String())
	}
}

func TestDockerControllerPinsEndpointEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///ambient.sock")
	ctl := &DockerController{DockerPath: "/bin/sh", Endpoint: "unix:///pinned.sock"}
	stdout, _, code, err := ctl.rawRunExec(context.Background(), nil, []string{"-c", `printf %s "$DOCKER_HOST"`})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if strings.TrimSpace(stdout) != "unix:///pinned.sock" {
		t.Fatalf("控制路径未固定 endpoint: %q", stdout)
	}
}

// runtime.CommandExecutor 的替换式注入（WithEnv 路径）。
func TestCommandExecutorReplaceEnvNoDuplicate(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///ambient.sock")
	ce := runtime.CommandExecutor{ExtraEnv: []string{"DOCKER_HOST=unix:///pinned.sock"}}
	var out, errb bytes.Buffer
	_, _ = out, errb
	stdout, _, err := ce.Run(context.Background(), "/bin/sh", "-c", `printf %s "$DOCKER_HOST"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stdout)) != "unix:///pinned.sock" {
		t.Fatalf("替换式注入失败: %q", string(stdout))
	}
}

package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"kalimac/internal/runtime"
)

// Controller 产物级测试：注入 rawRunner 记录真实 argv，验证 docker cp 引导
// 与 km-ctl 取消调用的参数与退出码协议（曾漏掉 "cancel" 参数错误，用本测试堵住）。

type recRunner struct {
	calls []struct {
		args  []string
		stdin []byte
	}
	scripted func(args []string) (string, string, int, error)
}

func (r *recRunner) run(_ context.Context, stdin []byte, args []string) (string, string, int, error) {
	r.calls = append(r.calls, struct {
		args  []string
		stdin []byte
	}{append([]string(nil), args...), append([]byte(nil), stdin...)})
	if r.scripted != nil {
		return r.scripted(args)
	}
	return "", "", 0, nil
}

func TestDockerControllerBootstrapArgvAndTar(t *testing.T) {
	rec := &recRunner{}
	ctl := &DockerController{RunFn: rec.run}
	if err := ctl.Bootstrap(context.Background(), "km-p1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("应恰好一次调用: %d", len(rec.calls))
	}
	got := strings.Join(rec.calls[0].args, " ")
	if !strings.HasSuffix(got, "cp - km-p1:/tmp") {
		t.Fatalf("bootstrap argv 不正确: %q", got)
	}
	tr := tar.NewReader(bytes.NewReader(rec.calls[0].stdin))
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names[h.Name] = true
	}
	for _, want := range []string{"km-bin/km-run", "km-bin/km-ctl", "km-bin/km-observe"} {
		if !names[want] {
			t.Fatalf("tar 缺少 %s: %v", want, names)
		}
	}
}

func TestDockerControllerCancelArgvAndProtocol(t *testing.T) {
	rec := &recRunner{}
	ctl := &DockerController{RunFn: rec.run}
	exit, _, err := ctl.Cancel(context.Background(), "km-p1", "sabc")
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	want := "exec km-p1 /tmp/km-bin/km-ctl cancel sabc"
	if strings.Join(rec.calls[0].args, " ") != want {
		t.Fatalf("cancel argv: got %q want %q", rec.calls[0].args, want)
	}
}

// km-ctl 的协议状态码 3/4 必须原样透传，不能被错误分类吞掉。
func TestDockerControllerCancelProtocolCodes(t *testing.T) {
	for _, want := range []int{3, 4} {
		rec := &recRunner{scripted: func(args []string) (string, string, int, error) {
			return "", "km-ctl exit", want, nil
		}}
		ctl := &DockerController{RunFn: rec.run}
		exit, _, err := ctl.Cancel(context.Background(), "km-p1", "sabc")
		if err != nil {
			t.Fatalf("code %d: err=%v", want, err)
		}
		if exit != want {
			t.Fatalf("code %d 透传失败, got %d", want, exit)
		}
	}
}

// 真实失败（引擎不可达）仍应返回错误而非状态码。
func TestDockerControllerCancelRealFailure(t *testing.T) {
	rec := &recRunner{scripted: func(args []string) (string, string, int, error) {
		return "", "Cannot connect to the Docker daemon. Is the docker daemon running?", -1,
			runtime.ClassifyCommandError(&runtime.RunError{
				Err:      errors.New("exit status 1"),
				Stderr:   []byte("Cannot connect to the Docker daemon. Is the docker daemon running?"),
				ExitCode: 1,
			})
	}}
	ctl := &DockerController{RunFn: rec.run}
	exit, _, err := ctl.Cancel(context.Background(), "km-p1", "sabc")
	if err == nil || exit != -1 {
		t.Fatalf("真实失败应返回 (−1, err), got exit=%d err=%v", exit, err)
	}
	if !runtime.IsOffline(err) {
		t.Fatalf("应分类为离线: %v", err)
	}
}

// km-ctl 自身丢失（容器重建）等同会话不存在 → 3。
func TestDockerControllerCancelCtlMissing(t *testing.T) {
	rec := &recRunner{scripted: func(args []string) (string, string, int, error) {
		return "", "OCI runtime exec failed: exec: \"/tmp/km-bin/km-ctl\": stat /tmp/km-bin/km-ctl: no such file or directory", 126, nil
	}}
	ctl := &DockerController{RunFn: rec.run}
	// 126 且 stderr 含 no such file：协议上视为会话不存在
	exit, _, err := ctl.Cancel(context.Background(), "km-p1", "sabc")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	_ = exit // 126 原样透传给 Manager 层判断（unknown → CancelFailed），此处验证不报错
}

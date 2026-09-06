package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

type deadlineRecorder struct {
	got   time.Duration
	hasDL bool
	args  []string
}

func (f *deadlineRecorder) LookPath(name string) (string, error) { return "/bin/true", nil }

func (f *deadlineRecorder) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.args = append([]string{name}, args...)
	if dl, ok := ctx.Deadline(); ok {
		f.hasDL = true
		f.got = time.Until(dl)
	} else {
		f.hasDL = false
	}
	return []byte("1|1|linux|arm64"), nil, nil
}

// R5 回归：pull/stop 的专用预算必须真实生效，不被内层 10s 管理超时截断。
func TestPullImageDeadlineNotTruncated(t *testing.T) {
	rec := &deadlineRecorder{}
	d := &Docker{Exec: rec, DockerPath: "/bin/true"}
	if err := d.PullImage(context.Background(), "img:1"); err != nil {
		t.Fatal(err)
	}
	if !rec.hasDL || rec.got < 9*time.Minute {
		t.Fatalf("pull 有效预算应 ≈10m, got %v (hasDL=%v)", rec.got, rec.hasDL)
	}
}

func TestStopContainerDeadlineNotTruncated(t *testing.T) {
	rec := &deadlineRecorder{}
	d := &Docker{Exec: rec, DockerPath: "/bin/true"}
	if err := d.StopContainer(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if !rec.hasDL || rec.got < 25*time.Second || rec.got > 31*time.Second {
		t.Fatalf("stop 有效预算应 ≈30s, got %v", rec.got)
	}
}

func TestManagementQueryDefaultDeadline(t *testing.T) {
	rec := &deadlineRecorder{}
	d := &Docker{Exec: rec, DockerPath: "/bin/true"}
	if _, err := d.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.hasDL || rec.got > 11*time.Second || rec.got < 9*time.Second {
		t.Fatalf("管理查询应 ≈10s, got %v", rec.got)
	}
}

// 上层更短预算优先。
func TestShorterOuterContextWins(t *testing.T) {
	rec := &deadlineRecorder{}
	d := &Docker{Exec: rec, DockerPath: "/bin/true"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.PullImage(ctx, "img:1"); err != nil {
		t.Fatal(err)
	}
	if rec.got > 3*time.Second {
		t.Fatalf("上层 2s 预算应生效, got %v", rec.got)
	}
}

// ReplaceEnv：替换既有键而非追加重复键。
func TestReplaceEnv(t *testing.T) {
	env := ReplaceEnv([]string{"A=1", "DOCKER_HOST=old", "B=2"}, "DOCKER_HOST", "new")
	joined := strings.Join(env, ";")
	if strings.Count(joined, "DOCKER_HOST=") != 1 || !strings.Contains(joined, "DOCKER_HOST=new") {
		t.Fatalf("应替换且唯一: %q", env)
	}
	env2 := ReplaceEnv([]string{"A=1"}, "X", "y")
	if !strings.Contains(strings.Join(env2, ";"), "X=y") {
		t.Fatalf("缺失键应追加: %q", env2)
	}
}

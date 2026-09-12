// Package runtime wraps the local Docker CLI. The command executor is
// injectable so tests can drive km with a fake docker binary.
package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// RunError carries the exit status and captured stderr of a failed command.
type RunError struct {
	Err      error
	Stderr   []byte
	ExitCode int // -1 when the process did not start or was not an exit error
}

func (e *RunError) Error() string {
	if len(e.Stderr) > 0 {
		return string(bytes.TrimRight(e.Stderr, "\n"))
	}
	return e.Err.Error()
}

func (e *RunError) Unwrap() error { return e.Err }

// Executor runs an external command and captures its streams. argv is
// passed through verbatim; implementations must never build a shell string.
type Executor interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args ...string) (stdout []byte, stderr []byte, err error)
}

// CommandExecutor is the production Executor backed by os/exec. ExtraEnv,
// when set, is appended to the child environment (used to pin DOCKER_HOST).
type CommandExecutor struct {
	ExtraEnv []string
}

func (c CommandExecutor) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (c CommandExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if len(c.ExtraEnv) > 0 {
		env := os.Environ()
		for _, kv := range c.ExtraEnv {
			if k, _, ok := strings.Cut(kv, "="); ok {
				env = ReplaceEnv(env, k, kv[len(k)+1:])
			}
		}
		cmd.Env = env
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), nil
	}
	re := &RunError{Err: err, Stderr: stderr.Bytes(), ExitCode: -1}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		re.ExitCode = ee.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), re
}

// ReplaceEnv returns environ with every KEY=VALUE replaced by the given
// value, or appended when absent. 追加式会产生重复键，其读取优先级取决于
// libc 实现——所有 km 子进程环境注入统一走本函数。
func ReplaceEnv(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	replaced := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

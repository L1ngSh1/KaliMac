// Package runtime wraps the local Docker CLI. The command executor is
// injectable so tests can drive km with a fake docker binary.
package runtime

import (
	"bytes"
	"context"
	"os/exec"
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

// CommandExecutor is the production Executor backed by os/exec.
type CommandExecutor struct{}

func (CommandExecutor) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (CommandExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	re := &RunError{Err: err, Stderr: stderr.Bytes(), ExitCode: -1}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		re.ExitCode = ee.ExitCode()
	}
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), nil
	}
	return stdout.Bytes(), stderr.Bytes(), re
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

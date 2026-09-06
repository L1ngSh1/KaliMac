package runtime

import (
	"errors"
	"fmt"
	"strings"
)

// KM error codes. They identify km infrastructure failures independently of
// exit codes, because tool exit codes and infrastructure errors can overlap
// numerically.
const (
	CodeRuntimeMissing    = "KM_RUNTIME_MISSING"
	CodeRuntimeOffline    = "KM_RUNTIME_OFFLINE"
	CodeNotFound          = "KM_NOT_FOUND"
	CodeEndpointRemote    = "KM_ENDPOINT_REMOTE"
	CodeProjectMissing    = "KM_PROJECT_MISSING"
	CodeConfigInvalid     = "KM_CONFIG_INVALID"
	CodeStateInvalid      = "KM_STATE_INVALID"
	CodeContainerConflict = "KM_CONTAINER_CONFLICT"
	CodeNotImplemented    = "KM_NOT_IMPLEMENTED"
	CodeUsage             = "KM_USAGE"
	CodeProjectNested     = "KM_PROJECT_NESTED"
	CodeImageDrift        = "KM_IMAGE_DRIFT"
	CodeSessionActive     = "KM_SESSION_ACTIVE"
	CodeSessionUnknown    = "KM_SESSION_UNKNOWN"
	CodeTimeout           = "KM_TIMEOUT"
	CodeCanceled          = "KM_CANCELED"
	CodeRuntimeMismatch   = "KM_RUNTIME_MISMATCH"
)

// Error is a km infrastructure error carrying a stable KM_* code.
type Error struct {
	Code string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ClassifyCommandError inspects the failure of an external command (at this
// stage the docker CLI) and maps it to a KM_* code.
func ClassifyCommandError(err error) error {
	if err == nil {
		return nil
	}
	var re *RunError
	if !errors.As(err, &re) {
		return errf(CodeRuntimeMissing, "无法执行外部命令: %v", err)
	}
	stderr := strings.ToLower(string(re.Stderr))
	switch {
	case strings.Contains(stderr, "no such container"),
		strings.Contains(stderr, "no such image"),
		strings.Contains(stderr, "no such object"):
		return errf(CodeNotFound, "docker 资源不存在")
	case re.ExitCode == 127, strings.Contains(stderr, "executable file not found"),
		strings.Contains(stderr, "oci runtime exec failed"):
		return errf(CodeRuntimeMissing, "容器内命令不存在或无法启动")
	case strings.Contains(stderr, "cannot connect to the docker daemon"),
		strings.Contains(stderr, "is the docker daemon running"),
		strings.Contains(stderr, "no such file or directory"):
		return errf(CodeRuntimeOffline, "Docker 引擎不可达")
	default:
		return errf(CodeRuntimeOffline, "docker 命令失败（退出码 %d）", re.ExitCode)
	}
}

// IsOffline reports whether err is a KM_RUNTIME_OFFLINE error.
func IsOffline(err error) bool { return hasCode(err, CodeRuntimeOffline) }

// IsNotFound reports whether err is a KM_NOT_FOUND error.
func IsNotFound(err error) bool { return hasCode(err, CodeNotFound) }

// IsMissing reports whether err is a KM_RUNTIME_MISSING error.
func IsMissing(err error) bool { return hasCode(err, CodeRuntimeMissing) }

// IsTimeout reports whether err is a KM_TIMEOUT error.
func IsTimeout(err error) bool { return hasCode(err, CodeTimeout) }

// IsCanceled reports whether err is a KM_CANCELED error.
func IsCanceled(err error) bool { return hasCode(err, CodeCanceled) }

func hasCode(err error, code string) bool {
	var kmerr *Error
	return errors.As(err, &kmerr) && kmerr.Code == code
}

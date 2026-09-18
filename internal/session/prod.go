package session

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kalimac/internal/runtime"
)

// ExecStarter is the production Starter: it launches the docker CLI with
// streamed stdio. The process is deliberately not bound to a context —
// its lifecycle is owned by the Manager (ctl-driven cancel + Kill backstop).
// Env is injected into the child environment with replace semantics
// (R4: 用于把本轮解析出的 DOCKER_HOST 固定到流式执行路径)。
type ExecStarter struct {
	DockerPath string
	Env        []string
}

func (s *ExecStarter) LookPath() (string, error) {
	if s.DockerPath != "" {
		return s.DockerPath, nil
	}
	return exec.LookPath("docker")
}

func (s *ExecStarter) Start(argv []string, stdin io.Reader, stdout, stderr io.Writer) (Proc, error) {
	if len(argv) == 0 {
		return nil, errors.New("空 argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if len(s.Env) > 0 {
		env := os.Environ()
		for _, kv := range s.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env = runtime.ReplaceEnv(env, k, v)
			}
		}
		cmd.Env = env
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProc{cmd: cmd}, nil
}

type osProc struct{ cmd *exec.Cmd }

func (p *osProc) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), err
	}
	return -1, err
}

func (p *osProc) Kill() error { return p.cmd.Process.Kill() }

func (p *osProc) Signal(sig syscall.Signal) error { return p.cmd.Process.Signal(sig) }

// rawRunner 执行一条 docker 命令：喂入 stdin、捕获输出、返回协议级退出码。
// exitCode 为 -1 且 err 非 nil 表示真实故障（引擎不可达等）。
type rawRunner func(ctx context.Context, stdin []byte, args []string) (stdout, stderr string, exitCode int, err error)

// DockerController is the production Controller: bootstrap via a streamed
// `docker cp -` tar (no shell), cancel via a bounded `docker exec km-ctl`.
// Every call is capture-based and bounded — these are control operations,
// not the tool data path. RunFn is injectable for tests; nil uses os/exec.
type DockerController struct {
	// DockerPath overrides the docker binary lookup (tests).
	DockerPath string
	// BootstrapTimeout bounds the docker cp call; 0 → runtime.DefaultManagementTimeout.
	BootstrapTimeout time.Duration
	// RunFn overrides the low-level docker runner (tests).
	RunFn rawRunner
	// Endpoint 本轮解析出的本地引擎（R4）：设置后全部 docker 子进程以
	// 替换式 DOCKER_HOST 固定到它，不继承宿主可能变化的默认 context。
	Endpoint string
}

func (c *DockerController) runner() rawRunner {
	if c.RunFn != nil {
		return c.RunFn
	}
	return c.rawRunExec
}

func (c *DockerController) lookPath() (string, error) {
	if c.DockerPath != "" {
		return c.DockerPath, nil
	}
	return exec.LookPath("docker")
}

// rawRun runs docker with argv, feeding stdin, capturing streams, bounded
// by ctx. exitCode is -1 when the process could not start or ended without
// a status; err is non-nil only for genuine failures (engine unreachable,
// binary missing, ...), never for plain non-zero exit codes.
func (c *DockerController) rawRun(ctx context.Context, stdin []byte, args ...string) (string, string, int, error) {
	return c.runner()(ctx, stdin, args)
}

// rawRunExec is the production rawRunner backed by os/exec.
func (c *DockerController) rawRunExec(ctx context.Context, stdin []byte, args []string) (stdout, stderr string, exitCode int, err error) {
	bin, err := c.lookPath()
	if err != nil {
		return "", "", -1, &runtime.Error{Code: runtime.CodeRuntimeMissing, Msg: "无法执行 docker CLI", Err: err}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if c.Endpoint != "" {
		cmd.Env = runtime.ReplaceEnv(os.Environ(), "DOCKER_HOST", c.Endpoint)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	rerr := cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()
	if rerr == nil {
		return stdout, stderr, 0, nil
	}
	re := &runtime.RunError{Err: rerr, Stderr: errBuf.Bytes(), ExitCode: -1}
	var ee *exec.ExitError
	if errors.As(rerr, &ee) {
		re.ExitCode = ee.ExitCode()
	}
	if re.ExitCode < 0 && errors.Is(rerr, context.DeadlineExceeded) {
		// 管理类有界调用的超时：按合同返回 KM_TIMEOUT（而非误报 OFFLINE）
		return stdout, stderr, -1, &runtime.Error{Code: runtime.CodeTimeout, Msg: "docker 命令超时"}
	}
	if re.ExitCode >= 0 {
		// docker 自身给出的非零退出码：调用方按协议解释（如 km-ctl 3/4）。
		return stdout, stderr, re.ExitCode, nil
	}
	return stdout, stderr, -1, runtime.ClassifyCommandError(re)
}

// Bootstrap installs the controller scripts via `docker cp - <c>:/tmp`
// from an in-memory tar. Idempotent; safe to call before every run.
func (c *DockerController) Bootstrap(ctx context.Context, container string) error {
	bt := c.BootstrapTimeout
	if bt <= 0 {
		bt = runtime.DefaultManagementTimeout
	}
	bctx, cancel := context.WithTimeout(ctx, bt)
	defer cancel()

	tarBytes, err := scriptsTar()
	if err != nil {
		return err
	}
	_, stderr, code, err := c.rawRun(bctx, tarBytes, "cp", "-", container+":/tmp")
	if err != nil {
		return fmt.Errorf("docker cp 引导失败: %w (stderr: %s)", err, stderr)
	}
	if code != 0 {
		return fmt.Errorf("docker cp 引导失败 (退出码 %d, stderr: %s)", code, stderr)
	}
	return nil
}

// Cancel runs the container-side km-ctl cancel for one session under the
// caller-provided (fresh, bounded) context. km-ctl 的 0/3/4 是协议状态码。
func (c *DockerController) Cancel(ctx context.Context, container, sid string) (int, string, error) {
	stdout, stderr, code, err := c.rawRun(ctx, nil, "exec", container, CtlScriptPath, "cancel", sid)
	if err != nil {
		var kmerr *runtime.Error
		if errors.As(err, &kmerr) && kmerr.Code == runtime.CodeNotFound {
			// km-ctl 自身丢失（容器重建等）：等同于会话不存在。
			return 3, "km-ctl missing", nil
		}
		return -1, stderr, err
	}
	return code, strings.TrimSpace(stdout + stderr), nil
}

// scriptsTar builds the tar stream consumed by `docker cp -`.
func scriptsTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "km-bin/", Mode: 0o755, Typeflag: tar.TypeDir, Uid: 0, Gid: 0,
		ModTime: time.Unix(0, 0)}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	for path, content := range scripts() {
		h := &tar.Header{Name: "km-bin/" + filepath.Base(path), Mode: 0o755, Typeflag: tar.TypeReg,
			Uid: 0, Gid: 0, Size: int64(len(content)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Sessions 只读列出容器内会话状态。有界调用。
// 协议约定：stdout 为 ACTIVE/STALE 行，stderr 仅承载诊断；exit 非 0 或
// 输出无法解析时调用方必须视为“会话状态未知”并阻断，不得当作无会话。
func (c *DockerController) Sessions(ctx context.Context, container string) (stdout, stderr string, exitCode int, err error) {
	sctx, cancel := context.WithTimeout(ctx, runtime.DefaultManagementTimeout)
	defer cancel()
	stdout, stderr, exitCode, err = c.rawRun(sctx, nil, "exec", container, CtlScriptPath, "sessions")
	return stdout, stderr, exitCode, err
}

// Alive 只读判定登记会话的 bash 是否仍存活（exit 0=存活，1=已退出，
// 3=登记缺失）。有界调用。
func (c *DockerController) Alive(ctx context.Context, container, sid string) (int, string, error) {
	sctx, cancel := context.WithTimeout(ctx, runtime.DefaultManagementTimeout)
	defer cancel()
	stdout, stderr, code, err := c.rawRun(sctx, nil, "exec", container, CtlScriptPath, "alive", sid)
	if err != nil {
		return -1, stderr, err
	}
	return code, strings.TrimSpace(stdout + stderr), nil
}

// Sweep 清理组已空的遗留会话目录，stdout 为仍活跃的会话数。有界调用。
func (c *DockerController) Sweep(ctx context.Context, container string) (stdout, stderr string, exitCode int, err error) {
	sctx, cancel := context.WithTimeout(ctx, runtime.DefaultManagementTimeout)
	defer cancel()
	stdout, stderr, exitCode, err = c.rawRun(sctx, nil, "exec", container, CtlScriptPath, "sweep")
	return stdout, stderr, exitCode, err
}

// ExecCapture 在容器内执行一条短命令并捕获输出——管理类有界调用
// （默认 10s 管理超时），供 tools 等只读探测使用。退出码 >= 0 时 err 为 nil
// （协议级非零由调用方解释）；无法启动/引擎故障等真实故障 err 非 nil。
// 与 Sessions 同样经固定 endpoint 注入（R4）与 RunFn 测试注入。
func (c *DockerController) ExecCapture(ctx context.Context, container string, cmdline []string) (stdout, stderr string, exitCode int, err error) {
	ectx, cancel := context.WithTimeout(ctx, runtime.DefaultManagementTimeout)
	defer cancel()
	args := append([]string{"exec", container}, cmdline...)
	return c.rawRun(ectx, nil, args...)
}

// ParseSessions 严格解析 km-ctl sessions 的 stdout。
// 每个非空行必须是 "ACTIVE <id>" 或 "STALE <id>"；出现任何其他内容、
// 空 id 或混入诊断文本都判为解析失败（ok=false），由调用方阻断。
func ParseSessions(out string) (active, stale []string, ok bool) {
	ok = true
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		kind, id, found := strings.Cut(line, " ")
		id = strings.TrimSpace(id)
		if !found || id == "" || strings.ContainsAny(id, " \t") {
			ok = false
			continue
		}
		switch kind {
		case "ACTIVE":
			active = append(active, id)
		case "STALE":
			stale = append(stale, id)
		default:
			ok = false
		}
	}
	return active, stale, ok
}

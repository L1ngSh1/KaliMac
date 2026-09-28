package runtime

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultManagementTimeout bounds every docker management query (version,
// inspect, ps, ...). Long-running tool executions (P2) must not use it.
const DefaultManagementTimeout = 10 * time.Second

// Docker wraps the local docker CLI. Every call goes through the injectable
// Executor so that unit tests can assert the exact argv km builds.
type Docker struct {
	Exec Executor
	// DockerPath overrides the docker binary lookup; used by tests.
	DockerPath string
	// ManagementTimeout bounds each docker management query; 0 selects
	// DefaultManagementTimeout.
	ManagementTimeout time.Duration
	// EndpointOverride, when set, is injected as DOCKER_HOST into every
	// child docker process so that all calls of one km run hit the same
	// engine regardless of concurrent environment changes.
	EndpointOverride string
}

// Version holds the minimal identity fields of client and server.
type Version struct {
	Client    string
	Server    string
	ServerOS  string
	ServerArc string
}

// ContainerSummary is a trimmed `docker ps` record.
type ContainerSummary struct {
	ID    string
	Name  string
	State string
}

// EndpointInfo describes the endpoint the docker CLI will actually use.
type EndpointInfo struct {
	Source   string // "DOCKER_HOST" | "DOCKER_CONTEXT" | "context"
	Context  string // context name, empty when DOCKER_HOST overrides
	Endpoint string
}

// InspectResult is the trimmed result of `docker container inspect` for one
// container: identity, state, project label, image content and mounts.
type InspectResult struct {
	ID          string
	Name        string
	State       string
	ProjectID   string
	Image       string // container's actual image ID (content identity)
	MountSource string // source path of the /workspace bind mount, empty if absent
}

func (d *Docker) lookPath() (string, error) {
	if d.DockerPath != "" {
		return d.DockerPath, nil
	}
	path, err := d.Exec.LookPath("docker")
	if err != nil {
		return "", errf(CodeRuntimeMissing, "PATH 中没有 docker CLI；请安装并启动所选运行时后重试")
	}
	return path, nil
}

// run executes docker with args and returns trimmed stdout. Each call is
// bounded by the management timeout; failures are classified into KM_*
// errors with the raw stderr attached.
func (d *Docker) managementTimeout() time.Duration {
	if d.ManagementTimeout > 0 {
		return d.ManagementTimeout
	}
	return DefaultManagementTimeout
}

// run 用管理超时执行 docker 命令。
func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	return d.runWithTimeout(ctx, d.managementTimeout(), args...)
}

// runWithTimeout 以显式超时执行 docker 命令。若上层 ctx 已有更短 deadline，
// 以更短者为准（context.WithTimeout 语义）；pull/stop 等专用预算不再被
// 内层固定 10 秒截断。
func (d *Docker) runWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bin, err := d.lookPath()
	if err != nil {
		return "", err
	}
	mt := timeout
	if mt <= 0 {
		mt = d.managementTimeout()
	}
	cctx, cancel := context.WithTimeout(ctx, mt)
	defer cancel()

	ex := d.Exec
	if d.EndpointOverride != "" {
		ex = WithEnv(ex, "DOCKER_HOST="+d.EndpointOverride)
	}

	stdout, stderr, rerr := ex.Run(cctx, bin, args...)
	if rerr != nil {
		if errors.Is(cctx.Err(), context.Canceled) {
			return "", errf(CodeCanceled, "km 已取消 docker %s 调用", args[0])
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return "", errf(CodeTimeout, "docker %s 超时（超过 %s 未响应）", args[0], mt)
		}
		re := &RunError{Err: rerr, Stderr: stderr, ExitCode: -1}
		if re2, ok := rerr.(*RunError); ok {
			re = re2
		}
		return "", ClassifyCommandError(re)
	}
	return strings.TrimRight(string(stdout), "\n"), nil
}

// EffectiveEndpoint resolves the endpoint the docker CLI would use, without
// contacting the daemon. Precedence mirrors the docker CLI: DOCKER_HOST >
// DOCKER_CONTEXT > current context (`docker context show`).
func (d *Docker) EffectiveEndpoint(ctx context.Context) (EndpointInfo, error) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return EndpointInfo{Source: "DOCKER_HOST", Endpoint: host}, nil
	}
	ctxName := os.Getenv("DOCKER_CONTEXT")
	source := "DOCKER_CONTEXT"
	if ctxName == "" {
		name, err := d.run(ctx, "context", "show")
		if err != nil {
			return EndpointInfo{}, err
		}
		ctxName = name
		source = "context"
	}
	ep, err := d.run(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}", ctxName)
	if err != nil {
		return EndpointInfo{}, err
	}
	if ep == "" {
		return EndpointInfo{}, errf(CodeRuntimeMissing, "context %q 未定义 docker endpoint", ctxName)
	}
	return EndpointInfo{Source: source, Context: ctxName, Endpoint: ep}, nil
}

// IsLocalEndpoint reports whether the endpoint targets this machine: unix
// sockets, Windows named pipes, or TCP loopback. Everything else (ssh,
// non-loopback tcp, ...) counts as remote for v0.1.
func IsLocalEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "unix", "npipe":
		return true
	case "tcp":
		switch strings.ToLower(u.Hostname()) {
		case "127.0.0.1", "localhost", "::1":
			return true
		}
		return false
	default:
		return false
	}
}

// WithEnv returns an Executor that sets the given KEY=VALUE pairs in the
// child environment, REPLACING any existing entry for the same key (appending
// would leave a duplicate whose precedence is undefined across libc/getenv
// implementations). For non-CommandExecutor implementations (test fakes)
// the variables are ignored.
func WithEnv(inner Executor, kv ...string) Executor {
	return envExecutor{inner: inner, extra: kv}
}

type envExecutor struct {
	inner Executor
	extra []string
}

func (e envExecutor) LookPath(name string) (string, error) {
	return e.inner.LookPath(name)
}

func (e envExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if ce, ok := e.inner.(CommandExecutor); ok {
		ce.ExtraEnv = append(append([]string(nil), ce.ExtraEnv...), e.extra...)
		return ce.Run(ctx, name, args...)
	}
	return e.inner.Run(ctx, name, args...)
}

// Version returns client and server identity. It fails with
// KM_RUNTIME_OFFLINE when the CLI exists but the engine is unreachable.
func (d *Docker) Version(ctx context.Context) (Version, error) {
	out, err := d.run(ctx, "version", "--format", "{{.Client.Version}}|{{.Server.Version}}|{{.Server.Os}}|{{.Server.Arch}}")
	if err != nil {
		return Version{}, err
	}
	parts := strings.Split(out, "|")
	if len(parts) != 4 {
		return Version{}, errf(CodeRuntimeOffline, "docker version 输出格式异常: %q", out)
	}
	return Version{Client: parts[0], Server: parts[1], ServerOS: parts[2], ServerArc: parts[3]}, nil
}

// ContainerName returns the km container name for a project id.
func ContainerName(projectID string) string { return "km-" + projectID }

// ProjectLabel is the label key km stamps on the containers it owns.
const ProjectLabel = "km.project"

// Additional label keys for environment-switch transactions: every resource
// of one operation carries the same op id so a lost create response can be
// reconciled by label even when the client never saw the container ID.
const (
	OpLabel   = "km.op"   // value: envtxn op id ("e"+16hex)
	GenLabel  = "km.gen"  // value: generation number of a candidate container
	RoleLabel = "km.role" // value: "candidate" | "probe"
)

// FindContainersByLabel lists containers carrying the given label=value.
func (d *Docker) FindContainersByLabel(ctx context.Context, label, value string) ([]ContainerSummary, error) {
	out, err := d.run(ctx, "ps", "-a",
		"--filter", "label="+label+"="+value,
		"--format", "{{.ID}} {{.Names}} {{.State}}")
	if err != nil {
		return nil, err
	}
	var result []ContainerSummary
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		result = append(result, ContainerSummary{ID: fields[0], Name: fields[1], State: fields[2]})
	}
	return result, nil
}

// InspectContainer inspects a container by any reference (full ID preferred
// or name) and returns identity, state, project label, actual image content
// and the /workspace mount. ok=false when the container does not exist.
func (d *Docker) InspectContainer(ctx context.Context, ref string) (InspectResult, bool, error) {
	format := "{{.Id}}|{{.Name}}|{{.State.Status}}|{{index .Config.Labels \"" + ProjectLabel + "\"}}|{{.Image}}|{{range .Mounts}}{{if eq .Destination \"/workspace\"}}{{.Source}}{{end}}{{end}}"
	out, err := d.run(ctx, "container", "inspect", "--format", format, ref)
	if err != nil {
		if IsNotFound(err) {
			return InspectResult{}, false, nil
		}
		return InspectResult{}, false, err
	}
	parts := strings.SplitN(out, "|", 6)
	if len(parts) < 6 {
		return InspectResult{}, false, errf(CodeStateInvalid, "docker inspect 输出格式异常: %q", out)
	}
	res := InspectResult{
		ID:          parts[0],
		Name:        strings.TrimPrefix(parts[1], "/"),
		State:       parts[2],
		ProjectID:   parts[3],
		Image:       parts[4],
		MountSource: parts[5],
	}
	return res, true, nil
}

// ImageID returns the local image ID for ref; ok=false when not present.
func (d *Docker) ImageID(ctx context.Context, ref string) (string, bool, error) {
	out, err := d.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		if IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// ContainerCreateOpts describes the km-managed container to create.
type ContainerCreateOpts struct {
	Name       string // km-<project-id> or km-<project-id>-g<N>
	ProjectID  string // stamped into the km.project label
	Image      string
	ProjectDir string // host path bound to /workspace
	// Platform 兑现配置声明（如 linux/arm64）；空 = 不传 --platform（native，
	// 旧配置未声明平台时的历史行为）。
	Platform string
	// ExtraLabels are appended verbatim as --label k=v entries.
	ExtraLabels []string
	// WorkspaceReadOnly binds /workspace read-only (probe containers only;
	// project containers always use the real read-write mount).
	WorkspaceReadOnly bool
	// Cmd overrides the container command; nil selects the project default
	// (`sleep infinity`).
	Cmd []string
}

// CreateContainer creates the project container with --init (reaping PID1)
// and the ownership labels. The main process is `sleep infinity`, which the
// init wrapper turns into a fast, clean stop target.
func (d *Docker) CreateContainer(ctx context.Context, o ContainerCreateOpts) (string, error) {
	args := []string{"run", "-d", "--init",
		"--name", o.Name,
		"--label", ProjectLabel + "=" + o.ProjectID,
		"--label", "km.owner=km",
		"--label", "km.schema=1"}
	for _, l := range o.ExtraLabels {
		args = append(args, "--label", l)
	}
	mount := o.ProjectDir + ":/workspace"
	if o.WorkspaceReadOnly {
		mount += ":ro"
	}
	args = append(args, "-v", mount)
	if o.Platform != "" {
		args = append(args, "--platform", o.Platform)
	}
	args = append(args, o.Image)
	if len(o.Cmd) > 0 {
		args = append(args, o.Cmd...)
	} else {
		args = append(args, "sleep", "infinity")
	}
	out, err := d.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// StartContainer starts an existing (created or stopped) container.
func (d *Docker) StartContainer(ctx context.Context, ref string) error {
	_, err := d.run(ctx, "start", ref)
	return err
}

// stopTimeout bounds docker stop; the 10s docker grace period plus margin.
const stopTimeout = 30 * time.Second

// StopContainer stops the container (data preserved). It uses its own bound
// — longer than management queries — but is still bounded.
func (d *Docker) StopContainer(ctx context.Context, ref string) error {
	_, err := d.runWithTimeout(ctx, stopTimeout, "stop", ref)
	return err
}

// PullTimeout bounds an image pull. Pulls are explicit (init) and rare.
const PullTimeout = 10 * time.Minute

// PullImage pulls the image reference with a generous but bounded timeout;
// 非空 platform 时兑现配置声明（docker pull --platform），
// 空 = 原样拉取（native）。
func (d *Docker) PullImage(ctx context.Context, ref, platform string) error {
	args := []string{"pull"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(args, ref)
	_, err := d.runWithTimeout(ctx, PullTimeout, args...)
	return err
}

// ImageOSArch 查询镜像/引用的操作系统与架构（如 linux/arm64）。归属核验外的
// 只读查询；init 复用前用它兑现平台声明的一致性。
func (d *Docker) ImageOSArch(ctx context.Context, ref string) (string, error) {
	out, err := d.run(ctx, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RemoveContainer removes a container by full ID (init rollback only;
// km never removes project containers elsewhere).
func (d *Docker) RemoveContainer(ctx context.Context, fullID string) error {
	_, err := d.run(ctx, "rm", "-f", fullID)
	return err
}

package runtime

import (
	"context"
	"strings"
)

// Docker wraps the local docker CLI. Every call goes through the injectable
// Executor so that unit tests can assert the exact argv km builds.
type Docker struct {
	Exec Executor
	// DockerPath overrides the docker binary lookup; used by tests.
	DockerPath string
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

// InspectResult is the trimmed result of `docker container inspect` for one
// container: identity, state, project label and mounts.
type InspectResult struct {
	ID          string
	Name        string
	State       string
	ProjectID   string
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

// run executes docker with args and returns trimmed stdout. Failures are
// classified into KM_* errors; the raw stderr travels inside the error.
func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	bin, err := d.lookPath()
	if err != nil {
		return "", err
	}
	stdout, stderr, rerr := d.Exec.Run(ctx, bin, args...)
	if rerr != nil {
		re := &RunError{Err: rerr, Stderr: stderr, ExitCode: -1}
		if re2, ok := rerr.(*RunError); ok {
			re = re2
		}
		return "", ClassifyCommandError(re)
	}
	return strings.TrimRight(string(stdout), "\n"), nil
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

// ContextShow returns the current docker context name.
func (d *Docker) ContextShow(ctx context.Context) (string, error) {
	return d.run(ctx, "context", "show")
}

// ContainerName returns the km container name for a project id.
func ContainerName(projectID string) string { return "km-" + projectID }

// ProjectLabel is the label key km stamps on the containers it owns.
const ProjectLabel = "km.project"

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

// InspectContainer returns identity, state, project label and workspace
// mount for one container. ok=false when the container does not exist.
func (d *Docker) InspectContainer(ctx context.Context, name string) (InspectResult, bool, error) {
	format := "{{.Id}}|{{.Name}}|{{.State.Status}}|{{index .Config.Labels \"" + ProjectLabel + "\"}}|{{range .Mounts}}{{if eq .Destination \"/workspace\"}}{{.Source}}{{end}}{{end}}"
	out, err := d.run(ctx, "container", "inspect", "--format", format, name)
	if err != nil {
		if IsNotFound(err) {
			return InspectResult{}, false, nil
		}
		return InspectResult{}, false, err
	}
	parts := strings.SplitN(out, "|", 5)
	if len(parts) < 5 {
		return InspectResult{}, false, errf(CodeStateInvalid, "docker inspect 输出格式异常: %q", out)
	}
	res := InspectResult{
		ID:          parts[0],
		Name:        strings.TrimPrefix(parts[1], "/"),
		State:       parts[2],
		ProjectID:   parts[3],
		MountSource: parts[4],
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

// ExecToolArgs builds the argv for running a tool inside a container without
// a shell: docker exec [flags] CONTAINER TOOL ARG...  The tool argv is kept
// verbatim; nothing is ever joined into a shell string.
func ExecToolArgs(container, workdir string, tty bool, tool string, toolArgs []string) []string {
	args := []string{"exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, "-i")
	if tty {
		args = append(args, "-t")
	}
	args = append(args, container, tool)
	return append(args, toolArgs...)
}

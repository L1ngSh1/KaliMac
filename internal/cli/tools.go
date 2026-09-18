package cli

// km tools：当前项目容器内的精选工具可用性查看（tool-discovery 计划第一版）。
// 行为合同见 docs/tool-discovery-plan.md：
//   - 只读探测：不写配置、不装脚本、不清扫、不安装软件；单次批量 exec，
//     工具清单经 argv 传入固定脚本（无任意 shell 拼接），整体 10s 管理超时；
//   - AVAILABLE 仅表示可从容器 PATH 定位，不保证版本/执行成功/功能完整；
//   - 查询失败、输出协议异常一律 UNKNOWN（KM_* 非零），绝不把执行失败
//     当成 MISSING；
//   - 容器非 running（停止/暂停/其他）明示状态与未检查原因，退出 1，不自动
//     启动或恢复；不取项目执行锁，已有 run/shell 时仍可查询。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"kalimac/internal/runtime"
)

// toolsChecklist 是第一版固定精选清单（编译期常量；不做自定义清单）。
var toolsChecklist = []string{"python3", "curl", "jq", "file", "openssl", "nmap"}

// toolsProbeScript 是固定探测脚本：工具名经 "$@"（argv）传入，绝不拼接。
const toolsProbeScript = `for t in "$@"; do
  if p=$(command -v "$t" 2>/dev/null); then
    printf '%s=AVAILABLE:%s\n' "$t" "$p"
  else
    printf '%s=MISSING\n' "$t"
  fi
done`

type toolEntry struct {
	Name string
	Path string // AVAILABLE 时的解析路径
}

// parseToolsOutput 严格解析探测输出：恰好 len(checklist) 行、每行
// NAME=AVAILABLE:<path> 或 NAME=MISSING、NAME 属于清单且不重复。
// 返回 (条目, ok)；ok=false 表示协议异常。
func parseToolsOutput(out string, checklist []string) ([]toolEntry, bool) {
	expected := map[string]bool{}
	for _, t := range checklist {
		expected[t] = true
	}
	seen := map[string]bool{}
	var entries []toolEntry
	// TrimSuffix 只去最后一个换行：结尾多余空行属协议异常（恰好 N 行）
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(checklist) {
		return nil, false
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			return nil, false
		}
	}
	for _, line := range lines {
		name, rest, found := strings.Cut(line, "=")
		if !found || !expected[name] || seen[name] {
			return nil, false
		}
		seen[name] = true
		switch {
		case rest == "MISSING":
			entries = append(entries, toolEntry{Name: name})
		case strings.HasPrefix(rest, "AVAILABLE:"):
			path := strings.TrimPrefix(rest, "AVAILABLE:")
			if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\r\n") {
				return nil, false
			}
			entries = append(entries, toolEntry{Name: name, Path: path})
		default:
			return nil, false
		}
	}
	if len(entries) != len(checklist) {
		return nil, false
	}
	return entries, true
}

func runToolsCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km tools 不接受参数（清单为固定精选六项）")
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	root, _, st, err := loadProjectStack(wd)
	if err != nil {
		return envError(stderr, err)
	}
	ep, res, err := verifyStackForQuery(ctx, dk, root, st)
	if err != nil {
		return envError(stderr, err)
	}
	switch res.State {
	case "running":
		// 继续探测
	case "paused":
		fmt.Fprintf(stderr, "km tools: 容器已暂停，工具可用性未检查（任务仍驻留内存）；docker unpause 恢复后重试\n")
		return ExitEnv
	case "created", "exited":
		fmt.Fprintf(stderr, "km tools: 容器已停止，工具可用性未检查；下次 km run/shell 会自动启动\n")
		return ExitEnv
	default:
		return envError(stderr, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: fmt.Sprintf("容器处于未支持状态 %q，工具可用性未知；请 km doctor 复核", res.State)})
	}

	ctl := newSessionController(ep.Endpoint)
	argv := append([]string{"/bin/sh", "-c", toolsProbeScript, "sh"}, toolsChecklist...)
	sOut, sErrStr, sExit, perr := ctl.ExecCapture(ctx, st.Container.ID, argv)
	combined := sOut + sErrStr
	if perr != nil || sExit != 0 {
		// 真实执行失败（引擎/权限/超时等）：UNKNOWN + 保留诊断，绝不当作 MISSING
		code := runtime.CodeToolsProtocol
		var rerr *runtime.Error
		if errors.As(perr, &rerr) {
			code = rerr.Code
		}
		return envError(stderr, &runtime.Error{Code: code,
			Msg: fmt.Sprintf("工具探测执行失败（exit=%d，stderr: %s）；工具可用性未知", sExit, strings.TrimSpace(combined)), Err: perr})
	}
	entries, ok := parseToolsOutput(sOut, toolsChecklist)
	if !ok {
		return envError(stderr, &runtime.Error{Code: runtime.CodeToolsProtocol,
			Msg: fmt.Sprintf("工具探测输出不符合协议（%q）；工具可用性未知", sOut)})
	}
	byName := map[string]toolEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	fmt.Fprintf(stdout, "km tools（仅覆盖精选清单，非容器全部软件；AVAILABLE=PATH 可定位，不保证版本或执行结果）\n")
	missing := 0
	for _, t := range toolsChecklist {
		e := byName[t]
		if e.Path == "" {
			missing++
			fmt.Fprintf(stdout, "  MISSING  %s\n", e.Name)
			continue
		}
		fmt.Fprintf(stdout, "  AVAILABLE %-9s %s\n", e.Name, e.Path)
	}
	if missing > 0 {
		fmt.Fprintf(stdout, "缺失 %d/%d 项。可持续方案：维护镜像 Dockerfile（images/kali）并重建；容器内临时安装不随容器删除保留，不构成可复现配置。\n", missing, len(toolsChecklist))
	} else {
		fmt.Fprintf(stdout, "六项精选工具全部可用。\n")
	}
	return ExitOK
}

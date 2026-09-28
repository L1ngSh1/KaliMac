package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"

	"golang.org/x/term"
)

// runEnvCommand implements `km env <sub>`: switch | rollback | recover.
// 合同见 docs/adr-environment-transactions.md §1；本分派加入管理命令后，
// 与 env 重名的工具仍可经 `km run -- env ...` 调用。
func runEnvCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) == 0 {
		return usageError(stderr, "km env 需要子命令: switch | rollback | recover（km help env 查看说明）")
	}
	sub, args := rest[0], rest[1:]
	if sub == "--help" || sub == "-h" {
		if len(args) != 0 {
			return usageError(stderr, "km env --help 不接受参数")
		}
		PrintCommandHelp(stdout, "env")
		return ExitOK
	}
	switch sub {
	case "switch":
		if envSubHelp(args, stdout) {
			return ExitOK
		}
		return runEnvSwitch(ctx, args, stdout, stderr, dk)
	case "rollback":
		if envSubHelp(args, stdout) {
			return ExitOK
		}
		return runEnvRollback(ctx, args, stdout, stderr, dk)
	case "recover":
		if envSubHelp(args, stdout) {
			return ExitOK
		}
		return runEnvRecover(ctx, args, stdout, stderr, dk)
	default:
		return usageError(stderr, "未知 km env 子命令 %q（支持 switch | rollback | recover）", sub)
	}
}

func envSubHelp(args []string, stdout io.Writer) bool {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		PrintCommandHelp(stdout, "env")
		return true
	}
	return false
}

// envFlags 是 env 子命令的显式参数；未知、重复、空值一律拒绝（退出码 2）。
type envFlags struct {
	image  string
	dryRun bool
	yes    bool
}

func parseEnvFlags(rest []string, allowImage bool) (*envFlags, error) {
	f := &envFlags{}
	for i := 0; i < len(rest); i++ {
		cur := rest[i]
		switch {
		case cur == "--dry-run":
			if f.dryRun {
				return nil, fmt.Errorf("--dry-run 重复")
			}
			f.dryRun = true
		case cur == "--yes":
			if f.yes {
				return nil, fmt.Errorf("--yes 重复")
			}
			f.yes = true
		case cur == "--image" || strings.HasPrefix(cur, "--image="):
			if !allowImage {
				return nil, fmt.Errorf("未知参数 %q", cur)
			}
			if f.image != "" {
				return nil, fmt.Errorf("--image 重复")
			}
			if strings.HasPrefix(cur, "--image=") {
				f.image = strings.TrimPrefix(cur, "--image=")
			} else {
				if i+1 >= len(rest) {
					return nil, fmt.Errorf("--image 需要一个值")
				}
				i++
				f.image = rest[i]
			}
			if strings.TrimSpace(f.image) == "" {
				return nil, fmt.Errorf("--image 需要一个非空镜像引用")
			}
			if strings.ContainsAny(f.image, " \t\n") {
				return nil, fmt.Errorf("--image 不能包含空白字符")
			}
		default:
			return nil, fmt.Errorf("未知参数 %q（仅支持 --image / --dry-run / --yes）", cur)
		}
	}
	return f, nil
}

// reloadProjectFiles 在取得项目锁后重读 .km.json 与 state.json（独立审查
// P1-1）：锁内的新鲜读取是状态派生事实的唯一依据，锁外加载的对象只用于预览。
func reloadProjectFiles(root string) (*project.Config, *project.State, error) {
	cfg, err := project.LoadConfig(filepath.Join(root, project.ConfigFileName))
	if err != nil {
		return nil, nil, &runtime.Error{Code: runtime.CodeConfigInvalid, Msg: "配置读取失败（保留原文件）", Err: err}
	}
	st, err := project.LoadState(root)
	if err != nil {
		return nil, nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "本机状态读取失败（保留原文件）", Err: err}
	}
	return cfg, st, nil
}

// loadOpenTransaction 返回当前打开的事务；没有则为 nil。损坏的事务记录是
// 错误，绝不等价于“无事务”——宁可阻断也不越过中间态。
func loadOpenTransaction(root string) (*envtxn.Transaction, error) {
	if !envtxn.TransactionExists(root) {
		return nil, nil
	}
	return envtxn.LoadTransaction(root)
}

func errTxnPending(txn *envtxn.Transaction) error {
	return &runtime.Error{Code: runtime.CodeTransactionPending,
		Msg: fmt.Sprintf("存在未完成环境事务（op=%s kind=%s stage=%s）；变更命令已阻断。先运行 km env recover --dry-run 查看恢复方案，确认后运行 km env recover", txn.OpID, txn.Kind, txn.Stage)}
}

// refuseIfEnvTxnPending 是 run/init/stop 取得项目锁后的阻断检查（ADR §5.4）。
func refuseIfEnvTxnPending(root string) error {
	txn, err := loadOpenTransaction(root)
	if err != nil {
		return &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: ".km/env/transaction.json 无法解析，无法确认项目是否处于事务中间态；请人工核验（原始文件已保留）", Err: err}
	}
	if txn != nil {
		return errTxnPending(txn)
	}
	return nil
}

// envSessionGate 是 env 命令的会话门禁（ADR §5.7 冻结）：
//   - exited 容器内没有任何进程，不可能存在活跃 km 会话，免查询；
//   - running 容器先 Bootstrap 安装 km-ctl（与 km run 同一幂等动作）再查询——
//     缺脚本不直接证明无任务，必须安装后核实（ACTIVE/UNKNOWN 阻断，STALE 清扫）；
//   - withQuery=false（dry-run/确认前预览）不安装、不查询，保持零副作用；
//     会话核验在用户确认后的执行路径中进行。
func envSessionGate(ctx context.Context, ep runtime.EndpointInfo, res runtime.InspectResult, withQuery bool, stderr io.Writer) error {
	if !withQuery || res.State != "running" {
		return nil
	}
	ctl := newSessionController(ep.Endpoint)
	if err := ctl.Bootstrap(ctx, res.ID); err != nil {
		return &runtime.Error{Code: runtime.CodeRuntimeOffline, Msg: "会话脚本引导失败", Err: err}
	}
	return verifySessionGate(ctx, ctl, res.ID, stderr, true)
}

// envProbeDeps 是 km 会话内核所需的最小容器内命令集（Bootstrap 经 docker cp
// 安装脚本，脚本为 /bin/sh；bash 供 km shell 使用）。工具可用性由 km tools
// 另行展示，这里只验证会话运行依赖。
var envProbeDeps = []string{"sh", "setsid", "mkdir", "cat", "rm", "sleep", "cut", "basename", "bash"}

const envProbeSh = `for t in "$@"; do
  if command -v "$t" >/dev/null 2>&1; then printf '%s=OK\n' "$t"; else printf '%s=MISSING\n' "$t"; fi
done`

// parseEnvProbe 严格解析探测输出：必须恰好按顺序输出每个依赖的 NAME=OK 或
// NAME=MISSING；任何缺行、多行、未知名字都判为协议失败。
func parseEnvProbe(out string, deps []string) (missing []string, ok bool) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(deps) {
		return nil, false
	}
	for i, line := range lines {
		want := deps[i] + "=OK"
		switch line {
		case want:
		case deps[i] + "=MISSING":
			missing = append(missing, deps[i])
		default:
			return nil, false
		}
	}
	return missing, true
}

// confirmEnvAction 在预览展示后要求显式确认。yes=true 直接通过；
// 非交互且无 --yes 已在参数检查阶段拒绝（用法错误）。用户拒绝 → 返回
// KM_CANCELED / 退出码 1（冻结：明确“未执行任何变更”）。
func confirmEnvAction(stdout, stderr io.Writer, yes bool) (bool, int) {
	if yes {
		return true, ExitOK
	}
	fmt.Fprint(stdout, "确认执行？输入 yes 继续（其他输入取消）: ")
	line, _ := bufio.NewReader(envStdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, ExitOK
	default:
		fmt.Fprintln(stderr, "KM_CANCELED: 用户拒绝确认，未执行任何变更。")
		return false, ExitEnv
	}
}

// envStdin 是确认提示的输入源（测试注入点）。
var envStdin io.Reader = os.Stdin

// envStdinIsTTY 报告 stdin 是否为交互终端（测试注入点）。
var envStdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// requireInteractiveConsent 在任何门禁/预览之前执行的非交互检查：
// 变更型调用（非 dry-run）在非交互 stdin 下必须显式 --yes，否则用法错误
// 退出码 2，不发生任何变更。
func requireInteractiveConsent(f *envFlags, stderr io.Writer) bool {
	if f.dryRun || f.yes || envStdinIsTTY() {
		return true
	}
	usageError(stderr, "非交互环境执行变更必须显式 --yes（--dry-run 可只读预览）；本次未执行任何变更")
	return false
}

// withRecoverHint 给事务期间的失败补上恢复指引。
func withRecoverHint(err error) error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*runtime.Error); ok {
		cp := *e
		cp.Msg = e.Msg + "；事务保持未完成，可运行 km env recover --dry-run 查看恢复方案"
		return &cp
	}
	return err
}

// boundaryNote 是预览与成功输出的固定数据边界提示（ADR §8）。
const boundaryNote = "注意：环境切换/回退不会撤销新环境运行期间对项目文件的改动；容器可写层不迁移。"

// generationDisplay 渲染代号（0 = 原始代）。
func generationDisplay(g int) string {
	if g == 0 {
		return "0（原始代）"
	}
	return fmt.Sprintf("%d", g)
}

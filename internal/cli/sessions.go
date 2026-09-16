package cli

// km sessions / km cancel <id>：会话查看与显式恢复入口（session-recovery 计划）。
// 行为合同见 docs/session-recovery-plan.md「S2 合同冻结结论」：
//   - 只读/定向操作，不获取项目执行锁（必须恰在锁持有人卡死或死亡时可用）；
//     与执行、自然退出的并发安全由容器侧 km-ctl 协议（0/3/4）幂等兜底。
//   - 先经 verifyStackForQuery 做容器归属门禁（不含镜像内容检查）；
//     再以 Sessions 列表确认目标 ID 存在，最后才取消——"未知 ID"与
//     "已完成会话"由此区分，绝不把未知当作取消成功。
//   - 不启动已停止容器、不安装脚本、不清扫登记。

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"kalimac/internal/runtime"
	"kalimac/internal/session"
)

// sessionIDPattern 匹配 NewSessionID 的产物：s + 16 位十六进制（8 字节）。
var sessionIDPattern = regexp.MustCompile(`^s[0-9a-f]{16}$`)

// cancelBudget 覆盖容器侧 km-ctl cancel 的最坏路径（等目录 5s + 等登记 5s +
// TERM/KILL 排空约 10s+5s）并留出引擎慢的余量；超时本身按失败处理，不静默。
const cancelBudget = 45 * time.Second

// runSessionsCommand 实现 `km sessions`：列出当前项目的容器内会话（只读）。
func runSessionsCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km sessions 不接受参数")
	}
	sessions, _, _, info, code := collectSessions(ctx, stdout, stderr, dk)
	if code != ExitOK {
		return code
	}
	if info != "" {
		// 查询不可执行的信息状态（容器未运行/脚本未安装）：说明原因即完成，
		// 不存在「无会话」与「未知」的混淆。
		fmt.Fprintln(stdout, info)
		return ExitOK
	}
	if len(sessions.kind) == 0 {
		fmt.Fprintln(stdout, "当前项目无会话")
		return ExitOK
	}
	// 按协议词输出完整可复制 ID；辅助提示走 stderr，stdout 保持可解析。
	for _, id := range sessions.order {
		fmt.Fprintf(stdout, "%s %s\n", sessions.kind[id], id)
	}
	if len(sessions.active) > 0 {
		fmt.Fprintf(stderr, "取消活跃会话: km cancel <上面列出的 ID>\n")
	}
	if len(sessions.stale) > 0 {
		fmt.Fprintf(stderr, "STALE 为组空遗留：下次执行自动清扫，也可 km cancel <ID> 清除登记\n")
	}
	return ExitOK
}

// runCancelCommand 实现 `km cancel <id>`：显式取消当前项目的指定会话并核验终态。
func runCancelCommand(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) != 1 {
		return usageError(stderr, "km cancel 需要且只需要一个完整会话 ID，用法: km cancel <id>（完整 ID 见 km sessions）")
	}
	sid := rest[0]
	if !sessionIDPattern.MatchString(sid) {
		return usageError(stderr, "会话 ID 格式非法（应为 s 开头的 16 位十六进制，完整 ID 见 km sessions）: %q", sid)
	}
	sessions, containerID, ctl, info, code := collectSessions(ctx, stdout, stderr, dk)
	if code != ExitOK {
		return code
	}
	if info != "" {
		// 查询不可执行的信息状态（容器未运行/脚本未安装）：无法列出会话，
		// 但「取消」在这两种状态下没有可作用的对象——说明原因并成功返回，
		// 不报 KM_SESSION_UNKNOWN（审查收口：与冻结合同对齐）。
		fmt.Fprintln(stdout, info)
		return ExitOK
	}
	kind, known := sessions.kind[sid]
	if !known {
		return envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("会话 %s 不在当前项目的会话列表中（检查 ID，或运行 km sessions 查看；不操作其他项目）", sid)})
	}
	cancelCtx, cancel := context.WithTimeout(ctx, cancelBudget)
	defer cancel()
	exit, diag, cerr := ctl.Cancel(cancelCtx, containerID, sid)
	switch {
	case cerr != nil:
		return envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("取消会话 %s 失败，诊断已保留", sid), Err: cerr})
	case exit == 0 && kind == "ACTIVE":
		fmt.Fprintf(stdout, "已取消并确认收尾: %s\n", sid)
		return ExitOK
	case exit == 0:
		fmt.Fprintf(stdout, "该会话已结束，登记已清除: %s\n", sid)
		return ExitOK
	case exit == 3:
		fmt.Fprintf(stdout, "会话已不存在（自然结束或已被清扫），无需处理: %s\n", sid)
		return ExitOK
	default: // 4 = 清理未在预算内确认；其他 = 协议外状态，一律不宣称成功
		return envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("取消会话 %s 未确认（km-ctl 退出码 %d，诊断: %s）；请运行 km sessions 复核实际状态", sid, exit, strings.TrimSpace(diag))})
	}
}

// sessionListing 是一次容器内会话列表的解析视图（字典序稳定输出）。
type sessionListing struct {
	order         []string          // 按字典序的会话 ID
	kind          map[string]string // id → ACTIVE/STALE
	active, stale []string
}

// collectSessions 完成恢复命令共用的前置链路：项目栈 → 只读归属门禁 →
// 容器内会话列表（严格解析）。任何失败都以 KM_* 稳定标识非零退出。
// 「容器未运行」「脚本未安装（127）」是查询不可执行的信息状态：info 返回
// 说明文本、code 为 ExitOK、listing 为空——调用方须输出 info 并成功返回，
// 不得把这两种状态当作「无会话」或「未知 ID」（两者只在查询成功时判定）。
// 返回列表视图、记录的容器完整 ID 与已固定 endpoint 的控制器（cancel 用）。
func collectSessions(ctx context.Context, stdout, stderr io.Writer, dk *runtime.Docker) (sessionListing, string, *session.DockerController, string, int) {
	listing := sessionListing{kind: map[string]string{}}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return listing, "", nil, "", ExitEnv
	}
	root, _, st, err := loadProjectStack(wd)
	if err != nil {
		return listing, "", nil, "", envError(stderr, err)
	}
	ep, res, err := verifyStackForQuery(ctx, dk, root, st)
	if err != nil {
		return listing, "", nil, "", envError(stderr, err)
	}
	if res.State != "running" {
		return listing, st.Container.ID, nil,
			fmt.Sprintf("容器未在运行（%s）：活跃任务不可能存在；登记目录将在下次执行时自动清扫", res.State),
			ExitOK
	}
	ctl := newSessionController(ep.Endpoint)
	sOut, sErrStr, sExit, serr := ctl.Sessions(ctx, st.Container.ID)
	switch {
	case serr != nil:
		return listing, "", nil, "", envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: "容器内会话状态查询失败，无法确认会话状态", Err: serr})
	case sExit == 127:
		return listing, st.Container.ID, ctl,
			"会话脚本未安装（init 后首次 run/shell 时安装）：当前项目不存在通过 km 登记的会话",
			ExitOK
	case sExit != 0:
		return listing, "", nil, "", envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("容器内会话状态查询退出码 %d（stderr: %s）", sExit, strings.TrimSpace(sErrStr))})
	}
	active, stale, parseOK := session.ParseSessions(sOut)
	if !parseOK {
		return listing, "", nil, "", envError(stderr, &runtime.Error{Code: runtime.CodeSessionUnknown,
			Msg: fmt.Sprintf("容器内会话状态输出异常（%q）", strings.TrimSpace(sOut))})
	}
	for _, id := range active {
		listing.kind[id] = "ACTIVE"
		listing.order = append(listing.order, id)
	}
	for _, id := range stale {
		if _, dup := listing.kind[id]; !dup {
			listing.order = append(listing.order, id)
		}
		listing.kind[id] = "STALE"
		listing.stale = append(listing.stale, id)
	}
	listing.active = active
	sort.Strings(listing.order)
	return listing, st.Container.ID, ctl, "", ExitOK
}

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// runEnvList implements `km env list`（ADR §10.6 冻结）：只读角色总览。
// 不取锁、不写任何文件、不安装脚本、不清扫；MISSING 不触发清理；
// 记录损坏/角色冲突/待恢复事务/查询不完整 → 退出码 1。
func runEnvList(ctx context.Context, rest []string, stdout, stderr io.Writer, dk *runtime.Docker) int {
	if len(rest) > 0 {
		return usageError(stderr, "km env list 不接受参数")
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "KM_ENV: 无法获取当前目录: %v\n", err)
		return ExitEnv
	}
	root, cfg, st, err := loadProjectStack(wd)
	if err != nil {
		return envError(stderr, err)
	}

	listing, err := collectEnvList(ctx, dk, root, cfg, st)
	if err != nil {
		return envError(stderr, err)
	}
	renderEnvList(stdout, listing)
	if !listing.Consistent {
		fmt.Fprintf(stdout, "结果: 记录或查询存在异常（见上），不作为健康总览；先解决标记项。\n")
		return ExitEnv
	}
	return ExitOK
}

// envResourceRow 是 list 的一行资源。
type envResourceRow struct {
	Role        string // CURRENT/PREVIOUS/RETAINED/TRANSACTION/UNTRACKED/CONFLICT
	Generation  string // 代号或 "—"
	ContainerID string // 完整 ID；缺失为 "—"
	Name        string
	State       string // 实际状态 / MISSING / UNKNOWN
	ImageID     string
	Reason      string
	Removable   string // 是 / 否（原因）
	Notes       []string
}

// envListResult 是一次只读收集的结果。
type envListResult struct {
	ProjectID  string
	Endpoint   string
	Generation string
	Rows       []envResourceRow
	Note       string
	// Consistent=false 时 list 必须以退出码 1 结束（不宣称健康）。
	Consistent bool
	TxnPending bool
	TxnSummary string
}

// envRecordFingerprint 是本地记录集合的指纹（state/previous/retained/transaction
// 的存在性与内容哈希），用于 list 的前后快照一致性检查（独立审计 P2：并发
// 变更时不输出混合角色的“健康”总览）。只读，不写任何文件。
func envRecordFingerprint(root string) string {
	parts := []string{}
	for _, p := range []string{
		project.StatePath(root), envtxn.PreviousPath(root),
		envtxn.RetainedPath(root), envtxn.TransactionPath(root),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			parts = append(parts, "absent")
			continue
		}
		parts = append(parts, envtxn.HashBytes(raw))
	}
	return strings.Join(parts, "|")
}

// roleRank 决定展示顺序（ADR：CURRENT、PREVIOUS、RETAINED、事务/异常）。
var roleRank = map[string]int{
	"CURRENT": 0, "PREVIOUS": 1, "RETAINED": 2, "TRANSACTION": 3, "UNTRACKED": 4, "CONFLICT": 3,
}

// collectEnvList 执行全部只读收集与核验。任何写操作都不得出现在本函数。
// 前后快照校验：记录集合在查看期间发生变化（并发 switch/remove 等）时有界
// 重读一次；持续变化 → 标记未核实（exit 1），不输出混合角色的假总览。
func collectEnvList(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State) (*envListResult, error) {
	for attempt := 0; ; attempt++ {
		before := envRecordFingerprint(root)
		res, err := collectEnvListOnce(ctx, dk, root, cfg, st)
		if err != nil {
			return nil, err
		}
		after := envRecordFingerprint(root)
		if before == after || attempt == 1 {
			if before != after {
				res.Consistent = false
				res.Note = "本地记录在查看期间发生变化（并发变更），以上为部分核实结果，不作为健康总览"
			}
			return res, nil
		}
		// 记录已变化：有界重读一次（仍只读）
		st2, err := project.LoadState(root)
		if err != nil {
			return nil, &runtime.Error{Code: runtime.CodeStateInvalid, Msg: "重读状态失败", Err: err}
		}
		st = st2
	}
}

// collectEnvListOnce 是单次只读收集；由 collectEnvList 做快照一致性包装。
func collectEnvListOnce(ctx context.Context, dk *runtime.Docker, root string, cfg *project.Config, st *project.State) (*envListResult, error) {
	res := &envListResult{ProjectID: st.ProjectID, Consistent: true, Generation: "—"}
	if st.Env != nil {
		res.Generation = fmt.Sprintf("%d", st.Env.Generation)
	}

	// 引擎：解析并固定；与项目记录漂移时无法给出可信事实 → 明确报错
	ep, err := resolveEngine(ctx, dk)
	if err != nil {
		return nil, err
	}
	if err := verifyProjectEngine(st, ep.Endpoint); err != nil {
		return nil, err
	}
	res.Endpoint = ep.Endpoint

	type roleMark struct {
		roles []string
		notes []string
		// 记录载荷
		prev       *envtxn.Previous
		entry      *envtxn.RetainedEntry
		imageID    string // 记录的镜像内容（用于身份比对）
		name       string // 记录的名称
		generation int
	}
	known := map[string]*roleMark{}
	addRole := func(id string, role string) *roleMark {
		if id == "" {
			return nil
		}
		m, ok := known[id]
		if !ok {
			m = &roleMark{}
			known[id] = m
		}
		m.roles = append(m.roles, role)
		return m
	}

	// 记录损坏 → exit 1（不猜测、不修复）
	txn, terr := envtxn.LoadTransaction(root)
	if terr != nil && !os.IsNotExist(terr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "事务记录无法解析（原样保留）", Err: terr}
	}
	prev, perr := envtxn.LoadPrevious(root)
	if perr != nil && !os.IsNotExist(perr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "回退槽位记录无法解析（原样保留）", Err: perr}
	}
	ret, rerr := envtxn.LoadRetained(root)
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, &runtime.Error{Code: runtime.CodeStateInvalid,
			Msg: "retained 账本无法解析（原样保留）", Err: rerr}
	}

	// CURRENT
	if st.Container.ID != "" {
		m := addRole(st.Container.ID, "CURRENT")
		m.name = st.Container.Name
		m.imageID = st.Container.ImageID
		if st.Env != nil {
			m.generation = st.Env.Generation
		}
	}
	// PREVIOUS
	if prev != nil {
		m := addRole(prev.ContainerID, "PREVIOUS")
		m.prev = prev
		m.name = prev.ContainerName
		m.imageID = prev.ImageID
		m.generation = prev.Generation
	}
	// RETAINED
	if ret != nil {
		for i := range ret.Containers {
			e := ret.Containers[i]
			m := addRole(e.ContainerID, "RETAINED")
			m.entry = &e
			m.name = e.ContainerName
			m.imageID = e.ImageID
			m.generation = e.Generation
		}
	}
	// TRANSACTION 引用资源
	if txn != nil {
		res.TxnPending = true
		res.TxnSummary = fmt.Sprintf("kind=%s stage=%s op=%s → km env recover", txn.Kind, txn.Stage, txn.OpID)
		addTxnRef := func(id, what string) {
			if id == "" {
				return
			}
			m := addRole(id, "TRANSACTION")
			if m == nil {
				return
			}
			for _, n := range m.notes {
				if n == what {
					return
				}
			}
			m.notes = append(m.notes, what)
		}
		addTxnRef(txn.Old.ContainerID, "事务原环境")
		addTxnRef(txn.New.ContainerID, "事务目标环境")
		for _, p := range txn.Probes {
			addTxnRef(p.ContainerID, "事务探测容器")
		}
	}

	// 枚举实际容器（完整 ID）
	actual, enumErr := dk.FindContainersByLabelFull(ctx, runtime.ProjectLabel, st.ProjectID)
	if enumErr != nil {
		res.Consistent = false
	}
	actualState := map[string]string{}
	for _, s := range actual {
		actualState[s.ID] = s.State
	}

	// 冲突标记：同一 ID 出现在多个互斥角色记录中 → 保护优先（不按角色放行）
	for _, m := range known {
		if len(m.roles) > 1 {
			m.notes = append(m.notes, "同一容器出现在多个角色记录中（"+strings.Join(uniqStr(m.roles), "；")+"）；保护优先，不作为删除授权来源")
			m.roles = []string{"CONFLICT"}
			res.Consistent = false
		}
	}

	// 逐个核验已知资源（完整 ID inspect）
	for id, m := range known {
		row := envResourceRow{
			Role: m.roles[0], Generation: "—", ContainerID: id,
			Name: m.name, State: "UNKNOWN", Notes: m.notes,
		}
		if len(m.roles) > 0 && m.roles[0] == "CONFLICT" {
			row.Role = "CONFLICT"
			row.Removable = "否（记录冲突，保护优先）"
		}
		if m.generation > 0 || (m.roles[0] == "CURRENT" && st.Env != nil) {
			row.Generation = fmt.Sprintf("%d", m.generation)
		} else if m.roles[0] == "CURRENT" || m.roles[0] == "PREVIOUS" || m.roles[0] == "RETAINED" {
			row.Generation = "0（原始代）"
		}
		if m.entry != nil {
			row.Reason = m.entry.Reason
		}
		// inspect 完整 ID
		ext, exists, ierr := dk.InspectContainerExtended(ctx, id)
		switch {
		case ierr != nil:
			row.State = "UNKNOWN"
			row.Notes = append(row.Notes, "查询失败："+ierr.Error())
			res.Consistent = false
			row.Removable = "否（状态未核实）"
		case !exists:
			row.State = "MISSING"
			if m.roles[0] == "RETAINED" {
				row.Removable = "是（仅清理失效记录，容器已不存在）"
				row.Notes = append(row.Notes, "失效记录：km env remove <完整ID> 可在确认后仅移除本条记录")
			} else {
				row.Removable = "否（容器不存在）"
			}
		default:
			row.State = ext.State
			row.ImageID = shortID(ext.Image)
			// 身份核验：与 remove/recover 共用同一套判定（缺失挂载不得视为匹配）
			var probeEntry *envtxn.RetainedEntry
			if m.entry != nil && m.entry.Reason == envtxn.ReasonProbeCleanupFailed {
				probeEntry = m.entry
			}
			problems := envIdentityProblems(ext, st, root, m.name, m.imageID, probeEntry)
			if len(problems) > 0 {
				row.Role = "CONFLICT"
				row.Notes = append(row.Notes, problems...)
				res.Consistent = false
				row.Removable = "否（身份与记录不符）"
			} else if m.roles[0] == "RETAINED" {
				if ext.State == "exited" && !res.TxnPending {
					row.Removable = "是（km env remove <完整ID>）"
				} else if ext.State != "exited" {
					row.Removable = fmt.Sprintf("否（状态 %s，仅 exited 可删除）", ext.State)
				} else {
					row.Removable = "否（存在未完成事务，先 km env recover）"
				}
			} else {
				row.Removable = "否（" + map[string]string{
					"CURRENT": "当前环境受保护", "PREVIOUS": "回退目标受保护",
					"TRANSACTION": "事务资源受保护，先 recover", "UNTRACKED": "未知归属，仅报告",
				}[m.roles[0]] + "）"
			}
		}
		res.Rows = append(res.Rows, row)
	}

	// UNTRACKED：带项目标签但不在任何已知记录中的容器（仅报告，不接管不删除）
	for id, state := range actualState {
		if _, ok := known[id]; ok {
			continue
		}
		row := envResourceRow{
			Role: "UNTRACKED", Generation: "—", ContainerID: id,
			State: state, Removable: "否（未知归属，仅报告）",
			Notes: []string{"带本项目标签但不在任何记录中；请人工核验来源"},
		}
		ext, exists, ierr := dk.InspectContainerExtended(ctx, id)
		switch {
		case ierr != nil:
			// 账本外资源同样服从查询完整性合同：失败是 UNKNOWN，不是健康行
			row.State = "UNKNOWN"
			row.Notes = append(row.Notes, "查询失败："+ierr.Error())
			res.Consistent = false
		case !exists:
			row.State = "MISSING"
			row.Notes = append(row.Notes, "枚举后已消失（并发删除）")
		default:
			row.Name = ext.Name
			row.ImageID = shortID(ext.Image)
		}
		res.Rows = append(res.Rows, row)
	}

	// 枚举失败：无法证明看到的集合完整，不宣称任何资源可删除（ADR §10.6）
	if enumErr != nil {
		for i := range res.Rows {
			if strings.HasPrefix(res.Rows[i].Removable, "是") {
				res.Rows[i].Removable = "否（引擎枚举失败，资源集合未核实）"
			}
		}
	}

	// 稳定排序：角色 → 代号 → 完整 ID
	sort.SliceStable(res.Rows, func(i, j int) bool {
		ri, rj := roleRank[res.Rows[i].Role], roleRank[res.Rows[j].Role]
		if ri != rj {
			return ri < rj
		}
		if res.Rows[i].Generation != res.Rows[j].Generation {
			return res.Rows[i].Generation < res.Rows[j].Generation
		}
		return res.Rows[i].ContainerID < res.Rows[j].ContainerID
	})
	return res, nil
}

func renderEnvList(w io.Writer, res *envListResult) {
	fmt.Fprintf(w, "km env list（只读，不做任何变更）\n")
	fmt.Fprintf(w, "  项目: %s（当前代号 %s）\n", res.ProjectID, res.Generation)
	fmt.Fprintf(w, "  引擎: %s\n", res.Endpoint)
	if res.TxnPending {
		fmt.Fprintf(w, "  ⚠ 未完成事务: %s（以下 TRANSACTION 资源受保护，需先恢复）\n", res.TxnSummary)
	}
	if res.Note != "" {
		fmt.Fprintf(w, "  ⚠ %s\n", res.Note)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "ROLE        GEN          CONTAINER_ID                                                        NAME                     STATE      IMAGE_ID    REASON                    REMOVABLE\n")
	for _, r := range res.Rows {
		gen := truncate(r.Generation, 12) // 按字符截断（字节截断曾切断“代”字）
		fmt.Fprintf(w, "%-10s  %-12s %-64s %-24s %-10s %-11s %-24s %s\n",
			r.Role, gen, r.ContainerID, truncate(r.Name, 24), r.State, r.ImageID, truncate(r.Reason, 24), r.Removable)
		for _, n := range r.Notes {
			fmt.Fprintf(w, "    · %s\n", n)
		}
	}
	if len(res.Rows) == 0 {
		fmt.Fprintf(w, "  （无已知资源）\n")
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func uniqStr(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

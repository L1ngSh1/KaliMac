package cli

import (
	"fmt"
	"path/filepath"

	"kalimac/internal/envtxn"
	"kalimac/internal/project"
	"kalimac/internal/runtime"
)

// envIdentityProblems 是 list / remove / recover 共用的纯只读身份核验
// （独立审计 P2-4：两处判定必须一致，缺失挂载不得被视为匹配）。
// recName/recImage 是记录中的名称与镜像内容；probeEntry 非 nil 时按探测容器
// 角色核验（km-probe-* 命名 + 只读挂载），否则要求项目容器读写挂载。
// 返回问题列表；空列表 = 身份与记录一致。
func envIdentityProblems(ext runtime.InspectExtended, st *project.State, root, recName, recImage string, probeEntry *envtxn.RetainedEntry) []string {
	var problems []string
	if recName != "" && ext.Name != recName {
		problems = append(problems, fmt.Sprintf("名称 %q 与记录 %q 不符（同名新容器不能替代原 ID）", ext.Name, recName))
	}
	if ext.ProjectID != st.ProjectID {
		problems = append(problems, fmt.Sprintf("项目标签 %q 与本项目不符", ext.ProjectID))
	}
	if recImage != "" && ext.Image != recImage {
		problems = append(problems, fmt.Sprintf("镜像内容 %s… 与记录 %s… 不符", shortID(ext.Image), shortID(recImage)))
	}
	if filepath.Clean(ext.MountSource) != filepath.Clean(root) {
		if ext.MountSource == "" {
			problems = append(problems, "缺少 /workspace 挂载")
		} else {
			problems = append(problems, fmt.Sprintf("/workspace 挂载源 %q 与项目根不符", ext.MountSource))
		}
	}
	if probeEntry != nil {
		// 探测容器角色完整核验（ADR §10.3.5）：无法按角色核验即拒绝
		if ext.Name != "km-probe-"+probeEntry.OpID {
			problems = append(problems, "probe-cleanup-failed 条目要求 km-probe-* 命名")
		}
		if ext.MountSource != "" && ext.MountRW {
			problems = append(problems, "探测容器工作区应为只读挂载")
		}
	} else if ext.MountSource != "" && !ext.MountRW {
		problems = append(problems, "项目容器工作区应为读写挂载")
	}
	return problems
}

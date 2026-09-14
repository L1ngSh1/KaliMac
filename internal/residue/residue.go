// Package residue 把测试套件终检对登记资源的核对拆成可注入的纯判定：
// 「确认消失」「确认残留」「因查询失败无法核实」三类必须区分，
// 查询失败不得等价为资源不存在。
package residue

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// InspectFunc 报告一个容器是否仍然存在。查询失败必须以非 nil 错误返回。
type InspectFunc func(id string) (exists bool, err error)

// Unverified 记录一个无法核实状态的容器及原因。
type Unverified struct {
	ID  string
	Err string
}

// Verdict 是一次终检分类结果。
type Verdict struct {
	Residue      []string     // 确认仍存在的容器（完整 ID）
	Unverifiable []Unverified // 因查询失败无法核实的容器
}

// Clean 报告是否「确认全部清理」：既无残留，也无未核实项。
func (v Verdict) Clean() bool {
	return len(v.Residue) == 0 && len(v.Unverifiable) == 0
}

// Classify 逐个核对登记的完整 ID；inspect 返回错误的一律进入
// Unverifiable，绝不并入「已消失」。
func Classify(ids []string, inspect InspectFunc) Verdict {
	var v Verdict
	for _, id := range ids {
		exists, err := inspect(id)
		switch {
		case err != nil:
			v.Unverifiable = append(v.Unverifiable, Unverified{ID: id, Err: err.Error()})
		case exists:
			v.Residue = append(v.Residue, id)
		}
	}
	return v
}

// DockerInspect 用 docker container inspect 核对单个容器。仅当引擎明确
// 回答「容器不存在」（顶层 inspect 为 No such object，container inspect
// 为 No such container）才判定不存在；连接失败、daemon 不可达等一律作为
// 错误返回，由调用方按「无法核实」处理。
func DockerInspect(id string) (bool, error) {
	cmd := exec.Command("docker", "container", "inspect", "--format", "{{.Id}}", id)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	msg := errb.String()
	if strings.Contains(msg, "No such object") || strings.Contains(msg, "No such container") {
		return false, nil
	}
	return false, fmt.Errorf("docker container inspect %s: %v (stderr=%s)", id, err, strings.TrimSpace(msg))
}

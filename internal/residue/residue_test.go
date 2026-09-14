package residue

import (
	"errors"
	"testing"
)

// fakeInspect 按表驱动模拟 inspect 结果：errTable 命中 → 返回错误
// （模拟 daemon 不可达等查询失败）；否则按 existing 判定存在性。
func fakeInspect(errTable map[string]error, existing map[string]bool) InspectFunc {
	return func(id string) (bool, error) {
		if err, ok := errTable[id]; ok {
			return false, err
		}
		return existing[id], nil
	}
}

func TestClassifyEmptyRegistry(t *testing.T) {
	v := Classify(nil, fakeInspect(nil, nil))
	if !v.Clean() {
		t.Fatalf("空登记应干净: %+v", v)
	}
}

func TestClassifyAllRemoved(t *testing.T) {
	v := Classify([]string{"a", "b"}, fakeInspect(nil, map[string]bool{}))
	if !v.Clean() {
		t.Fatalf("全部消失应干净: %+v", v)
	}
}

func TestClassifyResidueReported(t *testing.T) {
	v := Classify([]string{"a", "b", "c"}, fakeInspect(nil, map[string]bool{"b": true}))
	if len(v.Residue) != 1 || v.Residue[0] != "b" {
		t.Fatalf("应仅报告残留 b: %+v", v)
	}
	if v.Clean() {
		t.Fatal("有残留不得判为干净")
	}
}

// 负向回归（M1 验收核心）：修复前 exitCodeOf 把非 ExitError 查询失败记为
// -1，与「容器不存在」混同，终检静默通过；修复后查询失败必须进入
// Unverifiable 且 Clean() 为 false。
func TestClassifyQueryErrorIsNotAbsence(t *testing.T) {
	daemonDown := errors.New("docker: Cannot connect to the Docker daemon")
	v := Classify([]string{"c1", "c2"}, fakeInspect(map[string]error{"c1": daemonDown}, map[string]bool{}))
	if len(v.Unverifiable) != 1 || v.Unverifiable[0].ID != "c1" {
		t.Fatalf("查询失败应记为无法核实: %+v", v)
	}
	if len(v.Residue) != 0 {
		t.Fatalf("查询失败不得伪造成残留或消失: %+v", v)
	}
	if v.Clean() {
		t.Fatal("存在无法核实项时不得判为干净")
	}
}

func TestClassifyMixed(t *testing.T) {
	daemonDown := errors.New("timeout")
	v := Classify([]string{"gone", "live", "blind"},
		fakeInspect(map[string]error{"blind": daemonDown}, map[string]bool{"live": true}))
	if len(v.Residue) != 1 || v.Residue[0] != "live" {
		t.Fatalf("残留判定错误: %+v", v)
	}
	if len(v.Unverifiable) != 1 || v.Unverifiable[0].ID != "blind" {
		t.Fatalf("无法核实判定错误: %+v", v)
	}
	if v.Clean() {
		t.Fatal("混合场景不得判为干净")
	}
}

package runtime

import (
	"context"
	"strings"
	"testing"
)

// 独立审计反例（P2）：扩展 inspect 的挂载源与 RW 必须是独立字段——
// 用户路径中的逗号（如 /tmp/project,notes）不得被当作分隔符截断。
func TestAuditExtendedInspectPreservesCommaPath(t *testing.T) {
	id := strings.Repeat("a", 64)
	root := "/tmp/project,notes"
	d := &Docker{Exec: &FakeExecutor{Respond: func(name string, args []string) ([]byte, []byte, error) {
		// 10 字段格式：挂载源与 RW 分离
		return []byte(id + "|/km-demo|exited|pdemo|sha256:aaa|" + root + "|true|||"), nil, nil
	}}}
	got, exists, err := d.InspectContainerExtended(context.Background(), id)
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if got.MountSource != root || !got.MountRW {
		t.Fatalf("comma in valid directory name must survive inspect serialization: mount=%q rw=%v", got.MountSource, got.MountRW)
	}
}

package envtxn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadTransactionRoundTrip(t *testing.T) {
	root := t.TempDir()
	txn := &Transaction{
		OpID:           "e0123456789abcdef",
		Kind:           KindSwitch,
		Stage:          StagePrepared,
		Old:            Snapshot{ContainerID: strings.Repeat("a", 64), ContainerName: "km-p1", ImageID: "sha256:x", WasRunning: true, Generation: 0},
		New:            Snapshot{ContainerName: "km-p1-g1", ImageID: "sha256:y", WasRunning: true, Generation: 1},
		ConfigBackup:   NewFileBackup([]byte(`{"schema_version":1,"image":"a:1"}`)),
		StateBackup:    NewFileBackup([]byte(`{"state_version":1}`)),
		TargetImageRef: "b:2",
	}
	if err := SaveTransaction(root, txn); err != nil {
		t.Fatalf("SaveTransaction: %v", err)
	}
	if txn.CreatedAt == "" || txn.UpdatedAt == "" {
		t.Fatalf("时间戳未填充: %q %q", txn.CreatedAt, txn.UpdatedAt)
	}
	got, err := LoadTransaction(root)
	if err != nil {
		t.Fatalf("LoadTransaction: %v", err)
	}
	if got.OpID != txn.OpID || got.Kind != KindSwitch || got.Stage != StagePrepared {
		t.Fatalf("往返不一致: %+v", got)
	}
	if got.Old.ContainerName != "km-p1" || got.New.Generation != 1 {
		t.Fatalf("快照往返不一致: %+v", got)
	}
	if got.ConfigBackup.SHA256 != txn.ConfigBackup.SHA256 {
		t.Fatalf("备份哈希不一致")
	}
}

func TestLoadTransactionRejectsUnknownEnvVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	p := TransactionPath(root)
	raw := `{"env_version":99,"op_id":"e0123456789abcdef","kind":"switch","stage":"PREPARED"}`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTransaction(root)
	if err == nil || !strings.Contains(err.Error(), "不支持的记录版本 99") {
		t.Fatalf("未知版本应被拒绝，得到 %v", err)
	}
	// 原始证据必须原样保留（拒绝写入/修复）
	if got, rerr := os.ReadFile(p); rerr != nil || string(got) != raw {
		t.Fatalf("原始文件被改动: %v %q", rerr, got)
	}
}

func TestLoadTransactionRejectsCorruptAndInvalidFields(t *testing.T) {
	t.Run("坏JSON", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(Dir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(TransactionPath(root), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTransaction(root); err == nil {
			t.Fatal("坏 JSON 应被拒绝")
		}
	})
	t.Run("stage非法", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(Dir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := `{"env_version":1,"op_id":"e0123456789abcdef","kind":"switch","stage":"WAT"}`
		if err := os.WriteFile(TransactionPath(root), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTransaction(root); err == nil || !strings.Contains(err.Error(), "stage 非法") {
			t.Fatalf("非法 stage 应被拒绝，得到 %v", err)
		}
	})
	t.Run("rollback缺目标容器", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(Dir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := `{"env_version":1,"op_id":"e0123456789abcdef","kind":"rollback","stage":"PREPARED","old":{"container_name":"x"},"new":{"container_name":"y"},"config_backup":{"sha256":"a"},"state_backup":{"sha256":"b"}}`
		if err := os.WriteFile(TransactionPath(root), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTransaction(root); err == nil || !strings.Contains(err.Error(), "上一代容器 ID") {
			t.Fatalf("rollback 缺目标应被拒绝，得到 %v", err)
		}
	})
}

func TestClearTransactionIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := ClearTransaction(root); err != nil {
		t.Fatalf("清除不存在的事务应幂等成功: %v", err)
	}
	if TransactionExists(root) {
		t.Fatal("不应存在事务")
	}
}

func TestPreviousRoundTripAndValidation(t *testing.T) {
	root := t.TempDir()
	prev := &Previous{
		Generation: 0, ContainerID: strings.Repeat("b", 64), ContainerName: "km-p1",
		ImageID: "sha256:x", ImageRef: "a:1", WasRunning: true, OpID: "e0123456789abcdef",
	}
	if err := SavePrevious(root, prev); err != nil {
		t.Fatalf("SavePrevious: %v", err)
	}
	got, err := LoadPrevious(root)
	if err != nil {
		t.Fatalf("LoadPrevious: %v", err)
	}
	if got.ImageRef != "a:1" || got.ContainerID != prev.ContainerID {
		t.Fatalf("往返不一致: %+v", got)
	}
	if err := RemovePrevious(root); err != nil {
		t.Fatalf("RemovePrevious: %v", err)
	}
	if err := RemovePrevious(root); err != nil {
		t.Fatalf("移除应幂等: %v", err)
	}
	if PreviousExists(root) {
		t.Fatal("槽位应已移除")
	}

	// 完整性校验：缺 image_ref 拒绝
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"env_version":1,"generation":0,"container_id":"` + strings.Repeat("b", 64) + `","container_name":"km-p1","image_id":"sha256:x","op_id":"e0123456789abcdef"}`
	if err := os.WriteFile(PreviousPath(root), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrevious(root); err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("缺 image_ref 应被拒绝，得到 %v", err)
	}
}

func TestAppendRetainedDedupes(t *testing.T) {
	root := t.TempDir()
	e := RetainedEntry{ContainerID: strings.Repeat("c", 64), ContainerName: "km-p1", ImageID: "sha256:x", Generation: 0, Reason: ReasonRolledBack, OpID: "e0123456789abcdef"}
	if changed, err := AppendRetained(root, e); err != nil || !changed {
		t.Fatalf("首次追加: changed=%v err=%v", changed, err)
	}
	if changed, err := AppendRetained(root, e); err != nil || changed {
		t.Fatalf("重复追加应去重: changed=%v err=%v", changed, err)
	}
	r, err := LoadRetained(root)
	if err != nil {
		t.Fatalf("LoadRetained: %v", err)
	}
	if len(r.Containers) != 1 {
		t.Fatalf("账本应只有 1 条: %+v", r)
	}
}

func TestRetainedRejectsUnknownVersionAndDuplicates(t *testing.T) {
	t.Run("未知版本", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(Dir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(RetainedPath(root), []byte(`{"env_version":7,"containers":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRetained(root); err == nil {
			t.Fatal("未知版本应被拒绝")
		}
	})
	t.Run("重复容器", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(Dir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		cid := strings.Repeat("c", 64)
		raw := `{"env_version":1,"containers":[{"container_id":"` + cid + `","container_name":"a"},{"container_id":"` + cid + `","container_name":"a"}]}`
		if err := os.WriteFile(RetainedPath(root), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRetained(root); err == nil || !strings.Contains(err.Error(), "重复") {
			t.Fatalf("重复条目应被拒绝，得到 %v", err)
		}
	})
}

func TestWriteFailureLeavesNoPartialRecord(t *testing.T) {
	root := t.TempDir()
	// 把 env 目录造成一个普通文件：MkdirAll 失败，写入必须报错且不留半成品
	if err := os.MkdirAll(filepath.Dir(Dir(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Dir(root), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	txn := &Transaction{OpID: "e0123456789abcdef", Kind: KindSwitch, Stage: StagePrepared}
	if err := SaveTransaction(root, txn); err == nil {
		t.Fatal("写入应失败")
	}
	if TransactionExists(root) {
		t.Fatal("失败后不应存在事务文件")
	}
}

func TestFileBackupDetectsEdits(t *testing.T) {
	orig := []byte(`{"a":1}`)
	b := NewFileBackup(orig)
	if !b.Matches(orig) {
		t.Fatal("原文应匹配")
	}
	if b.Matches([]byte(`{"a":2}`)) {
		t.Fatal("篡改内容不应匹配")
	}
	raw, err := b.Bytes()
	if err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("备份解码不一致: %q %v", raw, err)
	}
}

func TestStageOrder(t *testing.T) {
	if !AtOrAfterCommitIntent(StageCommitIntent) || !AtOrAfterCommitIntent(StageCurrentCommitted) {
		t.Fatal("COMMIT_INTENT 及之后应判定为提交点后")
	}
	for _, s := range []string{StagePrepared, StageCandidateCreated, StageCandidateVerified, StageOldStopped} {
		if AtOrAfterCommitIntent(s) {
			t.Fatalf("%s 不应判定为提交点后", s)
		}
	}
	if _, ok := StageRank("NOPE"); ok {
		t.Fatal("未知 stage 不应有秩")
	}
}

func TestNewOpIDFormat(t *testing.T) {
	id, err := NewOpID()
	if err != nil {
		t.Fatalf("NewOpID: %v", err)
	}
	if !ValidOpID(id) {
		t.Fatalf("op id 格式非法: %q", id)
	}
	id2, _ := NewOpID()
	if id == id2 {
		t.Fatal("op id 应唯一")
	}
}

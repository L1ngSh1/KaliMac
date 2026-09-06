package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLockAcquireRelease(t *testing.T) {
	root := t.TempDir()
	l, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := os.Stat(LockPath(root)); err != nil {
		t.Fatalf("锁文件应存在: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(LockPath(root)); !os.IsNotExist(err) {
		t.Fatal("释放后锁文件应删除")
	}
	// 幂等释放
	if err := l.Release(); err != nil {
		t.Fatalf("重复释放应无害: %v", err)
	}
}

func TestLockBusyOnLiveHolder(t *testing.T) {
	root := t.TempDir()
	l1, err := AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	// 当前进程持有锁 → 第二次获取应 BUSY
	_, err = AcquireLock(root)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("应返回 BusyError, got %v", err)
	}
	if busy.Pid != os.Getpid() {
		t.Fatalf("BusyError 应记录持有人 pid: %d", busy.Pid)
	}
}

func TestLockStaleHolderTakeover(t *testing.T) {
	root := t.TempDir()
	// 伪造遗留锁：pid 几乎不可能存活
	stale := "pid=99999999\ncreated_at=2026-01-01T00:00:00Z\n"
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LockPath(root), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("遗留锁应被接管: %v", err)
	}
	defer l.Release()
	if !l.BrokeStale() {
		t.Fatal("应标记 BrokeStale")
	}
}

func TestLockCorruptContentTakeover(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(LockPath(root), []byte("garbage"), 0o644)
	l, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("损坏锁应被接管: %v", err)
	}
	defer l.Release()
}

func TestWorkspaceDirMapping(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "中文 目录", "sub one")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cwd  string
		want string
	}{
		{root, "/workspace"},
		{sub, "/workspace/中文 目录/sub one"},
	}
	for _, tc := range cases {
		got, err := WorkspaceDir(root, tc.cwd)
		if err != nil {
			t.Fatalf("WorkspaceDir(%s): %v", tc.cwd, err)
		}
		if got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestWorkspaceDirSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := WorkspaceDir(root, link)
	if err == nil || !strings.Contains(err.Error(), "之外") {
		t.Fatalf("符号链接逃逸应被拒绝: %v", err)
	}
	// root 自身经符号链接访问应正常
	rootLink := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	got, err := WorkspaceDir(rootLink, rootLink)
	if err != nil || got != "/workspace" {
		t.Fatalf("根路径符号链接应解析: %q %v", got, err)
	}
}

// Release 的所有权核验：锁被他人接管后，旧持有者的 Release 不应删除新锁。
func TestReleaseOwnershipGuard(t *testing.T) {
	root := t.TempDir()
	l, err := AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟锁被接管：内容换成他人 token（持有人是本进程 → 活跃）
	foreign := fmt.Sprintf("pid=%d\ntoken=deadbeef\ntoken_owner=other\n", os.Getpid())
	if err := os.WriteFile(LockPath(root), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(LockPath(root)); err != nil {
		t.Fatal("所有权不符时不应删除锁文件")
	}
}

// 并发 AcquireLock：持有期间其他获取者必须全部 BUSY。
func TestConcurrentAcquireSingleHolder(t *testing.T) {
	root := t.TempDir()
	const others = 8
	l1, err := AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	start := make(chan struct{})
	errs := make(chan error, others)
	var wg sync.WaitGroup
	for i := 0; i < others; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := AcquireLock(root)
			errs <- err
		}()
	}
	close(start)
	wg.Wait() // 全部在 l1 持有期间尝试
	for i := 0; i < others; i++ {
		err := <-errs
		var busy *BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("持有期间的其他获取应 BUSY: %v", err)
		}
	}
}

// 空文件遗留锁（旧版本产物）仍应被接管而非卡死。
func TestEmptyStaleLockTakeover(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(LockPath(root), []byte(""), 0o644)
	l, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("空文件遗留锁应被接管: %v", err)
	}
	defer l.Release()
}

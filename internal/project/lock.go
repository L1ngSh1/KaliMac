package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockPath returns the project lock file path (inside .km/).
func LockPath(root string) string { return filepath.Join(root, StateDirName, "lock") }

// BusyError means another live process holds this project's lock. Different
// projects are unaffected (per-project lock).
type BusyError struct {
	Pid       int
	HeldSince string
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%s: 项目正被其他 km 会话占用（pid=%d，自 %s）；同项目执行串行，其他项目不受影响",
		CodeBusy, e.Pid, e.HeldSince)
}

// CodeBusy is the KM_* marker for BusyError.
const CodeBusy = "KM_PROJECT_BUSY"

// Lock is a held project lock. Release only removes the file when it still
// carries our token（所有权核验）. Breaking a stale lock does NOT imply
// container tasks ended — container-side session state is the authority for
// running tools.
type Lock struct {
	path       string
	token      string
	acquired   bool
	brokeStale bool
}

// BrokeStale reports whether a stale lock (dead holder) had to be removed.
func (l *Lock) BrokeStale() bool { return l != nil && l.brokeStale }

// AcquireLock creates the project lock exclusively and atomically: the
// holder first writes a complete lock file (pid/token/timestamp) to a
// unique temp name, then publishes it via link(2), which fails with EEXIST
// when the lock exists. 消除了“先创建后写内容”窗口里空文件被当作损坏锁
// 删除的竞争。When the published lock belongs to a dead holder it is taken
// over (BrokeStale). 释放锁不代表容器任务结束。
func AcquireLock(root string) (*Lock, error) {
	dir := filepath.Join(root, StateDirName)
	path := LockPath(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	brokeStale := false
	for attempt := 0; attempt < 3; attempt++ {
		token, err := newLockToken()
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(dir, "lock.tmp-*")
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(tmp, "pid=%d\ntoken=%s\ncreated_at=%s\n", os.Getpid(), token, time.Now().UTC().Format(time.RFC3339))
		tmp.Close()
		linkErr := os.Link(tmp.Name(), path)
		os.Remove(tmp.Name())
		if linkErr == nil {
			return &Lock{path: path, token: token, acquired: true, brokeStale: brokeStale}, nil
		}
		if !os.IsExist(linkErr) {
			return nil, linkErr
		}
		pid, since, _, perr := readLock(path)
		if perr == nil && lockHolderAlive(pid) {
			return nil, &BusyError{Pid: pid, HeldSince: since}
		}
		// 遗留（持有人已死亡）或损坏/空文件：接管
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, rmErr
		}
		brokeStale = true
	}
	return nil, &BusyError{Pid: 0, HeldSince: "unknown"}
}

func newLockToken() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Release removes the lock file only when it still carries our token
// （所有权核验：锁已被他人接管时不动它）。
func (l *Lock) Release() error {
	if l == nil || !l.acquired {
		return nil
	}
	l.acquired = false
	if _, err := os.Stat(l.path); err == nil {
		_, _, tok, perr := readLock(l.path)
		if perr != nil || tok != l.token {
			return nil // 锁已被他人接管，不动它
		}
	}
	err := os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func readLock(path string) (pid int, since string, token string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "pid="); ok {
			pid, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 0, "", "", err
			}
		}
		if v, ok := strings.CutPrefix(line, "created_at="); ok {
			since = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "token="); ok {
			token = strings.TrimSpace(v)
		}
	}
	if pid <= 0 {
		return 0, "", "", fmt.Errorf("锁文件缺有效 pid")
	}
	return pid, since, token, nil
}

// lockHolderAlive checks pid liveness via signal 0.
func lockHolderAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// WorkspaceDir maps cwd (inside project root) to the container-side
// directory under /workspace. Both sides are symlink-resolved first: a cwd
// that resolves outside the project root is rejected instead of silently
// mapped.
func WorkspaceDir(root, cwd string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	resolvedCwd, err := filepath.EvalSymlinks(absCwd)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedCwd)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "/workspace", nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("工作目录 %s 解析后位于项目根 %s 之外", cwd, root)
	}
	return "/workspace/" + filepath.ToSlash(rel), nil
}

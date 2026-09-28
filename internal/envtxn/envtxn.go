// Package envtxn implements the versioned local records for project
// environment switching: the pending transaction journal, the single
// rollback slot and the retained-container ledger under <root>/.km/env/.
// 本包只负责记录层（schema、校验、原子读写），不接触 Docker。
package envtxn

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// EnvVersion is the only record format this build reads and writes.
const EnvVersion = 1

// Transaction stages in execution order. Rank decides the frozen recovery
// direction: rank < RankCommitIntent converges to the previous environment,
// rank >= RankCommitIntent converges to the new one.
const (
	StagePrepared          = "PREPARED"
	StageCandidateCreated  = "CANDIDATE_CREATED"
	StageCandidateVerified = "CANDIDATE_VERIFIED"
	StageOldStopped        = "OLD_STOPPED"
	StageCommitIntent      = "COMMIT_INTENT"
	StageCurrentCommitted  = "CURRENT_COMMITTED"
)

// Transaction kinds.
const (
	KindSwitch   = "switch"
	KindRollback = "rollback"
)

var stageRank = map[string]int{
	StagePrepared:          0,
	StageCandidateCreated:  1,
	StageCandidateVerified: 2,
	StageOldStopped:        3,
	StageCommitIntent:      4,
	StageCurrentCommitted:  5,
}

// StageRank returns the order rank of a stage; ok=false for unknown stages.
func StageRank(stage string) (int, bool) { r, ok := stageRank[stage]; return r, ok }

// AtOrAfterCommitIntent reports whether the transaction already passed the
// point of no return (the frozen recovery-direction rule).
func AtOrAfterCommitIntent(stage string) bool {
	r, ok := stageRank[stage]
	return ok && r >= stageRank[StageCommitIntent]
}

// Snapshot records one environment generation: the container that embodied
// it and the state needed to restore or activate it.
type Snapshot struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	ImageID       string `json:"image_id"`
	WasRunning    bool   `json:"was_running"`
	Generation    int    `json:"generation"`
}

// FileBackup keeps the exact bytes (base64) and their hash of one file so a
// transaction can detect external edits and restore the previous state.
type FileBackup struct {
	SHA256 string `json:"sha256"`
	Data   string `json:"data_base64"`
}

// ProbeRecord registers one read-only probe container created by the
// transaction so the ledger can account for it even if cleanup failed.
type ProbeRecord struct {
	ContainerID string `json:"container_id"`
	Removed     bool   `json:"removed"`
}

// Transaction is the pending-transaction journal (.km/env/transaction.json).
// Its presence means the project is mid-operation and must be recovered
// before any other mutating command proceeds.
type Transaction struct {
	EnvVersion int    `json:"env_version"`
	OpID       string `json:"op_id"`
	Kind       string `json:"kind"`
	Stage      string `json:"stage"`

	Old Snapshot `json:"old"`
	New Snapshot `json:"new"`

	ConfigBackup FileBackup `json:"config_backup"`
	StateBackup  FileBackup `json:"state_backup"`

	Probes []ProbeRecord `json:"probes,omitempty"`

	TargetImageRef string `json:"target_image_ref,omitempty"` // switch: user ref
	PrevImageRef   string `json:"prev_image_ref,omitempty"`   // rollback: previous gen's config ref

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Previous is the rollback slot (.km/env/previous.json): the environment the
// most recent successful switch replaced. Consumed by a successful rollback.
type Previous struct {
	EnvVersion    int    `json:"env_version"`
	Generation    int    `json:"generation"`
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	ImageID       string `json:"image_id"`
	ImageRef      string `json:"image_ref"` // the .km.json image field before the switch
	WasRunning    bool   `json:"was_running"`
	OpID          string `json:"op_id"`
	SwitchedAt    string `json:"switched_at"`
}

// Retained reasons.
const (
	ReasonSupersededBySwitch = "superseded-by-switch"
	ReasonRolledBack         = "rolled-back"
	ReasonProbeCleanupFailed = "probe-cleanup-failed"
)

// RetainedEntry is one intentionally kept container in the resource ledger.
type RetainedEntry struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	ImageID       string `json:"image_id"`
	Generation    int    `json:"generation"`
	Reason        string `json:"reason"`
	OpID          string `json:"op_id"`
	RetainedAt    string `json:"retained_at"`
}

// Retained is the ledger (.km/env/retained.json). Entries are only appended
// (deduplicated by container ID); cleanup is a documented manual procedure.
type Retained struct {
	EnvVersion int             `json:"env_version"`
	Containers []RetainedEntry `json:"containers"`
}

// Error marks a damaged or incompatible env record file.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string { return e.Path + ": " + e.Msg }

func envErr(path, format string, a ...any) error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, a...)}
}

// Paths ---------------------------------------------------------------------

// Dir returns the env record directory inside the project root.
func Dir(root string) string { return filepath.Join(root, ".km", "env") }

func path(root, name string) string { return filepath.Join(Dir(root), name) }

// TransactionPath returns the pending-transaction file path.
func TransactionPath(root string) string { return path(root, "transaction.json") }

// PreviousPath returns the rollback-slot file path.
func PreviousPath(root string) string { return path(root, "previous.json") }

// RetainedPath returns the retained-ledger file path.
func RetainedPath(root string) string { return path(root, "retained.json") }

// Op IDs --------------------------------------------------------------------

var opIDPattern = regexp.MustCompile(`^e[0-9a-f]{16}$`)

// ValidOpID reports whether s looks like an operation ID minted by NewOpID.
func ValidOpID(s string) bool { return opIDPattern.MatchString(s) }

// NewOpID mints a unique operation id ("e" + 16 hex chars).
func NewOpID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成操作 ID 失败: %w", err)
	}
	return "e" + hex.EncodeToString(buf), nil
}

// Hashing -------------------------------------------------------------------

// HashBytes returns the lowercase hex sha256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewFileBackup captures the exact bytes of a file for later edit detection
// and restore.
func NewFileBackup(data []byte) FileBackup {
	return FileBackup{SHA256: HashBytes(data), Data: base64.StdEncoding.EncodeToString(data)}
}

// Bytes decodes the backup payload.
func (b FileBackup) Bytes() ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Matches reports whether data still equals the backed-up content.
func (b FileBackup) Matches(data []byte) bool {
	return HashBytes(data) == b.SHA256
}

// Transaction ---------------------------------------------------------------

// LoadTransaction reads the pending transaction. os.ErrNotExist passes
// through when no transaction is open.
func LoadTransaction(root string) (*Transaction, error) {
	p := TransactionPath(root)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var t Transaction
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, envErr(p, "不是合法 JSON: %v", err)
	}
	if t.EnvVersion != EnvVersion {
		return nil, envErr(p, "不支持的记录版本 %d（本构建支持 %d）", t.EnvVersion, EnvVersion)
	}
	if !ValidOpID(t.OpID) {
		return nil, envErr(p, "op_id 缺失或格式非法")
	}
	if t.Kind != KindSwitch && t.Kind != KindRollback {
		return nil, envErr(p, "kind 非法: %q", t.Kind)
	}
	if _, ok := StageRank(t.Stage); !ok {
		return nil, envErr(p, "stage 非法: %q", t.Stage)
	}
	if t.Kind == KindRollback && t.New.ContainerID == "" {
		return nil, envErr(p, "rollback 事务缺少上一代容器 ID")
	}
	if t.Old.ContainerName == "" || t.New.ContainerName == "" {
		return nil, envErr(p, "old/new 快照不完整")
	}
	if t.ConfigBackup.SHA256 == "" || t.StateBackup.SHA256 == "" {
		return nil, envErr(p, "缺少配置/状态备份")
	}
	return &t, nil
}

// TransactionExists reports whether a pending-transaction file is present.
// A file that exists but cannot be parsed is still "present": callers must
// treat parse errors separately and never proceed over a damaged journal.
func TransactionExists(root string) bool {
	_, err := os.Stat(TransactionPath(root))
	return err == nil
}

// SaveTransaction atomically writes the transaction journal.
func SaveTransaction(root string, t *Transaction) error {
	t.EnvVersion = EnvVersion
	t.UpdatedAt = nowUTC()
	if t.CreatedAt == "" {
		t.CreatedAt = t.UpdatedAt
	}
	return writeJSON(TransactionPath(root), t)
}

// ClearTransaction removes the journal; a missing file is success (idempotent
// finalize).
func ClearTransaction(root string) error {
	err := os.Remove(TransactionPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Previous ------------------------------------------------------------------

// LoadPrevious reads the rollback slot. os.ErrNotExist passes through.
func LoadPrevious(root string) (*Previous, error) {
	p := PreviousPath(root)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var prev Previous
	if err := json.Unmarshal(raw, &prev); err != nil {
		return nil, envErr(p, "不是合法 JSON: %v", err)
	}
	if prev.EnvVersion != EnvVersion {
		return nil, envErr(p, "不支持的记录版本 %d（本构建支持 %d）", prev.EnvVersion, EnvVersion)
	}
	if !ValidOpID(prev.OpID) {
		return nil, envErr(p, "op_id 缺失或格式非法")
	}
	if prev.ContainerID == "" || prev.ContainerName == "" || prev.ImageID == "" || prev.ImageRef == "" {
		return nil, envErr(p, "上一代快照不完整")
	}
	return &prev, nil
}

// PreviousExists reports whether a rollback slot file is present.
func PreviousExists(root string) bool {
	_, err := os.Stat(PreviousPath(root))
	return err == nil
}

// SavePrevious atomically writes the rollback slot.
func SavePrevious(root string, prev *Previous) error {
	prev.EnvVersion = EnvVersion
	return writeJSON(PreviousPath(root), prev)
}

// RemovePrevious consumes the rollback slot; a missing file is success.
func RemovePrevious(root string) error {
	err := os.Remove(PreviousPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Retained ------------------------------------------------------------------

// LoadRetained reads the ledger. os.ErrNotExist passes through.
func LoadRetained(root string) (*Retained, error) {
	p := RetainedPath(root)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Retained
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, envErr(p, "不是合法 JSON: %v", err)
	}
	if r.EnvVersion != EnvVersion {
		return nil, envErr(p, "不支持的记录版本 %d（本构建支持 %d）", r.EnvVersion, EnvVersion)
	}
	seen := map[string]bool{}
	for _, e := range r.Containers {
		if e.ContainerID == "" || e.ContainerName == "" {
			return nil, envErr(p, "账本条目不完整")
		}
		if seen[e.ContainerID] {
			return nil, envErr(p, "账本存在重复容器 %s", e.ContainerID)
		}
		seen[e.ContainerID] = true
	}
	return &r, nil
}

// AppendRetained adds an entry unless its container ID is already recorded
// (idempotent). It writes through only when the ledger changed.
func AppendRetained(root string, entry RetainedEntry) (bool, error) {
	r, err := LoadRetained(root)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		r = &Retained{}
	}
	for _, e := range r.Containers {
		if e.ContainerID == entry.ContainerID {
			return false, nil
		}
	}
	entry.RetainedAt = nowUTC()
	r.Containers = append(r.Containers, entry)
	if err := SaveRetained(root, r); err != nil {
		return false, err
	}
	return true, nil
}

// SaveRetained atomically writes the ledger.
func SaveRetained(root string, r *Retained) error {
	r.EnvVersion = EnvVersion
	return writeJSON(RetainedPath(root), r)
}

// writers -------------------------------------------------------------------

func writeJSON(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

package project

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// State is the local-only project state under <root>/.km/state.json. It
// records the random project identity and the runtime identity this project
// was prepared with, so later commands always target the same container.
type State struct {
	StateVersion int    `json:"state_version"`
	ProjectID    string `json:"project_id"`
	Container    struct {
		// ID is the full immutable container ID recorded at init. All
		// ownership checks must go through it; the name is informational.
		ID      string `json:"id,omitempty"`
		Name    string `json:"name"`
		ImageID string `json:"image_id,omitempty"`
	} `json:"container"`
	Runtime struct {
		Context  string `json:"context"`
		Endpoint string `json:"endpoint"`
	} `json:"runtime"`
	CreatedAt string `json:"created_at"`

	// Env 存在当且仅当该项目已采纳环境切换功能（state_version 2）。
	// v1 文件不携带该块；旧构建只认 v1，因此 v2 状态本身就是旧二进制的
	// 明确拒绝门槛（见 docs/adr-environment-transactions.md §3）。
	Env *EnvState `json:"env,omitempty"`
}

// EnvState is the env extension in state_version 2: the adopted-record
// format version and the current generation number (0 = 原始代).
type EnvState struct {
	EnvVersion int `json:"env_version"`
	Generation int `json:"generation"`
}

// SupportedStateVersion is the legacy state format every build reads.
const SupportedStateVersion = 1

// SupportedStateVersionEnv is the state format that carries the env block.
// Projects adopt it permanently on their first environment switch; older
// binaries reject it outright instead of misusing mid-transaction state.
const SupportedStateVersionEnv = 2

// EnvRecordVersion is the only env-block format this build reads.
const EnvRecordVersion = 1

// StateError marks a damaged or incompatible local state file.
type StateError struct{ Msg string }

func (e *StateError) Error() string { return ".km/state.json: " + e.Msg }

// NewProjectID returns a fresh random project id like "p1a2b3c4d5".
func NewProjectID() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成项目 ID 失败: %w", err)
	}
	return "p" + hex.EncodeToString(buf), nil
}

// StatePath returns the state file path inside the project root.
func StatePath(root string) string { return filepath.Join(root, StateDirName, "state.json") }

// ContainerIDPattern is the exact format of a Docker container ID: 64
// lowercase hex chars. An empty ID is tolerated only as legacy state; all
// ownership checks must refuse to adopt by name in that case.
var ContainerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadState reads and validates local state. os.ErrNotExist passes through
// when the project has no state yet.
func LoadState(root string) (*State, error) {
	raw, err := os.ReadFile(StatePath(root))
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, &StateError{Msg: "不是合法 JSON: " + err.Error()}
	}
	if st.StateVersion == SupportedStateVersion {
		if st.Env != nil {
			return nil, &StateError{Msg: "state_version 1 不应包含 env 块"}
		}
	} else if st.StateVersion == SupportedStateVersionEnv {
		if st.Env == nil {
			return nil, &StateError{Msg: "state_version 2 缺少 env 块"}
		}
		if st.Env.EnvVersion != EnvRecordVersion {
			return nil, &StateError{Msg: fmt.Sprintf("不支持的 env 记录版本 %d（本构建支持 %d）", st.Env.EnvVersion, EnvRecordVersion)}
		}
		if st.Env.Generation < 0 {
			return nil, &StateError{Msg: "env.generation 非法（负数）"}
		}
	} else {
		return nil, &StateError{Msg: fmt.Sprintf("不支持的状态版本 %d（本构建支持 %d；若由旧版 km 写入，请升级 km 后重试）", st.StateVersion, SupportedStateVersionEnv)}
	}
	if len(st.ProjectID) < 3 {
		return nil, &StateError{Msg: "project_id 缺失或过短"}
	}
	if st.Container.Name == "" {
		return nil, &StateError{Msg: "container.name 缺失"}
	}
	if st.Container.ID != "" && !ContainerIDPattern.MatchString(st.Container.ID) {
		return nil, &StateError{Msg: "container.id 不是合法的 64 位十六进制容器 ID"}
	}
	return &st, nil
}

// SaveState atomically writes state.json (temp file + rename). Callers own
// backing up anything they care about; this only ever creates .km/.
func SaveState(root string, st *State) error {
	dir := filepath.Join(root, StateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, "state.json.tmp-*")
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
	return os.Rename(tmp.Name(), StatePath(root))
}

// StateExists reports whether the project root already has local state.
func StateExists(root string) bool {
	_, err := os.Stat(StatePath(root))
	return err == nil
}

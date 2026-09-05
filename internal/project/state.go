package project

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is the local-only project state under <root>/.km/state.json. It
// records the random project identity and the runtime identity this project
// was prepared with, so later commands always target the same container.
type State struct {
	StateVersion int    `json:"state_version"`
	ProjectID    string `json:"project_id"`
	Container    struct {
		Name    string `json:"name"`
		ImageID string `json:"image_id,omitempty"`
	} `json:"container"`
	Runtime struct {
		Context  string `json:"context"`
		Endpoint string `json:"endpoint"`
	} `json:"runtime"`
	CreatedAt string `json:"created_at"`
}

// SupportedStateVersion is the only state format this build reads.
const SupportedStateVersion = 1

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
	if st.StateVersion != SupportedStateVersion {
		return nil, &StateError{Msg: fmt.Sprintf("不支持的状态版本 %d", st.StateVersion)}
	}
	if len(st.ProjectID) < 3 {
		return nil, &StateError{Msg: "project_id 缺失或过短"}
	}
	if st.Container.Name == "" {
		return nil, &StateError{Msg: "container.name 缺失"}
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

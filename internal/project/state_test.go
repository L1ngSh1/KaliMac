package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleState() *State {
	st := &State{StateVersion: SupportedStateVersion, ProjectID: "p1a2b3c4d5", CreatedAt: "2026-09-06T00:00:00Z"}
	st.Container.Name = ContainerNameFor(st.ProjectID)
	st.Runtime.Context = "desktop-linux"
	st.Runtime.Endpoint = "unix:///Users/x/.docker/run/docker.sock"
	return st
}

// ContainerNameFor mirrors runtime.ContainerName without an import cycle in
// tests; both must stay "km-" + projectID.
func ContainerNameFor(id string) string { return "km-" + id }

func TestNewProjectIDShape(t *testing.T) {
	id, err := NewProjectID()
	if err != nil {
		t.Fatalf("NewProjectID: %v", err)
	}
	if !strings.HasPrefix(id, "p") || len(id) != 11 {
		t.Fatalf("ID 形状不正确: %q", id)
	}
	other, _ := NewProjectID()
	if other == id {
		t.Fatalf("ID 应随机: %q == %q", id, other)
	}
}

func TestStateRoundtrip(t *testing.T) {
	root := t.TempDir()
	st := sampleState()
	if err := SaveState(root, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(root)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.ProjectID != st.ProjectID || got.Container.Name != st.Container.Name || got.Runtime.Context != "desktop-linux" {
		t.Fatalf("往返不一致: %+v", got)
	}
	if !StateExists(root) {
		t.Fatal("StateExists 应为 true")
	}
}

func TestStateMissing(t *testing.T) {
	root := t.TempDir()
	_, err := LoadState(root)
	if !os.IsNotExist(err) {
		t.Fatalf("无状态应返回 ErrNotExist, got %v", err)
	}
}

func TestStateCorrupt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState(root)
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("损坏状态应返回 StateError, got %v", err)
	}
}

func TestStateUnknownVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(StatePath(root), []byte(`{"state_version":42,"project_id":"p1","container":{"name":"km-p1"}}`), 0o644)
	_, err := LoadState(root)
	var se *StateError
	if !errors.As(err, &se) || !strings.Contains(se.Error(), "42") {
		t.Fatalf("未知状态版本应报错: %v", err)
	}
}

func TestStateMissingContainerName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(StatePath(root), []byte(`{"state_version":1,"project_id":"p1a2b3c4d5"}`), 0o644)
	_, err := LoadState(root)
	var se *StateError
	if !errors.As(err, &se) || !strings.Contains(se.Error(), "container.name") {
		t.Fatalf("缺 container.name 应报错: %v", err)
	}
}

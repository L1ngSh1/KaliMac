package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStateFile(t *testing.T, root, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(root), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validV1 = `{"state_version":1,"project_id":"p1a2b3c4d5","container":{"name":"km-p1a2b3c4d5"},"runtime":{},"created_at":"2026-01-01T00:00:00Z"}`

const validV2 = `{"state_version":2,"project_id":"p1a2b3c4d5","container":{"name":"km-p1a2b3c4d5"},"runtime":{},"created_at":"2026-01-01T00:00:00Z","env":{"env_version":1,"generation":2}}`

func TestLoadStateV1StillSupported(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, validV1)
	st, err := LoadState(root)
	if err != nil {
		t.Fatalf("v1 状态必须继续可用: %v", err)
	}
	if st.Env != nil {
		t.Fatal("v1 状态不应有 env 块")
	}
}

func TestLoadStateV2WithEnvBlock(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, validV2)
	st, err := LoadState(root)
	if err != nil {
		t.Fatalf("v2 状态应可读取: %v", err)
	}
	if st.Env == nil || st.Env.EnvVersion != EnvRecordVersion || st.Env.Generation != 2 {
		t.Fatalf("env 块解析不一致: %+v", st.Env)
	}
}

func TestLoadStateV2WithoutEnvBlockRejected(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, strings.Replace(validV2, `,"env":{"env_version":1,"generation":2}`, "", 1))
	raw, _ := os.ReadFile(StatePath(root))
	if strings.Contains(string(raw), "env") {
		t.Fatalf("测试前提不成立: %s", raw)
	}
	if _, err := LoadState(root); err == nil || !strings.Contains(err.Error(), "缺少 env 块") {
		t.Fatalf("v2 缺 env 块应被拒绝: %v", err)
	}
}

func TestLoadStateV1WithEnvBlockRejected(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, strings.Replace(validV1, `{"state_version":1,`, `{"state_version":1,"env":{"env_version":1,"generation":0},`, 1))
	if _, err := LoadState(root); err == nil || !strings.Contains(err.Error(), "不应包含 env") {
		t.Fatalf("v1 带 env 块应被拒绝: %v", err)
	}
}

// TestLoadStateRejectsFutureVersion 是旧二进制兼容合同的锚点：任何高于本构建
// 支持版本的状态都必须被明确拒绝，绝不允许猜测修复或静默继续。
func TestLoadStateRejectsFutureVersion(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, strings.Replace(validV2, `"state_version":2`, `"state_version":3`, 1))
	_, err := LoadState(root)
	if err == nil {
		t.Fatal("未来版本应被拒绝")
	}
	if !strings.Contains(err.Error(), "不支持的状态版本 3") {
		t.Fatalf("错误消息应包含版本信息: %v", err)
	}
}

func TestSaveLoadStateV2RoundTrip(t *testing.T) {
	root := t.TempDir()
	var st State
	if err := json.Unmarshal([]byte(validV2), &st); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(root, &st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(root)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	if got.StateVersion != SupportedStateVersionEnv || got.Env.Generation != 2 {
		t.Fatalf("v2 往返不一致: %+v", got)
	}
}

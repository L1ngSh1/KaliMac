package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":1,"image":"kalilinux/kali-rolling:latest","name":"demo"}`)
	cfg, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Image != "kalilinux/kali-rolling:latest" || cfg.Name != "demo" {
		t.Fatalf("字段不正确: %+v", cfg)
	}
	if cfg.Platform != DefaultPlatform {
		t.Fatalf("platform 应有默认值 %q, got %q", DefaultPlatform, cfg.Platform)
	}
}

func TestLoadConfigMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":1, "image":`)
	_, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 ConfigError, got %v", err)
	}
}

func TestLoadConfigUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":99,"image":"x"}`)
	_, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Field != "schema_version" {
		t.Fatalf("期望指出 schema_version, got %v", err)
	}
}

func TestLoadConfigUnknownField(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":1,"image":"x","evil_hook":"rm -rf /"}`)
	_, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	var ce *ConfigError
	if !errors.As(err, &ce) || !strings.Contains(ce.Field, "evil_hook") {
		t.Fatalf("期望指出未知字段, got %v", err)
	}
}

func TestLoadConfigMissingImage(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":1}`)
	_, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Field != "image" {
		t.Fatalf("期望指出 image, got %v", err)
	}
}

func TestLoadConfigWrongType(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"schema_version":"one","image":"x"}`)
	_, err := LoadConfig(filepath.Join(dir, ConfigFileName))
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 ConfigError, got %v", err)
	}
}

// LoadConfig 是只读的：无论配置多坏，原文件必须原样保留。
func TestLoadConfigNeverWrites(t *testing.T) {
	dir := t.TempDir()
	content := `{"schema_version":99}`
	writeConfig(t, dir, content)
	_, _ = LoadConfig(filepath.Join(dir, ConfigFileName))
	raw, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if string(raw) != content {
		t.Fatalf("配置文件被改动: %q", raw)
	}
}

func TestFindConfigNearestRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "中文 目录", "sub one")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, `{"schema_version":1,"image":"x"}`)
	got, ok, err := FindConfig(sub)
	if err != nil || !ok || got != root {
		t.Fatalf("got root=%q ok=%v err=%v", got, ok, err)
	}
}

func TestFindConfigNone(t *testing.T) {
	dir := t.TempDir()
	_, ok, err := FindConfig(dir)
	if err != nil || ok {
		t.Fatalf("无配置应 (\"\", false, nil), got ok=%v err=%v", ok, err)
	}
}

func TestParentProjectExcludesSelf(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"schema_version":1,"image":"x"}`)
	if _, ok, _ := ParentProject(root); ok {
		t.Fatal("项目根本身不应被当作父项目")
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ParentProject(sub)
	if err != nil || !ok || got != root {
		t.Fatalf("子目录应找到父项目: root=%q ok=%v err=%v", got, ok, err)
	}
}

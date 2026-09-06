// Package project handles the declarative project config (.km.json) and the
// local-only state directory (.km/).
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFileName is the shareable declarative project config.
const ConfigFileName = ".km.json"

// StateDirName is the local-only state directory; it must never be committed.
const StateDirName = ".km"

// Config mirrors .km.json. Only declarative fields live here; nothing
// executable, nothing machine-specific.
type Config struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name,omitempty"`
	Image         string `json:"image"`
	Platform      string `json:"platform,omitempty"`
}

// SupportedSchemaVersion is the only schema this build understands.
const SupportedSchemaVersion = 1

// DefaultImage is used by init (P2); declared here so validation and init
// agree on one value.
const DefaultImage = "kalilinux/kali-rolling:latest"

// DefaultPlatform is the first supported verification platform.
const DefaultPlatform = "linux/arm64"

// ConfigError points at the offending field while keeping the original file
// untouched.
type ConfigError struct {
	Field string
	Msg   string
}

func (e *ConfigError) Error() string { return fmt.Sprintf(".km.json 字段 %q: %s", e.Field, e.Msg) }

// LoadConfig reads and validates the config at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, &ConfigError{Field: "(整体)", Msg: "不是合法 JSON: " + err.Error()}
	}
	allowed := map[string]bool{"schema_version": true, "name": true, "image": true, "platform": true}
	var unknown []string
	for k := range cfg {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, &ConfigError{Field: strings.Join(unknown, ", "), Msg: "未知字段（本版本不支持）"}
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		// map JSON type errors back to a field name when possible
		field := "(整体)"
		if te, ok := err.(*json.UnmarshalTypeError); ok {
			field = te.Field
		}
		return nil, &ConfigError{Field: field, Msg: "字段类型错误: " + err.Error()}
	}
	if c.SchemaVersion == 0 {
		return nil, &ConfigError{Field: "schema_version", Msg: "缺失"}
	}
	if c.SchemaVersion != SupportedSchemaVersion {
		return nil, &ConfigError{Field: "schema_version", Msg: fmt.Sprintf("不支持的版本 %d（本构建支持 %d）", c.SchemaVersion, SupportedSchemaVersion)}
	}
	if strings.TrimSpace(c.Image) == "" {
		return nil, &ConfigError{Field: "image", Msg: "缺失或为空"}
	}
	if strings.ContainsAny(c.Image, " \t\n") {
		return nil, &ConfigError{Field: "image", Msg: "不能包含空白字符"}
	}
	if c.Platform == "" {
		c.Platform = DefaultPlatform
	}
	return &c, nil
}

// FindConfig walks up from startDir and returns the nearest directory that
// contains .km.json. ok=false when no project root is found.
func FindConfig(startDir string) (root string, ok bool, err error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false, err
	}
	for {
		path := filepath.Join(dir, ConfigFileName)
		if st, statErr := os.Stat(path); statErr == nil && !st.IsDir() {
			return dir, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

// ParentProject reports the nearest project root at or above startDir,
// excluding startDir itself. Used by init to refuse nesting explicitly.
func ParentProject(startDir string) (root string, ok bool, err error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false, err
	}
	parent := filepath.Dir(dir)
	return FindConfig(parent)
}

// WriteConfig writes a new .km.json atomically (temp file + rename). It is
// only used by init for a MISSING config; existing files are never silently
// overwritten.
func WriteConfig(path string, cfg *Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".km.json.tmp-*")
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
	return os.Rename(tmp.Name(), path)
}

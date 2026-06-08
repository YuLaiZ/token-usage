package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigTemplate_ContainsAllSections(t *testing.T) {
	template := DefaultConfigTemplate()

	required := []string{
		"data_dir",
		"[clients.claude]",
		"[clients.opencode]",
		"[clients.codex]",
		"[clients.workbuddy]",
		"[clients.zcode]",
		"[clients.autoclaw]",
		"[routers.cc_switch]",
		"[daemon]",
		"[log]",
		"[provider_aliases]",
	}
	for _, section := range required {
		if !strings.Contains(template, section) {
			t.Errorf("template missing section %q", section)
		}
	}
}

func TestDefaultConfigTemplate_RouterUsesTableNameAsType(t *testing.T) {
	template := DefaultConfigTemplate()

	if strings.Contains(template, `type = "cc_switch"`) {
		t.Error("router config should not include deprecated type field")
	}
}

func TestWriteDefaultConfig_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.toml")

	err := WriteDefaultConfig(cfgPath)
	if err != nil {
		t.Fatalf("WriteDefaultConfig failed: %v", err)
	}

	content, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}

	if !strings.Contains(string(content), "token-usage 配置文件") {
		t.Error("config missing header comment")
	}
}

func TestDefaultConfig_CanBeLoaded(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.toml")

	err := WriteDefaultConfig(cfgPath)
	if err != nil {
		t.Fatalf("WriteDefaultConfig failed: %v", err)
	}

	// 默认模板已显式写入 poll_interval=30 / level=info / max_days=7，
	// LoadUserConfig 直接读取这些字面值（不再 applyDefaults，effective 解析在 runtimecfg）。
	cfg, err := LoadUserConfig(cfgPath)
	if err != nil {
		t.Fatalf("default config should be loadable: %v", err)
	}

	if cfg.Daemon.PollInterval != 30 {
		t.Errorf("Daemon.PollInterval = %d, want 30", cfg.Daemon.PollInterval)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "info")
	}
	if cfg.Log.MaxDays != 7 {
		t.Errorf("Log.MaxDays = %d, want 7", cfg.Log.MaxDays)
	}
}

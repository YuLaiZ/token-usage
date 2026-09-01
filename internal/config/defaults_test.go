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

// 默认模板所有客户端关闭、无默认 router、无默认 provider 映射：
// 用户按需逐个开启，避免新装即全量采集与带出示例映射。
func TestDefaultConfigTemplate_AllClientsDisabledByDefault(t *testing.T) {
	template := DefaultConfigTemplate()
	for _, name := range []string{"claude", "opencode", "codex", "workbuddy", "zcode", "autoclaw"} {
		section := "[" + "clients." + name + "]"
		idx := strings.Index(template, section)
		if idx < 0 {
			t.Fatalf("template missing section %q", section)
		}
		rest := template[idx+len(section):]
		next := strings.Index(rest, "[")
		if next < 0 {
			next = len(rest)
		}
		body := rest[:next]
		if strings.Contains(body, "enabled = true") {
			t.Errorf("client %q 默认应为 enabled = false", name)
		}
		if !strings.Contains(body, "enabled = false") {
			t.Errorf("client %q 默认应显式 enabled = false", name)
		}
	}
	// 只检查非注释行：注释里的示例配置（router / provider 映射）允许存在。
	effectiveLine := func(line string) bool {
		trimmed := strings.TrimSpace(line)
		return trimmed != "" && !strings.HasPrefix(trimmed, "#")
	}
	for _, line := range strings.Split(template, "\n") {
		if !effectiveLine(line) {
			continue
		}
		if strings.Contains(line, "router = ") {
			t.Errorf("默认模板不应带生效的 router 配置（只允许注释示例）: %s", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "\"") && strings.Contains(line, " = ") {
			t.Errorf("默认模板不应带默认 provider 映射（只允许注释示例）: %s", line)
		}
	}
}

// 默认模板只含 query 注释示例,不写生效 query 段:
// 解析后两个 raw 载体均为 nil,旧版本二进制可安全读取(降级兼容)。
func TestDefaultConfigTemplate_QuerySectionCommentOnly(t *testing.T) {
	template := DefaultConfigTemplate()
	for _, want := range []string{"[query]", "subqueries", "groups", "default", "[query.output]", "columns", "cache_create"} {
		if !strings.Contains(template, want) {
			t.Errorf("默认模板应含 query 注释示例(含 %q):\n%s", want, template)
		}
	}
	cfg, err := ParseUserConfig([]byte(template))
	if err != nil {
		t.Fatalf("默认模板必须可解析: %v", err)
	}
	if cfg.RawQuery != nil || cfg.RawQueryTopLevelIssues != nil {
		t.Errorf("默认模板不得写生效 query 段: %#v / %#v", cfg.RawQuery, cfg.RawQueryTopLevelIssues)
	}
	// 注释行必须是注释形态(不以生效键写出)。
	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "default =") || strings.HasPrefix(trimmed, "mpc =") || strings.HasPrefix(trimmed, "columns =") {
			t.Errorf("模板不得包含生效的 query 键: %q", line)
		}
	}
}

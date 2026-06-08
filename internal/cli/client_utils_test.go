package cli

import (
	"sort"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
)

// TestEnabledClientNames_Sorted 反字典序插入，断言返回仍为字典序。
func TestEnabledClientNames_Sorted(t *testing.T) {
	// 反字典序插入（zcode → claude），验证返回仍字典序
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"zcode":     {Enabled: true},
			"workbuddy": {Enabled: true},
			"opencode":  {Enabled: true},
			"codex":     {Enabled: true},
			"claude":    {Enabled: true},
			"autoclaw":  {Enabled: true},
		},
	}
	names := enabledClientNames(cfg)

	expected := []string{"autoclaw", "claude", "codex", "opencode", "workbuddy", "zcode"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d names, got %d: %v", len(expected), len(names), names)
	}
	for i, n := range names {
		if n != expected[i] {
			t.Errorf("names[%d] = %q, want %q（应字典序排序）", i, n, expected[i])
		}
	}
	// 双保险：sort.StringsAreSorted 防止未来新增 client 名破坏字典序
	if !sort.StringsAreSorted(names) {
		t.Errorf("names 未按字典序排序: %v", names)
	}
}

// TestEnabledClientNames_ExcludesDisabled enabled=false 的客户端不包含。
func TestEnabledClientNames_ExcludesDisabled(t *testing.T) {
	cfg := &config.Config{
		Clients: map[string]config.Client{
			"claude":   {Enabled: true},
			"opencode": {Enabled: false},
		},
	}
	names := enabledClientNames(cfg)
	if len(names) != 1 || names[0] != "claude" {
		t.Errorf("期望仅 [claude]，实际 %v", names)
	}
}

// TestEnabledClientNames_EmptyConfig 空 cfg.Clients 返回空切片。
func TestEnabledClientNames_EmptyConfig(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{}}
	names := enabledClientNames(cfg)
	if len(names) != 0 {
		t.Errorf("期望空切片，实际 %v", names)
	}
}

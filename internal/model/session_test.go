// internal/model/session_test.go
package model

import (
	"sort"
	"testing"
)

func TestRawClientToClient_Mapping(t *testing.T) {
	tests := []struct {
		raw      string
		expected string
	}{
		{RawClientClaudeCode, ClientClaudeCode},
		{RawClientClaudeDesktop, ClientClaudeDesktop},
		{RawClientOpenCode, ClientOpenCode},
		{RawClientCodexCLI, ClientCodexCLI},
		{RawClientCodexApp, ClientCodexApp},
		{RawClientWorkBuddy, ClientWorkBuddy},
		{RawClientZhipuAutoClaw, ClientZhipuAutoClaw},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := RawClientToClient[tt.raw]
			if !ok {
				t.Errorf("RawClientToClient[%q] not found", tt.raw)
			}
			if got != tt.expected {
				t.Errorf("RawClientToClient[%q] = %q, want %q", tt.raw, got, tt.expected)
			}
		})
	}
}

func TestRawClientToClient_ZCode(t *testing.T) {
	got, ok := RawClientToClient[RawClientZCode]
	if !ok {
		t.Fatalf("RawClientToClient[%q] not found", RawClientZCode)
	}
	if got != ClientZCode {
		t.Errorf("RawClientToClient[%q] = %q, want %q", RawClientZCode, got, ClientZCode)
	}
	if RawClientZCode != "zcode" {
		t.Errorf("RawClientZCode = %q, want %q", RawClientZCode, "zcode")
	}
	if ClientZCode != "ZCode" {
		t.Errorf("ClientZCode = %q, want %q", ClientZCode, "ZCode")
	}
}

func TestRawClientToClient_AutoClaw(t *testing.T) {
	got, ok := RawClientToClient[RawClientZhipuAutoClaw]
	if !ok {
		t.Fatalf("RawClientToClient[%q] not found", RawClientZhipuAutoClaw)
	}
	if got != ClientZhipuAutoClaw {
		t.Errorf("RawClientToClient[%q] = %q, want %q", RawClientZhipuAutoClaw, got, ClientZhipuAutoClaw)
	}
	if RawClientZhipuAutoClaw != "zhipu_autoclaw" {
		t.Errorf("RawClientZhipuAutoClaw = %q, want %q", RawClientZhipuAutoClaw, "zhipu_autoclaw")
	}
	if ClientZhipuAutoClaw != "Zhipu-AutoClaw" {
		t.Errorf("ClientZhipuAutoClaw = %q, want %q", ClientZhipuAutoClaw, "Zhipu-AutoClaw")
	}
}

func TestSubtractCache(t *testing.T) {
	tests := []struct {
		name                     string
		input, cacheRead, create int64
		want                     int64
	}{
		{"normal", 1000, 300, 100, 600},
		{"equal", 400, 300, 100, 0},
		{"clamp", 100, 90, 20, 0},
		{"no cache", 1000, 0, 0, 1000},
		{"cache only", 1000, 600, 0, 400},
		{"zero input with cache", 0, 50, 50, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubtractCache(tt.input, tt.cacheRead, tt.create); got != tt.want {
				t.Fatalf("SubtractCache() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestClientToDisplayNames_DRYGuard 防止新增 client 时漏更新 ClientToDisplayNames。
// RawClientToClient 的所有 value（显示名）必须出现在 ClientToDisplayNames 的某个列表中。
func TestClientToDisplayNames_DRYGuard(t *testing.T) {
	// 收集 ClientToDisplayNames 所有已登记的显示名
	registered := make(map[string]bool)
	for _, names := range ClientToDisplayNames {
		for _, n := range names {
			registered[n] = true
		}
	}

	// RawClientToClient 的每个 value 都应被登记
	for _, displayName := range RawClientToClient {
		if !registered[displayName] {
			t.Errorf("显示名 %q 存在于 RawClientToClient 但未登记到 ClientToDisplayNames，"+
				"新增 client 时需同步更新两处映射", displayName)
		}
	}
}

// TestClientToDisplayNames_ClaudeMultiMapping 验证 claude 配置 key 一对多映射。
func TestClientToDisplayNames_ClaudeMultiMapping(t *testing.T) {
	names, ok := ClientToDisplayNames["claude"]
	if !ok {
		t.Fatal("ClientToDisplayNames 缺少 claude 配置 key")
	}
	if len(names) != 2 {
		t.Errorf("claude 应映射到 2 个显示名（Claude Code + Claude Desktop），实际 %d: %v", len(names), names)
	}
	sort.Strings(names)
	if names[0] != ClientClaudeCode || names[1] != ClientClaudeDesktop {
		t.Errorf("claude 映射应为 [Claude Code, Claude Desktop]，实际 %v", names)
	}
}

// TestClientToDisplayNames_AllConfigKeys 验证 6 个配置 key 全部登记。
func TestClientToDisplayNames_AllConfigKeys(t *testing.T) {
	expected := []string{"claude", "opencode", "codex", "workbuddy", "zcode", "autoclaw"}
	for _, key := range expected {
		if _, ok := ClientToDisplayNames[key]; !ok {
			t.Errorf("ClientToDisplayNames 缺少配置 key %q", key)
		}
	}
}

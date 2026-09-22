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
		{RawClientMimoCode, ClientMiMoCode},
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

// TestRawClientToClient_MimoCode 固定 raw client 字符串值与正式 client 名。
// 配置 key/raw 值/正式名三者刻意不同（mimocode / "mimocode" / "MiMo Code"），
// 防止后续重构漂移；Desktop 与 CLI 虽共库，但按 session.version 分派为两个
// 正式 client。LegacyClientXiaomiMiMoCode 仅供 migration 与旧版回写兼容
// trigger 使用，不是正式 client。
func TestRawClientToClient_MimoCode(t *testing.T) {
	got, ok := RawClientToClient[RawClientMimoCode]
	if !ok {
		t.Fatalf("RawClientToClient[%q] not found", RawClientMimoCode)
	}
	if got != ClientMiMoCode {
		t.Errorf("RawClientToClient[%q] = %q, want %q", RawClientMimoCode, got, ClientMiMoCode)
	}
	if RawClientMimoCode != "mimocode" {
		t.Errorf("RawClientMimoCode = %q, want %q", RawClientMimoCode, "mimocode")
	}
	if ClientMiMoCode != "MiMo Code" {
		t.Errorf("ClientMiMoCode = %q, want %q", ClientMiMoCode, "MiMo Code")
	}
	if LegacyClientXiaomiMiMoCode != "Xiaomi MiMo / MiMo Code" {
		t.Errorf("LegacyClientXiaomiMiMoCode = %q, want %q", LegacyClientXiaomiMiMoCode, "Xiaomi MiMo / MiMo Code")
	}
	if ClientMiMoCode == LegacyClientXiaomiMiMoCode {
		t.Error("正式名与 legacy 名不得相等")
	}
}

// TestClientToDisplayNames_MimoCode：mimocode 的查询过滤名必须包含两个正式
// 落库名（router backfill 等按显示名查 messages 的路径据此命中）。
func TestClientToDisplayNames_MimoCode(t *testing.T) {
	names, ok := ClientToDisplayNames["mimocode"]
	if !ok {
		t.Fatal("ClientToDisplayNames 缺少 mimocode 配置 key")
	}
	if len(names) != 2 || names[0] != ClientMiMoCode || names[1] != ClientMiMoDesktop {
		t.Errorf("ClientToDisplayNames[mimocode] = %v, want [%q %q]", names, ClientMiMoCode, ClientMiMoDesktop)
	}
}

// TestClientDisplayName 是防御性历史兼容兜底：legacy 长名只在异常残留
// （未迁移库被直接查询等）时出现，渲染为正式名；其余 client 恒等返回。
func TestClientDisplayName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{LegacyClientXiaomiMiMoCode, ClientMiMoCode},
		{ClientMiMoCode, ClientMiMoCode},
		{ClientClaudeCode, ClientClaudeCode},
		{ClientZCode, ClientZCode},
		{"", ""},
		{"unknown", "unknown"},
	}
	for _, c := range cases {
		if got := ClientDisplayName(c.in); got != c.want {
			t.Errorf("ClientDisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
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

// TestClientToDisplayNames_AllConfigKeys 验证 7 个配置 key 全部登记。
func TestClientToDisplayNames_AllConfigKeys(t *testing.T) {
	expected := []string{"claude", "opencode", "codex", "workbuddy", "zcode", "autoclaw", "mimocode"}
	for _, key := range expected {
		if _, ok := ClientToDisplayNames[key]; !ok {
			t.Errorf("ClientToDisplayNames 缺少配置 key %q", key)
		}
	}
}

// TestMiMoSessionClient：终裁判别矩阵——严格 desktop-<hash> 归 Desktop，
// 其余一切形态归 MiMo Code；provider/model 不参与（不在函数输入内）。
func TestMiMoSessionClient(t *testing.T) {
	cases := []struct {
		version, want string
	}{
		{"desktop-bdfe497", ClientMiMoDesktop},
		{"desktop-1d6a9fe", ClientMiMoDesktop},
		{"desktop-0123456789abcdef", ClientMiMoDesktop},
		{"0.1.14", ClientMiMoCode},
		{"2.1.156", ClientMiMoCode},
		{"2.1.278", ClientMiMoCode},
		{"", ClientMiMoCode},
		{"9.9.9-unknown", ClientMiMoCode},
		{"desktop-", ClientMiMoCode},
		{"desktop-xxx", ClientMiMoCode},     // 非 hex
		{"desktop-ABC123", ClientMiMoCode},  // 大写非 [0-9a-f]
		{"Desktop-bdfe497", ClientMiMoCode}, // 前缀大小写敏感
		{"desktop-bdfe497 ", ClientMiMoCode},
	}
	for _, c := range cases {
		if got := MiMoSessionClient(c.version); got != c.want {
			t.Errorf("MiMoSessionClient(%q) = %q, want %q", c.version, got, c.want)
		}
	}
	if ClientMiMoDesktop != "MiMo Desktop" {
		t.Errorf("ClientMiMoDesktop = %q", ClientMiMoDesktop)
	}
}

package config

import (
	"testing"
)

// 注意：原 LoadFrom 的默认值/路径展开/负值 clamp 行为已迁移到 internal/runtimecfg
// （ResolveEffectiveConfig / LoadEffectiveConfig），相关测试见 runtimecfg 包。
// raw config 层不再做 effective 解析（Load/LoadFrom 已删除），此处仅保留结构体方法测试。

func TestClientConfig_Exists(t *testing.T) {
	cfg := &Config{
		Clients: map[string]Client{
			"claude": {Enabled: true, Router: "cc_switch"},
		},
	}

	client, ok := cfg.ClientConfig("claude")
	if !ok {
		t.Fatal("expected claude to exist")
	}
	if !client.Enabled {
		t.Error("expected client to be enabled")
	}
}

func TestClientConfig_NotExists(t *testing.T) {
	cfg := &Config{
		Clients: map[string]Client{},
	}

	_, ok := cfg.ClientConfig("nonexistent")
	if ok {
		t.Error("expected nonexistent client to not exist")
	}
}

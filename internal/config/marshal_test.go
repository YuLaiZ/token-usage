package config

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// 验证 toml tag 策略:Client.Enabled 无 omitempty(禁用 client 保留 enabled=false);
// Daemon/Log 全零时整段 omit;nil map 不输出对应段;DataDir 总输出。
func TestMarshalTagStrategy(t *testing.T) {
	cfg := &Config{
		DataDir: "/tmp/data",
		Clients: map[string]Client{
			"codex": {Enabled: false},
		},
	}
	out, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "[clients.codex]") {
		t.Errorf("禁用 client 应输出段头 [clients.codex]:\n%s", s)
	}
	if !strings.Contains(s, "enabled = false") {
		t.Errorf("Client.Enabled 无 omitempty,应输出 enabled = false:\n%s", s)
	}
	if strings.Contains(s, "[daemon]") {
		t.Errorf("Daemon 全零应 omit [daemon]:\n%s", s)
	}
	if strings.Contains(s, "[log]") {
		t.Errorf("Log 全零应 omit [log]:\n%s", s)
	}
	if !strings.Contains(s, "data_dir") {
		t.Errorf("DataDir 应总输出:\n%s", s)
	}
}

func TestMarshalTagStrategy_EmptyMapsOmitted(t *testing.T) {
	cfg := &Config{DataDir: "/tmp/data"}
	out, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, seg := range []string{"[clients", "[routers", "[provider_aliases"} {
		if strings.Contains(s, seg) {
			t.Errorf("nil map 应 omit %s:\n%s", seg, s)
		}
	}
}

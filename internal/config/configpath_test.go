package config

import "testing"

// TestConfigPath_DerivesFromHome 纯函数：固定 home 时路径唯一确定，不读 os.UserHomeDir。
func TestConfigPath_DerivesFromHome(t *testing.T) {
	tests := []struct {
		home, want string
	}{
		{"/tmp/h", "/tmp/h/.token-usage/config.toml"},
		{"/Users/alice", "/Users/alice/.token-usage/config.toml"},
		{"", ".token-usage/config.toml"},
		{"rel", "rel/.token-usage/config.toml"},
	}
	for _, tt := range tests {
		got := ConfigPath(tt.home)
		if got != tt.want {
			t.Errorf("ConfigPath(%q) = %q, want %q", tt.home, got, tt.want)
		}
	}
}

// TestDefaultConfigPath_DelegatesToConfigPath DefaultConfigPath 对同一 home 返回与 ConfigPath 一致的结果。
func TestDefaultConfigPath_DelegatesToConfigPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/bootstrap-home")
	t.Setenv("USERPROFILE", "/tmp/bootstrap-home")
	p, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	want := ConfigPath("/tmp/bootstrap-home")
	if p != want {
		t.Errorf("DefaultConfigPath = %q, ConfigPath(home) = %q（应一致）", p, want)
	}
}

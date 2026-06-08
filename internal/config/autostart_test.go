package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonConfig_AutoStartRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := &Config{
		DataDir: "/x",
		Daemon:  DaemonConfig{PollInterval: 30, AutoStart: true},
	}
	if err := WriteUserConfigAtomic(path, src); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Daemon.AutoStart {
		t.Errorf("AutoStart 往返后应为 true，实际 %v", got.Daemon.AutoStart)
	}
}

// autostart=false 也应显式写入（不加 omitempty）
func TestDaemonConfig_AutoStartFalseWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := &Config{
		DataDir: "/x",
		Daemon:  DaemonConfig{PollInterval: 30, AutoStart: false},
	}
	if err := WriteUserConfigAtomic(path, src); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "autostart") {
		t.Errorf("autostart=false 应显式写入文件，实际内容:\n%s", string(data))
	}
}

func TestDefaultTemplate_ContainsAutoStart(t *testing.T) {
	if !strings.Contains(defaultConfigTemplate, "autostart") {
		t.Error("默认模板应含 autostart 字段")
	}
}

func TestSetGet_AutoStart(t *testing.T) {
	cfg := &Config{DataDir: "/x", Daemon: DaemonConfig{}}
	if err := Set(cfg, "daemon.autostart", "true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !cfg.Daemon.AutoStart {
		t.Error("Set autostart true 后 AutoStart 应为 true")
	}
	got, err := Get(cfg, "daemon.autostart")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "true" {
		t.Errorf("Get autostart = %q want true", got)
	}
}

func TestSetGet_AutoStartInvalid(t *testing.T) {
	cfg := &Config{DataDir: "/x"}
	if err := Set(cfg, "daemon.autostart", "yes"); err == nil {
		t.Error("非 bool 值应报错")
	}
}

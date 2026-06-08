package cli

import (
	"testing"
)

func TestNewConfigCmd_HasSubcommands(t *testing.T) {
	cmd := newConfigCmd()
	subs := cmd.Commands()
	names := map[string]bool{}
	for _, c := range subs {
		names[c.Use] = true
	}
	for _, want := range []string{"show", "get <key>", "set <key> <value>", "init"} {
		if !names[want] {
			t.Errorf("config 命令组应含子命令 %q,实际 %v", want, names)
		}
	}
}

func TestRootCmd_NoTopLevelInit(t *testing.T) {
	root := NewRootCmd()
	hasConfig := false
	for _, c := range root.Commands() {
		if c.Use == "init" {
			t.Error("顶层不应再有 init(应降为 config 子命令)")
		}
		if c.Use == "config" {
			hasConfig = true
		}
	}
	if !hasConfig {
		t.Error("顶层应有 config 命令组")
	}
}

func TestNewConfigCmd_RejectsUnexpectedPositionals(t *testing.T) {
	cmd := newConfigCmd()
	if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
		t.Error("裸 config 必须拒绝位置参数，不能忽略后直接进入 TUI")
	}
}

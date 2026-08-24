package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"
)

// 成功与错误两条路径都必须恰好调用一次 restore，且错误路径返回退出码 1。
func TestExecuteWithConsoleRestoresOnBothPaths(t *testing.T) {
	cases := []struct {
		name     string
		cmdErr   error
		wantExit int
	}{
		{"success", nil, 0},
		{"failure", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restored := 0
			orig := initConsoleFn
			initConsoleFn = func() func() {
				return func() { restored++ }
			}
			defer func() { initConsoleFn = orig }()

			cmd := &cobra.Command{Use: "t", RunE: func(*cobra.Command, []string) error { return tc.cmdErr }}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true

			if exit := ExecuteWithConsole(cmd); exit != tc.wantExit {
				t.Errorf("exit = %d, want %d", exit, tc.wantExit)
			}
			if restored != 1 {
				t.Errorf("restore 调用 %d 次，应恰好一次", restored)
			}
		})
	}
}

// 全重定向或非 Windows 平台 InitConsole 返回 nil：跳过恢复且不 panic。
func TestExecuteWithConsoleNilRestore(t *testing.T) {
	orig := initConsoleFn
	initConsoleFn = func() func() { return nil }
	defer func() { initConsoleFn = orig }()

	cmd := &cobra.Command{Use: "t", Run: func(*cobra.Command, []string) {}}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if exit := ExecuteWithConsole(cmd); exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
}

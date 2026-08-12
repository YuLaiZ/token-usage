package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExecVersionProbe_ParsesStrictOutput 生产 probe 的解析层只接受 root --version
// 合同规定的一行严格 Release tag。
func TestExecVersionProbe_ParsesStrictOutput(t *testing.T) {
	stagePath := filepath.Join(t.TempDir(), "stage")
	var gotPath string
	probe := execVersionProbe{run: func(ctx context.Context, path string) ([]byte, error) {
		gotPath = path
		return []byte("token-usage v0.2.0-rc.1\n"), nil
	}}

	got, err := probe.ProbeVersion(context.Background(), stagePath)
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if got != "v0.2.0-rc.1" {
		t.Fatalf("version=%q want v0.2.0-rc.1", got)
	}
	if gotPath != stagePath {
		t.Errorf("stage path=%q want %q", gotPath, stagePath)
	}
}

// TestParseStageVersionOutput_RejectsMalformedOutput 防止额外输出、错误程序名、
// 非法 tag 或无换行的 stage 被误当作可安装版本。
func TestParseStageVersionOutput_RejectsMalformedOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"empty", ""},
		{"no newline", "token-usage v0.2.0"},
		{"multiple lines", "token-usage v0.2.0\nextra\n"},
		{"wrong program", "other v0.2.0\n"},
		{"invalid version", "token-usage v02.0.0\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStageVersionOutput([]byte(tc.output)); err == nil {
				t.Fatal("畸形输出应被拒绝")
			}
		})
	}
}

// TestExecVersionProbe_RejectsBadInvocation 执行器错误、空路径或相对路径都不能
// 降级为可安装状态。
func TestExecVersionProbe_RejectsBadInvocation(t *testing.T) {
	stagePath := filepath.Join(t.TempDir(), "stage")
	probe := execVersionProbe{run: func(ctx context.Context, path string) ([]byte, error) {
		return nil, errors.New("exec failed")
	}}
	if _, err := probe.ProbeVersion(context.Background(), stagePath); err == nil {
		t.Fatal("执行失败应返回错误")
	}
	if _, err := probe.ProbeVersion(context.Background(), ""); err == nil {
		t.Fatal("空路径应返回错误")
	}
	if _, err := probe.ProbeVersion(context.Background(), "relative-stage"); err == nil {
		t.Fatal("相对路径应返回错误")
	}
}

// TestParseStageVersionOutput_RejectsOversizedOutput 限制异常 stage 输出的内存占用。
func TestParseStageVersionOutput_RejectsOversizedOutput(t *testing.T) {
	output := []byte(strings.Repeat("x", maxStageVersionOutputBytes+1))
	if _, err := parseStageVersionOutput(output); !errors.Is(err, errStageVersionOutputTooLarge) {
		t.Fatalf("oversized output err=%v, want errStageVersionOutputTooLarge", err)
	}
}

// TestNewExecVersionProbe_ReturnsProbe 确认生产构造器不会返回 nil。
func TestNewExecVersionProbe_ReturnsProbe(t *testing.T) {
	if NewExecVersionProbe() == nil {
		t.Fatal("NewExecVersionProbe 不应返回 nil")
	}
}

// TestExecVersionProbe_RunsStage 在 Unix 上实际执行一个临时 stage，覆盖生产路径中
// exec.CommandContext、stdout 捕获和严格解析的接线。Windows 实机执行留给 RC 验收。
func TestExecVersionProbe_RunsStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows stage 执行由 Windows RC 实机验收")
	}
	stagePath := filepath.Join(t.TempDir(), "stage")
	script := "#!/bin/sh\nprintf '%s\\n' 'token-usage v0.2.0'\n"
	if err := os.WriteFile(stagePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stage script: %v", err)
	}

	got, err := NewExecVersionProbe().ProbeVersion(context.Background(), stagePath)
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if got != "v0.2.0" {
		t.Errorf("version=%q want v0.2.0", got)
	}
}

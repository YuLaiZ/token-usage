package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestErrors_UnknownDateFlag --date flag 被移除后成为 unknown flag，cobra 直接报错。
func TestErrors_UnknownDateFlag(t *testing.T) {
	setupErrorsEnv(t)
	cmd := newErrorsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--date", "20260701"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("--date 应为 unknown flag 并报错")
	}
	// cobra 的 unknown flag 错误信息形如: unknown flag: --date
	if !strings.Contains(err.Error(), "--date") {
		t.Errorf("错误应提及 --date，实际: %v", err)
	}
}

// TestCollect_UnknownDateFlag collect 命令的 --date 同样成为 unknown flag。
func TestCollect_UnknownDateFlag(t *testing.T) {
	cmd := newCollectCmd()
	// ParseFlags 应报 unknown flag；兜底再校验 --date flag 已不存在。
	parseErr := cmd.ParseFlags([]string{"--date", "20260701"})
	if parseErr == nil && cmd.Flags().Lookup("date") != nil {
		t.Fatal("--date flag 应已从 collect 移除")
	}
	if cmd.Flags().Lookup("date") != nil {
		t.Fatal("--date flag 应已从 collect 移除")
	}
}

// TestQuery_UnknownDateFlag query 命令的 --date 已移除。
func TestQuery_UnknownDateFlag(t *testing.T) {
	cmd := newQueryCmd()
	parseErr := cmd.ParseFlags([]string{"--date", "20260701"})
	if parseErr == nil && cmd.Flags().Lookup("date") != nil {
		t.Fatal("--date flag 应已从 query 移除")
	}
	if cmd.Flags().Lookup("date") != nil {
		t.Fatal("--date flag 应已从 query 移除")
	}
}

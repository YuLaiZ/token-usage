package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestMessageLevel_CollectThenQuery 验证消息级采集与查询的端到端 CLI 链路：
//
//  1. 隔离 HOME：config.Load() 读取 $tmpHome/.token-usage/config.toml；
//  2. 真实 Claude collector 读 projects_dir 下的 JSONL fixture；
//  3. `collect 20260708 --client claude --force` 写入真实 usage.db；
//  4. `query summary 20260708` 输出「请求总数」消息级聚合（不再有 daily_stats / 会话级残留）。
//
// 端到端：真实 collector + 真实 DB（不使用 mock）。
func TestMessageLevel_CollectThenQuery(t *testing.T) {
	tmpHome := t.TempDir()
	tokenUsageDir := filepath.Join(tmpHome, ".token-usage")
	if err := os.MkdirAll(tokenUsageDir, 0755); err != nil {
		t.Fatal(err)
	}

	// claude projects_dir：写入 fixture（一条 assistant usage 消息，date=2026-07-08）。
	projectsDir := filepath.Join(tokenUsageDir, "claude-projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:00:00+08:00","entrypoint":"cli","cwd":"/tmp/demo","message":{"id":"msg-001","role":"assistant","model":"claude-sonnet","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-07-08T10:01:00+08:00","entrypoint":"cli","cwd":"/tmp/demo","message":{"id":"msg-002","role":"assistant","model":"claude-sonnet","usage":{"input_tokens":50,"output_tokens":10}}}
`
	if err := os.WriteFile(filepath.Join(projectsDir, "sess-001.jsonl"), []byte(jsonl), 0600); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tokenUsageDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(tokenUsageDir, "logs")

	// config.toml：只启用 claude，显式指向 fixture 目录，避免默认路径回填到真实 HOME。
	cfgContent := `data_dir = "` + dataDir + `"

[clients.claude]
enabled = true

[clients.claude.paths]
projects_dir = "` + projectsDir + `"

[clients.codex]
enabled = false

[clients.opencode]
enabled = false

[clients.workbuddy]
enabled = false

[clients.zcode]
enabled = false

[log]
level = "debug"
dir = "` + logDir + `"
max_days = 7
`
	if err := os.WriteFile(filepath.Join(tokenUsageDir, "config.toml"), []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	// 隔离 HOME：config.Load() 用 os.UserHomeDir() 定位 ~/.token-usage/config.toml。
	// 同时避免 collector.ApplyDefaultPaths 把未配置 client 的 path 回填到真实用户目录。
	t.Setenv("HOME", tmpHome)

	// === 步骤 1：collect 20260708 --client claude --force ===
	root := NewRootCmd()
	var collectOut bytes.Buffer
	root.SetOut(&collectOut)
	root.SetErr(&collectOut)
	root.SetArgs([]string{"collect", "20260708", "--client", "claude", "--force"})
	if err := root.Execute(); err != nil {
		t.Fatalf("collect 失败: %v\noutput: %s", err, collectOut.String())
	}

	collectStr := collectOut.String()
	// 消息级采集输出不应再出现会话级残留措辞。
	if strings.Contains(collectStr, "采集 ") && strings.Contains(collectStr, "个会话") {
		t.Errorf("collect 输出仍含会话级残留：\n%s", collectStr)
	}

	// === 步骤 2：query summary 20260708 ===
	root2 := NewRootCmd()
	var queryOut bytes.Buffer
	root2.SetOut(&queryOut)
	root2.SetErr(&queryOut)
	root2.SetArgs([]string{"query", "summary", "20260708"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("query 失败: %v\noutput: %s", err, queryOut.String())
	}

	queryStr := queryOut.String()
	// 消息级查询应展示请求总数（消息/请求数），而非 daily_stats 或会话数。
	if !strings.Contains(queryStr, "请求总数") {
		t.Errorf("query 输出缺少「请求总数」消息级聚合头：\n%s", queryStr)
	}
	if !strings.Contains(queryStr, "请求总数: 2") {
		t.Errorf("query 请求总数应为 2（两条消息），实际：\n%s", queryStr)
	}
	if strings.Contains(queryStr, "daily_stats") {
		t.Errorf("query 输出不应暴露内部表名 daily_stats：\n%s", queryStr)
	}

	// === 步骤 3：query client 20260708（默认分组）===
	// 表头应为「请求数」（消息/请求数），不再出现「会话数」。
	root3 := NewRootCmd()
	var byClientOut bytes.Buffer
	root3.SetOut(&byClientOut)
	root3.SetErr(&byClientOut)
	root3.SetArgs([]string{"query", "client", "20260708"})
	if err := root3.Execute(); err != nil {
		t.Fatalf("query by-client 失败: %v\noutput: %s", err, byClientOut.String())
	}
	if !strings.Contains(byClientOut.String(), "请求数") {
		t.Errorf("query by-client 表头缺少「请求数」：\n%s", byClientOut.String())
	}
	if strings.Contains(byClientOut.String(), "daily_stats") {
		t.Errorf("query by-client 不应暴露内部表名：\n%s", byClientOut.String())
	}

	// 安抚 go vet / unused：cobra.Command 引用占位。
	var _ *cobra.Command = root
}

// TestMessageLevel_QueryEmptyDate_UsesMessageLedger 验证无数据日期的 query 不 panic，
// 且输出仍走消息级分支（请求总数: 0），不会回退到 daily_stats 路径。
func TestMessageLevel_QueryEmptyDate_UsesMessageLedger(t *testing.T) {
	tmpHome := t.TempDir()
	tokenUsageDir := filepath.Join(tmpHome, ".token-usage")
	if err := os.MkdirAll(tokenUsageDir, 0755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(tokenUsageDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgContent := `data_dir = "` + dataDir + `"

[clients.claude]
enabled = false

[log]
level = "info"
dir = "` + filepath.Join(tokenUsageDir, "logs") + `"
max_days = 7
`
	if err := os.WriteFile(filepath.Join(tokenUsageDir, "config.toml"), []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"query", "summary", "20260101"})
	if err := root.Execute(); err != nil {
		t.Fatalf("query 空数据日期失败: %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "请求总数: 0") {
		t.Errorf("空数据日期应输出请求总数: 0，实际：\n%s", out.String())
	}
}

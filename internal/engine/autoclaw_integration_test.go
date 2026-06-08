package engine

// autoclaw_integration_test.go AutoClaw 采集端到端集成测试。
//
// 使用真实 NewDeps（含 NewAutoClawCollector）+ 真实 DB（db.Open）+ testdata fixture，
// 验证 engine.RunCollect 端到端落库。fixture 复制到 t.TempDir 再指向，不假设工作目录是仓库根，
// 也不让测试读写仓库 fixture。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// copyAutoClawFixture 把 testdata/autoclaw/agents 完整复制到 t.TempDir 下的 agents/，
// 返回复制后的 agents/ 根目录（作为 sessions_dir）。
func copyAutoClawFixture(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "autoclaw", "agents"))
	if err != nil {
		t.Fatalf("解析源 fixture 路径失败: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatalf("mkdir 目标 agents 失败: %v", err)
	}
	// 递归复制源 fixture 到 dst
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		t.Fatalf("复制 fixture 失败: %v", err)
	}
	return dst
}

// TestAutoClaw_RunCollect_EndToEnd 验证 RunCollect 端到端落库：
// messages 表写入 client=Zhipu-AutoClaw 的行，sessions 表写入 session 元数据。
func TestAutoClaw_RunCollect_EndToEnd(t *testing.T) {
	sessionsDir := copyAutoClawFixture(t)

	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := newAutoClawEngineCfg(sessionsDir)
	deps := NewDeps(cfg)

	ctx := context.Background()
	res := RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "autoclaw",
		collector.CollectRequest{Source: collector.CollectSourceClient}, true, false)
	if err := ValidateResult("autoclaw", res); err != nil {
		t.Fatalf("RunCollect 失败: %v (result=%+v)", err, res)
	}

	// 断言 messages 表写入 client=Zhipu-AutoClaw 的行
	var n int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = ?`,
		model.ClientZhipuAutoClaw).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n == 0 {
		t.Errorf("messages 表应写入 client=%s 的行，实际 0 行", model.ClientZhipuAutoClaw)
	}

	// 断言 session-aaa 的 aaa-m2 字段值与 fixture 吻合
	var input, cacheRead, total int64
	var client string
	err = usageDB.QueryRow(
		`SELECT input_tokens, cache_read_tokens, total_tokens, client FROM messages WHERE id = ?`,
		"aaa-m2").Scan(&input, &cacheRead, &total, &client)
	if err != nil {
		t.Fatalf("查询 aaa-m2: %v", err)
	}
	if input != 40507 {
		t.Errorf("aaa-m2 input_tokens = %d, want 40507", input)
	}
	if cacheRead != 1536 {
		t.Errorf("aaa-m2 cache_read_tokens = %d, want 1536", cacheRead)
	}
	if total != 42198 {
		t.Errorf("aaa-m2 total_tokens = %d, want 42198", total)
	}
	if client != model.ClientZhipuAutoClaw {
		t.Errorf("aaa-m2 client = %q, want %q", client, model.ClientZhipuAutoClaw)
	}

	// 断言 sessions 表写入 session 元数据
	var sessN int
	if err := usageDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE client = ?`,
		model.ClientZhipuAutoClaw).Scan(&sessN); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessN == 0 {
		t.Errorf("sessions 表应写入 client=%s 的行，实际 0 行", model.ClientZhipuAutoClaw)
	}
}

// TestAutoClaw_RunCollect_Idempotent 同一 fixture 二次 RunCollect，messages 行数不变（upsert by ID）。
func TestAutoClaw_RunCollect_Idempotent(t *testing.T) {
	sessionsDir := copyAutoClawFixture(t)

	usageDB, err := db.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer usageDB.Close()

	cfg := newAutoClawEngineCfg(sessionsDir)
	deps := NewDeps(cfg)
	ctx := context.Background()
	req := collector.CollectRequest{Source: collector.CollectSourceClient}

	// 第一次
	RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "autoclaw", req, true, false)
	var n1 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = ?`,
		model.ClientZhipuAutoClaw).Scan(&n1)

	// 第二次（幂等：upsert by ID，行数不变）
	RunCollect(ctx, deps, usageDB, silentLogger(), io.Discard, "autoclaw", req, true, false)
	var n2 int
	usageDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE client = ?`,
		model.ClientZhipuAutoClaw).Scan(&n2)

	if n2 != n1 {
		t.Errorf("幂等失败：二次 RunCollect 后 messages 行数 %d != 首次 %d", n2, n1)
	}
	if n1 == 0 {
		t.Errorf("首次 RunCollect 应写入消息，实际 0 行")
	}
}

// newAutoClawEngineCfg 构造含 autoclaw enabled + sessions_dir 的 engine 测试 cfg。
func newAutoClawEngineCfg(sessionsDir string) *config.Config {
	return &config.Config{
		Clients: map[string]config.Client{
			"autoclaw": {Enabled: true, Paths: map[string]string{"sessions_dir": sessionsDir}},
		},
	}
}

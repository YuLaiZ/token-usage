// internal/analyzer/integration_test.go
package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// capturedReq 记录一次 collectFunc 调用的 client 与 request 快照。
type capturedReq struct {
	client string
	req    collector.CollectRequest
}

// TestIntegration_FullFlow 真实 watcher + 真实 SQLitePoller 端到端验证。
//
// 断言：
//  1. JSONL watcher 触发时回调收到绝对 ChangedFile 路径（不再是空或相对路径）；
//  2. 临时 SQLitePoller（opencode）在主库 mtime 变化后收到 Incremental request；
//  3. 全程使用 ready channel + 带 timeout 的 select 同步，不把固定 sleep 作为成功条件。
func TestIntegration_FullFlow(t *testing.T) {
	tmpDir := t.TempDir()

	// 创建目录结构（模拟真实子目录：projects_dir/<proj>/session.jsonl）
	claudeDir := filepath.Join(tmpDir, "claude", "my-project")
	opencodeDbPath := filepath.Join(tmpDir, "opencode.db")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 创建 OpenCode 数据库文件（poller 初始 mtime 锚点）
	if err := os.WriteFile(opencodeDbPath, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	// 收集所有 ExecuteFunc 调用，分通道存放便于按类型断言。
	jsonlReqs := make(chan capturedReq, 8)
	sqliteReqs := make(chan capturedReq, 8)

	execute := func(ctx context.Context, client string, req collector.CollectRequest) error {
		cr := capturedReq{client: client, req: req}
		// ChangedFile 非空 → JSONL watcher 触发；否则（Incremental/Source）→ SQLite poller 触发。
		if req.ChangedFile != "" {
			jsonlReqs <- cr
		} else {
			sqliteReqs <- cr
		}
		return nil
	}

	// ready 现在等待生产 Ready() barrier（Analyzer 自管理），不再向 NewFromConfig
	// 传测试专用 onReady。0 monitor 时 Run 会先返回 error，不会发布 ready。
	var expected int32

	// 创建配置：claude(JSONL) + opencode(SQLite poller)，poll_interval=1s。
	cfgPath := filepath.Join(tmpDir, "config.toml")
	cfgContent := `
data_dir = "` + tmpDir + `"

[clients.claude]
enabled = true

[clients.claude.paths]
projects_dir = "` + filepath.Join(tmpDir, "claude") + `"

[clients.opencode]
enabled = true

[clients.opencode.paths]
db = "` + opencodeDbPath + `"

[clients.codex]
enabled = false

[clients.workbuddy]
enabled = false

[daemon]
poll_interval = 1

[log]
level = "debug"
dir = "` + tmpDir + `/logs"
max_days = 7
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadTestEffectiveConfig(t, cfgPath, tmpDir)

	// debounce 用 100ms（生产为 5s）：确保 jsonl watcher→采集链路在测试窗口内真正触发。
	a := NewFromConfig(cfg, execute, nil, 100*time.Millisecond)

	// 动态推导就绪阈值（本配置 = 1 claude watcher + 1 opencode poller = 2）。
	atomic.StoreInt32(&expected, int32(len(a.jsonlWatchers)+len(a.sqlitePollers)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// 等待生产 Ready() barrier（带 timeout，非固定 sleep）。
	select {
	case <-a.Ready():
		// 所有组件已就绪
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for components ready")
	}

	// === 断言 1: JSONL watcher 回调收到绝对 ChangedFile 路径 ===
	// 在就绪后创建新 JSONL 文件，debounce 合并后会触发 collectFunc。
	jsonlFile := filepath.Join(claudeDir, "session-001.jsonl")
	absJsonlFile, err := filepath.Abs(jsonlFile)
	if err != nil {
		t.Fatalf("abs jsonl path: %v", err)
	}
	if err := os.WriteFile(jsonlFile, []byte(`{"type":"message","role":"assistant"}`), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-jsonlReqs:
		if got.client != "claude" {
			t.Errorf("jsonl callback client = %q, want claude", got.client)
		}
		if got.req.ChangedFile == "" {
			t.Error("jsonl callback ChangedFile 为空，期望绝对路径")
		}
		if got.req.ChangedFile != absJsonlFile {
			t.Errorf("jsonl callback ChangedFile = %q, want 绝对路径 %q", got.req.ChangedFile, absJsonlFile)
		}
		if !filepath.IsAbs(got.req.ChangedFile) {
			t.Errorf("jsonl callback ChangedFile = %q 不是绝对路径", got.req.ChangedFile)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for jsonl watcher callback")
	}

	// === 断言 2: SQLitePoller 在主库 mtime 变化后收到 Incremental request ===
	// 修改 opencode.db 内容（mtime 推进），poller 下一个 tick 应触发回调。
	// 先小睡确保新写入 mtime 严格大于初始锚点（文件系统 mtime 精度）。
	time.Sleep(60 * time.Millisecond)
	if err := os.WriteFile(opencodeDbPath, []byte("updated data for poller"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-sqliteReqs:
		if got.client != "opencode" {
			t.Errorf("sqlite poller callback client = %q, want opencode", got.client)
		}
		if !got.req.Incremental {
			t.Error("sqlite poller callback Incremental = false, want true")
		}
		if got.req.ChangedFile != "" {
			t.Errorf("sqlite poller callback ChangedFile = %q, want empty", got.req.ChangedFile)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timeout waiting for sqlite poller Incremental callback")
	}

	// 优雅退出
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("analyzer returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("analyzer did not stop within timeout")
	}
}

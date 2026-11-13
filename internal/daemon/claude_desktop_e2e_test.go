package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// createCCSwitchE2EDB 建最小可采集的 cc-switch 库：providers + proxy_request_logs，
// 含一条 claude-desktop 代理记录（router 增量采集源）。列布局对齐 CC Switch 真实库
// 中被 collector 读取的字段。
func createCCSwitchE2EDB(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "cc-switch.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开 cc-switch 测试库: %v", err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TABLE providers (id TEXT PRIMARY KEY, app_type TEXT NOT NULL, name TEXT NOT NULL)`,
		`CREATE TABLE proxy_request_logs (
			request_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, app_type TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', request_model TEXT NOT NULL DEFAULT '',
			provider_id TEXT NOT NULL DEFAULT '', provider_type TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			total_cost_usd REAL NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0,
			first_token_ms INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0, error_message TEXT,
			is_streaming INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`INSERT INTO providers (id, app_type, name) VALUES ('provider-desktop','claude-desktop','DeepSeek')`,
		// request_id 前缀 session: 使 message_id 关联键为 desktop-e2e-msg。
		`INSERT INTO proxy_request_logs (request_id,session_id,app_type,model,provider_id,input_tokens,output_tokens,created_at)
			VALUES ('session:desktop-e2e-msg','sess-desktop-e2e','claude-desktop','deepseek-v4','provider-desktop',320,120,1781092800)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("初始化 cc-switch 库: %v", err)
		}
	}
	return dbPath
}

func countUsageRows(t *testing.T, dataDir, query string, args ...any) int {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(dataDir, "usage.db"))
	if err != nil {
		return -1
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(query, args...).Scan(&n); err != nil {
		return -1
	}
	return n
}

// T6：隔离数据目录下的完整采集链回归——真实 daemon.Run + analyzer + engine +
// Claude collector。有效 Claude Desktop JSONL（entrypoint=claude-desktop-3p）
// 经 startup catch-up 入库为 Claude Desktop 消息，cc-switch router 增量独立入库
// raw_router_logs；运行期追加写入经 JSONL 事件与 debounce 入库；不以进程仍在
// 作为唯一通过条件（收尾检查 daemon.Run 的真实返回错误）。
func TestClaudeDesktopDaemonEndToEnd(t *testing.T) {
	home := t.TempDir()
	// 隔离 HOME：analyzer 的 cc-switch 多账号可选根与 Claude collector 的账号
	// 目录发现都基于 os.UserHomeDir()，不隔离会读取/监听真实用户目录，断言也
	// 可能被真实数据误满足。
	t.Setenv("HOME", home)
	projectsDir := filepath.Join(home, "claude", "projects")
	projDir := filepath.Join(projectsDir, "-tmp-e2e-project")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 有效 Desktop 会话：assistant + 非零 usage + entrypoint=claude-desktop-3p。
	line := `{"type":"assistant","sessionId":"sess-desktop-e2e","timestamp":"2026-06-10T12:00:00Z","entrypoint":"claude-desktop-3p","cwd":"/tmp/e2e-project","message":{"id":"desktop-e2e-msg","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "sess-desktop-e2e.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ccDB := createCCSwitchE2EDB(t, home)

	cfg := &config.Config{
		DataDir: filepath.Join(home, "data"),
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Router: "cc_switch", Paths: map[string]string{"projects_dir": projectsDir}},
		},
		Routers: map[string]config.RouterConfig{"cc_switch": {DBPath: ccDB}},
		Daemon:  config.DaemonConfig{PollInterval: 1},
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- Run(ctx, cfg, RunOptions{
			InstanceID: "claude-desktop-e2e",
			OnDaemonLockCommit: func() error {
				close(ready)
				return nil
			},
			OpenResources: openTestResources(t, nil),
		})
	}()
	defer cancel() // 断言失败提前返回时保证 daemon 停止（done 缓冲 1，不阻塞）
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for daemon lock commit")
	}

	// 轮询真实 usage DB：Desktop 消息与 router 原始记录都必须经 catch-up 实际入库。
	deadline := time.Now().Add(15 * time.Second)
	var msgs, routerLogs int
	for {
		msgs = countUsageRows(t, cfg.DataDir,
			`SELECT COUNT(*) FROM messages WHERE client = ?`, model.ClientClaudeDesktop)
		routerLogs = countUsageRows(t, cfg.DataDir,
			`SELECT COUNT(*) FROM raw_router_logs WHERE router_name = 'cc_switch' AND request_id = 'session:desktop-e2e-msg'`)
		if msgs >= 1 && routerLogs >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: Claude Desktop messages=%d, cc-switch router logs=%d", msgs, routerLogs)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 消息内容细节：provider 由 JSONL 推断为 Anthropic，模型透传。
	if n := countUsageRows(t, cfg.DataDir,
		`SELECT COUNT(*) FROM messages WHERE client = ? AND provider = 'Anthropic' AND model = 'claude-sonnet-4-5'`,
		model.ClientClaudeDesktop); n < 1 {
		t.Fatalf("Claude Desktop 消息的 provider/model 解析不正确: n=%d", n)
	}

	// 运行期事件路径：追加写入新会话，经 JSONL 事件 + debounce（生产 5s）入库，
	// 不只验证启动补采。
	live := filepath.Join(projDir, "sess-desktop-live.jsonl")
	liveLine := `{"type":"assistant","sessionId":"sess-desktop-live","timestamp":"2026-06-10T12:05:00Z","entrypoint":"claude-desktop-3p","cwd":"/tmp/e2e-project","message":{"id":"desktop-live-msg","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":30,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(live, []byte(liveLine), 0o600); err != nil {
		t.Fatal(err)
	}
	liveDeadline := time.Now().Add(15 * time.Second)
	for {
		if countUsageRows(t, cfg.DataDir,
			`SELECT COUNT(*) FROM messages WHERE client = ? AND id = 'desktop-live-msg'`,
			model.ClientClaudeDesktop) >= 1 {
			break
		}
		if time.Now().After(liveDeadline) {
			t.Fatal("timeout: 运行期追加写入未经 JSONL 事件入库")
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("daemon.Run 返回错误: %v", err)
	}
}

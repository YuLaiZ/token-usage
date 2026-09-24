package analyzer

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
)

// === Codex 标题索引轮询装配合同 ===
//
// codex state_dir 配置即装配 session_index.jsonl 的周期提交 poller
//（alwaysSubmit：每 tick 无条件提交 SyncTitles 纯同步请求）——App 改名可能只写
// 索引（state DB 与 rollout 均不变），且同步失败后的重试不得依赖文件再次变化；
// 无 state_dir 不装配。

// TestSetupFromConfigCodexTitleIndexPoller：装配断言——state_dir 配置时索引
// poller 存在、路径/请求/client 正确且为周期提交模式（alwaysSubmit），state 源
// poller 保持变化触发模式；未配置时不存在。
func TestSetupFromConfigCodexTitleIndexPoller(t *testing.T) {
	cases := []struct {
		name       string
		stateDir   string
		wantPoller bool
	}{
		{"state_dir configured", t.TempDir(), true},
		{"state_dir empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := map[string]string{"sessions_dir": t.TempDir()}
			if tc.stateDir != "" {
				paths["state_dir"] = tc.stateDir
			}
			cfg := &config.Config{Clients: map[string]config.Client{
				"codex": {Enabled: true, Paths: paths},
			}}
			a := NewFromConfig(cfg, func(context.Context, string, collector.CollectRequest) error {
				return nil
			}, nil, 5*time.Second)
			var found bool
			for _, p := range a.sqlitePollers {
				if p.clientName == "codex" && p.request.SyncTitles {
					found = true
					if p.dbPath != collector.CodexTitleIndexPath(tc.stateDir) {
						t.Fatalf("索引 poller 路径 = %q, want %q", p.dbPath, collector.CodexTitleIndexPath(tc.stateDir))
					}
					if p.request.Incremental || p.request.ChangedFile != "" {
						t.Fatalf("索引 poller 应为纯同步请求: %+v", p.request)
					}
					if !p.alwaysSubmit {
						t.Fatal("索引 poller 应为周期提交模式（alwaysSubmit）——失败重试不得依赖文件变化")
					}
				}
				// 其余 poller（state 源 / router）保持变化触发模式。
				if !p.request.SyncTitles && p.alwaysSubmit {
					t.Fatalf("非索引 poller[%s] 不应为周期提交模式", p.clientName)
				}
			}
			if found != tc.wantPoller {
				t.Fatalf("索引 poller 装配 = %v, want %v", found, tc.wantPoller)
			}
		})
	}
}

// TestSQLitePoller_TitleIndexPeriodicSubmit：行为级——周期提交模式下每 tick
// 无条件提交 SyncTitles 请求（client=codex）：索引文件不存在、存在且不变、
// 变化后的全部阶段都持续提交，失败后的重试不依赖文件再次变化。
func TestSQLitePoller_TitleIndexPeriodicSubmit(t *testing.T) {
	stateDir := t.TempDir()
	indexPath := collector.CodexTitleIndexPath(stateDir)

	var mu sync.Mutex
	var gotClient string
	var gotReq collector.CollectRequest
	var submitted int32
	poller := NewSQLitePoller(
		indexPath,
		"codex",
		collector.CollectRequest{Source: collector.CollectSourceClient, SyncTitles: true},
		30*time.Millisecond,
		func(client string, req collector.CollectRequest) {
			mu.Lock()
			gotClient, gotReq = client, req
			mu.Unlock()
			atomic.AddInt32(&submitted, 1)
		},
		nil,
	)
	poller.alwaysSubmit = true

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go poller.Run(ctx)
	t.Cleanup(poller.Stop)

	// 阶段一：索引不存在（2+ 个 tick）——周期提交照常发生。
	time.Sleep(150 * time.Millisecond)
	absent := atomic.LoadInt32(&submitted)
	if absent < 2 {
		t.Fatalf("索引不存在时应周期提交: submitted=%d, want >=2", absent)
	}

	// 阶段二：索引出现且随后不再变化——提交继续按周期发生（不依赖变化检测）。
	if err := os.WriteFile(indexPath, []byte(
		`{"id":"t-1","thread_name":"改名","updated_at":"2026-09-24T12:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	stable := atomic.LoadInt32(&submitted)
	if stable-absent < 2 {
		t.Fatalf("索引稳定期间应继续周期提交: %d → %d", absent, stable)
	}

	mu.Lock()
	firstClient, firstReq := gotClient, gotReq
	mu.Unlock()
	if firstClient != "codex" || !firstReq.SyncTitles {
		t.Fatalf("提交请求形态错误: client=%q req=%+v", firstClient, firstReq)
	}
}

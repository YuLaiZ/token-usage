package analyzer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/model"
)

// 纯定时提交器：进入运行循环即就绪（恰好一次），每 tick 提交固定请求，
// Stop 后不再提交。
func TestPeriodicSubmitter_ReadyAndTicks(t *testing.T) {
	var mu sync.Mutex
	var submitted []collector.CollectRequest
	submit := func(client string, req collector.CollectRequest) {
		mu.Lock()
		defer mu.Unlock()
		submitted = append(submitted, req)
	}

	p := newPeriodicSubmitter("workbuddy", collector.CollectRequest{Source: collector.CollectSourceClient},
		10*time.Millisecond, submit, nil)
	readyCh := make(chan struct{})
	readyCount := 0
	p.signalReady = func() {
		readyCount++
		close(readyCh)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	select {
	case <-readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("进入运行循环后应发一次就绪信号")
	}

	// 首个 tick 提交固定请求；随后停止
	waitFor := func(n int) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(submitted)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("提交数 = %d, want >= %d", len(submitted), n)
	}
	waitFor(1)

	p.Stop()
	mu.Lock()
	first := submitted[0]
	countAtStop := len(submitted)
	mu.Unlock()
	if first.Source != collector.CollectSourceClient || first.Incremental || first.ChangedFile != "" || len(first.Dates) != 0 {
		t.Errorf("请求形态应为无日期全量复核: %+v", first)
	}
	// 停止后无新增提交（等待两个周期确认静默）
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(submitted) > countAtStop {
		t.Errorf("停止后仍有提交: %d > %d", len(submitted), countAtStop)
	}
	if readyCount != 1 {
		t.Errorf("就绪信号应恰好一次, got %d", readyCount)
	}
}

// 装配合同：只要 WorkBuddy 启用就装配纯定时提交器（db/projects_dir 均空也是），
// 且只装配一个；禁用不装配。装配后计入 readyWg（Run 不因 0 watcher 失败）。
func TestSetupFromConfig_WorkBuddyPeriodicAssembly(t *testing.T) {
	newCfg := func(workbuddyEnabled bool, db, projectsDir string) *config.Config {
		return &config.Config{Clients: map[string]config.Client{
			"workbuddy": {Enabled: workbuddyEnabled, Paths: map[string]string{"db": db, "projects_dir": projectsDir}},
		}}
	}

	t.Run("enabled with empty paths", func(t *testing.T) {
		a := New(func(context.Context, string, collector.CollectRequest) error { return nil }, nil)
		a.setupFromConfig(newCfg(true, "", ""), 5*time.Second)
		if len(a.periodicSubmitters) != 1 {
			t.Fatalf("路径全空仍应装配 1 个周期提交器, got %d", len(a.periodicSubmitters))
		}
		if len(a.jsonlWatchers) != 0 {
			t.Errorf("projects_dir 为空不应装配 JSONL watcher, got %d", len(a.jsonlWatchers))
		}
	})

	t.Run("disabled", func(t *testing.T) {
		a := New(func(context.Context, string, collector.CollectRequest) error { return nil }, nil)
		a.setupFromConfig(newCfg(false, "/db", "/projects"), 5*time.Second)
		if len(a.periodicSubmitters) != 0 {
			t.Fatalf("禁用不应装配, got %d", len(a.periodicSubmitters))
		}
	})

	t.Run("watcher and periodic coexist", func(t *testing.T) {
		a := New(func(context.Context, string, collector.CollectRequest) error { return nil }, nil)
		a.setupFromConfig(newCfg(true, "/db", "/projects"), 5*time.Second)
		if len(a.jsonlWatchers) != 1 || len(a.periodicSubmitters) != 1 {
			t.Fatalf("路径齐全应有 watcher+periodic, got %d/%d", len(a.jsonlWatchers), len(a.periodicSubmitters))
		}
	})
}

// HasMonitorTargets 与装配同源：仅启用 WorkBuddy、两条路径均空 → true（start
// 不误报「没有已启用的客户端」）；禁用且无其他目标 → false（仍拦截启动）；
// 其他客户端的路径门控不变。
func TestHasMonitorTargets_WorkBuddyPeriodicPredicate(t *testing.T) {
	wbCfg := func(enabled bool, db, projectsDir string) *config.Config {
		return &config.Config{Clients: map[string]config.Client{
			"workbuddy": {Enabled: enabled, Paths: map[string]string{"db": db, "projects_dir": projectsDir}},
		}}
	}
	if !HasMonitorTargets(wbCfg(true, "", "")) {
		t.Error("仅启用 WorkBuddy（路径全空）应判定有监控目标")
	}
	if !HasMonitorTargets(wbCfg(true, "/db", "")) {
		t.Error("db 配置 + projects_dir 空应判定有目标")
	}
	if HasMonitorTargets(wbCfg(false, "/db", "/projects")) {
		t.Error("禁用且无其他目标应拦截启动")
	}
	if HasMonitorTargets(&config.Config{}) {
		t.Error("空配置应拦截启动")
	}
	// 其他客户端路径门控不变：enabled + 路径空 → 无目标
	if HasMonitorTargets(&config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": ""}},
	}}) {
		t.Error("claude 路径门控不应被放宽")
	}
	if !HasMonitorTargets(&config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/x"}},
	}}) {
		t.Error("claude 路径齐全应有目标")
	}
}

// Run 的 0 监控目标检查：仅周期提交器（无 watcher/poller）时不应报「无存活
// 监控」，且 ready barrier 正常关闭。
func TestAnalyzerRun_PeriodicOnlyReady(t *testing.T) {
	cfg := &config.Config{Clients: map[string]config.Client{
		"workbuddy": {Enabled: true, Paths: map[string]string{"db": "", "projects_dir": ""}},
	}}
	a := NewFromConfig(cfg, func(context.Context, string, collector.CollectRequest) error { return nil }, nil, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	select {
	case <-a.Ready():
		// ready 正常关闭
	case <-time.After(2 * time.Second):
		t.Fatal("仅周期提交器时 ready barrier 应关闭")
	case err := <-errCh:
		t.Fatalf("Run 不应因 0 watcher/poller 失败: %v", err)
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 应随 ctx 取消退出")
	}
	_ = model.ClientWorkBuddy
}

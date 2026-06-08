package collector

import (
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// 编译期接口断言：确保各 collector/adapter 实现始终满足 Collector/RouterAdapter 接口。
// 接口签名变更时，这些断言会立即在编译期暴露缺失的方法。
var (
	_ Collector     = (*ClaudeCollector)(nil)
	_ Collector     = (*WorkBuddyCollector)(nil)
	_ Collector     = (*ZCodeCollector)(nil)
	_ Collector     = (*OpenCodeCollector)(nil)
	_ Collector     = (*CodexCollector)(nil)
	_ RouterAdapter = (*CCSwitchAdapter)(nil)
)

func TestRouterCapabilities_Fields(t *testing.T) {
	caps := RouterCapabilities{
		Provider:     true,
		Model:        true,
		InputTokens:  true,
		OutputTokens: true,
		CacheTokens:  true,
	}

	if !caps.Provider {
		t.Error("Provider should be true")
	}
	if !caps.Model {
		t.Error("Model should be true")
	}
}

func TestSyncSourceConstantsUnique(t *testing.T) {
	sources := []string{
		SyncSourceZCodeModelUsage,
		SyncSourceOpenCodeMessage,
		SyncSourceOpenCodeEvent,
		SyncSourceCodexState,
		SyncSourceCCSwitchRouter,
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if source == "" || seen[source] {
			t.Fatalf("invalid or duplicate source %q", source)
		}
		seen[source] = true
	}
}

func TestCollectRequestModes(t *testing.T) {
	cli := CollectRequest{Dates: []string{"2026-07-10"}}
	file := CollectRequest{ChangedFile: "/tmp/session.jsonl"}
	incremental := CollectRequest{Incremental: true, Cursors: map[string]model.SyncCursor{"source": {Value: 1, ID: "a"}}}
	router := CollectRequest{Source: CollectSourceRouter, Incremental: true}
	startupJSONL := CollectRequest{ScanExistingJSONL: true}
	if len(cli.Dates) != 1 || cli.ChangedFile != "" || cli.Incremental {
		t.Fatalf("invalid CLI request: %+v", cli)
	}
	if file.ChangedFile == "" || len(file.Dates) != 0 || file.Incremental {
		t.Fatalf("invalid file request: %+v", file)
	}
	if !incremental.Incremental || incremental.Cursors["source"].ID != "a" {
		t.Fatalf("invalid incremental request: %+v", incremental)
	}
	if router.Source != CollectSourceRouter || !router.Incremental {
		t.Fatalf("invalid router request: %+v", router)
	}
	if !startupJSONL.ScanExistingJSONL || startupJSONL.Incremental || startupJSONL.ChangedFile != "" {
		t.Fatalf("invalid startup JSONL request: %+v", startupJSONL)
	}
}

func TestProjectBase_NormalizesSlashStyles(t *testing.T) {
	tests := []struct {
		name      string
		directory string
		want      string
	}{
		{name: "posix", directory: "/Users/test/project", want: "project"},
		{name: "posix trailing slash", directory: "/Users/test/project/", want: "project"},
		{name: "windows", directory: `C:\Users\test\project`, want: "project"},
		{name: "windows trailing slash", directory: `C:\Users\test\project\`, want: "project"},
		{name: "blank", directory: " \t", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectBase(tt.directory); got != tt.want {
				t.Fatalf("projectBase(%q)=%q, want %q", tt.directory, got, tt.want)
			}
		})
	}
}

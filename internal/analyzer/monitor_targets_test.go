package analyzer

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
)

// HasMonitorTargets 必须与 setupFromConfig 的实际装配一致：谓词 true ⇔
// watchers+pollers > 0。两处判定漂移会让 start 前置拦截误放行/误拦截。
func TestHasMonitorTargetsMatchesAssembly(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"empty config", &config.Config{}},
		{"all clients disabled", &config.Config{
			Clients: map[string]config.Client{
				"claude":   {Enabled: false, Paths: map[string]string{"projects_dir": "/tmp/claude"}},
				"zcode":    {Enabled: false, Paths: map[string]string{"db": "/tmp/z.db"}},
				"opencode": {Enabled: false, Paths: map[string]string{"db": "/tmp/o.db"}},
			},
		}},
		{"enabled without paths", &config.Config{
			Clients: map[string]config.Client{
				"claude": {Enabled: true},
				"zcode":  {Enabled: true},
			},
		}},
		{"jsonl client enabled with dir", &config.Config{
			Clients: map[string]config.Client{
				"claude": {Enabled: true, Paths: map[string]string{"projects_dir": t.TempDir()}},
			},
		}},
		{"sqlite client enabled with db", &config.Config{
			Clients: map[string]config.Client{
				"zcode": {Enabled: true, Paths: map[string]string{"db": "/tmp/z.db"}},
			},
		}},
		{"codex state dir only", &config.Config{
			Clients: map[string]config.Client{
				"codex": {Enabled: true, Paths: map[string]string{"state_dir": t.TempDir()}},
			},
		}},
		{"router bound enabled client", &config.Config{
			Clients: map[string]config.Client{
				"claude": {Enabled: true, Router: "cc_switch"},
			},
			Routers: map[string]config.RouterConfig{
				"cc_switch": {DBPath: "/tmp/cc.db"},
			},
		}},
		{"router bound disabled client", &config.Config{
			Clients: map[string]config.Client{
				"claude": {Enabled: false, Router: "cc_switch"},
			},
			Routers: map[string]config.RouterConfig{
				"cc_switch": {DBPath: "/tmp/cc.db"},
			},
		}},
		{"router missing db path", &config.Config{
			Clients: map[string]config.Client{
				"claude": {Enabled: true, Router: "cc_switch"},
			},
			Routers: map[string]config.RouterConfig{
				"cc_switch": {},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			predicate := HasMonitorTargets(tc.cfg)
			a := NewFromConfig(tc.cfg, func(context.Context, string, collector.CollectRequest) error { return nil },
				slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
			assembled := len(a.jsonlWatchers)+len(a.sqlitePollers) > 0
			if predicate != assembled {
				t.Errorf("HasMonitorTargets=%v 与装配结果 watchers+pollers>0=%v 不一致（两处判定漂移）", predicate, assembled)
			}
		})
	}
}

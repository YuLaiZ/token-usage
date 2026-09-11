package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// appJSBehaviorReport 对应 testdata/app_behavior_runner.js 输出的单场景取证:
// init() 跑完后的选区输入值、回亮的预设、写回存储的选区,以及加载是否抛错。
type appJSBehaviorReport struct {
	Name   string   `json:"name"`
	Fatal  string   `json:"fatal"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Active []string `json:"active"`
	Saved  *struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"savedRange"`
	Today string `json:"today"`
}

// TestAppJSRangeRestoreBehavior 在 Node vm 沙箱里加载 app.js 并断言 init()
// 的选区产物,覆盖 sessionStorage 选区记忆修复的三条行为:同标签页 reload
// 恢复选区;新开页面(含带 opener 复制 sessionStorage 副本的标签页)一律默认
// Today;performance/Navigation Timing 不可用时安全降级 Today 且不阻断初始化。
// 沙箱只注入 fetch/定时器/DOM 桩,无第三方依赖;node 缺席时跳过(GitHub 托管
// 的三个 CI 平台 runner 均预装 node)。
func TestAppJSRangeRestoreBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH; skipping app.js behavior scenarios")
	}
	appSource, err := staticRoot.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	runnerPath, err := filepath.Abs(filepath.Join("testdata", "app_behavior_runner.js"))
	if err != nil {
		t.Fatalf("resolve runner path: %v", err)
	}

	// 历史固定区间:预设端点永远锚定当天,该区间与任何一天的预设都不重合,
	// 因此「恢复」与「回亮 Today」两种期望全时段稳定。
	const customFrom = "2000-01-01"
	const customTo = "2000-01-31"
	customRange := map[string]string{"from": customFrom, "to": customTo}

	scenarios := []struct {
		name        string
		opts        map[string]any
		wantRestore bool
	}{
		{name: "reload restores saved custom range", opts: map[string]any{"navType": "reload", "savedRange": customRange}, wantRestore: true},
		{name: "navigate with saved range (opener child) defaults to Today", opts: map[string]any{"navType": "navigate", "savedRange": customRange}},
		{name: "performance absent degrades to Today", opts: map[string]any{"savedRange": customRange}},
		{name: "empty navigation entries degrade to Today", opts: map[string]any{"navEntries": []any{}, "savedRange": customRange}},
	}
	defs := make([]map[string]any, 0, len(scenarios))
	for _, sc := range scenarios {
		defs = append(defs, map[string]any{"name": sc.name, "opts": sc.opts})
	}

	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.js")
	if err := os.WriteFile(appPath, appSource, 0o644); err != nil {
		t.Fatalf("write app.js copy: %v", err)
	}
	scenariosPath := filepath.Join(dir, "scenarios.json")
	defsJSON, err := json.Marshal(defs)
	if err != nil {
		t.Fatalf("marshal scenarios: %v", err)
	}
	if err := os.WriteFile(scenariosPath, defsJSON, 0o644); err != nil {
		t.Fatalf("write scenarios: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, node, runnerPath, appPath, scenariosPath)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run node behavior runner: %v\nstderr: %s", err, stderr.String())
	}
	var reports []appJSBehaviorReport
	if err := json.Unmarshal(out, &reports); err != nil {
		t.Fatalf("decode runner output: %v\nstdout: %s", err, out)
	}
	byName := make(map[string]appJSBehaviorReport, len(reports))
	for _, r := range reports {
		byName[r.Name] = r
	}

	for _, sc := range scenarios {
		r, ok := byName[sc.name]
		if !ok {
			t.Errorf("%s: runner produced no report", sc.name)
			continue
		}
		if r.Fatal != "" {
			t.Errorf("%s: app.js threw during init: %s", sc.name, r.Fatal)
			continue
		}
		if sc.wantRestore {
			if r.From != customFrom || r.To != customTo {
				t.Errorf("%s: range = %s..%s, want saved %s..%s", sc.name, r.From, r.To, customFrom, customTo)
			}
			if len(r.Active) != 0 {
				t.Errorf("%s: active presets = %v, want none for a custom range", sc.name, r.Active)
			}
			if r.Saved == nil || r.Saved.From != customFrom || r.Saved.To != customTo {
				t.Errorf("%s: saved range = %+v, want %s..%s", sc.name, r.Saved, customFrom, customTo)
			}
			continue
		}
		if r.From != r.Today || r.To != r.Today {
			t.Errorf("%s: range = %s..%s, want Today %s", sc.name, r.From, r.To, r.Today)
		}
		if len(r.Active) != 1 || r.Active[0] != "today" {
			t.Errorf("%s: active presets = %v, want [today]", sc.name, r.Active)
		}
	}
}

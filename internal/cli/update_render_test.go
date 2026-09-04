package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/update"
)

// update_render_test.go 参数化覆盖 update 结果输出的两条状态分流合同：
//   - renderApplyResult 四个成功出口（普通/--force × Installed/Deferred）×
//     替换前 daemon 运行态：主标题分流、既有次行保留、未运行提示纯追加；
//   - 无更新结果按版本比较方向分流（renderCheckResult 与 renderApplyResult
//     共用）：同版维持既有文案；本地严格高于目标明示双版本，不误称
//     「已是最新版本（较低版本）」；空/非法 tag 保守回退。

// daemonStartHintLine 是未运行提示的完整行（双语恒显）——仅用于替换已同步
// 完成的 Installed 分支；Deferred 分支必须使用确认优先提示（helper 会中止
// 「停止后意外运行」的 daemon 的替换，立即 start 会诱导失败）。
const daemonStartHintLine = "The daemon was not running before the update; run `token-usage start` to start it. / daemon 更新前未在运行；如需启动请运行 `token-usage start`。"

// daemonStartHintAfterReplacementLine 是 Deferred 分支的未运行提示完整行
// （先确认后台替换完成，再启动）。
const daemonStartHintAfterReplacementLine = "The daemon was not running before the update; after the background replacement completes (confirm with `token-usage version`), run `token-usage start` to start it. / daemon 更新前未在运行；待后台替换完成（用 `token-usage version` 确认）后，如需启动请运行 `token-usage start`。"

// TestRenderApplyResultDaemonStateMatrix 覆盖四个成功出口（普通/--force × Installed/Deferred）
// × 替换前 daemon 运行态的全 8 格。
// 版本对取 v9.9.9 → v9.9.10：已高于补全迁移门槛 v0.1.5，避免迁移提示混入断言视野。
func TestRenderApplyResultDaemonStateMatrix(t *testing.T) {
	cases := []struct {
		name          string
		forced        bool
		deferred      bool
		wasRunning    bool
		wantTitle     string // 主标题行（含版本后缀）
		wantHint      bool
		wantSecondary string // 该分支既有次行的锚点片段（必须保留）
	}{
		{
			name:   "installed running",
			forced: false, deferred: false, wasRunning: true,
			wantTitle:     "Updated and daemon restored / 已更新并恢复 daemon：v9.9.9 → v9.9.10",
			wantHint:      false,
			wantSecondary: "Run `token-usage version` to confirm the current version. / 可用 `token-usage version` 确认当前版本。",
		},
		{
			name:   "installed stopped",
			forced: false, deferred: false, wasRunning: false,
			wantTitle:     "Updated / 已更新：v9.9.9 → v9.9.10",
			wantHint:      true,
			wantSecondary: "Run `token-usage version` to confirm the current version. / 可用 `token-usage version` 确认当前版本。",
		},
		{
			name:   "forced installed running",
			forced: true, deferred: false, wasRunning: true,
			wantTitle:     "Updated and daemon restored (--force overwrite) / 已更新并恢复 daemon（--force 强制覆盖）：v9.9.9 → v9.9.10",
			wantHint:      false,
			wantSecondary: "Run `token-usage version` to confirm the current version. / 可用 `token-usage version` 确认当前版本。",
		},
		{
			name:   "forced installed stopped",
			forced: true, deferred: false, wasRunning: false,
			wantTitle:     "Updated (--force overwrite) / 已更新（--force 强制覆盖）：v9.9.9 → v9.9.10",
			wantHint:      true,
			wantSecondary: "Run `token-usage version` to confirm the current version. / 可用 `token-usage version` 确认当前版本。",
		},
		{
			name:   "deferred running",
			forced: false, deferred: true, wasRunning: true,
			wantTitle:     "Background replacement queued / 后台替换已排队：v9.9.9 → v9.9.10",
			wantHint:      false,
			wantSecondary: "Later run `token-usage version` or `token-usage update --check` to confirm the final version. / 请稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本。",
		},
		{
			name:   "deferred stopped",
			forced: false, deferred: true, wasRunning: false,
			wantTitle:     "Background replacement queued / 后台替换已排队：v9.9.9 → v9.9.10",
			wantHint:      true,
			wantSecondary: "Later run `token-usage version` or `token-usage update --check` to confirm the final version. / 请稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本。",
		},
		{
			name:   "forced deferred running",
			forced: true, deferred: true, wasRunning: true,
			wantTitle:     "Background replacement queued (--force overwrite) / 后台替换已排队（--force 强制覆盖）：v9.9.9 → v9.9.10",
			wantHint:      false,
			wantSecondary: "Later run `token-usage version` or `token-usage update --check` to confirm the final version. / 请稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本。",
		},
		{
			name:   "forced deferred stopped",
			forced: true, deferred: true, wasRunning: false,
			wantTitle:     "Background replacement queued (--force overwrite) / 后台替换已排队（--force 强制覆盖）：v9.9.9 → v9.9.10",
			wantHint:      true,
			wantSecondary: "Later run `token-usage version` or `token-usage update --check` to confirm the final version. / 请稍后运行 `token-usage version` 或 `token-usage update --check` 确认最终版本。",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := update.ApplyResult{
				CheckResult:      update.CheckResult{CurrentTag: "v9.9.9", TargetTag: "v9.9.10"},
				ProvenanceForced: c.forced,
				Installed:        !c.deferred,
				Deferred:         c.deferred,
				DaemonWasRunning: c.wasRunning,
			}
			var out, errOut bytes.Buffer
			if err := renderApplyResult(&out, &errOut, "darwin", res); err != nil {
				t.Fatalf("成功出口应返回 nil，got %v", err)
			}
			got := out.String()
			if !strings.Contains(got, c.wantTitle+"\n") {
				t.Errorf("主标题行不符：want %q，got:\n%s", c.wantTitle, got)
			}
			if !strings.Contains(got, c.wantSecondary) {
				t.Errorf("既有次行必须保留：%q 缺失，got:\n%s", c.wantSecondary, got)
			}
			if c.wantHint {
				// 未运行提示按分支分流：Installed（替换已完成）为即时提示；
				// Deferred 必须是「确认后台替换完成后再 start」，且不得出现即时提示
				//（立即启动会被 helper 判定为停止后意外运行而放弃替换）。
				if c.deferred {
					if !strings.Contains(got, daemonStartHintAfterReplacementLine) {
						t.Errorf("Deferred 未运行格应使用确认优先提示，got:\n%s", got)
					}
					if strings.Contains(got, daemonStartHintLine) {
						t.Errorf("Deferred 未运行格不得出现即时 start 提示（会诱导用户在 helper 替换前启动 daemon），got:\n%s", got)
					}
				} else if !strings.Contains(got, daemonStartHintLine) {
					t.Errorf("Installed 未运行格应追加即时启动提示，got:\n%s", got)
				}
			} else if strings.Contains(got, "token-usage start") {
				t.Errorf("daemon 原本运行时不应有启动提示，got:\n%s", got)
			}
		})
	}
}

// TestRenderUpToDateVersionComparison 无更新结果按版本比较方向分流，
// renderCheckResult 与 renderApplyResult 两入口同构。
func TestRenderUpToDateVersionComparison(t *testing.T) {
	cases := []struct {
		name       string
		currentTag string
		targetTag  string
		wantLine   string
		notWant    string // 必须不出现的片段
	}{
		{
			name:       "equal versions keeps existing text",
			currentTag: "v0.1.6", targetTag: "v0.1.6",
			wantLine: "Already up to date / 已是最新版本（v0.1.6）",
			notWant:  "已高于",
		},
		{
			name:       "rc ahead of stable reports local newer",
			currentTag: "v0.1.7-rc.2", targetTag: "v0.1.6",
			wantLine: "Local version v0.1.7-rc.2 is newer than the target release v0.1.6; nothing to update / 本地版本 v0.1.7-rc.2 已高于目标版本 v0.1.6，无需更新",
			notWant:  "已是最新版本",
		},
		{
			name:       "explicit downgrade request reports local newer",
			currentTag: "v0.1.6", targetTag: "v0.1.5",
			wantLine: "Local version v0.1.6 is newer than the target release v0.1.5; nothing to update / 本地版本 v0.1.6 已高于目标版本 v0.1.5，无需更新",
			notWant:  "已是最新版本",
		},
		{
			name:       "empty target falls back to existing text",
			currentTag: "v0.1.6", targetTag: "",
			wantLine: "Already up to date / 已是最新版本（v0.1.6）",
			notWant:  "已高于",
		},
		{
			name:       "unparseable tag falls back conservatively",
			currentTag: "dev", targetTag: "v0.1.6",
			wantLine: "Already up to date / 已是最新版本（v0.1.6）",
			notWant:  "已高于",
		},
	}
	for _, c := range cases {
		t.Run(c.name+"/check", func(t *testing.T) {
			var out bytes.Buffer
			if err := renderCheckResult(&out, update.CheckResult{CurrentTag: c.currentTag, TargetTag: c.targetTag}); err != nil {
				t.Fatalf("renderCheckResult err=%v", err)
			}
			assertUpToDateLine(t, out.String(), c.wantLine, c.notWant)
		})
		t.Run(c.name+"/apply", func(t *testing.T) {
			var out, errOut bytes.Buffer
			res := update.ApplyResult{CheckResult: update.CheckResult{CurrentTag: c.currentTag, TargetTag: c.targetTag}}
			if err := renderApplyResult(&out, &errOut, "darwin", res); err != nil {
				t.Fatalf("renderApplyResult err=%v", err)
			}
			assertUpToDateLine(t, out.String(), c.wantLine, c.notWant)
		})
	}
}

func assertUpToDateLine(t *testing.T, got, wantLine, notWant string) {
	t.Helper()
	if !strings.Contains(got, wantLine+"\n") {
		t.Errorf("无更新结果行不符：want %q，got:\n%s", wantLine, got)
	}
	if notWant != "" && strings.Contains(got, notWant) {
		t.Errorf("不应出现 %q，got:\n%s", notWant, got)
	}
}

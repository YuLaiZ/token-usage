package releasenotes

import (
	"strings"
	"testing"
)

// TestBuildBody 覆盖完整 body 的拼接：版本性质措辞、中英顺序与分隔、
// 致谢段的有/无、内容插入，以及资产/校验段的存在。断言刻意选取会让
// 「漏掉 owner 排除、致谢空时仍输出标题、中英顺序颠倒」等错误实现失败的判据。
func TestBuildBody(t *testing.T) {
	prs := []Contributor{{Login: "alice", Number: 12}, {Login: "bob", Number: 30}}
	issues := []Contributor{{Login: "carol", Number: 8}}

	tests := []struct {
		name   string
		opts   Options
		checks func(t *testing.T, body string)
	}{
		{
			name: "rc tag 使用 RC 措辞（中英）",
			opts: Options{Tag: "v0.1.0-rc.1", ContentEN: "### New\n- en", ContentZH: "### 新功能\n- zh"},
			checks: func(t *testing.T, body string) {
				if !strings.Contains(body, "pre-release (v0.1.0-rc.1)") {
					t.Errorf("英文段缺少 RC 措辞, got:\n%s", body)
				}
				if !strings.Contains(body, "预发布（v0.1.0-rc.1）") {
					t.Errorf("中文段缺少 RC 措辞, got:\n%s", body)
				}
			},
		},
		{
			name: "稳定 tag 使用稳定措辞且不含 RC 措辞",
			opts: Options{Tag: "v0.1.0", ContentEN: "### New\n- en", ContentZH: "### 新功能\n- zh"},
			checks: func(t *testing.T, body string) {
				if !strings.Contains(body, "stable release of v0.1.0") {
					t.Errorf("英文段缺少稳定措辞, got:\n%s", body)
				}
				if !strings.Contains(body, "v0.1.0 稳定版发布") {
					t.Errorf("中文段缺少稳定措辞, got:\n%s", body)
				}
				if strings.Contains(body, "pre-release") || strings.Contains(body, "预发布") {
					t.Errorf("稳定版不应出现 RC 措辞, got:\n%s", body)
				}
			},
		},
		{
			name: "英文段在 --- 之前，中文段在之后",
			opts: Options{Tag: "v0.1.0-rc.1", ContentEN: "### New\n- ENONLY", ContentZH: "### 新功能\n- ZHONLY"},
			checks: func(t *testing.T, body string) {
				sep := strings.Index(body, "\n---\n")
				if sep < 0 {
					t.Fatalf("缺少独占一行的 --- 分隔行, got:\n%s", body)
				}
				enIdx := strings.Index(body, "ENONLY")
				zhIdx := strings.Index(body, "ZHONLY")
				if !(enIdx >= 0 && enIdx < sep && sep < zhIdx) {
					t.Errorf("英文段应在 --- 之前、中文段应在之后: en=%d sep=%d zh=%d", enIdx, sep, zhIdx)
				}
				// 英文资产段在分隔前，中文资产段在分隔后。
				enAssets := strings.Index(body, "### Assets")
				zhAssets := strings.Index(body, "### 资产")
				if !(enAssets >= 0 && enAssets < sep && sep < zhAssets) {
					t.Errorf("资产段中英顺序错位: en=%d sep=%d zh=%d", enAssets, sep, zhAssets)
				}
			},
		},
		{
			name: "有贡献者时输出中英致谢段",
			opts: Options{
				Tag:       "v0.1.0",
				ContentEN: "EN",
				ContentZH: "ZH",
				Thanks:    ThanksData{PRs: prs, Issues: issues},
			},
			checks: func(t *testing.T, body string) {
				want := []string{
					"### Acknowledgements",
					"### 致谢",
					"- PRs: @alice (#12)",
					"- PRs: @bob (#30)",
					"- Issues: @carol (#8)",
					"- 代码贡献：@alice (#12)",
					"- 代码贡献：@bob (#30)",
					"- 问题反馈：@carol (#8)",
				}
				for _, w := range want {
					if !strings.Contains(body, w) {
						t.Errorf("缺少致谢片段 %q, got:\n%s", w, body)
					}
				}
			},
		},
		{
			name: "无贡献者时整段省略（连标题都不出现）",
			opts: Options{Tag: "v0.1.0", ContentEN: "EN", ContentZH: "ZH", Thanks: ThanksData{}},
			checks: func(t *testing.T, body string) {
				if strings.Contains(body, "### Acknowledgements") {
					t.Errorf("无贡献时不应输出英文致谢标题, got:\n%s", body)
				}
				if strings.Contains(body, "### 致谢") {
					t.Errorf("无贡献时不应输出中文致谢标题, got:\n%s", body)
				}
				if strings.Contains(body, "PRs:") || strings.Contains(body, "代码贡献：") {
					t.Errorf("无贡献时不应出现致谢条目, got:\n%s", body)
				}
			},
		},
		{
			name: "仅 PR 无 issue 时输出 PR 致谢且不出 Issues 行",
			opts: Options{Tag: "v0.1.0", ContentEN: "EN", ContentZH: "ZH", Thanks: ThanksData{PRs: []Contributor{{Login: "alice", Number: 12}}}},
			checks: func(t *testing.T, body string) {
				for _, w := range []string{"### Acknowledgements", "### 致谢", "- PRs: @alice (#12)", "- 代码贡献：@alice (#12)"} {
					if !strings.Contains(body, w) {
						t.Errorf("仅 PR 时应输出片段 %q, got:\n%s", w, body)
					}
				}
				for _, w := range []string{"- Issues:", "- 问题反馈："} {
					if strings.Contains(body, w) {
						t.Errorf("仅 PR 时不应出现 issue 条目 %q, got:\n%s", w, body)
					}
				}
			},
		},
		{
			name: "仅 issue 无 PR 时输出 issue 致谢且不出 PRs 行",
			opts: Options{Tag: "v0.1.0", ContentEN: "EN", ContentZH: "ZH", Thanks: ThanksData{Issues: []Contributor{{Login: "carol", Number: 8}}}},
			checks: func(t *testing.T, body string) {
				for _, w := range []string{"### Acknowledgements", "### 致谢", "- Issues: @carol (#8)", "- 问题反馈：@carol (#8)"} {
					if !strings.Contains(body, w) {
						t.Errorf("仅 issue 时应输出片段 %q, got:\n%s", w, body)
					}
				}
				for _, w := range []string{"- PRs:", "- 代码贡献："} {
					if strings.Contains(body, w) {
						t.Errorf("仅 issue 时不应出现 PR 条目 %q, got:\n%s", w, body)
					}
				}
			},
		},
		{
			name: "内容插入各自语言段",
			opts: Options{Tag: "v0.1.0", ContentEN: "### New\n- the EN content", ContentZH: "### 新功能\n- 中文内容"},
			checks: func(t *testing.T, body string) {
				if !strings.Contains(body, "the EN content") {
					t.Errorf("英文内容未插入, got:\n%s", body)
				}
				if !strings.Contains(body, "中文内容") {
					t.Errorf("中文内容未插入, got:\n%s", body)
				}
			},
		},
		{
			name: "资产与校验段（中英）均出现",
			opts: Options{Tag: "v0.1.0", ContentEN: "EN", ContentZH: "ZH"},
			checks: func(t *testing.T, body string) {
				want := []string{
					"### Assets",
					"### Verify & install",
					"### 资产",
					"### 校验与安装",
					"token-usage-darwin-arm64",
					"token-usage-darwin-amd64",
					"token-usage-windows-amd64.exe",
					"SHA256SUMS",
				}
				for _, w := range want {
					if !strings.Contains(body, w) {
						t.Errorf("缺少固定片段 %q, got:\n%s", w, body)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := BuildBody(tt.opts)
			tt.checks(t, body)
		})
	}
}

func TestNature(t *testing.T) {
	rcTag := "v2.0.0-rc.3"
	stableTag := "v2.0.0"

	t.Run("EN rc vs 稳定措辞不同", func(t *testing.T) {
		rc := NatureEN(rcTag)
		stable := NatureEN(stableTag)
		if rc == stable {
			t.Errorf("rc 与稳定英文措辞不应相同: %q", rc)
		}
		if !strings.Contains(rc, "pre-release") {
			t.Errorf("rc 英文缺 pre-release: %q", rc)
		}
		if !strings.Contains(stable, "stable release") {
			t.Errorf("稳定英文缺 stable release: %q", stable)
		}
	})

	t.Run("ZH rc vs 稳定措辞不同", func(t *testing.T) {
		rc := NatureZH(rcTag)
		stable := NatureZH(stableTag)
		if rc == stable {
			t.Errorf("rc 与稳定中文措辞不应相同: %q", rc)
		}
		if !strings.Contains(rc, "预发布") {
			t.Errorf("rc 中文缺 预发布: %q", rc)
		}
		if !strings.Contains(stable, "稳定版") {
			t.Errorf("稳定中文缺 稳定版: %q", stable)
		}
	})

	t.Run("tag 内嵌 rc 字样识别为 RC", func(t *testing.T) {
		// 任何含 -rc. 的 tag 都视为候选版。
		if !strings.Contains(NatureEN("v1.2.3-rc.10"), "pre-release") {
			t.Errorf("v1.2.3-rc.10 应识别为 RC")
		}
		if strings.Contains(NatureEN("v1.2.3"), "pre-release") {
			t.Errorf("v1.2.3 不应识别为 RC")
		}
	})
}

func TestSplitNotes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantEN  string
		wantZH  string
		wantErr bool
		errSub  string
	}{
		{
			name:   "正常分割",
			raw:    "<!-- en -->\n### New\n- feat\n\n<!-- zh -->\n### 新功能\n- 功能",
			wantEN: "### New\n- feat",
			wantZH: "### 新功能\n- 功能",
		},
		{
			name:    "缺英文标记",
			raw:     "### New\n- feat\n\n<!-- zh -->\n### 新功能",
			wantErr: true,
			errSub:  "en",
		},
		{
			name:    "缺中文标记",
			raw:     "<!-- en -->\n### New\n- feat",
			wantErr: true,
			errSub:  "zh",
		},
		{
			name:    "英文段为空",
			raw:     "<!-- en -->\n\n<!-- zh -->\n### 新功能",
			wantErr: true,
			errSub:  "英文",
		},
		{
			name:    "中文段为空",
			raw:     "<!-- en -->\n### New\n<!-- zh -->\n",
			wantErr: true,
			errSub:  "中文",
		},
		{
			name:    "中文标记出现在英文标记之前",
			raw:     "<!-- zh -->\n### 新功能\n<!-- en -->\n### New",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			en, zh, err := SplitNotes(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际无错: en=%q zh=%q", en, zh)
				}
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("错误信息应含 %q, got %q", tt.errSub, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("非期望错误: %v", err)
			}
			if en != tt.wantEN {
				t.Errorf("英文段不符: got %q want %q", en, tt.wantEN)
			}
			if zh != tt.wantZH {
				t.Errorf("中文段不符: got %q want %q", zh, tt.wantZH)
			}
		})
	}
}

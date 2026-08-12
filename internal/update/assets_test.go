package update

import (
	"strings"
	"testing"
)

// TestAssetName_MapsSupportedPlatforms 校验三组受支持平台精确映射到冻结资产名。
func TestAssetName_MapsSupportedPlatforms(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantName     string
	}{
		{"darwin", "arm64", "token-usage-darwin-arm64"},
		{"darwin", "amd64", "token-usage-darwin-amd64"},
		{"windows", "amd64", "token-usage-windows-amd64.exe"},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, ok := AssetName(c.goos, c.goarch)
			if !ok {
				t.Fatalf("AssetName(%q,%q) ok=false, want true", c.goos, c.goarch)
			}
			if got != c.wantName {
				t.Fatalf("AssetName(%q,%q) = %q, want %q", c.goos, c.goarch, got, c.wantName)
			}
		})
	}
}

// TestAssetName_RejectsUnsupported 校验 Linux / 其它架构稳定返回 unsupported。
// 不支持的组合必须返回 ("", false)，不得返回猜测名或空名 + ok。
func TestAssetName_RejectsUnsupported(t *testing.T) {
	unsupported := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "386"},
		{"darwin", "arm"},
		{"windows", "arm64"},
		{"windows", "386"},
		{"freebsd", "amd64"},
		{"openbsd", "amd64"},
		{"", "amd64"},
		{"darwin", ""},
		{"DARWIN", "ARM64"}, // 大小写敏感：不接受
		{"darwin", "ARM64"},
	}
	for _, c := range unsupported {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, ok := AssetName(c.goos, c.goarch)
			if ok {
				t.Fatalf("AssetName(%q,%q) = (%q,true), want (\"\",false)", c.goos, c.goarch, got)
			}
			if got != "" {
				t.Fatalf("AssetName(%q,%q) ok=false but name=%q, want \"\"", c.goos, c.goarch, got)
			}
		})
	}
}

// TestNeedsUnixExecMode 校验仅 windows 不需要 Unix 可执行位；其它平台（含 Linux）
// 需要。仅用于决定下载落地后的权限模式，与「是否受支持」解耦。
func TestNeedsUnixExecMode(t *testing.T) {
	no := []string{"windows"}
	for _, goos := range no {
		if NeedsUnixExecMode(goos) {
			t.Fatalf("NeedsUnixExecMode(%q) = true, want false", goos)
		}
	}
	yes := []string{"darwin", "linux", "freebsd", ""}
	for _, goos := range yes {
		if !NeedsUnixExecMode(goos) {
			t.Fatalf("NeedsUnixExecMode(%q) = false, want true", goos)
		}
	}
}

// TestValidateRelease_AcceptsCompleteAssetSet 校验含全部四项精确资产名的
// 非草稿、prerelease 一致 Release 通过校验。
func TestValidateRelease_AcceptsCompleteAssetSet(t *testing.T) {
	cases := []struct {
		name       string
		tag        string
		draft      bool
		prerelease bool
		allowPre   bool
	}{
		{"stable release", "v0.1.0", false, false, false},
		{"rc allowed", "v0.1.0-rc.1", false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newCompleteRelease(c.tag, c.draft, c.prerelease)
			if err := ValidateRelease(r, c.tag, c.allowPre); err != nil {
				t.Fatalf("ValidateRelease = %v, want nil", err)
			}
		})
	}
}

// TestValidateRelease_RejectsBadAssetSet 校验资产名集合必须 EXACTLY 匹配四项。
// 缺一项、多一项、重复、错名、空 map 都应被拒。
func TestValidateRelease_RejectsBadAssetSet(t *testing.T) {
	mk := func(names ...string) map[string]Asset {
		m := map[string]Asset{}
		for _, n := range names {
			m[n] = Asset{Name: n}
		}
		return m
	}
	cases := []struct {
		name   string
		assets map[string]Asset
	}{
		{"missing SHA256SUMS", mk("token-usage-darwin-arm64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe")},
		{"missing one binary", mk("token-usage-darwin-arm64", "token-usage-darwin-amd64", "SHA256SUMS")},
		{"extra unknown asset", mk("token-usage-darwin-arm64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe", "SHA256SUMS", "README.md")},
		{"wrong case binary", mk("token-usage-darwin-ARM64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe", "SHA256SUMS")},
		{"wrong case sums", mk("token-usage-darwin-arm64", "token-usage-darwin-amd64", "token-usage-windows-amd64.exe", "sha256sums")},
		{"empty asset set", map[string]Asset{}},
		{"nil asset set", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Release{Tag: "v0.1.0", Version: mustV("v0.1.0"), Assets: c.assets}
			err := ValidateRelease(r, "v0.1.0", false)
			if err == nil {
				t.Fatalf("ValidateRelease accepted bad asset set: %v", c.assets)
			}
			// 错误信息应提示「资产」相关，便于定位。
			if !strings.Contains(err.Error(), "资产") {
				t.Fatalf("error message %q should mention 资产", err.Error())
			}
		})
	}
}

// TestValidateRelease_RejectsDraftAndTagMismatchAndPrerelease 校验：
//   - Draft==true 一律拒绝（不接受草稿）；
//   - Release.Tag 与请求 wantTag 不一致拒绝；
//   - prerelease 版本但 allowPrerelease=false 拒绝；
//   - 稳定版标记 Prerelease=true 拒绝（版本与元数据矛盾）。
func TestValidateRelease_RejectsDraftAndTagMismatchAndPrerelease(t *testing.T) {
	// Draft。
	r := newCompleteRelease("v0.1.0", false, false)
	r.Draft = true
	if err := ValidateRelease(r, "v0.1.0", false); err == nil {
		t.Fatal("ValidateRelease should reject draft")
	}

	// tag 不匹配。
	r2 := newCompleteRelease("v0.1.0", false, false)
	if err := ValidateRelease(r2, "v0.2.0", false); err == nil {
		t.Fatal("ValidateRelease should reject tag mismatch")
	}

	// rc 但不允许预发布。
	r3 := newCompleteRelease("v0.1.0-rc.1", false, true)
	err := ValidateRelease(r3, "v0.1.0-rc.1", false)
	if err == nil {
		t.Fatal("ValidateRelease should reject prerelease when allowPrerelease=false")
	}

	// 稳定 tag 但 Prerelease 元数据为 true（版本与元数据矛盾）。
	r4 := newCompleteRelease("v0.1.0", false, true)
	if err := ValidateRelease(r4, "v0.1.0", false); err == nil {
		t.Fatal("ValidateRelease should reject stable tag with Prerelease=true")
	}
}

// TestValidateRelease_NilRelease 校验 nil 与零值 Release 安全报错，不 panic。
func TestValidateRelease_NilRelease(t *testing.T) {
	if err := ValidateRelease(nil, "v0.1.0", false); err == nil {
		t.Fatal("ValidateRelease(nil) should error")
	}
}

// ---- helpers ----

func mustV(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// newCompleteRelease 构造一个资产集合完整、版本与 tag 一致的 Release。
func newCompleteRelease(tag string, draft, prerelease bool) *Release {
	v := mustV(tag)
	assets := map[string]Asset{
		"token-usage-darwin-arm64":      {Name: "token-usage-darwin-arm64"},
		"token-usage-darwin-amd64":      {Name: "token-usage-darwin-amd64"},
		"token-usage-windows-amd64.exe": {Name: "token-usage-windows-amd64.exe"},
		"SHA256SUMS":                    {Name: "SHA256SUMS"},
	}
	return &Release{Tag: tag, Version: v, Draft: draft, Prerelease: prerelease, Assets: assets}
}

package update

import (
	"strings"
	"testing"
)

// TestParseManifest_AcceptsCanonicalThreeLines 校验恰好三行、按资产名 ASCII 升序、
// 64 位小写 hash、两空格分隔、尾随换行的合法清单能被解析。
// 三行为三个二进制资产（SHA256SUMS 自身不在清单内）。
func TestParseManifest_AcceptsCanonicalThreeLines(t *testing.T) {
	const body = "000000000000000000000000000000000000000000000000000000000000000a  token-usage-darwin-amd64\n" +
		"111111111111111111111111111111111111111111111111111111111111111b  token-usage-darwin-arm64\n" +
		"222222222222222222222222222222222222222222222222222222222222222c  token-usage-windows-amd64.exe\n"
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// 任一二进制资产名可查到对应 hash。
	for _, name := range []string{"token-usage-darwin-amd64", "token-usage-darwin-arm64", "token-usage-windows-amd64.exe"} {
		got, ok := m.HashFor(name)
		if !ok {
			t.Fatalf("HashFor(%q) ok=false, want true", name)
		}
		if len(got) != 64 {
			t.Fatalf("HashFor(%q) = %q (len %d), want 64 hex chars", name, got, len(got))
		}
	}
	// 未在清单中的资产名返回 false（SHA256SUMS 不在清单内）。
	if _, ok := m.HashFor("SHA256SUMS"); ok {
		t.Fatal("HashFor(SHA256SUMS) should be false (sums file not listed in its own manifest)")
	}
}

// TestParseManifest_RejectsFourLines 校验清单必须恰好三行（每行对应一个二进制资产）。
// SHA256SUMS 文件本身不出现在清单中（避免自引用），故四行（含 SHA256SUMS）必须被拒。
func TestParseManifest_RejectsFourLines(t *testing.T) {
	const body = "0000000000000000000000000000000000000000000000000000000000000000  SHA256SUMS\n" +
		"1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-amd64\n" +
		"2222222222222222222222222222222222222222222222222222222222222222  token-usage-darwin-arm64\n" +
		"3333333333333333333333333333333333333333333333333333333333333333  token-usage-windows-amd64.exe\n"
	if _, err := ParseManifest([]byte(body)); err == nil {
		t.Fatal("ParseManifest must reject 4-line body (SHA256SUMS not listed in its own manifest)")
	}
}

// TestParseManifest_RejectsMalformed 覆盖全部畸形清单的拒绝情形：
// 乱序、重复、缺项、多项、空行、错误空格、错误 hash、文件名注入、无尾随换行、CRLF 之外异常。
// 每条用例仅相对规范三行（二进制资产，SHA256SUMS 不在清单内）做一处扰动，
// 确保错误确实源自该项扰动（discriminating）。
func TestParseManifest_RejectsMalformed(t *testing.T) {
	good := func() string {
		// 规范三行：三个二进制资产，按 ASCII 升序，64 位小写 hex，两空格分隔，尾随换行。
		return "0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n" +
			"1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-arm64\n" +
			"2222222222222222222222222222222222222222222222222222222222222222  token-usage-windows-amd64.exe\n"
	}
	swap := func(body string) string {
		// 把第一行和第二行交换，制造乱序。
		lines := strings.Split(body, "\n")
		lines[0], lines[1] = lines[1], lines[0]
		return strings.Join(lines, "\n")
	}
	cases := []struct {
		name string
		body string
	}{
		{"empty input", ""},
		{"only newline", "\n"},
		{"too few lines", "0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n"},
		{
			"out of order (sorted asc required)",
			swap(good()),
		},
		{
			"duplicate entry",
			strings.Replace(good(),
				"token-usage-darwin-amd64\n",
				"token-usage-darwin-amd64\n0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n", 1),
		},
		{
			"missing entry (only 2 distinct)",
			"0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n" +
				"1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-arm64\n",
		},
		{
			"extra unknown entry",
			good()[:len(good())-len("\n")] + "\n4444444444444444444444444444444444444444444444444444444444444444  README.md\n",
		},
		{
			"empty line in middle",
			"0000000000000000000000000000000000000000000000000000000000000000  token-usage-darwin-amd64\n\n" +
				"1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-arm64\n" +
				"2222222222222222222222222222222222222222222222222222222222222222  token-usage-windows-amd64.exe\n",
		},
		{
			"single space separator",
			strings.Replace(good(), "  ", " ", 1),
		},
		{
			"triple space separator",
			strings.Replace(good(), "  ", "   ", 1),
		},
		{
			"tab separator",
			strings.Replace(good(), "  ", "\t", 1),
		},
		{
			"hash too short",
			strings.Replace(good(), "0000000000000000000000000000000000000000000000000000000000000000", "00000000000000000000000000000000000000000000000000000000000000", 1),
		},
		{
			"hash uppercase",
			strings.Replace(good(), "0000000000000000000000000000000000000000000000000000000000000000", "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", 1),
		},
		{
			"hash non-hex",
			strings.Replace(good(), "0000000000000000000000000000000000000000000000000000000000000000", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", 1),
		},
		{
			"filename injection path separator",
			strings.Replace(good(), "token-usage-darwin-arm64", "../token-usage-darwin-arm64", 1),
		},
		{
			"filename injection embedded space",
			strings.Replace(good(), "token-usage-darwin-arm64", "token-usage darwin-arm64", 1),
		},
		{
			"no trailing newline",
			strings.TrimSuffix(good(), "\n"),
		},
		{
			"double trailing newline",
			good() + "\n",
		},
		{
			"leading newline",
			"\n" + good(),
		},
		{
			"CRLF not strictly honored (mixed)",
			strings.ReplaceAll(good(), "\n", "\r\n") + "extra tail",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(c.body))
			if err == nil {
				t.Fatalf("ParseManifest accepted malformed body:\n%q\nmanifest=%+v", c.body, m)
			}
			if m != nil {
				t.Fatalf("error path must return nil manifest, got %+v", m)
			}
		})
	}
}

// TestParseManifest_AcceptsCRLFStrict 校验「正常 CRLF」是合法的（规范允许 CRLF，
// 仅拒绝 CRLF 之外的异常行）。每行以 \r\n 结尾、整体结尾仍需换行。
// 三行均为二进制资产（SHA256SUMS 自身不在清单内）。
func TestParseManifest_AcceptsCRLFStrict(t *testing.T) {
	const body = "1111111111111111111111111111111111111111111111111111111111111111  token-usage-darwin-amd64\r\n" +
		"2222222222222222222222222222222222222222222222222222222222222222  token-usage-darwin-arm64\r\n" +
		"3333333333333333333333333333333333333333333333333333333333333333  token-usage-windows-amd64.exe\r\n"
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest should accept canonical CRLF body: %v", err)
	}
	if h, ok := m.HashFor("token-usage-darwin-amd64"); !ok || !strings.HasPrefix(h, "1111") {
		t.Fatalf("HashFor(token-usage-darwin-amd64) = %q,%v, want 1111...,true", h, ok)
	}
}

// TestManifest_HashFor_NilSafe 校验零值/未解析 Manifest 的 HashFor 安全返回 false。
func TestManifest_HashFor_NilSafe(t *testing.T) {
	var m *Manifest
	got, ok := m.HashFor("SHA256SUMS")
	if ok || got != "" {
		t.Fatalf("nil manifest HashFor = (%q,%v), want (\"\",false)", got, ok)
	}
}

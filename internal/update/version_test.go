package update

import (
	"testing"
)

// TestParseVersion_AcceptsStrictTags 校验仅接受严格的 vMAJOR.MINOR.PATCH 与
// vMAJOR.MINOR.PATCH-rc.N 格式，每个数值分量不得有前导零，rc.N 的 N 为正整数。
func TestParseVersion_AcceptsStrictTags(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantV  Version
		wantRC bool
	}{
		{"stable minimal", "v0.0.0", Version{Major: 0, Minor: 0, Patch: 0}, false},
		{"stable typical", "v0.1.0", Version{Major: 0, Minor: 1, Patch: 0}, false},
		{"stable multi-digit", "v10.20.30", Version{Major: 10, Minor: 20, Patch: 30}, false},
		{"rc first", "v0.1.0-rc.1", Version{Major: 0, Minor: 1, Patch: 0, RC: 1}, true},
		{"rc multi-digit", "v1.2.3-rc.42", Version{Major: 1, Minor: 2, Patch: 3, RC: 42}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVersion(tc.input)
			if err != nil {
				t.Fatalf("ParseVersion(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.wantV {
				t.Fatalf("ParseVersion(%q) = %+v, want %+v", tc.input, got, tc.wantV)
			}
			if got.IsPrerelease() != tc.wantRC {
				t.Fatalf("IsPrerelease() = %v, want %v", got.IsPrerelease(), tc.wantRC)
			}
		})
	}
}

// TestParseVersion_RejectsMalformed 校验拒绝：缺 v 前缀、前导零、不完整、
// 构建元数据、未知预发布、空字符串、beta/nightly、多余后缀。
func TestParseVersion_RejectsMalformed(t *testing.T) {
	rejects := []string{
		"",                 // 空
		"1.0.0",            // 缺 v 前缀
		"V1.0.0",           // 大写 V 不是 v
		"v1.0",             // 不完整：缺 patch
		"v1",               // 不完整：缺 minor.patch
		"v1.0.0.0",         // 过多分量
		"v01.0.0",          // major 前导零
		"v1.01.0",          // minor 前导零
		"v1.0.01",          // patch 前导零
		"v1.0.0-rc.0",      // rc.N 必须 >= 1
		"v1.0.0-rc.01",     // rc 前导零
		"v1.0.0-",          // 预发布段不能为空
		"v1.0.0-rc",        // 缺 rc 编号
		"v1.0.0-rc.1.2",    // rc 多余分量
		"v1.0.0+meta",      // 构建元数据
		"v1.0.0-rc.1+meta", // rc + 构建元数据
		"v1.0.0-beta.1",    // 未知预发布标识
		"v1.0.0-alpha",     // 未知预发布标识
		"v1.0.0-nightly",   // nightly
		"v1.0.0-rc.x",      // rc 非数字
		"v1.0.0-rc.-1",     // rc 负数
		"v 1.0.0",          // 含空格
		"v1.0.0 ",          // 尾随空格
		" v1.0.0",          // 前导空格
		"latest",           // 非版本字面量
		"vX.Y.Z",           // 非数字
	}
	for _, in := range rejects {
		t.Run("reject/"+in, func(t *testing.T) {
			got, err := ParseVersion(in)
			if err == nil {
				t.Fatalf("ParseVersion(%q) = %+v, want error", in, got)
			}
			// 返回值必须是零值 Version，避免调用方误用半解析结果。
			if got != (Version{}) {
				t.Fatalf("ParseVersion(%q) error but got non-zero %+v", in, got)
			}
		})
	}
}

// TestVersion_Compare_OrdersRanksCorrectly 是 Compare 的核心判别测试：
//   - major → minor → patch 数值升序；
//   - 同数值时 stable 排在 rc 之上；
//   - rc 按 N 升序；
//   - v0.1.0-rc.1 < v0.1.0-rc.2 < v0.1.0；
//   - v0.1.10 > v0.1.9，禁止字典序比较。
func TestVersion_Compare_OrdersRanksCorrectly(t *testing.T) {
	// (a < b) 表示 a.Compare(b) == -1 且 b.Compare(a) == 1。
	less := []struct {
		name string
		a, b string
	}{
		{"rc.1 < rc.2", "v0.1.0-rc.1", "v0.1.0-rc.2"},
		{"rc.2 < stable", "v0.1.0-rc.2", "v0.1.0"},
		{"patch numeric not lexical", "v0.1.9", "v0.1.10"},
		{"major dominates", "v1.0.0", "v2.0.0"},
		{"minor dominates", "v2.1.0", "v2.2.0"},
		{"patch dominates", "v2.2.1", "v2.2.2"},
		{"stable > rc same triple", "v1.0.0-rc.5", "v1.0.0"},
		{"rc ascending", "v1.0.0-rc.9", "v1.0.0-rc.10"},
		{"rc vs rc lower triple", "v1.0.0-rc.99", "v1.0.1-rc.1"},
	}
	for _, c := range less {
		t.Run(c.name, func(t *testing.T) {
			va, errA := ParseVersion(c.a)
			if errA != nil {
				t.Fatalf("ParseVersion(%q): %v", c.a, errA)
			}
			vb, errB := ParseVersion(c.b)
			if errB != nil {
				t.Fatalf("ParseVersion(%q): %v", c.b, errB)
			}
			if got := va.Compare(vb); got != -1 {
				t.Errorf("%s.Compare(%s) = %d, want -1", c.a, c.b, got)
			}
			if got := vb.Compare(va); got != 1 {
				t.Errorf("%s.Compare(%s) = %d, want 1", c.b, c.a, got)
			}
		})
	}
}

// TestVersion_Compare_EqualIsZero 校验相同版本返回 0。
func TestVersion_Compare_EqualIsZero(t *testing.T) {
	pairs := []string{"v0.1.0", "v0.1.0-rc.1", "v10.20.30-rc.7"}
	for _, s := range pairs {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		if got := v.Compare(v); got != 0 {
			t.Errorf("%s.Compare(itself) = %d, want 0", s, got)
		}
	}
}

// TestVersion_String_RoundTrips 校验 String() 还原原始规范字面量，
// 非规范输入（即便解析成功）经 String 后应能再被解析。
func TestVersion_String_RoundTrips(t *testing.T) {
	cases := []string{"v0.0.0", "v0.1.0", "v1.2.3", "v0.1.0-rc.1", "v9.9.9-rc.42"}
	for _, s := range cases {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		got := v.String()
		if got != s {
			t.Fatalf("String() = %q, want %q", got, s)
		}
		// 往返：String() 输出必须能再次被严格解析。
		if _, err := ParseVersion(got); err != nil {
			t.Fatalf("round-trip ParseVersion(%q): %v", got, err)
		}
	}
}

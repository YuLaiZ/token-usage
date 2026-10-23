package cli

import "testing"

// TestRunQuery_DefaultSingleViewMatchesStatic 验证 query.default 指向单维视图时
// 与显式静态子命令走同一实现并产出相同文本：两者都不应进入单遍扫描路径。
func TestRunQuery_DefaultSingleViewMatchesStatic(t *testing.T) {
	open := memOpen(t)

	cmdDefault, bufDefault := newQueryOutputCmd()
	raw := map[string]any{"default": "provider"}
	if err := runQueryWithDeps(cmdDefault, []string{"20260709"}, viewDefault, loadWithRaw(raw, nil), open); err != nil {
		t.Fatalf("default single view: %v", err)
	}

	cmdStatic, bufStatic := newQueryOutputCmd()
	if err := runQueryWithDeps(cmdStatic, []string{"20260709"}, viewProvider, loadWithRaw(nil, nil), open); err != nil {
		t.Fatalf("static provider: %v", err)
	}

	if bufDefault.String() != bufStatic.String() {
		t.Errorf("default=provider 与 query provider 输出不一致\ndefault:\n%s\nstatic:\n%s",
			bufDefault.String(), bufStatic.String())
	}
}

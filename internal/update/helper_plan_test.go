package update

import (
	"os"
	"path/filepath"
	"testing"
)

// helper_plan_test.go 校验 helper 计划的路径派生与严格校验（平台无关，macOS 可跑）。
//
// 覆盖 plan 校验的关键路径与注入防御：
//   - 路径派生一致性（target/stage/backup/helper/plan/result 全部从 basename+nonce 派生）；
//   - plan 路径注入拒绝（跨目录、../ 注入、任意外部路径）；
//   - nonce 不匹配拒绝；
//   - symlink / 目录 / 近似前缀拒绝（plan 与 helper 自身）；
//   - 字段缺失 / 非法 nonce / target_basename 含分隔符拒绝。

// writeValidPlanFixture 在 dir 下构造一份合法的 helper.exe + plan 文件对（同 nonce 绑定），
// 返回 (selfExe, planPath, plan)。selfExe 即派生的 helper 路径，planPath 即派生的 plan 路径。
func writeValidPlanFixture(t *testing.T, dir, targetBasename, nonce string, plan helperPlan) (string, string) {
	t.Helper()
	paths := deriveHelperPaths(dir, targetBasename, nonce)
	// 写 helper.exe（普通文件）。
	if err := os.WriteFile(paths.Helper, []byte("helper-fake"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	// 写 plan（普通文件，0600）。
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return paths.Helper, paths.Plan
}

// writeValidCleanupPlanFixture 在 dir 下构造一份 target + plan 文件对，供新 target
// 执行 _update-cleanup 前的绑定校验使用。
func writeValidCleanupPlanFixture(t *testing.T, dir, targetBasename, nonce string, plan helperPlan) (string, string) {
	t.Helper()
	paths := deriveHelperPaths(dir, targetBasename, nonce)
	if err := os.WriteFile(paths.Target, []byte("target-fake"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return paths.Target, paths.Plan
}

// TestDeriveHelperPaths_NamingConvention 路径派生遵循既定命名约定。
func TestDeriveHelperPaths_NamingConvention(t *testing.T) {
	const dir = "/data/bin"
	const base = "token-usage"
	const nonce = "abcd1234abcd1234abcd1234abcd1234"
	p := deriveHelperPaths(dir, base, nonce)
	want := helperPaths{
		Target: filepath.Join(dir, base),
		Stage:  filepath.Join(dir, "."+base+updateStageSuffix+nonce),
		Backup: filepath.Join(dir, "."+base+updateBackupSuffix+nonce),
		Helper: filepath.Join(dir, "."+base+helperSuffix+nonce+helperExeExt),
		Plan:   filepath.Join(dir, "."+base+planSuffix+nonce),
		Result: filepath.Join(dir, "."+base+resultSuffix+nonce+resultExt),
	}
	if p != want {
		t.Fatalf("派生路径不匹配\ngot:  %+v\nwant: %+v", p, want)
	}
	// target/stage/backup/helper/plan/result 全部在同一目录。
	for _, pp := range []string{p.Target, p.Stage, p.Backup, p.Helper, p.Plan, p.Result} {
		if filepath.Dir(pp) != dir {
			t.Errorf("路径 %q 不在 %s 内", pp, dir)
		}
	}
}

// TestValidateHelperPlan_Valid 合法 plan + helper.exe 对：校验通过，返回派生路径。
func TestValidateHelperPlan_Valid(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: base,
		OldSHA256:      "1111111111111111111111111111111111111111111111111111111111111111",
		NewSHA256:      "2222222222222222222222222222222222222222222222222222222222222222",
		WasRunning:     true,
		Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
	}
	selfExe, planPath := writeValidPlanFixture(t, dir, base, nonce, plan)

	got, err := validateHelperPlan(selfExe, planPath)
	if err != nil {
		t.Fatalf("validate err=%v", err)
	}
	if got.Plan.Nonce != nonce {
		t.Errorf("Nonce=%q want %q", got.Plan.Nonce, nonce)
	}
	if got.Paths.Target != filepath.Join(dir, base) {
		t.Errorf("Target=%q want %q", got.Paths.Target, filepath.Join(dir, base))
	}
	if got.Paths.Helper != selfExe {
		t.Errorf("Helper=%q want %q", got.Paths.Helper, selfExe)
	}
	if got.Paths.Plan != planPath {
		t.Errorf("Plan=%q want %q", got.Paths.Plan, planPath)
	}
}

// TestValidateHelperPlan_PlanOutsideHelperDir 计划文件在 helper 目录外 → 拒绝。
func TestValidateHelperPlan_PlanOutsideHelperDir(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{Nonce: nonce, TargetBasename: "token-usage"}
	selfExe, _ := writeValidPlanFixture(t, dir, "token-usage", nonce, plan)
	// 在其它目录写一个同名 plan，用它的路径（跨目录注入）。
	otherPlan := filepath.Join(otherDir, filepath.Base(deriveHelperPaths(dir, "token-usage", nonce).Plan))
	if err := writeHelperPlan(otherPlan, plan); err != nil {
		t.Fatalf("write other plan: %v", err)
	}
	if _, err := validateHelperPlan(selfExe, otherPlan); err == nil {
		t.Fatal("跨目录 plan 应被拒绝")
	}
}

// TestValidateHelperPlan_PlanIsSymlink 计划文件是 symlink → 拒绝。
func TestValidateHelperPlan_PlanIsSymlink(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{Nonce: nonce, TargetBasename: "token-usage"}
	paths := deriveHelperPaths(dir, "token-usage", nonce)
	if err := os.WriteFile(paths.Helper, []byte("x"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	// 写真实 plan，再建一个 symlink 指向它，用 symlink 路径调用校验。
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	linkPath := paths.Plan + ".link"
	if err := os.Symlink(paths.Plan, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := validateHelperPlan(paths.Helper, linkPath); err == nil {
		t.Fatal("symlink plan 应被拒绝")
	}
}

// TestValidateHelperPlan_HelperIsSymlink helper 自身是 symlink → 拒绝。
func TestValidateHelperPlan_HelperIsSymlink(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{Nonce: nonce, TargetBasename: "token-usage"}
	paths := deriveHelperPaths(dir, "token-usage", nonce)
	// helper.exe 是 symlink 指向一个真实文件。
	realTarget := filepath.Join(dir, "real-helper")
	if err := os.WriteFile(realTarget, []byte("x"), 0o755); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink(realTarget, paths.Helper); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if _, err := validateHelperPlan(paths.Helper, paths.Plan); err == nil {
		t.Fatal("symlink helper 应被拒绝")
	}
}

// TestValidateHelperPlan_NonceMismatch plan 路径中的 nonce 与 plan 内容 nonce 不一致 → 拒绝。
func TestValidateHelperPlan_NonceMismatch(t *testing.T) {
	dir := t.TempDir()
	nonceA, _ := generateNonce()
	nonceB, _ := generateNonce()
	// helper.exe 与 plan 路径用 nonceA，但 plan 内容写 nonceB。
	paths := deriveHelperPaths(dir, "token-usage", nonceA)
	if err := os.WriteFile(paths.Helper, []byte("x"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	plan := helperPlan{Nonce: nonceB, TargetBasename: "token-usage", Parent: ProcessIdentity{PID: 1, CreationTime: 1}}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if _, err := validateHelperPlan(paths.Helper, paths.Plan); err == nil {
		t.Fatal("nonce 不一致应被拒绝")
	}
}

// TestValidateHelperPlan_MissingNonce plan 缺 nonce → 拒绝。
func TestValidateHelperPlan_MissingNonce(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{TargetBasename: "token-usage"} // 缺 nonce
	selfExe, planPath := writeValidPlanFixture(t, dir, "token-usage", nonce, plan)
	if _, err := validateHelperPlan(selfExe, planPath); err == nil {
		t.Fatal("缺 nonce 应被拒绝")
	}
}

// TestValidateHelperPlan_InvalidNonce nonce 非法（含非 hex）→ 拒绝。
func TestValidateHelperPlan_InvalidNonce(t *testing.T) {
	dir := t.TempDir()
	// 用一个非法 nonce 派生路径（绕过命名），再写非法 nonce 内容。
	badNonce := "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ" // 32 字符但非 hex
	plan := helperPlan{Nonce: badNonce, TargetBasename: "token-usage"}
	paths := deriveHelperPaths(dir, "token-usage", badNonce)
	if err := os.WriteFile(paths.Helper, []byte("x"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	if err := writeHelperPlan(paths.Plan, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if _, err := validateHelperPlan(paths.Helper, paths.Plan); err == nil {
		t.Fatal("非法 nonce 应被拒绝")
	}
}

// TestValidateHelperPlan_TargetBasenameWithSeparator target_basename 含路径分隔符 → 拒绝。
func TestValidateHelperPlan_TargetBasenameWithSeparator(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{Nonce: nonce, TargetBasename: "../evil"}
	// 用合法 basename 派生 helper/plan 路径写文件，但内容 basename 是恶意的。
	selfExe, planPath := writeValidPlanFixture(t, dir, "token-usage", nonce, plan)
	if _, err := validateHelperPlan(selfExe, planPath); err == nil {
		t.Fatal("target_basename 含分隔符应被拒绝")
	}
}

// TestValidateHelperPlan_NearbyPrefixRejected 近似前缀（路径相近但非精确派生）→ 拒绝。
func TestValidateHelperPlan_NearbyPrefixRejected(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	plan := helperPlan{Nonce: nonce, TargetBasename: "token-usage", Parent: ProcessIdentity{PID: 1, CreationTime: 1}}
	paths := deriveHelperPaths(dir, "token-usage", nonce)
	if err := os.WriteFile(paths.Helper, []byte("x"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	// 写一个近似前缀的 plan（命名相近但非精确派生路径）。
	nearby := filepath.Join(dir, ".token-usage.update-plan-FAKE"+nonce)
	if err := writeHelperPlan(nearby, plan); err != nil {
		t.Fatalf("write nearby plan: %v", err)
	}
	if _, err := validateHelperPlan(paths.Helper, nearby); err == nil {
		t.Fatal("近似前缀 plan 应被拒绝")
	}
}

// TestHelperPlan_ParentRoundTrip writeHelperPlan→readHelperPlan 保留 Parent 身份。
func TestHelperPlan_ParentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.json")
	parent := ProcessIdentity{PID: 4242, CreationTime: 0x1234567890abcdef}
	plan := helperPlan{
		Nonce:          "0123456789abcdef0123456789abcdef",
		TargetBasename: "token-usage",
		OldSHA256:      "aa",
		NewSHA256:      "bb",
		WasRunning:     true,
		Parent:         parent,
	}
	if err := writeHelperPlan(planPath, plan); err != nil {
		t.Fatalf("writeHelperPlan: %v", err)
	}
	got, err := readHelperPlan(planPath)
	if err != nil {
		t.Fatalf("readHelperPlan: %v", err)
	}
	if got.Parent != parent {
		t.Errorf("Parent 往返不匹配: got %+v want %+v", got.Parent, parent)
	}
}

// TestValidateHelperPlan_MissingParentIdentity Parent 零值（PID=0 或 CreationTime=0）→ 拒绝。
func TestValidateHelperPlan_MissingParentIdentity(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	for name, parent := range map[string]ProcessIdentity{
		"zero pid":      {PID: 0, CreationTime: 1},
		"zero creation": {PID: 1, CreationTime: 0},
		"both zero":     {},
	} {
		plan := helperPlan{
			Nonce:          nonce,
			TargetBasename: base,
			OldSHA256:      "11",
			NewSHA256:      "22",
			Parent:         parent,
		}
		selfExe, planPath := writeValidPlanFixture(t, dir, base, nonce, plan)
		if _, err := validateHelperPlan(selfExe, planPath); err == nil {
			t.Errorf("%s: 零值 Parent 身份应被拒绝", name)
		}
	}
}

// TestValidateHelperPlan_ParentIdentityPreserved 校验通过后 vplan.Plan.Parent 保留原值。
func TestValidateHelperPlan_ParentIdentityPreserved(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	parent := ProcessIdentity{PID: 7777, CreationTime: 0xdeadbeef}
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: base,
		OldSHA256:      "11",
		NewSHA256:      "22",
		Parent:         parent,
	}
	selfExe, planPath := writeValidPlanFixture(t, dir, base, nonce, plan)
	got, err := validateHelperPlan(selfExe, planPath)
	if err != nil {
		t.Fatalf("validate err=%v", err)
	}
	if got.Plan.Parent != parent {
		t.Errorf("Parent 不匹配: got %+v want %+v", got.Plan.Parent, parent)
	}
}

// TestValidateCleanupPlan_Valid 新 target 与 nonce 绑定的计划匹配时，cleanup 才能取得
// 受控派生路径做删除操作。
func TestValidateCleanupPlan_Valid(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	plan := helperPlan{
		Nonce:          nonce,
		TargetBasename: base,
		Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
	}
	selfExe, planPath := writeValidCleanupPlanFixture(t, dir, base, nonce, plan)

	got, err := ValidateCleanupPlan(selfExe, planPath)
	if err != nil {
		t.Fatalf("ValidateCleanupPlan: %v", err)
	}
	if got.Paths.Target != selfExe {
		t.Errorf("Target=%q want %q", got.Paths.Target, selfExe)
	}
	if got.Paths.Plan != planPath {
		t.Errorf("Plan=%q want %q", got.Paths.Plan, planPath)
	}
}

// TestValidateCleanupPlan_RejectsUnboundInput cleanup 不得用任意可执行文件或计划字段
// 推导删除路径；它必须绑定到本次替换的新 target 和其精确 plan 文件。
func TestValidateCleanupPlan_RejectsUnboundInput(t *testing.T) {
	const base = "token-usage"

	t.Run("different target", func(t *testing.T) {
		dir := t.TempDir()
		nonce, err := generateNonce()
		if err != nil {
			t.Fatalf("generateNonce: %v", err)
		}
		plan := helperPlan{
			Nonce:          nonce,
			TargetBasename: base,
			Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
		}
		_, planPath := writeValidCleanupPlanFixture(t, dir, base, nonce, plan)
		otherTarget := filepath.Join(dir, "other-target")
		if err := os.WriteFile(otherTarget, []byte("other"), 0o755); err != nil {
			t.Fatalf("write other target: %v", err)
		}
		if _, err := ValidateCleanupPlan(otherTarget, planPath); err == nil {
			t.Fatal("非派生 target 应被拒绝")
		}
	})

	t.Run("path separator in target basename", func(t *testing.T) {
		dir := t.TempDir()
		nonce, err := generateNonce()
		if err != nil {
			t.Fatalf("generateNonce: %v", err)
		}
		plan := helperPlan{
			Nonce:          nonce,
			TargetBasename: "../outside",
			Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
		}
		selfExe, planPath := writeValidCleanupPlanFixture(t, dir, base, nonce, plan)
		if _, err := ValidateCleanupPlan(selfExe, planPath); err == nil {
			t.Fatal("含路径分隔符的 target_basename 应被拒绝")
		}
	})

	t.Run("symlink target", func(t *testing.T) {
		dir := t.TempDir()
		nonce, err := generateNonce()
		if err != nil {
			t.Fatalf("generateNonce: %v", err)
		}
		plan := helperPlan{
			Nonce:          nonce,
			TargetBasename: base,
			Parent:         ProcessIdentity{PID: 1, CreationTime: 1},
		}
		paths := deriveHelperPaths(dir, base, nonce)
		realTarget := filepath.Join(dir, "real-target")
		if err := os.WriteFile(realTarget, []byte("target"), 0o755); err != nil {
			t.Fatalf("write real target: %v", err)
		}
		if err := os.Symlink(realTarget, paths.Target); err != nil {
			t.Fatalf("symlink target: %v", err)
		}
		if err := writeHelperPlan(paths.Plan, plan); err != nil {
			t.Fatalf("write plan: %v", err)
		}
		if _, err := ValidateCleanupPlan(paths.Target, paths.Plan); err == nil {
			t.Fatal("符号链接 target 应被拒绝")
		}
	})
}

// TestIsHexNonce nonce 合法性校验。
func TestIsHexNonce(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"abc", false},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", false},                                // 大写
		{"00000000000000000000000000000000", true},                                 // 32 位
		{"0123456789abcdef0123456789abcdef", true},                                 // 32 位
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true}, // 64 位
		{"0123456789abcdef0123456789abcdeg", false},                                // 含非 hex
	}
	for _, c := range cases {
		if got := isHexNonce(c.s); got != c.want {
			t.Errorf("isHexNonce(%q)=%v want %v", c.s, got, c.want)
		}
	}
}

// TestCleanupHelperTempFiles_DeletesNonceBoundFiles 清理 helper 临时文件：
// 删除 helper.exe/plan/stage/backup，保留 result（供下次完整 update 在来源校验通过后消费）。
func TestCleanupHelperTempFiles_DeletesNonceBoundFiles(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, _ := generateNonce()
	paths := deriveHelperPaths(dir, base, nonce)

	for _, p := range []string{paths.Helper, paths.Plan, paths.Stage, paths.Backup, paths.Result} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	if err := CleanupHelperTempFiles(dir, base, nonce); err != nil {
		t.Fatalf("CleanupHelperTempFiles err=%v", err)
	}
	for _, p := range []string{paths.Helper, paths.Plan, paths.Stage, paths.Backup} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("临时文件 %s 应被删除", p)
		}
	}
	// result 应保留（供下次完整 update 在来源校验通过后消费）。
	if _, err := os.Stat(paths.Result); err != nil {
		t.Errorf("result 文件应保留，stat err=%v", err)
	}
}

// TestCleanupHelperTempFiles_RejectsSymlink 清理目标含 symlink → 不跟随、报错，但普通文件仍删。
func TestCleanupHelperTempFiles_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	const base = "token-usage"
	nonce, _ := generateNonce()
	paths := deriveHelperPaths(dir, base, nonce)
	realTarget := filepath.Join(dir, "real")
	if err := os.WriteFile(realTarget, []byte("x"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink(realTarget, paths.Helper); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, p := range []string{paths.Plan, paths.Stage, paths.Backup} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	err := CleanupHelperTempFiles(dir, base, nonce)
	if err == nil {
		t.Fatal("清理 symlink helper 应返回错误")
	}
	for _, p := range []string{paths.Plan, paths.Stage, paths.Backup} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("普通文件 %s 应被删除", p)
		}
	}
}

// TestCleanupHelperTempFiles_Idempotent 不存在的文件视为已清理（幂等）。
func TestCleanupHelperTempFiles_Idempotent(t *testing.T) {
	dir := t.TempDir()
	nonce, _ := generateNonce()
	if err := CleanupHelperTempFiles(dir, "token-usage", nonce); err != nil {
		t.Fatalf("空目录清理应成功，err=%v", err)
	}
}

// TestCleanupHelperTempFiles_RejectsEmptyArgs 空参数 → 错误。
func TestCleanupHelperTempFiles_RejectsEmptyArgs(t *testing.T) {
	for _, args := range [][3]string{{"", "base", "nonce"}, {"dir", "", "nonce"}, {"dir", "base", ""}} {
		if err := CleanupHelperTempFiles(args[0], args[1], args[2]); err == nil {
			t.Errorf("空参数应返回错误: %v", args)
		}
	}
}

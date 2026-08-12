package update

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install_common_test.go 校验事务安装的平台无关部分：
//   - 事务文件精确命名（stage/backup/journal 三类，与 target basename 绑定）；
//   - journal 序列化/反序列化与字段校验；
//   - 精确前缀清理（只删普通文件，跳过目录/symlink/近似前缀）；
//   - 遗留 journal 检测（findLeftoverJournal）。
//
// 这些测试用 t.TempDir 做真实文件操作，不注入 fake——命名/清理是纯 FS 行为，
// 必须测真实 rename/link/remove 才有意义。

// ---- 命名测试 ----

// TestUpdateTempPrefix_NamingConvention 校验三类事务文件的命名遵循约定。
func TestUpdateTempPrefix_NamingConvention(t *testing.T) {
	target := "/opt/bin/token-usage"
	nonce := "abc123"

	if got := updateTempPrefix(target); got != ".token-usage" {
		t.Errorf("updateTempPrefix = %q, want .token-usage", got)
	}
	if got := stageFilePath(target, nonce); !strings.HasSuffix(got, ".token-usage.update-stage-abc123") {
		t.Errorf("stageFilePath = %q, 后缀应为 .token-usage.update-stage-abc123", got)
	}
	if got := backupFilePath(target, nonce); !strings.HasSuffix(got, ".token-usage.update-backup-abc123") {
		t.Errorf("backupFilePath = %q, 后缀应为 .token-usage.update-backup-abc123", got)
	}
	if got := journalFilePath(target, nonce); !strings.HasSuffix(got, ".token-usage.update-journal-abc123") {
		t.Errorf("journalFilePath = %q, 后缀应为 .token-usage.update-journal-abc123", got)
	}
	// 所有文件必须在 target 同目录。
	dir := filepath.Dir(target)
	for _, p := range []string{stageFilePath(target, nonce), backupFilePath(target, nonce), journalFilePath(target, nonce)} {
		if filepath.Dir(p) != dir {
			t.Errorf("事务文件 %q 不在 target 同目录 %q", p, dir)
		}
	}
}

// TestUpdateTempPrefix_DistinctFromFileutil 校验 update 前缀与 fileutil.TempPrefix 完全独立。
func TestUpdateTempPrefix_DistinctFromFileutil(t *testing.T) {
	target := "/opt/bin/token-usage"
	// fileutil 前缀：".token-usage.tmp-"
	// update 前缀：".token-usage.update-stage-" / ".update-backup-" / ".update-journal-"
	prefixes := updateTempPrefixesFor(target)
	for _, p := range prefixes {
		// 不应匹配 fileutil 的 .tmp- 前缀。
		if strings.Contains(p, ".tmp-") {
			t.Errorf("update 前缀 %q 不应包含 fileutil 的 .tmp- 前缀", p)
		}
	}
}

// ---- journal 序列化测试 ----

// TestJournalWriteRead_RoundTrip 校验 journal 写入后能完整读回。
func TestJournalWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".test-journal")
	rec := journalRecord{
		Nonce:          "deadbeef",
		Phase:          phaseInstalled,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-deadbeef",
		BackupBasename: ".token-usage.update-backup-deadbeef",
		OldSHA256:      "aaa",
		NewSHA256:      "bbb",
		WasRunning:     true,
	}
	if err := writeJournal(path, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	got, ok, err := readJournal(path)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if !ok {
		t.Fatal("应读到 journal")
	}
	if got.Nonce != rec.Nonce || got.Phase != rec.Phase || got.TargetBasename != rec.TargetBasename {
		t.Errorf("读回的 journal 不匹配: got %+v want %+v", got, rec)
	}
	if got.OldSHA256 != "aaa" || got.NewSHA256 != "bbb" || !got.WasRunning {
		t.Errorf("读回的 hash/wasRunning 不匹配: got %+v", got)
	}
}

// TestJournalRead_NotExistsReturnsFalse 文件不存在返回 (zero, false, nil)。
func TestJournalRead_NotExistsReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	got, ok, err := readJournal(filepath.Join(dir, ".no-such-journal"))
	if err != nil {
		t.Fatalf("readJournal 不存在的文件应返回 nil err, got %v", err)
	}
	if ok {
		t.Fatal("不存在的 journal 应返回 ok=false")
	}
	if got.Nonce != "" {
		t.Errorf("不存在的 journal 应返回零值, got %+v", got)
	}
}

// TestJournalRead_CorruptReturnsError 损坏的 journal 返回错误（不删，要求人工处理）。
func TestJournalRead_CorruptReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".corrupt-journal")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, ok, err := readJournal(path)
	if err == nil {
		t.Fatal("损坏的 journal 应返回错误")
	}
	if ok {
		t.Fatal("损坏的 journal 应返回 ok=false")
	}
}

// TestJournalRead_MissingFieldsReturnsError 缺少必填字段的 journal 返回错误。
func TestJournalRead_MissingFieldsReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".incomplete-journal")
	// 缺少 nonce 和 target_basename。
	if err := os.WriteFile(path, []byte(`{"phase":"prepared"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, ok, err := readJournal(path)
	if err == nil {
		t.Fatal("缺少必填字段的 journal 应返回错误")
	}
	if ok {
		t.Fatal("缺少必填字段的 journal 应返回 ok=false")
	}
}

// TestJournalUpdatePhase 校验更新 journal 阶段（prepared → installed）。
func TestJournalUpdatePhase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".test-journal")
	rec := journalRecord{
		Nonce:          "n1",
		Phase:          phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  "s",
		BackupBasename: "b",
		OldSHA256:      "old",
		NewSHA256:      "new",
	}
	if err := writeJournal(path, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	if err := updateJournalPhase(path, phaseInstalled); err != nil {
		t.Fatalf("updateJournalPhase: %v", err)
	}
	got, ok, err := readJournal(path)
	if err != nil || !ok {
		t.Fatalf("readJournal err=%v ok=%v", err, ok)
	}
	if got.Phase != phaseInstalled {
		t.Errorf("phase = %q, want installed", got.Phase)
	}
	// 其余字段保持不变。
	if got.Nonce != "n1" || got.OldSHA256 != "old" {
		t.Errorf("更新阶段不应改其它字段: got %+v", got)
	}
}

// TestJournalPermissions_Tightened journal 文件权限为 0600。
func TestJournalPermissions_Tightened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".test-journal")
	rec := journalRecord{Nonce: "n1", Phase: phasePrepared, TargetBasename: "t"}
	if err := writeJournal(path, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("journal 权限 = %o, want 0600", perm)
	}
}

// ---- journal 记录内容校验：不存完整路径 ----

// TestJournalRecord_StoresBasenameOnly 校验 journal 仅存 basename，不存完整路径。
func TestJournalRecord_StoresBasenameOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".test-journal")
	rec := journalRecord{
		Nonce:          "n1",
		Phase:          phasePrepared,
		TargetBasename: "token-usage",
		StageBasename:  ".token-usage.update-stage-n1",
		BackupBasename: ".token-usage.update-backup-n1",
	}
	if err := writeJournal(path, rec); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// 反序列化检查字段。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// basename 字段不应含路径分隔符。
	for _, field := range []string{"target_basename", "stage_basename", "backup_basename"} {
		v := string(raw[field])
		if strings.Contains(v, "/") || strings.Contains(v, "\\") {
			t.Errorf("journal 字段 %s 含路径分隔符: %s", field, v)
		}
	}
}

// ---- 精确前缀清理测试 ----

// TestCleanupUpdateTempByPrefix_DeletesOnlyExactPrefixRegularFiles 校验清理只删精确前缀的普通文件。
func TestCleanupUpdateTempByPrefix_DeletesOnlyExactPrefixRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	prefixes := updateTempPrefixesFor(target)
	// 创建应被删除的文件（精确前缀普通文件）。
	shouldDelete := []string{
		".token-usage.update-stage-n1",
		".token-usage.update-backup-n1",
		".token-usage.update-journal-n1",
	}
	for _, name := range shouldDelete {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	// 创建不应被删除的文件：近似前缀（不同后缀）。
	keepFiles := []string{
		".token-usage.tmp-n1",     // fileutil 前缀，不是 update
		".token-usage.updateX-n1", // 近似但不同（update 后是 X 而非 -）
		"token-usage",             // target 本身
		"other.update-stage-n1",   // 不同 base
	}
	for _, name := range keepFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("y"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if err := cleanupUpdateTempByPrefix(dir, prefixes); err != nil {
		t.Fatalf("cleanupUpdateTempByPrefix: %v", err)
	}
	// shouldDelete 应全部消失。
	for _, name := range shouldDelete {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("精确前缀文件 %s 应被删除, err=%v", name, err)
		}
	}
	// keepFiles 应保留。
	for _, name := range keepFiles {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("非 update 前缀文件 %s 应保留, err=%v", name, err)
		}
	}
}

// TestCleanupUpdateTempByPrefix_SkipsDirectoriesAndSymlinks 校验清理跳过目录和 symlink。
func TestCleanupUpdateTempByPrefix_SkipsDirectoriesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	prefixes := updateTempPrefixesFor(target)
	// 创建一个同前缀的目录：不应被删除。
	dirName := ".token-usage.update-stage-dir"
	if err := os.Mkdir(filepath.Join(dir, dirName), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// 创建一个同前缀的 symlink：不应被删除。
	linkName := ".token-usage.update-stage-link"
	if err := os.Symlink("/dev/null", filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := cleanupUpdateTempByPrefix(dir, prefixes); err != nil {
		t.Fatalf("cleanupUpdateTempByPrefix: %v", err)
	}
	// 目录与 symlink 应保留。
	if _, err := os.Lstat(filepath.Join(dir, dirName)); err != nil {
		t.Errorf("同前缀目录应保留, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, linkName)); err != nil {
		t.Errorf("同前缀 symlink 应保留, err=%v", err)
	}
}

// TestCleanupTransactionFiles_RemovesRegularFiles 校验按精确路径删除 stage/backup/journal。
func TestCleanupTransactionFiles_RemovesRegularFiles(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".s")
	backup := filepath.Join(dir, ".b")
	journal := filepath.Join(dir, ".j")
	for _, p := range []string{stage, backup, journal} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	if err := cleanupTransactionFiles(stage, backup, journal); err != nil {
		t.Fatalf("cleanupTransactionFiles: %v", err)
	}
	for _, p := range []string{stage, backup, journal} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("事务文件 %s 应被删除, err=%v", p, err)
		}
	}
}

// TestCleanupTransactionFiles_IdempotentOnMissing 删除不存在的文件视为成功（幂等）。
func TestCleanupTransactionFiles_IdempotentOnMissing(t *testing.T) {
	dir := t.TempDir()
	err := cleanupTransactionFiles(
		filepath.Join(dir, ".no-stage"),
		filepath.Join(dir, ".no-backup"),
		filepath.Join(dir, ".no-journal"),
	)
	if err != nil {
		t.Errorf("清理不存在的文件应返回 nil, got %v", err)
	}
}

// TestRemoveRegularFile_RejectsDirectory removeRegularFile 拒绝删除目录。
func TestRemoveRegularFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err := removeRegularFile(subDir)
	if err == nil {
		t.Error("removeRegularFile 应拒绝删除目录")
	}
	// 目录应保留。
	if _, err := os.Lstat(subDir); err != nil {
		t.Errorf("目录应保留, err=%v", err)
	}
}

// TestRemoveRegularFile_RejectsSymlink removeRegularFile 拒绝删除 symlink。
func TestRemoveRegularFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err := removeRegularFile(link)
	if err == nil {
		t.Error("removeRegularFile 应拒绝删除 symlink")
	}
	// symlink 应保留。
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink 应保留, err=%v", err)
	}
}

// ---- 遗留 journal 检测测试 ----

// TestFindLeftoverJournal_DetectsJournal 发现遗留 journal 文件。
func TestFindLeftoverJournal_DetectsJournal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	// 创建 target（普通文件）。
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 无 journal 时应返回 false。
	_, found, err := findLeftoverJournal(target)
	if err != nil {
		t.Fatalf("findLeftoverJournal: %v", err)
	}
	if found {
		t.Fatal("无 journal 时应返回 false")
	}
	// 创建一个 journal。
	journal := journalFilePath(target, "n1")
	if err := os.WriteFile(journal, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile journal: %v", err)
	}
	got, found, err := findLeftoverJournal(target)
	if err != nil {
		t.Fatalf("findLeftoverJournal: %v", err)
	}
	if !found {
		t.Fatal("有 journal 时应返回 true")
	}
	if got != journal {
		t.Errorf("findLeftoverJournal = %q, want %q", got, journal)
	}
}

// TestFindLeftoverJournal_SkipsSymlinkAndDir 校验检测跳过 symlink 与目录。
func TestFindLeftoverJournal_SkipsSymlinkAndDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token-usage")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 同前缀的目录与 symlink：不应被检测为 journal。
	if err := os.Mkdir(journalFilePath(target, "dir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	linkPath := journalFilePath(target, "link")
	if err := os.Symlink("/dev/null", linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, found, err := findLeftoverJournal(target)
	if err != nil {
		t.Fatalf("findLeftoverJournal: %v", err)
	}
	if found {
		t.Error("目录与 symlink 不应被检测为 journal")
	}
}

// ---- generateNonce 测试 ----

// TestGenerateNonce_LengthAndHex nonce 为 32 字符 hex。
func TestGenerateNonce_LengthAndHex(t *testing.T) {
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce: %v", err)
	}
	if len(nonce) != 32 {
		t.Errorf("nonce 长度 = %d, want 32", len(nonce))
	}
	for _, c := range nonce {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Errorf("nonce 含非 hex 字符 %q in %s", c, nonce)
		}
	}
}

// TestGenerateNonce_Unique 连续生成的 nonce 应不同（随机性）。
func TestGenerateNonce_Unique(t *testing.T) {
	n1, _ := generateNonce()
	n2, _ := generateNonce()
	if n1 == n2 {
		t.Errorf("连续生成的 nonce 相同: %s", n1)
	}
}

// ---- matchesUpdatePrefix 测试 ----

// TestMatchesUpdatePrefix 精确前缀匹配逻辑。
func TestMatchesUpdatePrefix(t *testing.T) {
	prefixes := []string{".token-usage.update-stage-", ".token-usage.update-backup-"}
	cases := []struct {
		name string
		want bool
	}{
		{".token-usage.update-stage-n1", true},
		{".token-usage.update-backup-n1", true},
		{".token-usage.update-journal-n1", false}, // 不在 prefixes
		{".token-usage.tmp-n1", false},            // fileutil 前缀
		{"token-usage.update-stage-n1", false},    // 缺前导点
		{"", false},                               // 空名
	}
	for _, c := range cases {
		got := matchesUpdatePrefix(c.name, prefixes)
		if got != c.want {
			t.Errorf("matchesUpdatePrefix(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMatchesUpdatePrefix_EmptyPrefixIgnored 空前缀不视为通配符。
func TestMatchesUpdatePrefix_EmptyPrefixIgnored(t *testing.T) {
	prefixes := []string{"", ".token-usage.update-stage-"}
	// 空前缀不应匹配所有文件。
	if matchesUpdatePrefix("anything", prefixes) {
		t.Error("空前缀不应匹配")
	}
	if !matchesUpdatePrefix(".token-usage.update-stage-n1", prefixes) {
		t.Error("非空前缀应匹配")
	}
}

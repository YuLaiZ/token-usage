package fileutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}

func TestWriteCompleteRejectsShortWrite(t *testing.T) {
	if err := writeComplete(shortWriter{}, []byte("payload")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("短写入必须返回 io.ErrShortWrite，实际: %v", err)
	}
}

// errSentinel 是测试用的可识别错误，方便 errors.Is 匹配。
var errSentinel = errors.New("sentinel test error")

// assertNoTempResidue 断言 dir 下没有任何 ".tmp-" 残留普通文件。
func assertNoTempResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("unexpected temp residue: %q", e.Name())
		}
	}
}

// TestReplaceCompleteFile_APIHasNoTempParameter 确认公共 API 不接受调用方传入 temp 路径，
// 调用方只能给出 target/data/mode。
func TestReplaceCompleteFile_APIHasNoTempParameter(t *testing.T) {
	// 仅通过编译期签名占位:函数必须仅有 (target, data, mode) 三参。
	var fn func(target string, data []byte, mode fs.FileMode) error = ReplaceCompleteFile
	_ = fn
}

// TestReplaceCompleteFile_CreatesTempInSameDir 验证 temp 与 target 同目录、同卷。
func TestReplaceCompleteFile_CreatesTempInSameDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")

	// 捕获 CreateTemp 的 dir 参数。
	observedDirs := map[string]bool{}
	ops := defaultReplaceOps()
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		observedDirs[d] = true
		if !strings.HasPrefix(pattern, ".config.toml.tmp-") {
			t.Fatalf("unexpected temp pattern %q", pattern)
		}
		return os.CreateTemp(d, pattern)
	}

	if err := replaceCompleteFileWithOps(target, []byte("hello"), 0o600, ops); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(observedDirs) != 1 || !observedDirs[dir] {
		t.Fatalf("temp not created in target dir %q; observed=%v", dir, observedDirs)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("target content = %q, want %q", got, "hello")
	}
	assertNoTempResidue(t, dir)
}

// TestReplaceCompleteFile_TargetAbsent 替换不存在的目标。
func TestReplaceCompleteFile_TargetAbsent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "absent.toml")
	if err := ReplaceCompleteFile(target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("replace absent target: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
	assertNoTempResidue(t, dir)
}

// TestReplaceCompleteFile_TargetExists 替换已存在的目标,旧内容被整体覆盖。
func TestReplaceCompleteFile_TargetExists(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "exists.toml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ReplaceCompleteFile(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("replace existing target: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	assertNoTempResidue(t, dir)
}

// TestReplaceCompleteFile_PreservesMode 验证最终文件 mode 与传入一致。
func TestReplaceCompleteFile_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mode.toml")
	if err := ReplaceCompleteFile(target, []byte("x"), 0o640); err != nil {
		t.Fatalf("replace: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
}

// failHooks 持有 write/sync/chmod/close 各步的失败注入开关。
// 通过注入到 ops.writeFile/syncFile/chmodFile/closeFile 实现各步失败。
type failHooks struct {
	failWrite bool
	failSync  bool
	failChmod bool
	failClose bool
}

func (h *failHooks) write(f *os.File, data []byte) error {
	if h.failWrite {
		return errSentinel
	}
	_, err := f.Write(data)
	return err
}

func (h *failHooks) sync(f *os.File) error {
	if h.failSync {
		return errSentinel
	}
	return f.Sync()
}

func (h *failHooks) chmod(f *os.File, mode fs.FileMode) error {
	if h.failChmod {
		return errSentinel
	}
	return f.Chmod(mode)
}

func (h *failHooks) close(f *os.File) error {
	// 先真正关闭 fd,确保后续 os.Remove 在 Windows 上不会因 fd 仍打开
	// (ERROR_SHARING_VIOLATION / ACCESS_DENIED) 而失败,保持测试跨平台可移植。
	_ = f.Close()
	if h.failClose {
		return errSentinel
	}
	return nil
}

// runFailurePointTest 在 write/sync/chmod/close/replace 各步注入失败,
// 断言返回错误可被 errors.Is 匹配、temp 被清理、fd 被关闭。
func runFailurePointTest(t *testing.T, inject func(ops *replaceOps, dir string)) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	// 预先存在 target,确保失败路径不影响已存在文件。
	if err := os.WriteFile(target, []byte("preserved"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ops := defaultReplaceOps()
	var tempName string
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(d, pattern)
		if err != nil {
			return nil, err
		}
		tempName = f.Name()
		return f, nil
	}
	inject(&ops, dir)

	err := replaceCompleteFileWithOps(target, []byte("data"), 0o600, ops)
	if err == nil {
		t.Fatalf("expected failure, got nil")
	}
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err not wrapping errSentinel: %v", err)
	}
	// temp 残留必须被清理。
	if tempName != "" {
		if _, statErr := os.Stat(tempName); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("temp residue not cleaned: %q stat=%v", tempName, statErr)
		}
	}
	// 已存在的 target 内容必须保持不变(完整旧值)。
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target after failure: %v", readErr)
	}
	if string(got) != "preserved" {
		t.Fatalf("target corrupted by failed replace: %q", got)
	}
}

// TestReplaceCompleteFile_FailureAtWrite write 失败 → 清理 + 返回错误。
func TestReplaceCompleteFile_FailureAtWrite(t *testing.T) {
	h := &failHooks{failWrite: true}
	runFailurePointTest(t, func(ops *replaceOps, dir string) {
		ops.writeFile = h.write
	})
}

// TestReplaceCompleteFile_FailureAtSync sync 失败 → 清理 + 返回错误。
func TestReplaceCompleteFile_FailureAtSync(t *testing.T) {
	h := &failHooks{failSync: true}
	runFailurePointTest(t, func(ops *replaceOps, dir string) {
		ops.syncFile = h.sync
	})
}

// TestReplaceCompleteFile_FailureAtChmod chmod 失败 → 清理 + 返回错误。
func TestReplaceCompleteFile_FailureAtChmod(t *testing.T) {
	h := &failHooks{failChmod: true}
	runFailurePointTest(t, func(ops *replaceOps, dir string) {
		ops.chmodFile = h.chmod
	})
}

// TestReplaceCompleteFile_FailureAtClose close 失败 → 清理 + 返回错误。
func TestReplaceCompleteFile_FailureAtClose(t *testing.T) {
	h := &failHooks{failClose: true}
	runFailurePointTest(t, func(ops *replaceOps, dir string) {
		ops.closeFile = h.close
	})
}

// TestReplaceCompleteFile_FailureAtReplace replace 失败 → 立即 remove temp + 返回 replace 错误。
func TestReplaceCompleteFile_FailureAtReplace(t *testing.T) {
	runFailurePointTest(t, func(ops *replaceOps, dir string) {
		ops.Replace = func(from, to string) error {
			return errSentinel
		}
	})
}

// TestReplaceCompleteFile_FailureAtCreateTemp CreateTemp 失败直接返回。
func TestReplaceCompleteFile_FailureAtCreateTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.toml")
	ops := defaultReplaceOps()
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		return nil, errSentinel
	}
	err := replaceCompleteFileWithOps(target, []byte("d"), 0o600, ops)
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err = %v, want errSentinel", err)
	}
	assertNoTempResidue(t, dir)
}

// TestReplaceCompleteFile_ReplaceAndRemoveBothFail replace 与 remove 双失败时,
// 用 errors.Join 合并,两个错误都能被 errors.Is 匹配。
func TestReplaceCompleteFile_ReplaceAndRemoveBothFail(t *testing.T) {
	replaceErr := errors.New("replace boom")
	removeErr := errors.New("remove boom")
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")

	ops := defaultReplaceOps()
	var tempPath string
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(d, pattern)
		if err != nil {
			return nil, err
		}
		tempPath = f.Name()
		return f, nil
	}
	ops.Replace = func(from, to string) error { return replaceErr }
	ops.Remove = func(path string) error { return removeErr }

	err := replaceCompleteFileWithOps(target, []byte("d"), 0o600, ops)
	if err == nil {
		t.Fatalf("expected joined error, got nil")
	}
	if !errors.Is(err, replaceErr) {
		t.Fatalf("joined err missing replaceErr: %v", err)
	}
	if !errors.Is(err, removeErr) {
		t.Fatalf("joined err missing removeErr: %v", err)
	}
	// tempPath 在测试替身 Remove 失败语义下不会真的删除,但实现仍应尝试调用。
	if tempPath == "" {
		t.Fatalf("temp path not captured")
	}
	// 清理测试自造残留。
	_ = os.Remove(tempPath)
}

func TestReplaceCompleteFile_WriteAndCleanupFailuresAreJoined(t *testing.T) {
	writeErr := errors.New("write boom")
	closeErr := errors.New("cleanup close boom")
	removeErr := errors.New("cleanup remove boom")
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	ops := defaultReplaceOps()
	var tempFile *os.File
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(d, pattern)
		tempFile = f
		return f, err
	}
	ops.writeFile = func(*os.File, []byte) error { return writeErr }
	ops.closeFile = func(f *os.File) error {
		_ = f.Close()
		return closeErr
	}
	ops.Remove = func(string) error { return removeErr }

	err := replaceCompleteFileWithOps(target, []byte("x"), 0o600, ops)
	for _, want := range []error{writeErr, closeErr, removeErr} {
		if !errors.Is(err, want) {
			t.Fatalf("joined error missing %v: %v", want, err)
		}
	}
	if tempFile != nil {
		_ = os.Remove(tempFile.Name())
	}
}

func TestReplaceCompleteFile_CloseAndRemoveFailuresAreJoined(t *testing.T) {
	closeErr := errors.New("close boom")
	removeErr := errors.New("remove boom")
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	ops := defaultReplaceOps()
	var tempFile *os.File
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(d, pattern)
		tempFile = f
		return f, err
	}
	ops.closeFile = func(f *os.File) error {
		_ = f.Close()
		return closeErr
	}
	ops.Remove = func(string) error { return removeErr }

	err := replaceCompleteFileWithOps(target, []byte("x"), 0o600, ops)
	if !errors.Is(err, closeErr) || !errors.Is(err, removeErr) {
		t.Fatalf("close 主失败与 remove 清理失败都必须保留: %v", err)
	}
	if tempFile != nil {
		_ = os.Remove(tempFile.Name())
	}
}

// TestReplaceCompleteFile_ReplaceFailureStillRemovesTemp replace 失败但 remove 成功,
// temp 被删除。
func TestReplaceCompleteFile_ReplaceFailureStillRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	ops := defaultReplaceOps()
	var tempPath string
	ops.CreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(d, pattern)
		if err != nil {
			return nil, err
		}
		tempPath = f.Name()
		return f, nil
	}
	ops.Replace = func(from, to string) error { return errSentinel }
	// ops.Remove 保持默认 os.Remove。

	err := replaceCompleteFileWithOps(target, []byte("d"), 0o600, ops)
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(tempPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("temp not removed after replace failure: %v", statErr)
	}
}

// TestCleanupKnownTempFiles_DeletesExactPrefixes 只删除调用方给出的精确前缀。
func TestCleanupKnownTempFiles_DeletesExactPrefixes(t *testing.T) {
	dir := t.TempDir()
	keepFile := filepath.Join(dir, ".config.toml.tmp") // 近似名,不匹配前缀
	delFile := filepath.Join(dir, ".config.toml.tmp-abc")
	other := filepath.Join(dir, "config.toml.tmp-xyz") // 前缀不匹配(开头无点)
	for _, p := range []string{keepFile, delFile, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	err := CleanupKnownTempFiles(dir, []string{".config.toml.tmp-"})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// delFile 被删除。
	if _, e := os.Stat(delFile); !errors.Is(e, fs.ErrNotExist) {
		t.Fatalf("delFile should be removed: %v", e)
	}
	// keepFile / other 必须保留。
	for _, p := range []string{keepFile, other} {
		if _, e := os.Stat(p); e != nil {
			t.Fatalf("file %q should remain: %v", p, e)
		}
	}
}

func TestCleanupKnownTempFiles_EmptyPrefixDoesNotMatchEverything(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "important.db")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupKnownTempFiles(dir, []string{""}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("空前缀不得删除任意文件: %v", err)
	}
}

func TestCleanupKnownTempFiles_RejectsEmptyDirectory(t *testing.T) {
	if err := CleanupKnownTempFiles("", []string{".config.toml.tmp-"}); err == nil {
		t.Fatal("空目录必须被拒绝，不能隐式清理当前工作目录")
	}
}

// TestCleanupKnownTempFiles_SkipsDirsAndSymlinks 不删除目录、不跟随 symlink。
func TestCleanupKnownTempFiles_SkipsDirsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	// 匹配前缀的目录应被跳过。
	dirEntry := filepath.Join(dir, ".config.toml.tmp-dir")
	if err := os.Mkdir(dirEntry, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 匹配前缀的 symlink,其 target 是普通文件;不应删除 target,也不应删除 symlink。
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("t"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, ".config.toml.tmp-link")
	if err := os.Symlink(target, link); err != nil {
		// 某些环境禁止 symlink,跳过该断言。
		t.Logf("symlink unsupported: %v", err)
	} else {
		if err := CleanupKnownTempFiles(dir, []string{".config.toml.tmp-"}); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if _, e := os.Stat(link); e != nil {
			t.Fatalf("symlink should remain: %v", e)
		}
		if _, e := os.Stat(target); e != nil {
			t.Fatalf("symlink target should remain: %v", e)
		}
	}
	// 目录应保留。
	if _, e := os.Stat(dirEntry); e != nil {
		t.Fatalf("matching-prefix dir should remain: %v", e)
	}
}

// TestCleanupKnownTempFiles_IgnoresMissingFile 忽略文件不存在。
func TestCleanupKnownTempFiles_IgnoresMissingFile(t *testing.T) {
	dir := t.TempDir()
	// 不创建任何匹配文件。
	if err := CleanupKnownTempFiles(dir, []string{".config.toml.tmp-"}); err != nil {
		t.Fatalf("cleanup on empty dir: %v", err)
	}
}

// TestCleanupKnownTempFiles_RemovesMatchingInTargetDir 验证多前缀清理。
func TestCleanupKnownTempFiles_RemovesMatchingInTargetDir(t *testing.T) {
	dir := t.TempDir()
	prefixes := []string{".config.toml.tmp-", ".token-usage.pid.tmp-"}
	files := []string{
		filepath.Join(dir, ".config.toml.tmp-1"),
		filepath.Join(dir, ".token-usage.pid.tmp-2"),
		filepath.Join(dir, ".token-usage.runtime.json.tmp-3"), // 不在列表
	}
	for _, p := range files {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := CleanupKnownTempFiles(dir, prefixes); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for i, p := range files {
		_, e := os.Stat(p)
		if i < 2 {
			if !errors.Is(e, fs.ErrNotExist) {
				t.Fatalf("file %q should be removed: %v", p, e)
			}
		} else {
			if e != nil {
				t.Fatalf("file %q should remain: %v", p, e)
			}
		}
	}
}

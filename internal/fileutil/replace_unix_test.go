//go:build !windows

package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRenameReplace_UsesOsRename 验证 POSIX 平台默认 Replace 走 os.Rename 同目录替换。
func TestRenameReplace_UsesOsRename(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "from")
	to := filepath.Join(dir, "to")
	if err := os.WriteFile(from, []byte("payload"), 0o600); err != nil {
		t.Fatalf("seed from: %v", err)
	}
	if err := renameReplace(from, to); err != nil {
		t.Fatalf("renameReplace: %v", err)
	}
	if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("from should be renamed away: %v", err)
	}
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("read to: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("to content = %q, want %q", got, "payload")
	}
}

// TestReplaceCompleteFile_DefaultOpsUsesRename 默认 ops 的 Replace 应与 os.Rename 等价:
// 用真实的 target 完成一次替换后 temp 不复存在、target 内容正确。
func TestReplaceCompleteFile_DefaultOpsUsesRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ReplaceCompleteFile(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("replace: %v", err)
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

// TestRenameReplace_SourceMissing os.Rename 在源不存在时返回错误。
func TestRenameReplace_SourceMissing(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "missing")
	to := filepath.Join(dir, "to")
	err := renameReplace(from, to)
	if err == nil {
		t.Fatalf("expected error renaming missing source")
	}
}

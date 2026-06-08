//go:build windows

package fileutil

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestRenameReplace_PassesMoveFileReplaceExisting 验证 Windows wrapper 只传
// MOVEFILE_REPLACE_EXISTING(不使用 ReplaceFileW/MoveFileW/delete+rename 等)。
func TestRenameReplace_PassesMoveFileReplaceExisting(t *testing.T) {
	var capturedFlags uint32
	var capturedFrom, capturedTo string
	moveFileEx = func(from, to *uint16, flags uint32) error {
		capturedFlags = flags
		capturedFrom = windows.UTF16PtrToString(from)
		capturedTo = windows.UTF16PtrToString(to)
		return nil
	}
	t.Cleanup(func() {
		moveFileEx = func(from, to *uint16, flags uint32) error {
			return windows.MoveFileEx(from, to, flags)
		}
	})

	if err := renameReplace(`C:\tmp\from.toml`, `C:\tmp\to.toml`); err != nil {
		t.Fatalf("renameReplace: %v", err)
	}
	if capturedFlags != windows.MOVEFILE_REPLACE_EXISTING {
		t.Fatalf("flags = %#x, want MOVEFILE_REPLACE_EXISTING=%#x",
			capturedFlags, windows.MOVEFILE_REPLACE_EXISTING)
	}
	if capturedFrom != `C:\tmp\from.toml` {
		t.Fatalf("from = %q", capturedFrom)
	}
	if capturedTo != `C:\tmp\to.toml` {
		t.Fatalf("to = %q", capturedTo)
	}
}

// TestMoveFileReplaceFlag_EqualsWindowsConstant 确保 const 与系统常量一致。
func TestMoveFileReplaceFlag_EqualsWindowsConstant(t *testing.T) {
	if moveFileReplaceFlag != windows.MOVEFILE_REPLACE_EXISTING {
		t.Fatalf("moveFileReplaceFlag = %#x, want %#x",
			moveFileReplaceFlag, windows.MOVEFILE_REPLACE_EXISTING)
	}
}

// TestRenameReplace_PropagatesMoveFileExError 替身返回错误时 wrapper 透传。
func TestRenameReplace_PropagatesMoveFileExError(t *testing.T) {
	moveFileEx = func(from, to *uint16, flags uint32) error {
		return errSentinel
	}
	t.Cleanup(func() {
		moveFileEx = func(from, to *uint16, flags uint32) error {
			return windows.MoveFileEx(from, to, flags)
		}
	})
	if err := renameReplace(`a`, `b`); err == nil {
		t.Fatalf("expected error propagation")
	}
}

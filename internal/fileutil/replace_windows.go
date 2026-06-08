//go:build windows

package fileutil

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// moveFileEx 是 windows.MoveFileEx 的可测试间接层(仅 Windows)。
// 默认直接调用系统 API;测试替换为捕获 flags。
var moveFileEx = func(from, to *uint16, flags uint32) error {
	return windows.MoveFileEx(from, to, flags)
}

// moveFileReplaceFlag 是替换现存目标必须使用的 flag。
// 不使用 syscall.MoveFile/syscall.Rename(不能覆盖现存目标)、ReplaceFileW(会合并
// 原文件 metadata/ACL)、MoveFileW 或 Windows os.Rename,也不做 delete+rename
// 非原子 fallback。temp 与 target 由 tempPattern 保证同卷。
const moveFileReplaceFlag = windows.MOVEFILE_REPLACE_EXISTING

// renameReplace 在 Windows 用 MoveFileEx + MOVEFILE_REPLACE_EXISTING 替换目标。
func renameReplace(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return fmt.Errorf("encode from path %q: %w", from, err)
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return fmt.Errorf("encode to path %q: %w", to, err)
	}
	if err := moveFileEx(fromPtr, toPtr, moveFileReplaceFlag); err != nil {
		return fmt.Errorf("movefileex %q -> %q: %w", from, to, err)
	}
	return nil
}

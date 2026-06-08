//go:build !windows

package fileutil

import "os"

// renameReplace 在 POSIX 系统用同目录 os.Rename 做原子替换。
// temp 与 target 由 tempPattern 保证同目录,同卷 rename 原子替换。
func renameReplace(from, to string) error {
	return os.Rename(from, to)
}

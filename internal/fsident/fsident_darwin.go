//go:build darwin

package fsident

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// platformSnapshot 取 (dev, ino) 作为文件实体标识（含设备号防跨设备同 inode 号）。
// stat 失败返回零值快照（调用方按不可用处理）；ino 为 0 视为无效（真实文件
// 不会占用 inode 0），Identity 保持空串。
// 所在文件系统不在可靠白名单（APFS/HFS+）内时 Identity 同样保持空串——
// FAT/exFAT、网络文件系统（smbfs/nfs/webdav 等）与未知类型的文件实体标识
// 不可靠，跳过门在这些文件系统上禁用（每次全读），不确定永远倒向重读。
func platformSnapshot(path string) model.FileSnapshot {
	fi, err := os.Stat(path)
	if err != nil {
		return model.FileSnapshot{}
	}
	snap := model.FileSnapshot{MtimeNS: fi.ModTime().UnixNano(), Size: fi.Size()}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return snap
	}
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(path, &fsStat); err != nil {
		return snap
	}
	fstyp := int8ToString(fsStat.Fstypename[:])
	if !reliableFSType(fstyp) {
		return snap
	}
	snap.Identity = fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	return snap
}

// int8ToString 把 Statfs_t.Fstypename 的 int8 数组转为字符串（NUL 截断）。
func int8ToString(b []int8) string {
	blank := [1]int8{}
	n := 0
	for n < len(b) && b[n] != blank[0] {
		n++
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), n))
}

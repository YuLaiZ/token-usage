//go:build windows

package fsident

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// platformSnapshot 取「卷序列号 + file index」作为文件实体标识。
// file index 需要已打开的文件句柄：这里仅为元数据短暂打开（FILE_READ_ATTRIBUTES，
// 不读文件内容，share 全开减少与写入方的共享冲突）；打开或查询失败、卷序列号或
// file index 为 0 时 Identity 保持空串（调用方按不可用处理，等价 cache miss 全读）。
func platformSnapshot(path string) model.FileSnapshot {
	fi, err := os.Stat(path)
	if err != nil {
		return model.FileSnapshot{}
	}
	snap := model.FileSnapshot{MtimeNS: fi.ModTime().UnixNano(), Size: fi.Size()}

	// 卷可靠性判定基于文件实际所属卷：仅本地固定盘（DRIVE_FIXED）且文件系统名
	// 在白名单（NTFS/ReFS）内才提供实体证据。网络盘（DRIVE_REMOTE，即使暴露为
	// NTFS）、FAT/exFAT 类、目录挂载的独立卷（其宿主盘符可能误判为本地固定盘）
	// 与一切未知驱动器类型，file index 语义不可靠——Identity 保持空（等价全读）。
	if dt, fsName := volumeReliabilityInputs(path); !reliableVolume(dt, fsName) {
		return snap
	}

	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return snap
	}
	handle, err := windows.CreateFile(
		p16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return snap
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return snap
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if info.VolumeSerialNumber == 0 || index == 0 {
		return snap
	}
	snap.Identity = fmt.Sprintf("%d:%d", info.VolumeSerialNumber, index)
	return snap
}

// volumeRootForPath 返回文件实际所属卷的根路径（含尾部反斜杠）。
// 必须用 GetVolumePathNameW 而非盘符级推导：挂载在目录下的独立卷
// （volume mount point，如 C:\mount\fat\ 下的 FAT 卷），其文件路径的盘符属于
// 宿主盘——按盘符取根会把宿主盘的分类（固定盘/NTFS）误用于实际卷。
// 包级变量形态是测试 seam（注入模拟目录挂载场景并锁定下游判定所用根）。
var volumeRootForPath = func(path string) (string, error) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var buf [261]uint16
	if err := windows.GetVolumePathName(p16, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:]), nil
}

// driveTypeForRoot 是 GetDriveType 的包装（测试 seam：注入捕获可靠性判定
// 实际使用的卷根，锁定「不得沿用宿主盘」的解析合同）。
var driveTypeForRoot = func(root16 *uint16) uint32 {
	return windows.GetDriveType(root16)
}

// volumeReliabilityInputs 取文件实际所属卷的驱动器类型与文件系统名。
// 任何获取失败都返回不可靠方向的值（driveNoRootDir / 空 FS 名）。
func volumeReliabilityInputs(path string) (driveType uint32, fsName string) {
	root, err := volumeRootForPath(path)
	if err != nil || root == "" {
		return driveNoRootDir, ""
	}
	root16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return driveNoRootDir, ""
	}
	dt := driveTypeForRoot(root16)
	var fs [64]uint16
	if err := windows.GetVolumeInformation(root16, nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return dt, ""
	}
	return dt, windows.UTF16ToString(fs[:])
}

// Package fsident 提供文件实体证据快照：startup 跳过门用它判定「同一文件实体」。
// Identity 覆盖原子替换类变化（mv/rename 顶替、日志轮转、删除重建都会改变
// dev:ino / file index，即使 mtime 与 size 被恢复）；原地截断覆盖（cp 覆盖、
// truncate+重写）不改变 identity，属跳过门的已知不可检测边界。
//
// Identity 获取失败、值无效或平台不可用时快照的 Identity 为空串；
// 调用方必须按「该文件不推进跳过门（每次全读）」处理，绝不倒向跳过。
package fsident

import (
	"strings"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// SnapshotOfFile 是快照获取入口。包级变量形态是测试 seam：注入替换以模拟
// identity 不可用的文件系统形态（网络盘、FAT 类粗粒度或不稳定 file index）；
// 替换期间相关测试不得并行（全局变量无锁，使用方为串行采集与串行测试）。
// 生产实现按平台分派（fsident_darwin.go / fsident_windows.go / fsident_other.go）。
var SnapshotOfFile = platformSnapshot

// Valid 报告快照的文件实体标识是否可用（非空且非占位值）。
// 不可用的快照一律不得用于跳过门命中判定或门记录写入。
func Valid(s model.FileSnapshot) bool {
	return s.Identity != ""
}

// reliableFSTypes 是「文件实体标识语义可靠」的文件系统类型白名单（小写）。
// 白名单之外（FAT/exFAT 类粗粒度或不稳定 file index、网络文件系统 smbfs/nfs/
// webdav 等、以及任何未知类型）一律不可靠：跳过门在不可靠文件系统上禁用
// （每次全读），不确定永远倒向重读。
var reliableFSTypes = map[string]bool{
	"apfs": true, // macOS Apple File System
	"hfs":  true, // macOS HFS+
	"ntfs": true, // Windows NTFS
	"refs": true, // Windows Resilient File System
}

// reliableFSType 报告文件系统类型名是否在可靠白名单内（大小写与空白不敏感）。
func reliableFSType(name string) bool {
	return reliableFSTypes[strings.ToLower(strings.TrimSpace(name))]
}

// Windows 驱动器类型值（GetDriveType 的返回，与 winbase 约定一致；在此声明
// 供跨平台纯函数判定，避免非 Windows 构建引入 windows 依赖）。
const (
	driveUnknown   uint32 = 0 // DRIVE_UNKNOWN
	driveNoRootDir uint32 = 1 // DRIVE_NO_ROOT_DIR
	driveRemovable uint32 = 2 // DRIVE_REMOVABLE
	driveFixed     uint32 = 3 // DRIVE_FIXED
	driveRemote    uint32 = 4 // DRIVE_REMOTE（网络映射盘/UNC 共享）
)

// reliableVolume 报告 Windows 卷是否可作为文件实体标识的来源：仅本地固定盘
// （DRIVE_FIXED）且文件系统名在白名单内才可靠。网络盘（DRIVE_REMOTE，SMB/NFS
// 映射即使报告 NTFS）与其余驱动器类型（可移动盘、未知/无效根）一律不可靠——
// 其 file index 跨会话不稳定，跳过门必须禁用。
func reliableVolume(driveType uint32, fsName string) bool {
	return driveType == driveFixed && reliableFSType(fsName)
}

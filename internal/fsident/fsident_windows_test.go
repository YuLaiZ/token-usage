//go:build windows

package fsident

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestDirectoryMountUsesActualVolumeRoot：目录挂载卷（volume mount point）下
// 的文件，可靠性判定必须基于文件实际所属卷的根（GetVolumePathName），不得沿用
// 宿主盘的盘符根——否则宿主为本地固定 NTFS 盘时，挂在目录下的 FAT/exFAT 卷
// 会被误判为可靠卷而启用跳过门。
// 注入 volumeRootForPath 模拟「文件实际属于 C:\mount\fat\ 卷」，捕获
// driveTypeForRoot 收到的根参数做合同断言。
func TestDirectoryMountUsesActualVolumeRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	origRoot, origDT := volumeRootForPath, driveTypeForRoot
	t.Cleanup(func() { volumeRootForPath, driveTypeForRoot = origRoot, origDT })

	volumeRootForPath = func(path string) (string, error) {
		// 模拟目录挂载卷：文件路径盘符是宿主 C:，实际卷根是挂载点目录。
		return `C:\mount\fat\`, nil
	}
	var gotRoots []string
	driveTypeForRoot = func(root16 *uint16) uint32 {
		gotRoots = append(gotRoots, windows.UTF16PtrToString(root16))
		return driveRemote // 任意不可靠类型；判定应据此拒绝
	}

	snap := SnapshotOfFile(file)
	if snap.Identity != "" {
		t.Errorf("不可靠卷（按实际卷根判定）不得提供 identity: %+v", snap)
	}
	if len(gotRoots) != 1 || gotRoots[0] != `C:\mount\fat\` {
		t.Fatalf("可靠性判定必须用文件实际所属卷根 %q（不得沿用宿主盘符根）, got %v", `C:\mount\fat\`, gotRoots)
	}
	// MtimeNS/Size 仍来自 stat（identity 无效不影响快照其余字段）。
	if snap.Size != 2 {
		t.Errorf("Size = %d, want 2（stat 元数据不受卷判定影响）", snap.Size)
	}
}

// TestVolumeRootForPathRealVolume：真实环境下 GetVolumePathName 返回的卷根
// 必须非空且以路径分隔符结尾（冒烟：正常本地路径解析合同）。
func TestVolumeRootForPathRealVolume(t *testing.T) {
	dir := t.TempDir()
	root, err := volumeRootForPath(dir)
	if err != nil {
		t.Skipf("GetVolumePathName 失败（环境差异）: %v", err)
	}
	if root == "" {
		t.Fatal("真实路径的卷根不得为空")
	}
	if root[len(root)-1] != '\\' && root[len(root)-1] != '/' {
		t.Fatalf("卷根应以路径分隔符结尾: %q", root)
	}
}

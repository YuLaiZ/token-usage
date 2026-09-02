package fsident

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFsidentSnapshot(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(a, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sa := SnapshotOfFile(a)
	if !Valid(sa) {
		// 项目仅支持 darwin / windows；其余平台 platformSnapshot 按设计恒不给
		// identity（跳过门禁用），identity 相关断言前提不成立。支持平台上真实
		// 文件系统的 identity 失效仍是产品缺陷，保持 Fatal。
		if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
			t.Skipf("平台 %s 不提供文件实体标识（跳过门禁用设计）", runtime.GOOS)
		}
		t.Fatalf("支持平台 %s 的真实文件快照 identity 应有效: %+v", runtime.GOOS, sa)
	}
	if sa.Size != 6 {
		t.Errorf("Size = %d, want 6", sa.Size)
	}
	if sa.MtimeNS == 0 {
		t.Error("MtimeNS = 0, want non-zero")
	}
	// 同文件两次快照一致；不同文件 identity 不同。
	sa2 := SnapshotOfFile(a)
	if sa.Identity != sa2.Identity {
		t.Errorf("同文件 identity 漂移: %q vs %q", sa.Identity, sa2.Identity)
	}
	sb := SnapshotOfFile(b)
	if sa.Identity == sb.Identity {
		t.Errorf("不同文件 identity 相同: %q", sa.Identity)
	}
	// stat 失败返回零值快照且 Valid=false（调用方按不可用处理）。
	if got := SnapshotOfFile(filepath.Join(dir, "missing.jsonl")); Valid(got) {
		t.Errorf("缺失文件快照应无效: %+v", got)
	}
}

// TestReliableFSType：文件系统类型白名单判定——FAT/exFAT、网络文件系统与
// 未知类型一律不可靠（跳过门禁用），已知可靠类型大小写不敏感通过。
func TestReliableFSType(t *testing.T) {
	reliable := []string{"apfs", "hfs", "ntfs", "NTFS", "ReFS", " apfs "}
	for _, name := range reliable {
		if !reliableFSType(name) {
			t.Errorf("reliableFSType(%q) = false, want true", name)
		}
	}
	unreliable := []string{"", "exfat", "FAT32", "msdos", "smbfs", "nfs", "webdav", "cifs", "tmpfs", "unknown-fs"}
	for _, name := range unreliable {
		if reliableFSType(name) {
			t.Errorf("reliableFSType(%q) = true, want false（不可靠类型必须禁用门）", name)
		}
	}
}

// TestReliableVolume：Windows 卷可靠性判定——网络映射盘（DRIVE_REMOTE）即使
// 暴露为 NTFS 也不得提供实体标识（file index 跨会话不稳定）；仅本地固定盘
// （DRIVE_FIXED）+ 白名单文件系统放行；可移动盘与未知/无效根一律拒绝。
func TestReliableVolume(t *testing.T) {
	cases := []struct {
		name      string
		driveType uint32
		fsName    string
		want      bool
	}{
		{"fixed ntfs", driveFixed, "NTFS", true},
		{"fixed refs", driveFixed, "ReFS", true},
		{"remote reports ntfs (SMB share)", driveRemote, "NTFS", false},
		{"remote unknown fs", driveRemote, "", false},
		{"fixed fat32", driveFixed, "FAT32", false},
		{"fixed exfat", driveFixed, "exFAT", false},
		{"removable ntfs", driveRemovable, "NTFS", false},
		{"unknown drive type", driveUnknown, "NTFS", false},
		{"no root dir", driveNoRootDir, "NTFS", false},
	}
	for _, tc := range cases {
		if got := reliableVolume(tc.driveType, tc.fsName); got != tc.want {
			t.Errorf("reliableVolume(%d, %q) = %v, want %v", tc.driveType, tc.fsName, got, tc.want)
		}
	}
}

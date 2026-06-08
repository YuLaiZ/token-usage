// internal/daemon/spawn_test.go
package daemon

import (
	"testing"
)

// TestSpawnOptions_Fields 验证 SpawnOptions 跨平台公共类型的字段赋值。
// 平台专属的 detachedSysProcAttr 断言按 build tag 隔离到 spawn_unix_test.go / spawn_windows_test.go
// （Windows 的 SysProcAttr 无 Setsid 字段、POSIX 的无 CreationFlags 字段，混在无 tag 文件会编译失败）。
func TestSpawnOptions_Fields(t *testing.T) {
	opts := SpawnOptions{BinPath: "/bin/tu", Args: []string{"_run"}, StdoutPath: "/tmp/o.log"}
	if opts.BinPath != "/bin/tu" || len(opts.Args) != 1 {
		t.Error("SpawnOptions 字段应正确赋值")
	}
	if opts.StdoutPath != "/tmp/o.log" {
		t.Errorf("StdoutPath=%q want /tmp/o.log", opts.StdoutPath)
	}
}

// TestSpawnOptions_LeaseFieldNilByDefault Lease 字段默认 nil（无父 lease 的独立 spawn）。
func TestSpawnOptions_LeaseFieldNilByDefault(t *testing.T) {
	opts := SpawnOptions{BinPath: "/bin/tu", Args: []string{"_run"}}
	if opts.Lease != nil {
		t.Error("Lease 字段默认应为 nil（无父 lease 走独立加锁路径）")
	}
}

// TestLeaseSpawnInput_InstanceID 验证 LeaseSpawnInput 的 InstanceID 字段。
func TestLeaseSpawnInput_InstanceID(t *testing.T) {
	li := &LeaseSpawnInput{InstanceID: "inst-xyz"}
	if li.InstanceID != "inst-xyz" {
		t.Errorf("InstanceID=%q want inst-xyz", li.InstanceID)
	}
}

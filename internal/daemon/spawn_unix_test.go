// internal/daemon/spawn_unix_test.go
//go:build !windows

package daemon

import (
	"testing"
)

// TestDetachedSysProcAttr_PosixSetsid 验证 POSIX 平台（darwin/linux）的 detached spawn
// 使用 Setsid 创建新会话脱离控制终端。
func TestDetachedSysProcAttr_PosixSetsid(t *testing.T) {
	attr := detachedSysProcAttr()
	if !attr.Setsid {
		t.Error("POSIX 平台 Setsid 应为 true")
	}
}

// internal/daemon/spawn_windows_test.go
//go:build windows

package daemon

import (
	"testing"
)

func TestDetachedSysProcAttr_WindowsFlags(t *testing.T) {
	attr := detachedSysProcAttr()
	if attr.CreationFlags&windowsCREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("Windows 应含 CREATE_NEW_PROCESS_GROUP")
	}
	if attr.CreationFlags&windowsCREATE_NO_WINDOW == 0 {
		t.Error("Windows 应含 CREATE_NO_WINDOW")
	}
	if attr.CreationFlags&0x00000008 != 0 { // DETACHED_PROCESS
		t.Error("Windows 不应含 DETACHED_PROCESS")
	}
}

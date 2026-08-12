package cli

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/update"
)

// update_helper_test.go 校验 _update-helper / _update-cleanup 命令的平台分派与 Hidden 注册。
//
// 非 Windows 平台这两个命令拒绝执行（POSIX 用同步事务性安装，无需 helper）；
// 命令本身在所有平台注册为 Hidden（不出现在 help / README）。

// TestUpdateHelperCmd_RejectedOnNonWindows 非 Windows 平台执行 helper 命令应被拒绝。
func TestUpdateHelperCmd_RejectedOnNonWindows(t *testing.T) {
	err := runUpdateHelperCmd(context.Background(), "/tmp/plan.json")
	if err == nil {
		t.Fatal("非 Windows 平台应拒绝 helper 命令")
	}
	if !errors.Is(err, errHelperNotSupported) {
		t.Fatalf("应返回 errHelperNotSupported，got %v", err)
	}
}

// TestUpdateCleanupCmd_RejectedOnNonWindows 非 Windows 平台执行 cleanup 命令应被拒绝。
func TestUpdateCleanupCmd_RejectedOnNonWindows(t *testing.T) {
	err := runUpdateCleanupCmd(context.Background(), "/tmp/plan.json", 123, 0)
	if err == nil {
		t.Fatal("非 Windows 平台应拒绝 cleanup 命令")
	}
	if !errors.Is(err, errHelperNotSupported) {
		t.Fatalf("应返回 errHelperNotSupported，got %v", err)
	}
}

// TestCleanupHelperIdentity cleanup 必须收到完整且在 Windows PID 范围内的 helper
// 显式身份，不能因参数遗漏降级为直接清理。
func TestCleanupHelperIdentity(t *testing.T) {
	got, err := cleanupHelperIdentity(123, 456)
	if err != nil {
		t.Fatalf("cleanupHelperIdentity: %v", err)
	}
	want := update.ProcessIdentity{PID: 123, CreationTime: 456}
	if got != want {
		t.Errorf("identity=%+v want %+v", got, want)
	}

	for _, tc := range []struct {
		name         string
		helperPID    int
		creationTime uint64
	}{
		{name: "zero pid", helperPID: 0, creationTime: 1},
		{name: "negative pid", helperPID: -1, creationTime: 1},
		{name: "zero creation time", helperPID: 1, creationTime: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cleanupHelperIdentity(tc.helperPID, tc.creationTime); err == nil {
				t.Fatal("不完整 helper 身份应被拒绝")
			}
		})
	}
	if strconv.IntSize > 32 {
		maxWindowsPID := uint64(^uint32(0))
		if _, err := cleanupHelperIdentity(int(maxWindowsPID+1), 1); err == nil {
			t.Fatal("超出 Windows PID 范围的 helper 身份应被拒绝")
		}
	}
}

// TestUpdateHelperCmd_HiddenAndRegistered 命令注册为 Hidden（不出现在 help）。
func TestUpdateHelperCmd_HiddenAndRegistered(t *testing.T) {
	helper := newUpdateHelperCmd()
	if !helper.Hidden {
		t.Error("_update-helper 应为 Hidden")
	}
	if helper.Flags().Lookup("plan") == nil {
		t.Error("_update-helper 应有 --plan flag")
	}
	cleanup := newUpdateCleanupCmd()
	if !cleanup.Hidden {
		t.Error("_update-cleanup 应为 Hidden")
	}
	if cleanup.Flags().Lookup("plan") == nil {
		t.Error("_update-cleanup 应有 --plan flag")
	}
	if cleanup.Flags().Lookup("helper-pid") == nil {
		t.Error("_update-cleanup 应有 --helper-pid flag")
	}
	if cleanup.Flags().Lookup("helper-creation-time") == nil {
		t.Error("_update-cleanup 应有 --helper-creation-time flag")
	}
}

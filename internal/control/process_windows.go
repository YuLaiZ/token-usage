// internal/control/process_windows.go
//go:build windows

package control

import (
	"fmt"
	"os/exec"
)

// newProductionProcessSignaler 返回 Windows 生产用 processSignaler（taskkill /PID <pid> /F）。
// 禁止按二进制名称杀进程——只对准确 PID 调 taskkill，避免误杀同名进程。
func newProductionProcessSignaler() processSignaler {
	return productionWindowsSignaler{}
}

// productionWindowsSignaler 向准确 PID 调 taskkill /PID <pid> /F。
type productionWindowsSignaler struct{}

func (productionWindowsSignaler) terminate(pid int) error {
	cmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/F")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill 失败: %w（输出: %s）", err, string(out))
	}
	return nil
}

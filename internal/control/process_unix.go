// internal/control/process_unix.go
//go:build !windows

package control

import "syscall"

// newProductionProcessSignaler 返回 POSIX 生产用 processSignaler（SIGTERM）。
func newProductionProcessSignaler() processSignaler {
	return productionPosixSignaler{}
}

// productionPosixSignaler 向准确 PID 发送 SIGTERM（POSIX 优雅退出）。
type productionPosixSignaler struct{}

func (productionPosixSignaler) terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// internal/service/service_other.go
//go:build !darwin && !windows

package service

// unsupportedManager 所有方法返回 ErrPlatformUnsupported。
// 同时实现 AutoStartManager（定义层）与 RuntimeStopper（进程停止层），
// 在不支持平台上均返回 ErrPlatformUnsupported，让调用方映射为非致命说明。
type unsupportedManager struct{}

func newPlatformManager() Manager { return unsupportedManager{} }

func (unsupportedManager) Enable(Options) error      { return ErrPlatformUnsupported }
func (unsupportedManager) Disable(Options) error     { return ErrPlatformUnsupported }
func (unsupportedManager) StopCurrent(Options) error { return ErrPlatformUnsupported }
func (unsupportedManager) Status(opts Options) (AutoStartStatus, error) {
	return AutoStartStatus{}, ErrPlatformUnsupported
}
func (unsupportedManager) Platform() string { return "unsupported" }

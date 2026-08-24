// internal/control/process_service.go
// 把 service.AutoStartManager + service.RuntimeStopper 适配到 control.serviceManagerLike。
// 单独成文：让 control 的 import service 集中在此文件，process.go 保持只 import daemon/config。
package control

import "github.com/YuLaiZ/token-usage/internal/service"

// daemonServiceLabel 与 service.Label 一致（服务标识），独立常量避免 control 直接暴露 service 常量。
const daemonServiceLabel = service.Label

// daemonFallbackLogName 与 service.FallbackLogFileName 一致（daemon 兜底输出文件名，
// 单一真相源在 logger 包）。process.go 经此别名引用，保持 service import 集中在本文件。
const daemonFallbackLogName = service.FallbackLogFileName

// serviceAdapter 把 service.Manager 适配到 control.serviceManagerLike。
//
// service 接口分为：
//   - AutoStartManager（纯 definition：Enable/Disable/Status，Status 返回 AutoStartStatus{Exists,SpecMatches}）
//   - RuntimeStopper（进程停止层：StopCurrent，macOS bootout / Windows taskkill）
//
// platform 只决定 macOS 是否先 best-effort bootout；它不读取 plist/注册表定义状态。
// stopCurrent 仅在 launchd 平台调用。Windows 与普通 POSIX 都由 control.processKill
// 对 Inspect 已取得的准确 PID 发信号，避免二次读 PID 或按名称 fallback。
type serviceAdapter struct {
	mgr service.Manager
}

// newProductionServiceMgr 返回包装 service.New() 的 serviceManagerLike。
func newProductionServiceMgr() serviceManagerLike {
	return &serviceAdapter{mgr: service.New()}
}

func (a *serviceAdapter) platform() string { return a.mgr.Platform() }

func (a *serviceAdapter) stopCurrent(opts serviceOptions) error {
	return a.mgr.StopCurrent(toServiceOptions(opts))
}

// toServiceOptions 把 control.serviceOptions 转回 service.Options。
func toServiceOptions(opts serviceOptions) service.Options {
	return service.Options{
		Label:   service.Label,
		BinPath: opts.BinPath,
		DataDir: opts.DataDir,
		Args:    opts.Args,
	}
}

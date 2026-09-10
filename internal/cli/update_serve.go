// internal/cli/update_serve.go
package cli

// update_serve.go 把 internal/serve 的共用生命周期能力适配为
// update.ServeLifecycle，供 update 命令工厂与 Windows 后台 helper 装配注入。
// 自更新借此保持 dashboard 原运行态：更新前探测并停止（复用 `serve stop` 的
// 同一编排——探活判停、陈旧/损坏条件删除、接管重路由），替换成功后以原监听
// 地址、用新二进制后台恢复（复用 `serve start` 的同一编排）。全部为进程内
// 函数调用：不经过 Cobra、不 spawn CLI 子进程、不经过 shell；恢复路径绝不
// 打开浏览器（浏览器打开只属于 serve start 命令层的 --open）。

import (
	"errors"

	"github.com/YuLaiZ/token-usage/internal/serve"
)

// updateServeLifecycle 是 update.ServeLifecycle 的生产适配器（无状态，零值可用）。
type updateServeLifecycle struct{}

// newUpdateServeLifecycle 构造生产适配器。
func newUpdateServeLifecycle() updateServeLifecycle { return updateServeLifecycle{} }

// DetectRunning 探测 dashboard 是否在运行：读 serve.json 并对记录地址探活。
// 缺失、损坏或陈旧状态一律视为未运行；纯读，绝不修改状态文件。
func (updateServeLifecycle) DetectRunning(dataDir string) (bool, string, error) {
	st, err := serve.ReadState(dataDir)
	if err != nil {
		if errors.Is(err, serve.ErrStateCorrupt) {
			// 损坏状态无法定位实例：按未运行处理，不误判、不清理
			//（清理属于 serve status/stop 命令的既有出口）。
			return false, "", nil
		}
		return false, "", err
	}
	if st == nil {
		return false, "", nil
	}
	if !serve.MetaAlive("http://"+st.Addr, serve.StaleProbeTimeout) {
		// 陈旧状态（SIGKILL/崩溃遗留）：不能误判为运行实例。
		return false, "", nil
	}
	return true, st.Addr, nil
}

// Stop 停止运行中的 dashboard（internal/serve.StopRun 静默编排：探活判停、
// 陈旧/损坏条件删除、接管重路由与 `serve stop` 命令逐字节同一实现）。
func (updateServeLifecycle) Stop(dataDir string) (bool, error) {
	res, err := serve.StopRun(dataDir, nil)
	if err != nil {
		return false, err
	}
	return res.Stopped, nil
}

// Start 以指定二进制、原监听地址后台恢复 dashboard（internal/serve.StartInBackground
// 静默编排：serve-start.lock 串行化、已运行预检、detached spawn `_serve-run --addr`、
// 探活确认）。不含任何浏览器打开逻辑。
func (updateServeLifecycle) Start(dataDir, binPath, addr string) error {
	_, err := serve.StartInBackground(serve.StartOptions{
		DataDir: dataDir,
		Addr:    addr,
		BinPath: binPath,
	})
	return err
}

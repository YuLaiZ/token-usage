// internal/cli/serve_collection_status.go
package cli

// serve 进程的采集状态来源：只读探测守护进程判活与运行阶段，交给
// daemonstatus.Load 与 daemon status / CLI JSON 共用同一判定。只读约束：
// 不创建锁文件（锁文件不存在即未运行）、不改元数据、不写库、不调用控制命令。

import (
	"context"
	"path/filepath"
	"time"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/daemon"
	"github.com/YuLaiZ/token-usage/internal/daemonstatus"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/runmeta"
)

// probeCollectionLock 把 daemon 包的三态只读探测映射为共享判定的 Probe；
// daemon status 与 serve 读端共用（不创建锁文件、不改锁内容）。
func probeCollectionLock(cfg *config.Config) daemonstatus.Probe {
	switch daemon.ProbeDaemonLock(filepath.Join(cfg.DataDir, "token-usage.lock")) {
	case daemon.ProbeRunning:
		return daemonstatus.ProbeRunning
	case daemon.ProbeUnknown:
		return daemonstatus.ProbeUnknown
	default:
		return daemonstatus.ProbeStopped
	}
}

// serveCollectionStatus 组装 serve 读端证据并判定 collection。
func serveCollectionStatus(ctx context.Context, cfg *config.Config, usageDB *db.DB) daemonstatus.Collection {
	probe := probeCollectionLock(cfg)
	snap := daemonstatus.Snapshot{Probe: probe}
	if probe == daemonstatus.ProbeRunning {
		if rst, ok := readRuntimePhase(cfg); ok {
			snap.PhaseAvailable = true
			snap.MonitorReady = rst.MonitorReady
			snap.CatchUp = rst.CatchUp
		}
	}
	return daemonstatus.Load(ctx, cfg.DataDir, snap, usageDB, time.Now())
}

// readRuntimePhase 只读核对 PID 文件与 runtime-state 的 PID+instanceID 全匹配，
// 返回完整运行状态（含 catch_up_failures）——daemon status 与 serve 两个读端
// 共用同一份快照，表格与 JSON 的补采失败计数不丢失。不匹配/缺失返回 ok=false，
// 不据此推断阶段；不创建锁文件、不改元数据。
func readRuntimePhase(cfg *config.Config) (runmeta.RuntimeState, bool) {
	pid, instanceID, err := runmeta.ReadPIDFile(runmeta.PIDPath(cfg.DataDir))
	if err != nil || pid <= 0 || instanceID == "" {
		return runmeta.RuntimeState{}, false
	}
	rst, err := runmeta.ReadRuntimeState(runmeta.StatePath(cfg.DataDir))
	if err != nil || rst.PID != pid || rst.InstanceID != instanceID {
		return runmeta.RuntimeState{}, false
	}
	return rst, true
}

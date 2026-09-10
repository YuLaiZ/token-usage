package update

// reporter.go 定义更新过程事件的对外通知 seam。
//
// Service.Apply 在关键步骤（版本判定、下载、校验、daemon 切换）发射事件，
// 由 CLI 层注入的 Reporter 渲染为用户可见的过程输出；nil Reporter 保持完全
// 静默（向后兼容既有调用方与测试）。事件只描述「动作已开始/进行中/已结束」，
// 最终成败仍以 ApplyResult 与返回 error 为唯一事实源。

// EventKind 标识更新过程事件的类别。
type EventKind int

const (
	// EventVersionsDiscovered 表示 Check 已完成且确有更新，携带当前/目标版本对。
	EventVersionsDiscovered EventKind = iota
	// EventUpdateStart 表示来源校验通过（可信或 --force 生效），即将下载安装。
	EventUpdateStart
	// EventDownloadStart 表示下载阶段开始（含清单拉取）。
	EventDownloadStart
	// EventDownloadProgress 表示下载进度帧，Copied/Total 为累计字节数。
	EventDownloadProgress
	// EventDownloadDone 表示下载阶段成功结束。
	EventDownloadDone
	// EventDownloadFailed 表示下载阶段失败（含清单拉取失败），是进度帧的
	// 确定性终结事件：无论失败发生在哪条路径，都在 downloadStage 返回错误前
	// 发射，保证任何退出路径不留未闭合的进度帧。
	EventDownloadFailed
	// EventVerifyStage 表示正在校验下载产物（stage --version 探针）。
	EventVerifyStage
	// EventStopDaemon 表示替换前正在停止 daemon。
	EventStopDaemon
	// EventStopServe 表示替换前正在停止运行中的 dashboard（update 的
	// dashboard 运行态保持编排：停止 → 替换 → 以原地址恢复）。
	EventStopServe
	// EventInstall 表示正在替换二进制。
	EventInstall
	// EventRestartDaemon 表示替换后正在用新二进制重启 daemon。
	EventRestartDaemon
	// EventStartServe 表示替换后正在以原监听地址、用新二进制恢复后台
	// dashboard。自动恢复绝不打开浏览器。
	EventStartServe
)

// Event 携带一次过程事件的参数；各 Kind 只使用相关字段。
type Event struct {
	Kind       EventKind
	CurrentTag string // EventVersionsDiscovered / EventUpdateStart
	TargetTag  string // EventVersionsDiscovered / EventUpdateStart
	Asset      string // EventDownloadStart
	Copied     int64  // EventDownloadProgress / EventDownloadDone / EventDownloadFailed
	Total      int64  // 同上；<0 表示服务端未给出总大小
}

// Reporter 接收 Service.Apply 执行过程的事件。实现必须自行保证并发安全——
// 当前事件全部在 Apply 的单一 goroutine 顺序流上发射，实现无需加锁。
// Report 不得阻塞（渲染层自行节流），不得因渲染失败 panic 影响更新主流程。
type Reporter interface {
	Report(event Event)
}

// report 是 Service 的事件发射入口：未注入 Reporter 时为 no-op，
// 使既有调用方与测试在零改动下保持原有静默行为。
func (s *Service) report(e Event) {
	if s.Reporter != nil {
		s.Reporter.Report(e)
	}
}

// internal/analyzer/periodic_submitter.go
package analyzer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// periodicSubmitter 是不依赖文件路径的纯定时提交器：进入运行循环即就绪，
// 随后每个 tick 无条件提交固定采集请求（首次数据复核由 startup catch-up
// 承担，定时器从下一个 tick 开始）。
//
// 用于 WorkBuddy 周期全量复核：仅元数据变化（expert/title/is_playground）
// 与源库故障恢复都不产生 JSONL 事件，变化检测模式会永久失去重试机会；纯
// 定时提交使失败在下一周期自然重试。不以 db 或 projects_dir 路径为锚点、
// 不读取 mtime/指纹，只要客户端启用就装配——路径为空或文件暂不存在属于
// 配置/数据故障，由采集端报告，不阻止定时器构造与就绪。
type periodicSubmitter struct {
	clientName  string
	request     collector.CollectRequest
	interval    time.Duration
	submit      MonitorSubmitFunc
	logger      *slog.Logger
	signalReady func() // Analyzer 预置的就绪回调（进入运行循环后恰好一次）
	readyOnce   sync.Once
	stopOnce    sync.Once
	stopCh      chan struct{}
}

func newPeriodicSubmitter(clientName string, request collector.CollectRequest,
	interval time.Duration, submit MonitorSubmitFunc, logger *slog.Logger) *periodicSubmitter {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &periodicSubmitter{
		clientName: clientName,
		request:    request,
		interval:   interval,
		submit:     submit,
		logger:     logger.With("component", "periodic_submitter", "client", clientName),
		stopCh:     make(chan struct{}),
	}
}

// Run 启动定时提交循环：先发一次就绪信号（无初始化 IO），随后每 tick 提交
// 固定请求；提交是预期心跳，日志降 Debug。与 watcher/poller 相同地由父 ctx
// 与 Stop 双通道终止。
func (p *periodicSubmitter) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	p.logger.Debug("starting periodic submitter", "interval", p.interval)
	p.readyOnce.Do(func() {
		if p.signalReady != nil {
			p.signalReady()
		}
	})
	for {
		select {
		case <-ctx.Done():
			p.logger.Debug("periodic submitter stopped")
			return
		case <-p.stopCh:
			p.logger.Debug("periodic submitter stopped")
			return
		case <-ticker.C:
			p.logger.Debug("periodic submit", "request", p.request)
			p.submit(p.clientName, p.request)
		}
	}
}

// Stop 停止提交器（可安全多次调用）。
func (p *periodicSubmitter) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

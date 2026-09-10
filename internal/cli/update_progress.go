package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/YuLaiZ/token-usage/internal/ui"
	"github.com/YuLaiZ/token-usage/internal/update"
)

// update_progress.go 实现 update 命令的过程输出渲染器：把 update.Service 发射的
// 过程事件翻译为面向用户的双语步骤行，并在终端（TTY）上以 \r 单行刷新展示
// 下载进度与平均速度。非终端输出（重定向、管道、测试 buffer）只打步骤行、
// 不写进度帧，保证脚本消费与日志文件干净。
//
// 帧的确定性收尾：进度帧由事件对开启/终结——首个进度帧开启帧态，
// EventDownloadDone / EventDownloadFailed 终结帧态（写终结帧 + 换行）；
// 下载阶段失败在 downloadStage 返回错误前发射 Failed，因此后续输出（原因行、
// 人工安装指引、错误）全部接在完整换行之后，不依赖「后续文本更长可覆盖残影」。
// 0-progress 失败（清单拉取失败等）帧从未开启，终结为 no-op；
// 终结事件只处理首个到达者，后续同型或迟到事件幂等忽略。

// writerIsTerminal 判定输出目标是否交互终端。包级 var 便于测试强制 TTY 分支；
// 生产实现用 go-isatty（含 Windows msys/cygwin pty 形态）。
var writerIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// progressRepaintInterval 是进度帧的最小重绘间隔；终结帧与首帧不受节流。
const progressRepaintInterval = 100 * time.Millisecond

// updateProgressPrinter 把 update.Event 渲染为步骤行与（仅 TTY）进度帧。
// now 为时间源 seam（生产 time.Now，测试注入确定性时钟）；
// repaintInterval 为节流间隔 seam（生产 progressRepaintInterval，测试可置 0 逐帧画）。
type updateProgressPrinter struct {
	out             io.Writer
	tty             bool
	now             func() time.Time
	repaintInterval time.Duration

	// 进度帧状态（仅 TTY 使用）。
	frameOpen   bool      // 已画出首个进度帧且未终结
	frameClosed bool      // 已收到终结事件（Done/Failed 先到者），此后帧事件全部忽略
	lastLineLen int       // 上一帧长度，用于补空格擦除残影
	startedAt   time.Time // 首个进度帧的时间源，平均速度计时起点
	lastCopied  int64
	lastTotal   int64
	lastPaintAt time.Time
	lastPainted int64 // 上一帧画出的 copied，节流的「有变化」判据
}

// newUpdateProgressPrinter 构造渲染器；out 同时承载步骤行与进度帧，
// 保证与最终结果输出同流、帧序一致。
func newUpdateProgressPrinter(out io.Writer) *updateProgressPrinter {
	return &updateProgressPrinter{
		out:             out,
		tty:             writerIsTerminal(out),
		now:             time.Now,
		repaintInterval: progressRepaintInterval,
	}
}

// Report 实现 update.Reporter。任何步骤行输出前先终结未闭合的进度帧（防御纵深），
// 保证步骤行不会接在半帧残影之后。
func (p *updateProgressPrinter) Report(event update.Event) {
	switch event.Kind {
	case update.EventVersionsDiscovered:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi(
			fmt.Sprintf("Current version: %s, target version: %s", event.CurrentTag, event.TargetTag),
			fmt.Sprintf("当前版本 %s，目标版本 %s", event.CurrentTag, event.TargetTag),
		))
	case update.EventUpdateStart:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi(
			fmt.Sprintf("Updating: %s → %s", event.CurrentTag, event.TargetTag),
			fmt.Sprintf("开始更新：%s → %s", event.CurrentTag, event.TargetTag),
		))
	case update.EventDownloadStart:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi(
			fmt.Sprintf("Downloading %s…", event.Asset),
			fmt.Sprintf("正在下载 %s…", event.Asset),
		))
	case update.EventDownloadProgress:
		p.paintProgress(event.Copied, event.Total, false)
	case update.EventDownloadDone:
		p.paintProgress(event.Copied, event.Total, true)
	case update.EventDownloadFailed:
		p.paintProgress(event.Copied, event.Total, true)
	case update.EventVerifyStage:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Verifying the downloaded asset…", "正在校验下载产物…"))
	case update.EventStopDaemon:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Stopping daemon…", "正在停止 daemon…"))
	case update.EventStopServe:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Stopping the dashboard…", "正在停止 dashboard…"))
	case update.EventInstall:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Installing the new version…", "正在安装新版本…"))
	case update.EventRestartDaemon:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Restarting daemon…", "正在重启 daemon…"))
	case update.EventStartServe:
		p.ensureFrameClosed()
		fmt.Fprintln(p.out, ui.Bi("Restoring the dashboard…", "正在恢复 dashboard…"))
	}
}

// paintProgress 处理一帧进度：非 TTY 全部 no-op（无帧可画）；
// final=true 表示终结帧（Done/Failed 携带），不受节流且必写换行收尾。
// 终结事件只处理首个到达者：frameClosed 后所有帧事件忽略。
// 帧从未开启（0-progress，如清单拉取失败）时终结为 no-op——只置 closed
// 保证迟到事件幂等，不写出多余的 Copied=0 帧。
func (p *updateProgressPrinter) paintProgress(copied, total int64, final bool) {
	if !p.tty || p.frameClosed {
		return
	}
	if final && !p.frameOpen {
		p.frameClosed = true
		return
	}
	now := p.now()
	if !p.frameOpen {
		// 首帧：开启帧态并记录速度计时起点。
		p.frameOpen = true
		p.startedAt = now
		p.lastCopied, p.lastTotal = copied, total
	} else {
		p.lastCopied, p.lastTotal = copied, total
		if !final {
			// 节流：距上一帧不足间隔、或字节数无变化时跳过重绘。
			if now.Sub(p.lastPaintAt) < p.repaintInterval || copied == p.lastPainted {
				return
			}
		}
	}
	frame := formatProgressLine(copied, total, now.Sub(p.startedAt))
	line := "  " + frame
	// 右端补空格擦除上一帧可能更长的残影。
	if pad := p.lastLineLen - len(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	fmt.Fprint(p.out, "\r"+line)
	p.lastLineLen = len(line)
	p.lastPaintAt = now
	p.lastPainted = copied
	if final {
		fmt.Fprint(p.out, "\n")
		p.frameClosed = true
	}
}

// ensureFrameClosed 在输出任何步骤行前终结未闭合的进度帧；
// 帧从未开启或已终结时为 no-op。
func (p *updateProgressPrinter) ensureFrameClosed() {
	if p.frameOpen && !p.frameClosed {
		fmt.Fprint(p.out, "\n")
		p.frameClosed = true
	}
}

// formatProgressLine 渲染纯数字进度行（无语言文字，双语用户通用）：
// 已知总大小 `45% 10.8/24.0 MB 1.2 MB/s`；未知总大小 `10.8 MB 1.2 MB/s`。
// elapsed 为速度计时区间（自首个进度帧起）；elapsed<=0 时不显示速度（首帧无意义）。
func formatProgressLine(copied, total int64, elapsed time.Duration) string {
	var frame string
	if total > 0 {
		pct := int(float64(copied) / float64(total) * 100)
		frame = fmt.Sprintf("%d%% %s/%s", pct, formatByteSize(copied), formatByteSize(total))
	} else {
		frame = formatByteSize(copied)
	}
	if elapsed > 0 {
		sec := elapsed.Seconds()
		frame += " " + formatByteSize(int64(float64(copied)/sec)) + "/s"
	}
	return frame
}

// formatByteSize 把字节数格式化为 1024 进制的人类可读形态：
// 512 B / 10.8 KB / 24.0 MB / 1.2 GB；KB 及以上保留 1 位小数（上限 GB）。
func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 2; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/update"
)

// update_progress_test.go 校验 update 命令过程输出的渲染合同：
//   - 字节/进度行纯函数格式（1024 进制、百分比、平均速度、未知总大小）；
//   - TTY 分支的 \r 帧序、终结换行、Done/Failed 幂等与防御性收帧；
//   - 非 TTY 分支无任何进度帧、只有步骤行；
//   - 命令层的「正在检查更新…」前置行（--check 与 Apply 一致）。

// forceTTYWriter 覆盖 writerIsTerminal seam，测试结束恢复。
func forceTTYWriter(t *testing.T, tty bool) {
	t.Helper()
	orig := writerIsTerminal
	writerIsTerminal = func(w io.Writer) bool { return tty }
	t.Cleanup(func() { writerIsTerminal = orig })
}

func TestFormatByteSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10 * 1024 * 1024, "10.0 MB"},
		{25899683, "24.7 MB"}, // 24.7 * 1024 * 1024 四舍五入
		{1288490189, "1.2 GB"},
	}
	for _, c := range cases {
		if got := formatByteSize(c.in); got != c.want {
			t.Errorf("formatByteSize(%d)=%q，want %q", c.in, got, c.want)
		}
	}
}

func TestFormatProgressLine(t *testing.T) {
	const mib = 1024 * 1024
	cases := []struct {
		name    string
		copied  int64
		total   int64
		elapsed time.Duration
		want    string
	}{
		{"known total with speed", 10 * mib, 24 * mib, 10 * time.Second, "41% 10.0 MB/24.0 MB 1.0 MB/s"},
		{"zero elapsed omits speed", 10 * mib, 24 * mib, 0, "41% 10.0 MB/24.0 MB"},
		{"unknown total", 10*mib + 512*1024, -1, 5 * time.Second, "10.5 MB 2.1 MB/s"},
		{"full", 24 * mib, 24 * mib, 12 * time.Second, "100% 24.0 MB/24.0 MB 2.0 MB/s"},
	}
	for _, c := range cases {
		if got := formatProgressLine(c.copied, c.total, c.elapsed); got != c.want {
			t.Errorf("%s: formatProgressLine(%d,%d,%s)=%q，want %q", c.name, c.copied, c.total, c.elapsed, got, c.want)
		}
	}
}

// newPrinterForTest 构造接 buffer 的渲染器并注入确定性时钟（返回可变句柄）与
// 零节流间隔（逐帧画，断言不受真实时间影响）。
func newPrinterForTest(t *testing.T, buf *bytes.Buffer, tty bool, base time.Time) (*updateProgressPrinter, *time.Time) {
	t.Helper()
	forceTTYWriter(t, tty)
	p := newUpdateProgressPrinter(buf)
	now := base
	p.now = func() time.Time { return now }
	p.repaintInterval = 0
	return p, &now
}

// TestUpdateProgressPrinterTTYStepsAndFrames TTY 分支：步骤行齐全、\r 帧
// 含百分比与速度、终结帧换行收尾、迟到的终结事件幂等忽略。
func TestUpdateProgressPrinterTTYStepsAndFrames(t *testing.T) {
	var buf bytes.Buffer
	base := time.Unix(0, 0)
	p, clock := newPrinterForTest(t, &buf, true, base)

	p.Report(update.Event{Kind: update.EventVersionsDiscovered, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventUpdateStart, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	// 首帧 elapsed=0 不显速度；后续帧速度 = copied/elapsed。
	*clock = base
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 5, Total: 24})
	*clock = base.Add(time.Second)
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 12, Total: 24})
	*clock = base.Add(2 * time.Second)
	p.Report(update.Event{Kind: update.EventDownloadDone, Copied: 24, Total: 24})
	p.Report(update.Event{Kind: update.EventVerifyStage})
	p.Report(update.Event{Kind: update.EventStopDaemon})
	p.Report(update.Event{Kind: update.EventInstall})
	p.Report(update.Event{Kind: update.EventRestartDaemon})

	out := buf.String()
	for _, want := range []string{
		"Current version: v0.1.6, target version: v0.1.7 / 当前版本 v0.1.6，目标版本 v0.1.7",
		"Updating: v0.1.6 → v0.1.7 / 开始更新：v0.1.6 → v0.1.7",
		"Downloading token-usage-darwin-arm64… / 正在下载 token-usage-darwin-arm64…",
		"Verifying the downloaded asset… / 正在校验下载产物…",
		"Stopping daemon… / 正在停止 daemon…",
		"Installing the new version… / 正在安装新版本…",
		"Restarting daemon… / 正在重启 daemon…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应包含步骤行 %q，got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\r") {
		t.Error("TTY 分支应包含 \\r 进度帧")
	}
	for _, want := range []string{"20% 5 B/24 B", "50% 12 B/24 B", "100% 24 B/24 B 12 B/s"} {
		if !strings.Contains(out, want) {
			t.Errorf("进度帧应包含 %q，got:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("输出应以换行收尾")
	}

	// 迟到终结事件（Done 之后又来 Failed）幂等忽略，不产生新输出。
	before := len(out)
	p.Report(update.Event{Kind: update.EventDownloadFailed, Copied: 24, Total: 24})
	if len(buf.String()) != before {
		t.Errorf("终结事件之后的迟到 Failed 应被忽略，输出从 %d 增至 %d", before, len(buf.String()))
	}
}

// TestUpdateProgressPrinterNonTTYStepsOnly 非 TTY：无任何 \r 帧，步骤行照常。
func TestUpdateProgressPrinterNonTTYStepsOnly(t *testing.T) {
	var buf bytes.Buffer
	p, _ := newPrinterForTest(t, &buf, false, time.Unix(0, 0))

	p.Report(update.Event{Kind: update.EventVersionsDiscovered, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventUpdateStart, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 5, Total: 24})
	p.Report(update.Event{Kind: update.EventDownloadDone, Copied: 24, Total: 24})

	out := buf.String()
	if strings.ContainsAny(out, "\r%") {
		t.Errorf("非 TTY 不应有进度帧（\\r 或 %%），got:\n%s", out)
	}
	if !strings.Contains(out, "Downloading token-usage-darwin-arm64… / 正在下载 token-usage-darwin-arm64…") {
		t.Errorf("非 TTY 应保留步骤行，got:\n%s", out)
	}
}

// TestUpdateProgressPrinterFailedFrameEndsWithNewline 下载失败：
// 终结帧以换行收尾，后续输出接在完整换行之后（不依赖后续文本覆盖残影）。
func TestUpdateProgressPrinterFailedFrameEndsWithNewline(t *testing.T) {
	var buf bytes.Buffer
	base := time.Unix(0, 0)
	p, clock := newPrinterForTest(t, &buf, true, base)

	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	*clock = base.Add(time.Second)
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 5, Total: 24})
	p.Report(update.Event{Kind: update.EventDownloadFailed, Copied: 5, Total: 24})
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("失败终结帧应以换行收尾，got:\n%q", buf.String())
	}

	// 后续步骤行（防御性收帧后的输出）接在换行之后，不再有悬空半帧。
	p.Report(update.Event{Kind: update.EventVerifyStage})
	out := buf.String()
	if !strings.Contains(out, "\nVerifying the downloaded asset… / 正在校验下载产物…") {
		t.Errorf("后续步骤行应接在完整换行后，got:\n%q", out)
	}
}

// TestUpdateProgressPrinterDefensiveCloseBeforeStep 防御纵深：进度帧未收到
// 终结事件就来了下一行步骤行（理论上不该发生），printer 先收帧再打步骤行。
func TestUpdateProgressPrinterDefensiveCloseBeforeStep(t *testing.T) {
	var buf bytes.Buffer
	p, _ := newPrinterForTest(t, &buf, true, time.Unix(0, 0))

	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 5, Total: 24})
	p.Report(update.Event{Kind: update.EventStopDaemon})

	out := buf.String()
	if !strings.Contains(out, "\nStopping daemon… / 正在停止 daemon…") {
		t.Errorf("步骤行前应先终结进度帧（换行分隔），got:\n%q", out)
	}
}

// TestUpdateCmdCheckingLineBothPaths 「正在检查更新…」前置行在 --check 与
// 默认 Apply 两路径都输出（服务调用前直印，不依赖服务事件）。
func TestUpdateCmdCheckingLineBothPaths(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"check", []string{"update", "--check"}},
		{"apply", []string{"update"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			stub := &stubUpdateService{
				checkResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0"},
				applyResult: update.ApplyResult{CheckResult: update.CheckResult{CurrentTag: "v0.1.0", TargetTag: "v0.1.0"}},
			}
			withStubUpdateService(t, stub)
			root := newRootCmd(fixedInfo)
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(c.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("%s 应退出 0，got %v", c.name, err)
			}
			if !strings.Contains(out.String(), "Checking for updates… / 正在检查更新…") {
				t.Errorf("%s 输出应包含检查前置行，got:\n%s", c.name, out.String())
			}
		})
	}
}

// TestUpdateProgressPrinterZeroProgressFinalIsNoop TTY 下 0-progress 终态
// （清单拉取失败等，帧从未开启）：终结事件不写出任何帧，只置幂等位。
func TestUpdateProgressPrinterZeroProgressFinalIsNoop(t *testing.T) {
	var buf bytes.Buffer
	p, _ := newPrinterForTest(t, &buf, true, time.Unix(0, 0))

	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	p.Report(update.Event{Kind: update.EventDownloadFailed, Copied: 0, Total: -1})
	out := buf.String()
	if out != "Downloading token-usage-darwin-arm64… / 正在下载 token-usage-darwin-arm64…\n" {
		t.Errorf("0-progress 终态不应写出帧，got:\n%q", out)
	}
	// 迟到 Progress/Done 也全部忽略。
	before := len(out)
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 5, Total: 24})
	p.Report(update.Event{Kind: update.EventDownloadDone, Copied: 24, Total: 24})
	if len(buf.String()) != before {
		t.Errorf("终结后的迟到帧事件应被忽略，输出从 %d 增至 %d", before, len(buf.String()))
	}
}

// TestUpdateProgressPrinterStepLineOrder 步骤行按事件顺序输出（Index 递增）。
func TestUpdateProgressPrinterStepLineOrder(t *testing.T) {
	var buf bytes.Buffer
	p, clock := newPrinterForTest(t, &buf, true, time.Unix(0, 0))

	p.Report(update.Event{Kind: update.EventVersionsDiscovered, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventUpdateStart, CurrentTag: "v0.1.6", TargetTag: "v0.1.7"})
	p.Report(update.Event{Kind: update.EventDownloadStart, Asset: "token-usage-darwin-arm64"})
	*clock = time.Unix(0, 0).Add(time.Second)
	p.Report(update.Event{Kind: update.EventDownloadProgress, Copied: 12, Total: 24})
	p.Report(update.Event{Kind: update.EventDownloadDone, Copied: 24, Total: 24})
	p.Report(update.Event{Kind: update.EventVerifyStage})
	p.Report(update.Event{Kind: update.EventStopDaemon})
	p.Report(update.Event{Kind: update.EventInstall})
	p.Report(update.Event{Kind: update.EventRestartDaemon})

	out := buf.String()
	steps := []string{
		"Current version: v0.1.6",
		"Updating: v0.1.6",
		"Downloading token-usage-darwin-arm64",
		"Verifying the downloaded asset",
		"Stopping daemon",
		"Installing the new version",
		"Restarting daemon",
	}
	last := -1
	for _, s := range steps {
		idx := strings.Index(out, s)
		if idx < 0 {
			t.Fatalf("缺少步骤行 %q，got:\n%s", s, out)
		}
		if idx < last {
			t.Errorf("步骤行 %q 顺序错乱（idx=%d < last=%d）", s, idx, last)
		}
		last = idx
	}
}

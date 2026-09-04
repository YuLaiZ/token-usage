package update

import (
	"context"
	"errors"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

// reporter_test.go 校验 Service.Apply 的过程事件发射合同：
//   - 可信全流程（daemon 运行中）的事件顺序与字段；
//   - 不可信 / 无更新路径不发「开始更新」与下载事件；
//   - 下载失败以 EventDownloadFailed 确定性收尾（hash 失败携带真实 Copied/Total，
//     清单缺陷为 0-progress 终态），任何路径不出现「DownloadStart 后无终态事件」。

// recordingReporter 记录全部事件供序列与字段断言。
type recordingReporter struct {
	events []Event
}

func (r *recordingReporter) Report(e Event) { r.events = append(r.events, e) }

func (r *recordingReporter) kinds() []EventKind {
	kinds := make([]EventKind, 0, len(r.events))
	for _, e := range r.events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func (r *recordingReporter) countOf(kind EventKind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func (r *recordingReporter) first(kind EventKind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == kind {
			return e, true
		}
	}
	return Event{}, false
}

// assertEventSequence 断言事件种类序列精确等于 want（DownloadProgress 允许多帧，
// 以通配 marker 表示）。
func assertEventSequence(t *testing.T, got []EventKind, want []EventKind) {
	t.Helper()
	i := 0
	for _, w := range want {
		if w == EventDownloadProgress {
			// 连续 Progress 帧折叠为一项。
			for i < len(got) && got[i] == EventDownloadProgress {
				i++
			}
			continue
		}
		if i >= len(got) || got[i] != w {
			t.Fatalf("事件序列不匹配：want[%d:]=%v，got=%v（对齐到 %d）", i, want, got, i)
		}
		i++
	}
	if i != len(got) {
		t.Fatalf("事件序列尾部多出事件：got=%v want=%v", got, want)
	}
}

// TestApply_ReporterTrustedRunningSequence 可信来源 + daemon 原本运行 + 真实下载：
// 事件顺序 VersionsDiscovered → UpdateStart → DownloadStart → Progress* → DownloadDone
// → VerifyStage → StopDaemon → Install → RestartDaemon，关键字段随事件携带。
func TestApply_ReporterTrustedRunningSequence(t *testing.T) {
	currentBin := []byte("current-official-bin")
	targetBin := []byte("target-official-bin-v0.2.0")

	manifests := map[string][]byte{
		"/v0.1.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))),
		"/v0.2.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex(targetBin))),
	}
	binaries := map[string][]byte{"v0.2.0": targetBin}
	srv := newApplyDownloadServer(t, manifests, binaries)
	d := newTestDownloader(t, srv)

	svc := makeService(t)
	svc.ProvenanceDeps.Manifest = d
	svc.AssetDownloader = d
	svc.VersionProbe = staticVersionProbe("v0.2.0", nil)
	sess := &fakeControlSession{state: control.RuntimeState{Running: true, PID: 42}}
	svc.ControlManager = &fakeControlManager{session: sess}
	svc.Installer = newFakeInstaller()
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load
	reporter := &recordingReporter{}
	svc.Reporter = reporter

	if _, err := svc.Apply(context.Background(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply err=%v", err)
	}

	assertEventSequence(t, reporter.kinds(), []EventKind{
		EventVersionsDiscovered, EventUpdateStart, EventDownloadStart,
		EventDownloadProgress, EventDownloadDone, EventVerifyStage,
		EventStopDaemon, EventInstall, EventRestartDaemon,
	})

	if vd, ok := reporter.first(EventVersionsDiscovered); !ok {
		t.Fatal("缺少 EventVersionsDiscovered")
	} else {
		if vd.CurrentTag != "v0.1.0" || vd.TargetTag != "v0.2.0" {
			t.Errorf("VersionsDiscovered 版本字段 got %q → %q，want v0.1.0 → v0.2.0", vd.CurrentTag, vd.TargetTag)
		}
	}
	if us, ok := reporter.first(EventUpdateStart); !ok {
		t.Fatal("缺少 EventUpdateStart")
	} else if us.CurrentTag != "v0.1.0" || us.TargetTag != "v0.2.0" {
		t.Errorf("UpdateStart 版本字段 got %q → %q", us.CurrentTag, us.TargetTag)
	}
	if ds, ok := reporter.first(EventDownloadStart); !ok {
		t.Fatal("缺少 EventDownloadStart")
	} else if ds.Asset != "token-usage-darwin-arm64" {
		t.Errorf("DownloadStart.Asset=%q，want token-usage-darwin-arm64", ds.Asset)
	}
	if dd, ok := reporter.first(EventDownloadDone); !ok {
		t.Fatal("缺少 EventDownloadDone")
	} else {
		if dd.Copied != int64(len(targetBin)) {
			t.Errorf("DownloadDone.Copied=%d，want %d", dd.Copied, len(targetBin))
		}
		if dd.Total != int64(len(targetBin)) {
			t.Errorf("DownloadDone.Total=%d，want %d（httptest 应携带 Content-Length）", dd.Total, len(targetBin))
		}
	}
	// 序列折叠允许 Progress 零帧，这里显式锚定 ≥1：若 Service 停止转发下载器的
	// progress 回调（但仍发 Done），TTY 实时进度将失去回归保护。
	if n := reporter.countOf(EventDownloadProgress); n < 1 {
		t.Errorf("真实下载必须转发至少一帧 Progress（Service→downloader 回调接线），got %d 帧", n)
	}
}

// TestApply_ReporterUntrustedStopsBeforeUpdateStart 来源不可信：
// 版本对比行已亮出（VersionsDiscovered），但不发 UpdateStart 与任何下载/安装事件。
func TestApply_ReporterUntrustedStopsBeforeUpdateStart(t *testing.T) {
	svc := makeService(t)
	// 覆盖为与二进制不匹配的清单 → provenance 不可信。
	svc.ProvenanceDeps.Manifest = staticManifestFetcher(buildSumsBody("token-usage-darwin-arm64", sumHex([]byte("other-bin"))))
	reporter := &recordingReporter{}
	svc.Reporter = reporter

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.ProvenanceTrusted {
		t.Fatal("前置失败：本用例应为不可信来源")
	}

	if _, ok := reporter.first(EventVersionsDiscovered); !ok {
		t.Error("不可信路径应已发射 VersionsDiscovered（版本信息先于来源校验亮出）")
	}
	for _, kind := range []EventKind{EventUpdateStart, EventDownloadStart, EventDownloadProgress, EventDownloadDone, EventDownloadFailed, EventVerifyStage, EventStopDaemon, EventInstall, EventRestartDaemon} {
		if n := reporter.countOf(kind); n != 0 {
			t.Errorf("不可信路径不应发射 %v，got %d 次", kind, n)
		}
	}
}

// TestApply_ReporterNoUpdateEmitsNoVersionEvents 无更新：两个版本事件均不发射
// （「已是最新版本」结果行已携带版本，过程行不重复）。
func TestApply_ReporterNoUpdateEmitsNoVersionEvents(t *testing.T) {
	svc := makeService(t)
	rc := svc.ReleaseClient.(*fakeReleaseClient)
	rc.byTag[""] = makeCurrentRelease("v0.1.0")
	rc.release = makeCurrentRelease("v0.1.0")
	reporter := &recordingReporter{}
	svc.Reporter = reporter

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if got.UpdateAvailable {
		t.Fatal("前置失败：本用例应无更新")
	}
	if n := len(reporter.events); n != 0 {
		t.Errorf("无更新路径不应发射任何事件，got %v", reporter.kinds())
	}
}

// TestApply_ReporterHashMismatchProgressThenFailed 目标 hash 错误：
// 下载全程有 Progress，失败以 DownloadFailed 收尾并携带最终 Copied/Total，无 DownloadDone。
func TestApply_ReporterHashMismatchProgressThenFailed(t *testing.T) {
	currentBin := []byte("current-official-bin")
	targetBin := []byte("target-official-bin-v0.2.0")

	manifests := map[string][]byte{
		"/v0.1.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))),
		// 目标版本清单给出错误 hash：下载完成后 hash 校验失败。
		"/v0.2.0": []byte(buildSumsBody("token-usage-darwin-arm64", sumHex([]byte("wrong-bin")))),
	}
	binaries := map[string][]byte{"v0.2.0": targetBin}
	srv := newApplyDownloadServer(t, manifests, binaries)
	d := newTestDownloader(t, srv)

	svc := makeService(t)
	svc.ProvenanceDeps.Manifest = d
	svc.AssetDownloader = d
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	svc.Installer = newFakeInstaller()
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load
	reporter := &recordingReporter{}
	svc.Reporter = reporter

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply 不应返回 error（hash 失败是领域结果），err=%v", err)
	}
	if got.ReadyToInstall {
		t.Fatal("前置失败：hash 失败应 ReadyToInstall=false")
	}

	assertEventSequence(t, reporter.kinds(), []EventKind{
		EventVersionsDiscovered, EventUpdateStart, EventDownloadStart,
		EventDownloadProgress, EventDownloadFailed,
	})
	if df, ok := reporter.first(EventDownloadFailed); !ok {
		t.Fatal("hash 失败必须发射 DownloadFailed（进度帧确定性收尾）")
	} else {
		if df.Copied != int64(len(targetBin)) {
			t.Errorf("DownloadFailed.Copied=%d，want %d（完整下载后校验失败）", df.Copied, len(targetBin))
		}
		if df.Total != int64(len(targetBin)) {
			t.Errorf("DownloadFailed.Total=%d，want %d", df.Total, len(targetBin))
		}
	}
	// 同成功用例：显式锚定真实下载的 Progress ≥1 帧，防回调转发接线回归。
	if n := reporter.countOf(EventDownloadProgress); n < 1 {
		t.Errorf("hash 失败用例同样要求至少一帧 Progress（完整下载后校验才失败），got %d 帧", n)
	}
}

// noCallAssetDownloader 断言缺陷路径不会走到真实下载：DownloadAsset 只应在不该被
// 调到的防御路径上兜底报错并记录。
type noCallAssetDownloader struct{ called bool }

func (d *noCallAssetDownloader) DownloadAsset(ctx context.Context, tag, assetName, expectedHash, targetDir, stagePrefix string, progress DownloadProgressFunc) (string, error) {
	d.called = true
	return "", errors.New("unexpected DownloadAsset call")
}

// TestApply_ReporterManifestDefectsZeroProgressFailed 目标清单缺陷（拉取失败 / 清单为空 /
// 缺本平台 hash）：DownloadStart 后无任何 Progress/Done，以 DownloadFailed 的 0-progress
// 终态（Copied=0、Total=-1）收尾。当前版本清单保持正确（provenance 才能通过——
// 生产中 Manifest 按同一 tag 参数化，当前/目标清单是两次独立拉取）。
func TestApply_ReporterManifestDefectsZeroProgressFailed(t *testing.T) {
	currentBin := []byte("current-official-bin")
	currentManifest := buildSumsBody("token-usage-darwin-arm64", sumHex(currentBin))

	cases := map[string]ManifestFetcher{
		"fetch manifest error": manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
			if tag == "v0.1.0" {
				return ParseManifest([]byte(currentManifest))
			}
			return nil, errors.New("manifest fetch failed")
		}),
		"empty manifest": manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
			if tag == "v0.1.0" {
				return ParseManifest([]byte(currentManifest))
			}
			return nil, nil
		}),
		"missing asset hash": manifestFetcherFunc(func(ctx context.Context, tag string) (*Manifest, error) {
			if tag == "v0.1.0" {
				return ParseManifest([]byte(currentManifest))
			}
			// buildSumsBody 恒定补齐全部平台资产（缺 hash 构造不出来），
			// 手写仅含 amd64 的清单体，模拟目标版本缺少本平台资产的发布事故。
			return ParseManifest([]byte(sumHex([]byte("x")) + "  token-usage-darwin-amd64\n"))
		}),
	}
	for name, fetcher := range cases {
		t.Run(name, func(t *testing.T) {
			svc := makeService(t)
			svc.ProvenanceDeps.Manifest = fetcher
			// AssetDownloader 必须已注入：downloadStage 才会进入下载阶段（发 DownloadStart）。
			downloader := &noCallAssetDownloader{}
			svc.AssetDownloader = downloader
			svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
			svc.Installer = newFakeInstaller()
			svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load
			reporter := &recordingReporter{}
			svc.Reporter = reporter

			got, err := svc.Apply(context.Background(), ApplyOptions{})
			if err != nil {
				t.Fatalf("Apply 不应返回 error，err=%v", err)
			}
			if got.ReadyToInstall {
				t.Fatalf("%s 应 ReadyToInstall=false", name)
			}

			assertEventSequence(t, reporter.kinds(), []EventKind{
				EventVersionsDiscovered, EventUpdateStart, EventDownloadStart, EventDownloadFailed,
			})
			df, ok := reporter.first(EventDownloadFailed)
			if !ok {
				t.Fatalf("%s 必须发射 DownloadFailed", name)
			}
			if df.Copied != 0 || df.Total != -1 {
				t.Errorf("%s DownloadFailed 应为 0-progress 终态（Copied=0、Total=-1），got Copied=%d Total=%d", name, df.Copied, df.Total)
			}
			if downloader.called {
				t.Errorf("%s 不应触发真实下载", name)
			}
		})
	}
}

// TestApply_ReporterNilSilent 未注入 Reporter：Apply 行为与结果不受影响（向后兼容）。
func TestApply_ReporterNilSilent(t *testing.T) {
	svc := makeService(t)
	svc.ControlManager = &fakeControlManager{session: &fakeControlSession{}}
	svc.Installer = newFakeInstaller()
	svc.ConfigLoader = (&recordingConfigLoader{cfg: &config.Config{DataDir: "/data"}}).load

	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall || !got.Installed {
		t.Fatalf("nil Reporter 下应照常完成安装，got %+v", got)
	}
}

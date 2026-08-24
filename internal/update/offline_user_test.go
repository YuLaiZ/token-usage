package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
)

// offline_user_test.go 校验「不曾 config init 的离线用户」场景：
//
// 行为约束：配置仅按现有 resolver 得到有效默认值、daemon 未运行时，
// 更新检查/安装路径不能创建 DB、日志、PID/runtime-state、plist 或 Registry 项。
//
// 测试策略：
//   - HOME 用临时目录，不写 config.toml（模拟「从未 config init」）；
//   - ConfigLoader 走 runtimecfg.ResolveEffectiveConfig + 标准 provider（零值 user config → 有效默认），
//     不复制默认路径/TOML 解析/校验逻辑——复用唯一解析边界；
//   - control 用真实 *control.Manager（经 NewControlManager 适配），验证 controlAdapter 端到端可用；
//   - daemon 未运行（临时 HOME 无任何 daemon lock/PID/state）；
//   - Installer 注入一个「记录但不落盘」的 fake（不创建文件）；
//   - Apply 跑完后断言 HOME 下不出现任何 daemon 运行产物（PID/runtime-state/lock/日志/DB）。
//
// 该测试同时验证：
//   - controlAdapter 把 *control.Manager 适配到 ControlManager 端到端可用（非仅编译期断言）；
//   - installUnderLock 在 daemon 未运行时正确跳过 Stop/Start，不触发 spawn。

// offlineDefaultConfigLoader 构造离线用户的 ConfigLoader：不走 LoadEffectiveConfig（它要求文件存在），
// 而是经 ResolveEffectiveConfig 把零值 user config 解析为有效默认配置。
// 这是「从未 config init」用户取得有效配置的唯一正确路径——复用 resolver，不复制默认路径逻辑。
func offlineDefaultConfigLoader(t *testing.T, home string) control.ConfigLoader {
	t.Helper()
	env := runtimecfg.ResolveEnv{
		Home:         home,
		GOOS:         runtime.GOOS,
		DefaultPaths: runtimecfg.NewStandardProvider(),
	}
	return func() (*config.Config, error) {
		// 零值 user config（无 client/router/显式 DataDir）→ resolver 填默认值。
		// 不读盘、不创建文件：ResolveEffectiveConfig 是纯解析（deep copy + expand + defaults）。
		return runtimecfg.ResolveEffectiveConfig(&config.Config{}, env)
	}
}

// TestApply_OfflineUser_NoDaemonArtifacts 离线用户（无 config init）+ daemon 未运行：
// 更新安装路径不创建 DB/日志/PID/runtime-state/lock。controlAdapter 端到端可用。
func TestApply_OfflineUser_NoDaemonArtifacts(t *testing.T) {
	home := t.TempDir() // 全新 HOME，无 config.toml、无 daemon 产物

	// 1. 装配 control.Manager + controlAdapter（真实 *control.Manager，验证端到端）。
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	adapter := NewControlManager(mgr)

	// 2. 装配 Service：可信来源 + 注入 control 装配 + 离线默认配置 loader。
	svc := makeService(t)
	svc.ControlManager = adapter
	svc.ConfigLoader = offlineDefaultConfigLoader(t, home)
	// Installer 用 fake（不落盘），验证编排骨架在无真实文件替换时也不创建 daemon 产物。
	svc.Installer = newFakeInstaller()

	// 先记录 HOME 在 Apply 前的已有内容（NewManager 只创建 .token-usage/ 配置目录 + control.lock 文件路径，
	// 不创建 daemon 产物）。control lock 文件由 flock 在 WithLock 时首次创建，属配置目录内、非 daemon 产物。
	before := snapshotTree(t, home)

	// 3. 运行 Apply（来源可信 → 进入锁内编排）。
	got, err := svc.Apply(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply err=%v", err)
	}
	if !got.ReadyToInstall {
		t.Fatalf("应到达 ReadyToInstall=true，reason=%q", got.Reason)
	}
	// daemon 未运行 → 编排跳过 Stop/Start；占位 Install 不创建文件 → Installed=true 但无 daemon 产物。
	if !got.Installed {
		t.Fatal("锁内编排应成功（Installed=true）")
	}

	// 4. 断言：HOME 下不新增任何 daemon 运行产物。
	after := snapshotTree(t, home)
	assertNoDaemonArtifacts(t, home, before, after)
}

// TestCheck_OfflineUser_NoDaemonArtifacts 离线用户 + Check（只读判定）：
// 不创建任何本地文件。Check 不触碰 control/daemon，但补一条断言覆盖「检查路径无副作用」。
func TestCheck_OfflineUser_NoDaemonArtifacts(t *testing.T) {
	home := t.TempDir()
	svc := makeService(t)
	// Check 不需要 ControlManager/ConfigLoader（只做判定），但断言其无副作用。
	svc.ControlManager = nil
	svc.ConfigLoader = nil

	before := snapshotTree(t, home)
	if _, err := svc.Check(context.Background(), CheckOptions{}); err != nil {
		t.Fatalf("Check err=%v", err)
	}
	after := snapshotTree(t, home)
	assertNoDaemonArtifacts(t, home, before, after)
}

// TestNewControlManager_NilManager NewControlManager(nil) 返回非 nil 适配器，
// 其 WithLock 在未装配 control.Manager 时返回错误（而非 panic）。
func TestNewControlManager_NilManager(t *testing.T) {
	a := NewControlManager(nil)
	if a == nil {
		t.Fatal("NewControlManager(nil) 不应返回 nil（应返回可调用的适配器）")
	}
	err := a.WithLock(context.Background(), func(ControlSession) error { return nil })
	if err == nil {
		t.Fatal("未装配 control.Manager 时 WithLock 应返回错误")
	}
}

// TestNewControlManager_NilCallback WithLock 传入 nil 回调应返回错误（与 control.Manager.WithLock 一致）。
func TestNewControlManager_NilCallback(t *testing.T) {
	mgr, err := control.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	a := NewControlManager(mgr)
	if err := a.WithLock(context.Background(), nil); err == nil {
		t.Fatal("nil 回调应返回错误")
	}
}

// snapshotTree 记录 home 下所有相对路径（用于前后对比新增文件）。
// control.NewManager 会创建 <home>/.token-usage 目录；control lock 文件由 flock 在 WithLock 时创建，
// 都在配置目录内，不属于 daemon 运行产物。
func snapshotTree(t *testing.T, home string) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{})
	_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == home {
			return nil
		}
		rel, _ := filepath.Rel(home, path)
		out[rel] = struct{}{}
		return nil
	})
	return out
}

// assertNoDaemonArtifacts 断言 after 相比 before 不新增任何「重量级 daemon 运行产物」。
//
// 重量级产物（禁止创建）：usage.db、logs/、daemon-fallback.log（daemon 兜底输出）、
// token-usage.pid（PID 协议文件）、token-usage.runtime.json、plist/Registry 项。
// 旧版 daemon.out/err.log 已随兜底输出合并进 logs/ 而不再产生（黑名单保留
// daemon-fallback.log 覆盖等价新产物）。
//
// 允许的 control 基础设施（非 daemon 运行产物）：
//   - .token-usage/ 配置目录（control.NewManager 创建）；
//   - token-usage.control.lock（control lock，flock 文件）；
//   - token-usage.lock（daemon lock 的 0 字节探针文件：daemon.IsDaemonRunning 用 flock
//     TryLock 探测存活，TryLock 会创建该文件，Unlock 不删除。这是 control 层判活的固有副作用，
//     非守护进程运行产物——守护进程真正运行时会持有该锁并写入 PID，探测退出后文件残留但无内容）。
//
// 区分依据：重量级产物（DB/日志/PID/runtime-state）只在守护进程实际 spawn 或命令路径主动打开时创建；
// 锁探针是 control 层判活的纯探测副作用，与「daemon 是否运行」无关。
func assertNoDaemonArtifacts(t *testing.T, home string, before, after map[string]struct{}) {
	t.Helper()
	// 重量级 daemon 运行产物黑名单（按 basename 命中即违规，无论在哪个子目录）。
	heavyArtifacts := map[string]bool{
		"daemon-fallback.log":      true,
		"token-usage.pid":          true,
		"token-usage.runtime.json": true,
		"usage.db":                 true,
	}
	// 允许的 control 基础设施 basename（配置目录、control lock、daemon lock 探针）。
	allowedControlInfra := map[string]bool{
		"token-usage.control.lock": true,
		"token-usage.lock":         true, // daemon.IsDaemonRunning flock 探针残留（0 字节，非运行产物）
		".token-usage":             true, // 配置目录
	}
	for rel := range after {
		if _, ok := before[rel]; ok {
			continue
		}
		base := filepath.Base(rel)
		if heavyArtifacts[base] {
			t.Errorf("离线用户更新路径不应创建重量级 daemon 运行产物: %q", rel)
		}
		if base == "logs" {
			t.Errorf("离线用户更新路径不应创建日志目录: %q", rel)
		}
		if base == "token-usage.plist" || base == "LaunchAgents" {
			t.Errorf("离线用户更新路径不应创建 plist/LaunchAgents: %q", rel)
		}
		// 允许的 control 基础设施不报错；其它新增项记录日志供调试。
		if !allowedControlInfra[base] {
			t.Logf("新增（非黑名单，需人工确认是否 daemon 产物）: %q", rel)
		}
	}
}

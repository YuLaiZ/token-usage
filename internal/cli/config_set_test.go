package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/configapp"
	"github.com/YuLaiZ/token-usage/internal/control"
	"github.com/YuLaiZ/token-usage/internal/runtimecfg"
	"github.com/YuLaiZ/token-usage/internal/service"
)

// fakeApplyFunc 构造一个可注入的 configSetApplyFunc，记录调用并返回可控行为。
// 返回的 fn 记录最后一次收到的 (expectedRevision, currentUser, confirm)。
func fakeApplyFunc(result configapp.ApplyConfigResult, err error) (fn configSetApplyFunc, calls *int, lastArgs *applyCall) {
	var n int
	var last applyCall
	fn = func(ctx context.Context, expectedRevision []byte, currentUser *config.Config, confirm bool) (configapp.ApplyConfigResult, error) {
		n++
		last = applyCall{
			expectedRevision:      append([]byte(nil), expectedRevision...),
			currentUserDataDir:    currentUser.DataDir,
			currentUserAutoStart:  currentUser.Daemon.AutoStart,
			confirmDataDirMigrate: confirm,
		}
		return result, err
	}
	calls = &n
	lastArgs = &last
	return fn, calls, lastArgs
}

// applyCall 记录 fakeApplyFunc 收到的关键入参（避免持有指针快照）。
type applyCall struct {
	expectedRevision      []byte
	currentUserDataDir    string
	currentUserAutoStart  bool
	confirmDataDirMigrate bool
}

// ---- 成功路径 stdout/stderr 合同 ----

// runConfigSet 是可注入 applyFn 的纯逻辑函数。直接测它最能锁定 stdout/stderr/exit 合同。
func TestRunConfigSet_Success_StdoutStableLine_StderrSuggestions(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, last := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:        true,
		Saved:          true,
		ConfigApplied:  true,
		SuccessMessage: "配置已保存",
		SuggestedSteps: []string{"token-usage restart"},
		ExplanatoryNotes: []string{
			"自启定义已关闭，下次登录/开机不再启动；当前 daemon 状态不变",
		},
	}, nil)

	var out, errOut bytes.Buffer
	exitErr := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "42", false, applyFn)

	if exitErr != nil {
		t.Fatalf("成功路径应退出 nil，实际: %v", exitErr)
	}
	stdout := out.String()
	stderr := errOut.String()
	// stdout 恰好含一行稳定成功行
	wantLine := "✓ daemon.poll_interval = 42"
	if got := strings.TrimSpace(stdout); got != wantLine {
		t.Errorf("stdout 应恰好为 %q，实际: %q", wantLine, got)
	}
	// 动作建议与说明写 stderr
	if !strings.Contains(stderr, "token-usage restart") {
		t.Errorf("stderr 应含动作建议，实际: %q", stderr)
	}
	if !strings.Contains(stderr, "自启定义已关闭") {
		t.Errorf("stderr 应含说明，实际: %q", stderr)
	}
	// applyFn 收到 confirm=false（非 data_dir）
	if last.confirmDataDirMigrate {
		t.Errorf("非 data_dir 字段不应传 confirm=true")
	}
}

// 成功但无动作建议：stdout 仍含稳定行；stderr 可为空。
func TestRunConfigSet_Success_NoSuggestions_StdoutOnly(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:        true,
		Saved:          true,
		ConfigApplied:  true,
		SuccessMessage: "配置已保存",
	}, nil)

	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "42", false, applyFn); err != nil {
		t.Fatalf("退出 nil，实际: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "✓ daemon.poll_interval = 42" {
		t.Errorf("stdout: %q", got)
	}
	if errOut.String() != "" {
		t.Errorf("无建议时 stderr 应为空，实际: %q", errOut.String())
	}
}

// ---- revision 冲突 ----

// 冲突（ErrConfigChangedExternally）：stdout 为空；stderr 含冲突与重试提示；退出非零。
func TestRunConfigSet_Conflict_StdoutEmpty_StderrHint(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{}, configapp.ErrConfigChangedExternally)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "42", false, applyFn)

	if err == nil {
		t.Fatal("冲突应退出非零")
	}
	if out.String() != "" {
		t.Errorf("冲突时 stdout 必须为空，实际: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "其他进程修改") {
		t.Errorf("stderr 应含冲突说明，实际: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "重新执行") {
		t.Errorf("stderr 应含重试提示，实际: %q", errOut.String())
	}
	// 返回的 err 应可被 errors.Is(ErrConfigChangedExternally) 识别
	if !errors.Is(err, configapp.ErrConfigChangedExternally) {
		t.Errorf("err 应 wrap ErrConfigChangedExternally，实际: %v", err)
	}
}

// ---- 部分失败（ConfigApplied=true 但有 PartialErrors）----

// 部分失败：stdout 含稳定成功行；stderr 写具体失败（boom）；退出非零。
func TestRunConfigSet_PartialFailure_StdoutSaved_StderrFailure(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	partialErr := errors.New("同步自启定义失败: boom")
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:        true,
		Saved:          true,
		ConfigApplied:  true,
		SuccessMessage: "配置已保存",
		PartialErrors:  []error{partialErr},
	}, errors.Join(partialErr))

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.autostart", "true", false, applyFn)

	if err == nil {
		t.Fatal("部分失败应退出非零")
	}
	if !strings.Contains(out.String(), "✓ daemon.autostart = true") {
		t.Errorf("部分失败 stdout 应含稳定成功行，实际: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "配置已保存") && !strings.Contains(out.String(), "配置已保存") {
		t.Errorf("应在 stdout 或 stderr 体现「配置已保存」，stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "boom") {
		t.Errorf("stderr 应含具体失败（boom），实际: %q", errOut.String())
	}
}

// ---- 顺序执行两次 set 不误报冲突 ----

// 用真实 ApplyConfig 两次连续 set 同一字段：第二次 expectedRevision 应取第一次写入后的磁盘 bytes，
// 不应误报冲突。
func TestConfigSetCmd_SequentialTwoSets_NoFalseConflict(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"
[daemon]
poll_interval = 15
`)
	applyFn := realApplyFuncForTest(t)
	ctx := context.Background()

	var out1, err1 bytes.Buffer
	if err := runConfigSet(ctx, &out1, &err1, "daemon.poll_interval", "20", false, applyFn); err != nil {
		t.Fatalf("第一次 set: %v (stderr=%q)", err, err1.String())
	}
	var out2, err2 bytes.Buffer
	if err := runConfigSet(ctx, &out2, &err2, "daemon.poll_interval", "30", false, applyFn); err != nil {
		t.Fatalf("第二次 set: %v (stderr=%q)", err, err2.String())
	}
	if !strings.Contains(out1.String(), "✓ daemon.poll_interval = 20") {
		t.Errorf("第一次 stdout: %q", out1.String())
	}
	if !strings.Contains(out2.String(), "✓ daemon.poll_interval = 30") {
		t.Errorf("第二次 stdout: %q", out2.String())
	}
	// 重新加载磁盘验证最终值
	home := os.Getenv("HOME")
	cfg, err := config.LoadUserConfig(filepath.Join(home, ".token-usage", "config.toml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Daemon.PollInterval != 30 {
		t.Errorf("最终 PollInterval=%d want 30", cfg.Daemon.PollInterval)
	}
}

// ---- 模拟外部改写拒绝覆盖 ----

// 在读取 snapshot 后、调用 applyFn 前，外部改写配置文件。ApplyConfig 以 expectedRevision
// 与磁盘 revision 不匹配为由拒绝（ErrConfigChangedExternally），不覆盖。
// 真正的并发冲突语义由 configapp.ApplyConfig 在锁内完成（configapp 包已覆盖）；此处的 CLI
// 集成测试验证：expectedRevision 来源是 runConfigSet 读到的磁盘 bytes，传给 applyFn 后由
// ApplyConfig 在锁内重读并比较。这里直接用 fake applyFn 模拟「ApplyConfig 返回冲突」，
// 锁定 CLI 的冲突输出合同。
func TestRunConfigSet_ConflictFromExternalRewrite_StdoutEmpty(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{}, configapp.ErrConfigChangedExternally)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "42", false, applyFn)

	if err == nil {
		t.Fatal("冲突应退出非零")
	}
	if out.String() != "" {
		t.Errorf("冲突时 stdout 必须为空，实际: %q", out.String())
	}
	if !errors.Is(err, configapp.ErrConfigChangedExternally) {
		t.Errorf("err 应 wrap ErrConfigChangedExternally，实际: %v", err)
	}
}

// ---- autostart 三种稳定说明及 drift repaired ----

// autostart 开启 + daemon 未运行：stderr 说明"下次登录生效"。
func TestRunConfigSet_AutoStartOn_DaemonNotRunning_NoteNextLogin(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:       true,
		Saved:         true,
		ConfigApplied: true,
		AutoStart: configapp.AutoStartOutcome{
			Requested:     true,
			DefinitionNow: true,
		},
		ExplanatoryNotes: []string{
			"自启定义已启用，下次登录/开机生效；当前未运行，如需现在运行可执行 token-usage start",
		},
	}, nil)

	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "daemon.autostart", "true", false, applyFn); err != nil {
		t.Fatalf("退出 nil: %v", err)
	}
	if !strings.Contains(errOut.String(), "下次登录") {
		t.Errorf("stderr 应含下次登录说明，实际: %q", errOut.String())
	}
}

// autostart 关闭：stderr 说明"当前 daemon 状态不变；不再自启"。
func TestRunConfigSet_AutoStartOff_NoteDaemonUnchanged(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:       true,
		Saved:         true,
		ConfigApplied: true,
		AutoStart: configapp.AutoStartOutcome{
			Requested: false,
		},
		ExplanatoryNotes: []string{
			"自启定义已关闭，下次登录/开机不再启动；当前 daemon 状态不变",
		},
	}, nil)

	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "daemon.autostart", "false", false, applyFn); err != nil {
		t.Fatalf("退出 nil: %v", err)
	}
	if !strings.Contains(errOut.String(), "状态不变") {
		t.Errorf("stderr 应含状态不变说明，实际: %q", errOut.String())
	}
}

// drift repaired：stderr 说明"自启定义已修复"。
func TestRunConfigSet_AutoStart_DriftRepaired(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:       true,
		Saved:         true,
		ConfigApplied: true,
		AutoStart: configapp.AutoStartOutcome{
			Requested:     true,
			DefinitionNow: true,
			DriftRepaired: true,
		},
		ExplanatoryNotes: []string{
			"自启定义已启用；当前 daemon 保持运行",
			"自启定义已修复（检测到漂移并重新收敛）",
		},
	}, nil)

	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "daemon.autostart", "true", false, applyFn); err != nil {
		t.Fatalf("退出 nil: %v", err)
	}
	if !strings.Contains(errOut.String(), "已修复") {
		t.Errorf("stderr 应含漂移修复说明，实际: %q", errOut.String())
	}
}

// ---- 非法 numeric/log/client/router/path key（ApplyConfig ValidateUserConfig 拒绝，CLI 传播）----

// 非法 poll_interval（负数）→ ApplyConfig 校验失败，CLI 传播错误，退出非零，stdout 无成功行。
func TestRunConfigSet_InvalidNumeric_RejectedByApplyConfig(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	// 用真实 ApplyConfig，触发 ValidateUserConfig 拒绝负数 poll_interval。
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "-5", false, applyFn)

	if err == nil {
		t.Fatal("非法 poll_interval 应退出非零")
	}
	if out.String() != "" {
		t.Errorf("校验失败 stdout 必须为空，实际: %q", out.String())
	}
}

// 非法 log level → ApplyConfig 校验失败。
func TestRunConfigSet_InvalidLogLevel_RejectedByApplyConfig(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "log.level", "verbose", false, applyFn)

	if err == nil {
		t.Fatal("非法 log level 应退出非零")
	}
	if out.String() != "" {
		t.Errorf("校验失败 stdout 必须为空，实际: %q", out.String())
	}
}

// 非法 client 名 → ApplyConfig 校验失败。
func TestRunConfigSet_InvalidClient_RejectedByApplyConfig(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "clients.foobar.enabled", "true", false, applyFn)

	if err == nil {
		t.Fatal("非法 client 名应退出非零")
	}
	if out.String() != "" {
		t.Errorf("校验失败 stdout 必须为空，实际: %q", out.String())
	}
}

// 非法 router 名 → ApplyConfig 校验失败。
func TestRunConfigSet_InvalidRouter_RejectedByApplyConfig(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "routers.ghost.db_path", "/x/db", false, applyFn)

	if err == nil {
		t.Fatal("非法 router 名应退出非零")
	}
	if out.String() != "" {
		t.Errorf("校验失败 stdout 必须为空，实际: %q", out.String())
	}
}

// 非法 client path key → ApplyConfig 校验失败。
func TestRunConfigSet_InvalidClientPathKey_RejectedByApplyConfig(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"
[clients.codex]
enabled = true
`)
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "clients.codex.paths.ghostpath", "/x", false, applyFn)

	if err == nil {
		t.Fatal("非法 client path key 应退出非零")
	}
	if out.String() != "" {
		t.Errorf("校验失败 stdout 必须为空，实际: %q", out.String())
	}
}

// ---- data_dir 迁移：--confirm-migrate ----

// data_dir 无 --confirm-migrate → ApplyConfig 返回 errDataDirMigrationNotConfirmed（经 ApplyConfig 包装）。
func TestRunConfigSet_DataDir_NoConfirm_Rejected(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/old"`)
	applyFn := realApplyFuncForTest(t)

	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "data_dir", "/new", false, applyFn)

	if err == nil {
		t.Fatal("data_dir 无 confirm 应退出非零")
	}
	if out.String() != "" {
		t.Errorf("拒绝时 stdout 必须为空，实际: %q", out.String())
	}
	// runConfigSet 在调用 ApplyConfig 前就拒绝（config.Set 返回 ErrDataDirNeedsConfirm 且无 confirm），
	// 错误文案含 --confirm-migrate 提示。
	if !strings.Contains(err.Error(), "confirm-migrate") {
		t.Errorf("错误应提示需 --confirm-migrate，实际: %q", err.Error())
	}
}

// data_dir 带 --confirm-migrate → applyFn 收到 confirm=true（运行状态验证由 ApplyConfig 做）。
func TestRunConfigSet_DataDir_WithConfirm_PassesConfirmToApply(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/old"`)
	applyFn, _, last := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:       true,
		Saved:         true,
		ConfigApplied: true,
	}, nil)

	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "data_dir", "/new", true, applyFn); err != nil {
		t.Fatalf("带 confirm 应通过，实际: %v", err)
	}
	if !last.confirmDataDirMigrate {
		t.Error("应把 confirm=true 传给 applyFn")
	}
	if last.currentUserDataDir != "/new" {
		t.Errorf("currentUser.DataDir 应为 /new，实际 %q", last.currentUserDataDir)
	}
}

// ---- 配置文件缺失提示 config init ----

func TestRunConfigSet_MissingConfigFile_HintsInit(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)

	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{}, nil)
	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "42", false, applyFn)
	if err == nil {
		t.Fatal("配置文件缺失时应报错")
	}
	if !strings.Contains(err.Error(), "config init") {
		t.Errorf("缺失文件错误应提示 config init，实际: %q", err.Error())
	}
}

// ---- config set 的 dotted-key Set 错误（类型不匹配）由 CLI 传播 ----

// poll_interval 非数字 → config.Set 返回错误，不进入 ApplyConfig。
func TestRunConfigSet_InvalidValueType_Propagated(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"`)
	called := false
	applyFn := func(ctx context.Context, rev []byte, cfg *config.Config, confirm bool) (configapp.ApplyConfigResult, error) {
		called = true
		return configapp.ApplyConfigResult{}, nil
	}
	var out, errOut bytes.Buffer
	err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "notanumber", false, applyFn)
	if err == nil {
		t.Fatal("非数字 poll_interval 应报错")
	}
	if called {
		t.Error("config.Set 失败不应调用 applyFn")
	}
}

// ---- Long help 内容 ----

func TestConfigSetCmd_LongExplainsRevisionProtection(t *testing.T) {
	cmd := newConfigSetCmd()
	for _, want := range []string{"revision", "其他进程", "重新执行", "采集"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("config set Long 应含 %q，实际: %q", want, cmd.Long)
		}
	}
}

func TestConfigSetCmd_HasConfirmMigrateFlag(t *testing.T) {
	cmd := newConfigSetCmd()
	if cmd.Flags().Lookup("confirm-migrate") == nil {
		t.Error("config set 应保留 --confirm-migrate flag")
	}
}

// ---- 辅助：真实 ApplyConfig 构造（集成测试用）----

// realApplyFuncForTest 构造一个走真实 ApplyConfig 的 configSetApplyFunc（用 control.Manager + 真 AutoStartManager）。
// HOME 已由 setupHomeConfig 指向 t.TempDir()，control.NewManager 会在该 HOME 下建 .token-usage/。
func realApplyFuncForTest(t *testing.T) configSetApplyFunc {
	t.Helper()
	home := os.Getenv("HOME")
	mgr, err := control.NewManager(home)
	if err != nil {
		t.Fatalf("control.NewManager: %v", err)
	}
	env := runtimecfg.ResolveEnv{Home: home, GOOS: "darwin", DefaultPaths: runtimecfg.NewStandardProvider()}
	app, err := configapp.NewApplication(home, env, mgr, service.NewAutoStartManager())
	if err != nil {
		t.Fatalf("configapp.NewApplication: %v", err)
	}
	return func(ctx context.Context, expectedRevision []byte, currentUser *config.Config, confirm bool) (configapp.ApplyConfigResult, error) {
		return app.ApplyConfig(ctx, expectedRevision, currentUser, confirm)
	}
}

// ---- data_dir 写回（现通过真实 ApplyConfig 验证运行状态后写盘）----

// data_dir 带确认且旧 daemon 未运行时写回成功。
// 注意：旧 data_dir 必须是「可创建 lock 文件的路径」（父目录存在），否则
// daemon.IsDaemonRunning 因 lock 不可创建而保守判为 Running，触发迁移拒绝。
// 故这里旧/新 data_dir 都用 TempDir 下的真实目录。
func TestConfigSetCmd_DataDirWithConfirm_WritesBack(t *testing.T) {
	oldDataDir := t.TempDir()
	newDataDir := t.TempDir()
	setupHomeConfig(t, fmt.Sprintf("data_dir = %q\n", oldDataDir))
	applyFn := realApplyFuncForTest(t)
	ctx := context.Background()

	var out, errOut bytes.Buffer
	if err := runConfigSet(ctx, &out, &errOut, "data_dir", newDataDir, true, applyFn); err != nil {
		t.Fatalf("set data_dir --confirm-migrate: %v (stderr=%q)", err, errOut.String())
	}
	home := os.Getenv("HOME")
	cfg, _ := config.LoadUserConfig(filepath.Join(home, ".token-usage", "config.toml"))
	if cfg.DataDir != newDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, newDataDir)
	}
}

// ---- 旧测试保留：零值提示已交给 ApplyConfig 的 ExplanatoryNotes，CLI 不再单独判断 ----

// 零值提示语义变更：现在由 ApplyConfig 的 Effects/ExplanatoryNotes 决定，CLI 不再硬编码零值判断。
// 保留一个回归测试确认 poll_interval=0 不再因 CLI 侧的旧逻辑报错。
func TestRunConfigSet_PollIntervalZero_NoCliSideHint(t *testing.T) {
	setupHomeConfig(t, `data_dir = "/x"
[daemon]
poll_interval = 30
`)
	applyFn, _, _ := fakeApplyFunc(configapp.ApplyConfigResult{
		Changed:       true,
		Saved:         true,
		ConfigApplied: true,
	}, nil)
	var out, errOut bytes.Buffer
	if err := runConfigSet(context.Background(), &out, &errOut, "daemon.poll_interval", "0", false, applyFn); err != nil {
		t.Fatalf("set 0: %v", err)
	}
	// 稳定成功行仍写 stdout
	if !strings.Contains(out.String(), "✓ daemon.poll_interval = 0") {
		t.Errorf("stdout 应含稳定行，实际: %q", out.String())
	}
}

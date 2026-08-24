// internal/cli/start_test.go
package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/control"
)

var errStartBoom = errors.New("start boom")

// injectStartConfig 注入 startConfigLoader 替身，返回还原函数。
// start 前置谓词读取配置，测试须固定配置内容避免依赖开发机 ~/.token-usage。
func injectStartConfig(cfg *config.Config, cfgErr error) func() {
	orig := startConfigLoader
	startConfigLoader = func() (*config.Config, error) { return cfg, cfgErr }
	return func() { startConfigLoader = orig }
}

// startCfgEnabled 返回带一个 enabled JSONL 客户端的最小配置（谓词放行）。
func startCfgEnabled() *config.Config {
	return &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: true, Paths: map[string]string{"projects_dir": "/tmp/claude"}},
		},
	}
}

// stubControlStartStop 实现 controlStartStopper，供 start/stop/status/restart 结果合同测试注入。
type stubControlStartStop struct {
	startRes   control.StartResult
	startErr   error
	stopRes    control.StopResult
	stopErr    error
	restartRes control.RestartResult
	restartErr error
	inspectSt  control.RuntimeState
	inspectErr error
}

func (s *stubControlStartStop) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	return s.startRes, s.startErr
}
func (s *stubControlStartStop) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return s.stopRes, s.stopErr
}
func (s *stubControlStartStop) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return s.restartRes, s.restartErr
}
func (s *stubControlStartStop) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return s.inspectSt, s.inspectErr
}

// TestStartCmd_NoArgs start 命令应声明 cobra.NoArgs（拒绝多余参数）。
func TestStartCmd_NoArgs(t *testing.T) {
	cmd := newStartCmd()
	if cmd.Args == nil {
		t.Fatal("start 命令应声明 Args（cobra.NoArgs）")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("NoArgs 应拒绝多余参数")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("NoArgs 对空参数应返回 nil，实际: %v", err)
	}
}

// TestStartCmd_LongSeparatesProcessAndAutostart Long 文案应明确「当前进程与自启定义分离」。
func TestStartCmd_LongSeparatesProcessAndAutostart(t *testing.T) {
	cmd := newStartCmd()
	if !strings.Contains(cmd.Long, "自启") {
		t.Error("start Long 应提及自启定义以区分当前进程")
	}
}

// TestStartCmd_LoadConfigFails 配置缺失时 load 在 control lock 内失败 → 返回 error（非零退出）。
func TestStartCmd_LoadConfigFails(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	cmd := newStartCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("配置缺失时应返回 error")
	}
}

// TestRunStart_AlreadyRunningContract start 已运行 → stdout 显示当前 PID，退出码 0（无 error）。
// 已在运行的实例不做前置拦截（谓词 false 也不提示）。
func TestRunStart_AlreadyRunningContract(t *testing.T) {
	restoreCfg := injectStartConfig(&config.Config{}, nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			inspectSt: control.RuntimeState{Running: true},
			startRes:  control.StartResult{PID: 4242, AlreadyRunning: true},
		}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("已运行应退出 0（无 error），实际: %v", e)
	}
	if !strings.Contains(out.String(), "4242") {
		t.Errorf("stdout 应显示当前 PID 4242，实际: %q", out.String())
	}
	if strings.Contains(out.String(), "No enabled clients") {
		t.Errorf("已在运行时不应前置拦截: %q", out.String())
	}
}

// TestRunStart_StartedContract start 未运行 → spawn 成功 → stdout 显示新 PID，退出 0。
func TestRunStart_StartedContract(t *testing.T) {
	restoreCfg := injectStartConfig(startCfgEnabled(), nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{
			startRes: control.StartResult{PID: 5555, AlreadyRunning: false},
		}, nil
	}

	var out bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if e := cmd.RunE(cmd, nil); e != nil {
		t.Fatalf("启动成功应退出 0，实际: %v", e)
	}
	if !strings.Contains(out.String(), "5555") {
		t.Errorf("stdout 应显示新 PID 5555，实际: %q", out.String())
	}
}

// TestRunStart_RealFailureReturnsContextError 真实失败 → 返回带上下文的 error
// （cobra 统一输出），命令自身不再手写 stderr（防 cause 双打）。
func TestRunStart_RealFailureReturnsContextError(t *testing.T) {
	restoreCfg := injectStartConfig(startCfgEnabled(), nil)
	defer restoreCfg()
	orig := controlManagerFactory
	defer func() { controlManagerFactory = orig }()
	controlManagerFactory = func() (controlStartStopper, error) {
		return &stubControlStartStop{startErr: errStartBoom}, nil
	}

	var out, errOut bytes.Buffer
	cmd := newStartCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("真实失败应退出非 0")
	}
	if !strings.Contains(err.Error(), "启动守护进程失败") || !strings.Contains(err.Error(), "start boom") {
		t.Errorf("返回 error 应含上下文与 cause: %v", err)
	}
	if errOut.String() != "" {
		t.Errorf("命令不得手写 stderr（由 cobra 统一输出）: %q", errOut.String())
	}
}

// TestRunStart_NoTargetsPreflight 三态前置拦截：
// 全关 → 双语提示 + error（不调用 Start）；仅 router 配置无 enabled client → 同样拦截；
// 有 enabled 客户端 → 放行调用 Start。
func TestRunStart_NoTargetsPreflight(t *testing.T) {
	onlyRouterCfg := &config.Config{
		Clients: map[string]config.Client{
			"claude": {Enabled: false, Router: "cc_switch"},
		},
		Routers: map[string]config.RouterConfig{
			"cc_switch": {DBPath: "/tmp/cc-switch.db"},
		},
	}
	cases := []struct {
		name       string
		cfg        *config.Config
		wantBlock  bool
		wantStdout []string
	}{
		{"all disabled", &config.Config{}, true,
			[]string{"No enabled clients", "没有任何已启用的客户端", "token-usage config set clients.claude.enabled true"}},
		{"router configured but no enabled client", onlyRouterCfg, true,
			[]string{"No enabled clients"}},
		{"enabled client", startCfgEnabled(), false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreCfg := injectStartConfig(tc.cfg, nil)
			defer restoreCfg()
			orig := controlManagerFactory
			defer func() { controlManagerFactory = orig }()

			startCalled := false
			stub := &startCallRecorder{stub: stubControlStartStop{startRes: control.StartResult{PID: 5555}}, called: &startCalled}
			controlManagerFactory = func() (controlStartStopper, error) { return stub, nil }

			var out bytes.Buffer
			cmd := newStartCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.RunE(cmd, nil)

			if tc.wantBlock {
				if err == nil {
					t.Fatal("无监控目标应返回 error（exit 1）")
				}
				if startCalled {
					t.Error("前置拦截后不得调用 Start")
				}
				for _, want := range tc.wantStdout {
					if !strings.Contains(out.String(), want) {
						t.Errorf("stdout 缺少 %q，实际: %q", want, out.String())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("有监控目标应放行，实际: %v", err)
			}
			if !startCalled {
				t.Error("有监控目标必须调用 Start")
			}
			if strings.Contains(out.String(), "No enabled clients") {
				t.Errorf("有监控目标不应输出前置提示: %q", out.String())
			}
		})
	}
}

// startCallRecorder 记录 Start 是否被调用（前置拦截测试断言 Start 不可达）。
type startCallRecorder struct {
	stub   stubControlStartStop
	called *bool
}

func (s *startCallRecorder) Start(ctx context.Context, load control.ConfigLoader) (control.StartResult, error) {
	*s.called = true
	return s.stub.Start(ctx, load)
}
func (s *startCallRecorder) Stop(ctx context.Context, load control.ConfigLoader) (control.StopResult, error) {
	return s.stub.Stop(ctx, load)
}
func (s *startCallRecorder) Restart(ctx context.Context, load control.ConfigLoader) (control.RestartResult, error) {
	return s.stub.Restart(ctx, load)
}
func (s *startCallRecorder) Inspect(ctx context.Context, cfg *config.Config) (control.RuntimeState, error) {
	return s.stub.Inspect(ctx, cfg)
}

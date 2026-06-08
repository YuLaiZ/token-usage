// internal/service/service_windows.go
//go:build windows

package service

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/runmeta"
	"golang.org/x/sys/windows/registry"
)

type registryManager struct{}

func newPlatformManager() Manager { return registryManager{} }

func (registryManager) Platform() string { return "registry" }

const runKeyName = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "token-usage"

// buildRegistryValue 生成注册表 Run 键的值数据（格式 `"<BinPath>" _run`）（纯函数，可单测）。
func buildRegistryValue(opts Options) string {
	return fmt.Sprintf(`"%s" %s`, opts.BinPath, strings.Join(opts.Args, " "))
}

// parseRegistryValue 解析注册表值数据 `"<BinPath>" _run`，返回 binPath 与 args（纯函数，可单测）。
// 格式：双引号包裹的 BinPath + 空格分隔的 Args。
func parseRegistryValue(value string) (binPath string, args []string, err error) {
	s := strings.TrimSpace(value)
	if !strings.HasPrefix(s, `"`) {
		return "", nil, fmt.Errorf("注册表值格式无效（应以双引号开头）: %q", value)
	}
	end := strings.Index(s[1:], `"`)
	if end < 0 {
		return "", nil, fmt.Errorf("注册表值格式无效（BinPath 引号未闭合）: %q", value)
	}
	binPath = s[1 : 1+end]
	rest := strings.TrimSpace(s[1+end+1:])
	if rest != "" {
		args = strings.Fields(rest)
	}
	return binPath, args, nil
}

// Enable 写注册表 Run 值，**不 spawn**。
// 注册表 Run 键是 logon trigger，不持有进程句柄，
// Enable/Disable 都不需要管进程。下次登录 Windows 才按 Run 键自启。
func (registryManager) Enable(opts Options) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyName, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(runValueName, buildRegistryValue(opts)); err != nil {
		return fmt.Errorf("写入注册表值失败: %w", err)
	}
	return nil
}

// Disable 关闭自启：删注册表 Run 值，**不 taskkill**。
// 只修改定义，不停止进程。
// 调用点：Sync(autostart=false) 收敛、漂移重装前的旧定义清理。
// 已运行的进程继续跑，直到用户手动 stop 或注销/重启。
// opts 当前 Windows Disable 实现不使用（保持签名一致）。
func (registryManager) Disable(opts Options) error {
	_ = opts
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyName, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除注册表值失败: %w", err)
	}
	return nil
}

// StopCurrent 仅按 PID 文件中的准确 PID 停止当前进程，**保留注册表 Run 值**。
// PID 缺失、损坏或 taskkill 失败均明确返回错误；禁止按二进制名 fallback，
// 避免误杀同名进程。control 的正常 stop 路径已掌握准确 PID，不调用本方法。
// 调用点：stop 命令。下次登录 Windows 仍会按 Run 键自启。
func (registryManager) StopCurrent(opts Options) error {
	return stopRunningInstanceByPID(opts)
}

func stopRunningInstanceByPID(opts Options) error {
	pid, _, err := runmeta.ReadPIDFile(runmeta.PIDPath(opts.DataDir))
	if err != nil {
		return fmt.Errorf("读取准确 PID 失败: %w", err)
	}
	return taskkillByPID(pid)
}

var runTaskkill = func(args ...string) ([]byte, error) {
	return exec.Command("taskkill", args...).CombinedOutput()
}

// taskkillByPID 用 taskkill /F /PID 精确杀进程。
func taskkillByPID(pid int) error {
	out, err := runTaskkill("/F", "/PID", strconv.Itoa(pid))
	if err != nil {
		return fmt.Errorf("taskkill /PID 失败: %w（输出: %s）", err, string(out))
	}
	return nil
}

// Status 只比较注册表 Run 值是否存在及内容是否匹配，不 spawn/taskkill、不查进程。
// Exists 表示注册表 Run 值存在。
func (registryManager) Status(opts Options) (AutoStartStatus, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyName, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return AutoStartStatus{Exists: false}, nil
		}
		return AutoStartStatus{}, fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	val, _, err := k.GetStringValue(runValueName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return AutoStartStatus{Exists: false}, nil
		}
		return AutoStartStatus{}, fmt.Errorf("读取注册表 Run 值失败: %w", err)
	}
	// 注册表值存在 = Exists
	binPath, args, perr := parseRegistryValue(val)
	if perr != nil {
		return AutoStartStatus{Exists: true, SpecMatches: false}, nil
	}
	wantArgs := append([]string{opts.BinPath}, opts.Args...)
	specOK := binPath == opts.BinPath && equalSlice(args, wantArgs[1:])
	return AutoStartStatus{Exists: true, SpecMatches: specOK}, nil
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Package runmeta 维护守护进程的双文件元数据协议：
//
//   - <data_dir>/token-usage.pid          —— 文本 "<pid> <instanceID>"（兼容旧 "<pid>"）
//   - <data_dir>/token-usage.runtime.json —— RuntimeState JSON（monitor_ready/catch_up 等）
//
// daemon lock 是存活唯一真相源；PID/runtime-state 是可降级的定位/状态元数据：
// 读失败由调用方降级（status 显示 PID 元数据不可用；start/stop 走安全错误），
// 绝不返回「ready」半成品。
//
// 所有写入通过 fileutil.ReplaceCompleteFile 做完整文件替换，避免半写/撕裂。
// 清理分两种：CleanupStaleMetadata（lock 未持有时按 stale 协议清理 PID+state+精确 temp）、
// CleanupOwnedMetadata（确认 instanceID 所有权后的正常退出清理）。
package runmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/fileutil"
)

// 文件名常量（全仓注释一致，不在此重复定义路径协议）。
const (
	pidFileName   = "token-usage.pid"
	stateFileName = "token-usage.runtime.json"

	catchUpPending   = "pending"
	catchUpRunning   = "running"
	catchUpSucceeded = "succeeded"
	catchUpFailed    = "failed"
)

// pidTempPrefix/stateTempPrefix 是 ReplaceCompleteFile 可能残留的精确 temp 前缀。
// 通过 fileutil.TempPrefix 按 target basename 推导,避免跨包硬编码耦合:
// 若 fileutil 改 temp 命名模式,这里自动跟随,不会静默过时。
var (
	pidTempPrefix   = fileutil.TempPrefix(pidFileName)
	stateTempPrefix = fileutil.TempPrefix(stateFileName)
)

// RuntimeState 是 runtime-state 文件的内容：监控/追赶进度的可降级状态元数据。
//
// 与 control.RuntimeState 同名但不可互赋。control.Inspect 组合
// 「daemon lock 判活 + runmeta 读 PID/state」时需逐字段拷贝并补 Running。
type RuntimeState struct {
	PID             int    `json:"pid"`
	InstanceID      string `json:"instance_id"`
	MonitorReady    bool   `json:"monitor_ready"`
	CatchUp         string `json:"catch_up"`
	CatchUpFailures int    `json:"catch_up_failures"`
}

// PIDPath 返回 <dataDir>/token-usage.pid。
func PIDPath(dataDir string) string {
	return filepath.Join(dataDir, pidFileName)
}

// StatePath 返回 <dataDir>/token-usage.runtime.json。
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, stateFileName)
}

// ReadPIDFile 读 PID 文件，兼容新旧两种格式：
//
//   - 新格式 "<pid> <instanceID>"：返回 (pid, instanceID, nil)。
//   - 旧格式 "<pid>"：返回 (pid, "", nil)（instanceID 空，不能满足新 start 的 instanceID ready 握手）。
//
// 文件缺失返回包装了 os.ErrNotExist 的错误；格式无效/越界返回普通错误。
func ReadPIDFile(path string) (pid int, instanceID string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) == 0 {
		return 0, "", fmt.Errorf("pid 文件为空: %q", string(data))
	}
	if len(fields) > 2 {
		return 0, "", fmt.Errorf("pid 文件字段过多: %q", string(data))
	}
	pid, perr := strconv.Atoi(fields[0])
	if perr != nil || pid <= 0 {
		return 0, "", fmt.Errorf("pid 文件格式无效: %q", string(data))
	}
	if len(fields) >= 2 {
		instanceID = fields[1]
	}
	return pid, instanceID, nil
}

// WritePIDFile 以完整文件替换写入新格式 "<pid> <instanceID>"。
// 用 fileutil.ReplaceCompleteFile 保证原子性（不半写、不残留旧内容）。
func WritePIDFile(path string, pid int, instanceID string) error {
	if pid <= 0 {
		return fmt.Errorf("pid 必须为正数: %d", pid)
	}
	if len(strings.Fields(instanceID)) > 1 || (instanceID != "" && strings.TrimSpace(instanceID) != instanceID) {
		return fmt.Errorf("instanceID 不能包含空白: %q", instanceID)
	}
	content := fmt.Sprintf("%d %s", pid, instanceID)
	return fileutil.ReplaceCompleteFile(path, []byte(content), 0o644)
}

// ReadRuntimeState 读 runtime-state 文件并反序列化为 RuntimeState。
// 解析失败返回错误，绝不返回半成品（调用方据此降级，绝不默认 ready）。
func ReadRuntimeState(path string) (RuntimeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeState{}, err
	}
	var st RuntimeState
	if err := json.Unmarshal(data, &st); err != nil {
		return RuntimeState{}, fmt.Errorf("解析 runtime-state 失败: %w", err)
	}
	if err := validateRuntimeState(st); err != nil {
		return RuntimeState{}, fmt.Errorf("runtime-state 字段无效: %w", err)
	}
	return st, nil
}

// WriteRuntimeState 以完整文件替换写入 RuntimeState JSON。
// 用 fileutil.ReplaceCompleteFile 保证原子性。
func WriteRuntimeState(path string, state RuntimeState) error {
	if err := validateRuntimeState(state); err != nil {
		return fmt.Errorf("runtime-state 字段无效: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("编码 runtime-state 失败: %w", err)
	}
	return fileutil.ReplaceCompleteFile(path, data, 0o644)
}

func validateRuntimeState(state RuntimeState) error {
	if state.PID <= 0 {
		return fmt.Errorf("pid 必须为正数: %d", state.PID)
	}
	if state.InstanceID == "" ||
		strings.TrimSpace(state.InstanceID) != state.InstanceID ||
		len(strings.Fields(state.InstanceID)) != 1 {
		return fmt.Errorf("instance_id 必须是非空且不含空白的单字段: %q", state.InstanceID)
	}
	switch state.CatchUp {
	case catchUpPending, catchUpRunning, catchUpSucceeded:
		if state.CatchUpFailures != 0 {
			return fmt.Errorf("catch_up=%q 时 catch_up_failures 必须为 0", state.CatchUp)
		}
	case catchUpFailed:
		if state.CatchUpFailures <= 0 {
			return fmt.Errorf("catch_up=%q 时 catch_up_failures 必须为正数", state.CatchUp)
		}
	default:
		return fmt.Errorf("未知 catch_up 状态 %q", state.CatchUp)
	}
	return nil
}

// CleanupStaleMetadata 在 daemon lock **未持有**时清理 PID + runtime-state 文件，
// 并清除 ReplaceCompleteFile 可能残留的精确 temp 文件。
//
// 前置条件：调用方必须已确认 daemon lock 未持有（强杀残留只在确认未持有后清理）。
// 缺失文件视为已清理（幂等）；目录读失败返回错误。
func CleanupStaleMetadata(dataDir string) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("清理 stale metadata 时 data_dir 不能为空")
	}
	var cleanupErr error
	// 精确 temp 残留（ReplaceCompleteFile 模式：.token-usage.pid.tmp-* / .token-usage.runtime.json.tmp-*）。
	if err := fileutil.CleanupKnownTempFiles(dataDir, []string{pidTempPrefix, stateTempPrefix}); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	// runtime-state 文件。
	if err := os.Remove(StatePath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理 runtime-state 文件失败: %w", err))
	}
	// PID 文件最后删除，保持正常退出协议的 state → PID 顺序。
	if err := os.Remove(PIDPath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理 PID 文件失败: %w", err))
	}
	return cleanupErr
}

// CleanupOwnedMetadata 在确认 instanceID 所有权后清理 PID + runtime-state 文件。
//
// 正常退出顺序：调用方先确认 PID 文件中的 instanceID 属于本实例，
// 再调本函数清理（state → PID），最后释放 daemon lock。
//
// 归属不匹配（PID 不同、或同 PID 不同 instanceID）时不删除该文件——
// 防止 PID 复用时误删他代 state/PID。PID 与 state 分独立判断归属，
// 只删属于自己的那个文件（PID 是自己的但 state 是他人的，只删 PID）。
// 缺失文件视为已清理（幂等）。
func CleanupOwnedMetadata(dataDir string, pid int, instanceID string) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("清理所有权元数据时 data_dir 不能为空")
	}
	if pid <= 0 || instanceID == "" || strings.TrimSpace(instanceID) != instanceID ||
		len(strings.Fields(instanceID)) != 1 {
		return fmt.Errorf("清理所有权参数无效: pid=%d instanceID=%q", pid, instanceID)
	}
	var cleanupErr error
	// 精确 temp 残留（上一代未清理干净的，按 stale 协议一并清）。
	if err := fileutil.CleanupKnownTempFiles(dataDir, []string{pidTempPrefix, stateTempPrefix}); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	// state 文件：仅当归属匹配时删。
	if st, err := ReadRuntimeState(StatePath(dataDir)); err == nil {
		if st.PID == pid && st.InstanceID == instanceID {
			if rerr := os.Remove(StatePath(dataDir)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理 runtime-state 文件失败: %w", rerr))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("读取 runtime-state 所有权失败，未删除文件: %w", err))
	}
	// PID 文件：仅当归属匹配时删。
	if filePID, fileInst, err := ReadPIDFile(PIDPath(dataDir)); err == nil {
		if filePID == pid && fileInst == instanceID {
			if rerr := os.Remove(PIDPath(dataDir)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("清理 PID 文件失败: %w", rerr))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("读取 PID 所有权失败，未删除文件: %w", err))
	}
	return cleanupErr
}

package update

// intent.go 实现「恢复意图快照」：在停止任何服务（dashboard / daemon）之前，
// 把两个服务的原运行态与旧 target 的 hash 持久化到 target 同目录的固定名
// 文件。它填补一个窗口：POSIX journal 在 Installer.Install 内部才落盘、
// Windows helper plan 同样在 Install 内写入，而停止动作发生在 Install 之前
// ——进程若在「已 stop、未 Install」之间被硬中断，journal/plan 都不存在，
// 下一次启动将无法恢复任何服务。
//
// 生命周期：
//   - 写入：锁内编排、首次停止之前（写失败则中止更新，服务未被触碰）；
//     快照同时记录旧 target 的 hash 与下载阶段已验证 stage 的 hash，恢复
//     只接受「target 与其一精确匹配」两种状态；
//   - 消费：下一次 Apply 的恢复路径。target 等于旧 hash（替换未发生）→ 以
//     旧二进制幂等恢复 daemon 与 dashboard（OldRestored）；等于新 hash
//     （替换已落地而服务恢复被中断，Windows 上 helper 可能死在 MoveFileEx
//     成功之后）→ 以新二进制幂等恢复（NewInstalled）；两者都不是（二进制
//     被异常替换、损坏或手工改动）→ 保留快照与现场、报错要求人工处理，
//     绝不启动任何服务；
//   - journal 优先：journal 与 intent 并存（中断点晚于 journal 落盘）时，
//     journal 恢复路径精确判定新旧版本并恢复服务，intent 作为冗余在恢复
//     完成点被清除；
//   - Deferred（Windows helper 接管）：intent 保留；helper 成功后清除，
//     helper 失败或被中断时由下一轮 Apply 按上述精确匹配消费。
//
// 与 POSIX journal 的分工：journal 记录「文件事务」的状态（prepared/installed、
// 新旧 hash、backup/stage 派生路径），intent 只记录「服务运行态快照」；两者
// 都记录运行态时以 journal 为准（它的二进制路径判定更精确）。intent 固定名、
// 无 nonce，不参与 SweepStaleTempFiles 的三类前缀清理。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// updateIntentFileSuffix 是恢复意图文件的固定后缀（接 updateTempPrefix，
// 形如 ".token-usage.update-intent"）。刻意避开 sweep 的三类 nonce 前缀。
const updateIntentFileSuffix = ".update-intent"

// intentCurrentVersion 是意图文件的 schema 版本；读取时不认识的版本按模糊
// 文件处理（保留待人工，绝不照做）。
const intentCurrentVersion = 1

// updateIntent 是停止服务前持久化的恢复意图快照。
type updateIntent struct {
	Version int `json:"version"`
	// TargetBasename 是被替换目标的 basename（校验快照与当前 executable 同位）。
	TargetBasename string `json:"target_basename"`
	// OldSHA256 是停止前的 target 内容 hash：恢复路径据此判定「替换未发生」
	//（一致才以旧二进制恢复服务）。
	OldSHA256 string `json:"old_sha256"`
	// NewSHA256 是下载阶段已验证（SHA256 + stage --version）的 stage 内容
	// hash：替换已落地时恢复路径只接受 target 与其精确匹配才以新二进制恢复
	// 服务；两个 hash 都不匹配说明二进制被异常替换、损坏或手工改动，保留
	// 快照要求人工处理，绝不启动。下载路径不可用（stagePath 为空）时留空，
	// 此时旧 hash 不匹配即报人工。
	NewSHA256 string `json:"new_sha256,omitempty"`
	// DaemonWasRunning / ServeWasRunning / ServeAddr 是两个服务的原运行态与
	// dashboard 的原监听地址。
	DaemonWasRunning bool   `json:"daemon_was_running"`
	ServeWasRunning  bool   `json:"serve_was_running"`
	ServeAddr        string `json:"serve_addr,omitempty"`
}

// intentFilePath 返回 target 同目录下恢复意图文件的完整路径。
func intentFilePath(target string) string {
	return filepath.Join(filepath.Dir(target), updateTempPrefix(target)+updateIntentFileSuffix)
}

// writeUpdateIntent 把快照以 0600 原子写入 target 同目录的意图文件。
var writeUpdateIntent = func(path string, intent updateIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("%s: %w", ui.Bi("failed to marshal update intent", "序列化恢复意图失败"), err)
	}
	return writeJournalFile(path, data)
}

// readUpdateIntent 读取并校验意图文件。文件缺失返回 (nil, false, nil)；
// 存在但 JSON 损坏、版本未知或字段非法返回模糊文件错误（保留待人工，绝不
// 照做——错误的快照照做会启动用户没有启动的服务）。
func readUpdateIntent(path string) (*updateIntent, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: %w", ui.Bi("failed to read update intent", "读取恢复意图失败"), err)
	}
	var intent updateIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, false, fmt.Errorf("%s: %w", ui.Bi(
			fmt.Sprintf("leftover update intent at %s is corrupt; files kept for manual handling", path),
			fmt.Sprintf("遗留恢复意图 %s 损坏，保留文件要求人工处理", path),
		), err)
	}
	if intent.Version != intentCurrentVersion {
		return nil, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover update intent at %s has unknown version %d; files kept for manual handling", path, intent.Version),
			fmt.Sprintf("遗留恢复意图 %s 版本未知（%d），保留文件要求人工处理", path, intent.Version),
		))
	}
	if intent.TargetBasename == "" || filepath.Base(path) != updateTempPrefixForBasename(intent.TargetBasename)+updateIntentFileSuffix {
		return nil, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover update intent at %s records a mismatched target basename %q; files kept for manual handling", path, intent.TargetBasename),
			fmt.Sprintf("遗留恢复意图 %s 记录的 target basename %q 与文件位置不符，保留文件要求人工处理", path, intent.TargetBasename),
		))
	}
	if len(intent.OldSHA256) != 64 {
		return nil, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover update intent at %s has an invalid old hash; files kept for manual handling", path),
			fmt.Sprintf("遗留恢复意图 %s 的旧 hash 非法，保留文件要求人工处理", path),
		))
	}
	if intent.NewSHA256 != "" && len(intent.NewSHA256) != 64 {
		return nil, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover update intent at %s has an invalid new hash; files kept for manual handling", path),
			fmt.Sprintf("遗留恢复意图 %s 的新 hash 非法，保留文件要求人工处理", path),
		))
	}
	if intent.ServeWasRunning && intent.ServeAddr == "" {
		return nil, false, fmt.Errorf("%s", ui.Bi(
			fmt.Sprintf("leftover update intent at %s records a running dashboard but is missing its address; files kept for manual handling", path),
			fmt.Sprintf("遗留恢复意图 %s 记录 dashboard 原在运行但缺少监听地址，保留文件要求人工处理", path),
		))
	}
	return &intent, true, nil
}

// updateTempPrefixForBasename 由 target basename 构造事务文件前缀（与
// updateTempPrefix(target) 等价的 basename 形态，供意图文件位置校验使用）。
func updateTempPrefixForBasename(base string) string {
	return "." + base
}

// removeUpdateIntent 删除意图文件，容忍文件已不存在。
func removeUpdateIntent(target string) error {
	err := os.Remove(intentFilePath(target))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", ui.Bi("failed to remove update intent", "删除恢复意图失败"), err)
	}
	return nil
}

// findIntent 读取 target 同目录的意图文件（只读探测，供 Apply 入口的无锁
// 快速路径；真正的恢复在 control lock 内进行）。
func findIntent(target string) (*updateIntent, bool, error) {
	return readUpdateIntent(intentFilePath(target))
}

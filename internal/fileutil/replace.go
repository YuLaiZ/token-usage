// Package fileutil 提供跨平台"完整文件替换"helper。
//
// reader 只接受完整旧值或完整新值：temp 与 target 同目录同卷创建，
// 写完整 bytes 并 Sync/Chmod/Close 后再原子替换；POSIX 用同目录 rename，
// Windows 用 MoveFileEx + MOVEFILE_REPLACE_EXISTING。
package fileutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// replaceOps 注入临时文件创建、替换、删除等底层操作。
// 生产入口 ReplaceCompleteFile 固定使用 defaultReplaceOps()。
// writeFile/syncFile/chmodFile/closeFile 是包内部对 *os.File 方法的可测试封装,
// 不构成对外契约,默认指向标准库行为。
type replaceOps struct {
	CreateTemp func(dir, pattern string) (*os.File, error)
	Replace    func(from, to string) error
	Remove     func(path string) error

	writeFile func(f *os.File, data []byte) error
	syncFile  func(f *os.File) error
	chmodFile func(f *os.File, mode fs.FileMode) error
	closeFile func(f *os.File) error
}

// defaultReplaceOps 返回生产用的默认操作集(POSIX/Windows 由 build tag 选择 rename)。
func defaultReplaceOps() replaceOps {
	return replaceOps{
		CreateTemp: os.CreateTemp,
		Replace:    renameReplace,
		Remove:     os.Remove,
		writeFile: func(f *os.File, data []byte) error {
			return writeComplete(f, data)
		},
		syncFile:  func(f *os.File) error { return f.Sync() },
		chmodFile: func(f *os.File, mode fs.FileMode) error { return f.Chmod(mode) },
		closeFile: func(f *os.File) error { return f.Close() },
	}
}

func writeComplete(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// tempPattern 返回与 target 同目录、同卷的 temp 命名 pattern。
// 由 API 结构保证 temp 与 target 同目录,调用方无法改成跨目录 temp。
func tempPattern(target string) (dir, pattern string) {
	return filepath.Dir(target), TempPrefix(target) + "*"
}

// TempPrefix 返回 ReplaceCompleteFile 在 target 同目录创建 temp 文件时使用的
// basename 前缀(不含 os.CreateTemp 追加的随机部分): "." + base + ".tmp-"。
//
// 导出此 helper 供调用方按 target 推导出与 fileutil 内部一致的精确 temp 前缀,
// 避免跨包硬编码造成耦合脆弱:若未来 tempPattern 改变命名模式,
// runmeta 等清理方自动跟随。返回值形如 ".token-usage.pid.tmp-"。
func TempPrefix(target string) string {
	return "." + filepath.Base(target) + ".tmp-"
}

// ReplaceCompleteFile 以完整替换方式写入 target：在 target 同目录创建 temp,
// 写完 bytes 经 Sync/Chmod/Close 后原子替换为 target。
// 任一步失败都尝试清理 temp;replace 与清理同时失败时用 errors.Join 合并返回,
// 保留主失败原因。
//
// 调用方不能传入 temp 路径或替换底层操作。
func ReplaceCompleteFile(target string, data []byte, mode fs.FileMode) error {
	return replaceCompleteFileWithOps(target, data, mode, defaultReplaceOps())
}

// RenameReplace 原子地把 from 移动到 to，覆盖已存在的 to。
// POSIX 用同目录 os.Rename；Windows 用 MoveFileEx + MOVEFILE_REPLACE_EXISTING。
// 导出版本供需要直接做「移动覆盖」（而非完整写）的调用方复用，例如自更新 helper
// 的 FileMover 把下载的新二进制替换到位。
func RenameReplace(from, to string) error {
	return renameReplace(from, to)
}

// replaceCompleteFileWithOps 用注入的 ops 执行完整替换,便于测试各失败点。
func replaceCompleteFileWithOps(target string, data []byte, mode fs.FileMode, ops replaceOps) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("完整替换目标路径不能为空")
	}
	dir, pattern := tempPattern(target)

	f, err := ops.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temp in %q: %w", dir, err)
	}
	tempName := f.Name()

	// cleanup 在 replace 之前任一步失败时清理 temp，并把清理失败一并返回。
	// ignore close=false 用于 close 本身已经失败/执行过的路径，避免二次关闭。
	cleanup := func(closeFile bool) error {
		var cleanupErr error
		if closeFile {
			if err := ops.closeFile(f); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close temp %q during cleanup: %w", tempName, err))
			}
		}
		if err := ops.Remove(tempName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup temp %q: %w", tempName, err))
		}
		return cleanupErr
	}
	failWithCleanup := func(primary error, closeFile bool) error {
		return errors.Join(primary, cleanup(closeFile))
	}

	if err := ops.writeFile(f, data); err != nil {
		return failWithCleanup(fmt.Errorf("write temp %q: %w", tempName, err), true)
	}
	if err := ops.syncFile(f); err != nil {
		return failWithCleanup(fmt.Errorf("sync temp %q: %w", tempName, err), true)
	}
	if err := ops.chmodFile(f, mode); err != nil {
		return failWithCleanup(fmt.Errorf("chmod temp %q: %w", tempName, err), true)
	}
	if err := ops.closeFile(f); err != nil {
		// close 失败仍需清理 temp(fd 可能已坏,Remove 用路径即可)。
		return failWithCleanup(fmt.Errorf("close temp %q: %w", tempName, err), false)
	}

	if err := ops.Replace(tempName, target); err != nil {
		// 文件已经关闭，只尝试 remove；两项都失败时保留全部原因。
		return failWithCleanup(fmt.Errorf("replace %q -> %q: %w", tempName, target, err), false)
	}
	return nil
}

// CleanupKnownTempFiles 删除 dir 下与 exactPrefixes 中任一精确 basename 前缀
// 匹配的普通文件,忽略文件不存在。不删除近似名、目录或 symlink target,
// 不按文件年龄猜测,不跨目录清理。调用方需自行持有对应锁后再调用。
func CleanupKnownTempFiles(dir string, exactPrefixes []string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("temp 清理目录不能为空")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read dir %q: %w", dir, err)
	}
	var cleanupErr error
	for _, e := range entries {
		name := e.Name()
		if !matchesAnyPrefix(name, exactPrefixes) {
			continue
		}
		// 跳过目录与 symlink:只删普通文件,避免误删目录或跟随 symlink target。
		info, err := e.Info()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stat %q: %w", filepath.Join(dir, name), err))
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			// 忽略文件不存在:残留可能在调用方读取前已被移除。
			if !errors.Is(err, fs.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %q: %w", path, err))
			}
		}
	}
	return cleanupErr
}

// matchesAnyPrefix 报告 name 是否以 prefixes 中任一项为精确前缀。
// "精确"指完整 basename 前缀(包含点号),不做模糊匹配。
func matchesAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		// 空前缀会匹配目录中的所有文件；清理 API 必须把它视为无效输入而非通配符。
		if p == "" {
			continue
		}
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

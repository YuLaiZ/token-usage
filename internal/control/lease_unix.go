// internal/control/lease_unix.go
//go:build !windows

// POSIX 平台的父子 lease 实现。
//
// 机制：父进程用 os.Pipe 创建匿名 pipe，read end 通过 cmd.ExtraFiles 显式传给 child。
// Go exec 包约定：ExtraFiles[i] 在 child 中成为 fd 3+i（0/1/2 是 stdin/out/err）。
// spawn helper 根据 read end 在 ExtraFiles 中的实际索引计算 fd=3+i 并写入
// TOKEN_USAGE_LEASE_FD 环境变量，child 解析该值——禁止硬编码 fd 3（测试会在
// ExtraFiles 前放占位 fd 验证 3+i 计算）。
// Setsid=true 只创建新 session，不得关闭显式继承 fd（ExtraFiles 透传不受 Setsid 影响）。
package control

import (
	"fmt"
	"os"
	"strconv"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// leaseReaderFromEnv 从 env 解析 POSIX 父 lease：读取 TOKEN_USAGE_LEASE_FD，打开对应 fd
// 构造 leaseReader。返回 (reader, ok)；fd 缺失/非法时 ok=false（调用方走独立加锁路径）。
//
// 注意：这里把 fd 转成 *os.File（fdopen 风格——os.NewFile 不 dup，仅包装现有 fd），
// child 关闭 reader 时会 close 该 fd。父进程的 write end 独立，互不影响。
func leaseReaderFromEnv(env []string) (leaseReader, bool) {
	fdStr := lookupEnvValue(env, envLeaseFD)
	if fdStr == "" {
		return nil, false
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil || fd < 3 {
		// fd 必须 >=3（0/1/2 是标准 IO）。非法值视为零散变量。
		return nil, false
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("lease-fd-%d", fd))
	if f == nil {
		return nil, false
	}
	return &fileLeaseReader{f: f}, true
}

// fileLeaseReader 包装 *os.File 实现 leaseReader（POSIX）。
type fileLeaseReader struct {
	f *os.File
}

// WaitForEOF 阻塞读直到 EOF（父关闭 write end）或错误。EOF 即「父 lease 消失」。
// 读到的字节丢弃（pipe 不传业务数据）。错误也视为 lease 消失（fd 失效等）。
func (r *fileLeaseReader) WaitForEOF() {
	if r.f == nil {
		return
	}
	buf := make([]byte, 1)
	for {
		_, err := r.f.Read(buf)
		if err != nil {
			return // EOF 或错误：lease 消失。
		}
	}
}

// Close 关闭 read end fd。幂等。
func (r *fileLeaseReader) Close() {
	if r.f != nil {
		_ = r.f.Close()
	}
}

// leasePipeHolder 持有父进程侧的 pipe 两端 + read end 在 ExtraFiles 中的索引。
type leasePipeHolder struct {
	readFile  *os.File // child 继承的 read end（放入 cmd.ExtraFiles）
	writeFile *os.File // 父进程持有，直到 ready/失败清理
	// extraFilesIndex read end 在 cmd.ExtraFiles 切片中的索引（用于计算 child fd = 3 + index）。
	extraFilesIndex int
}

// newLeasePipe 创建 POSIX lease pipe（实现 leaseHandle 工厂，由 newLeaseContext 调用）。
// extraFilesIndex 默认 0（read end 是唯一 ExtraFiles 条目）；调用方若需在 ExtraFiles 前放
// 占位 fd，应在 spawn helper 内自行管理（生产 spawner 把 readFile 直接 append，index=0）。
func newLeasePipe() (leaseHandle, error) {
	return newLeasePipeHolder(0)
}

// reader 返回 read end 作为 *os.File（daemon.SpawnDetached 放入 cmd.ExtraFiles）。
func (h *leasePipeHolder) reader() interface{} {
	return h.readFile
}

// appendEnv 把 lease fd 写入 env（只追加平台专属变量，instanceID 由 BuildChildEnv 统一负责）。
// fd = 3 + readFile 在 ExtraFiles 的索引。调用方负责把 readFile 放入 cmd.ExtraFiles[h.extraFilesIndex]。
func (h *leasePipeHolder) appendEnv(env []string) []string {
	fd := 3 + h.extraFilesIndex
	return append(env, envLeaseFD+"="+strconv.Itoa(fd))
}

// newLeasePipeHolder 创建匿名 pipe 并记录 read end 在 ExtraFiles 中的索引。
// extraFilesIndex 由调用方决定（read end 在 ExtraFiles 中的位置，可为非零）。
func newLeasePipeHolder(extraFilesIndex int) (*leasePipeHolder, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ui.Bi("failed to create lease pipe", "创建 lease pipe 失败"), err)
	}
	return &leasePipeHolder{
		readFile:        r,
		writeFile:       w,
		extraFilesIndex: extraFilesIndex,
	}, nil
}

// closeWrite 关闭父侧 write end（触发 child read end EOF）。用于 ready 成功后释放 lease。
func (h *leasePipeHolder) closeWrite() {
	if h.writeFile != nil {
		_ = h.writeFile.Close()
		h.writeFile = nil
	}
}

// closeRead 关闭父侧 read end 副本（父不读，避免 fd 泄漏；child 已继承独立副本）。
func (h *leasePipeHolder) closeRead() {
	if h.readFile != nil {
		_ = h.readFile.Close()
		h.readFile = nil
	}
}

// cleanup 父进程清理：关闭两端。spawn 失败/ready 失败时调用。
func (h *leasePipeHolder) cleanup() {
	h.closeRead()
	h.closeWrite()
}

// leaseEnvSummary 用于测试断言：返回 env 中 lease 相关变量（不依赖解析顺序）。
func leaseEnvSummary(env []string) (instance, fdStr string) {
	return lookupEnvValue(env, envInstance), lookupEnvValue(env, envLeaseFD)
}

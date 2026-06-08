// internal/daemon/spawn_unix_lease_test.go
//go:build !windows

// POSIX spawn 的 lease 集成测试。
//
// 关键行为：
//   - Lease != nil 时 read end 放入 ExtraFiles，env 写 fd=3+i（非零索引验证）。
//   - Setsid=true 不受 lease 影响。
//   - Start 后父侧 read end 仍由调用方持有并负责关闭。
//   - child env 过滤三项内部变量 + 追加 instance + fd。
//   - Lease=nil 时行为不变（无 ExtraFiles、env 不含 lease 变量）。
package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestBuildChildEnvWithLease_FiltersAndAppends 先过滤三项内部变量，追加 instance + fd。
func TestBuildChildEnvWithLease_FiltersAndAppends(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"TOKEN_USAGE_START_INSTANCE=OLD",
		"TOKEN_USAGE_LEASE_FD=99",
		"TOKEN_USAGE_LEASE_HANDLE=12345",
		"HOME=/root",
	}
	out := buildChildEnvWithLease(parent, "new-inst", 0)
	if !sliceContains(out, "PATH=/usr/bin") || !sliceContains(out, "HOME=/root") {
		t.Errorf("非内部变量应保留 out=%v", out)
	}
	if !sliceContains(out, "TOKEN_USAGE_START_INSTANCE=new-inst") {
		t.Errorf("应写入新 instance out=%v", out)
	}
	if !sliceContains(out, "TOKEN_USAGE_LEASE_FD=3") {
		t.Errorf("index 0 → fd=3 out=%v", out)
	}
	if sliceContains(out, "TOKEN_USAGE_START_INSTANCE=OLD") {
		t.Errorf("旧 instance 残留 out=%v", out)
	}
	if sliceContains(out, "TOKEN_USAGE_LEASE_HANDLE=12345") {
		t.Errorf("POSIX 不应写 handle out=%v", out)
	}
}

// TestBuildChildEnvWithLease_NonZeroIndex_FDIs3PlusI 非零索引 → fd=3+i（禁止硬编码 3）。
func TestBuildChildEnvWithLease_NonZeroIndex_FDIs3PlusI(t *testing.T) {
	cases := []int{1, 2, 5}
	for _, idx := range cases {
		out := buildChildEnvWithLease(nil, "inst", idx)
		want := "TOKEN_USAGE_LEASE_FD=" + strconv.Itoa(3+idx)
		if !sliceContains(out, want) {
			t.Errorf("index %d → %q 缺失 out=%v", idx, want, out)
		}
	}
}

// TestSpawnDetached_Lease_SetsExtraFilesAndEnvWithPlaceholderFDs 验证 lease 集成到 spawn：
// 在 ExtraFiles 前放占位 fd（模拟调用方先放了别的 ExtraFiles），再放 lease read end，
// 验证 env 中 fd = 3 + 实际索引（不是硬编码 3）。
//
// 这里不真正 spawn（避免依赖可执行文件），而是用一个 fake bin + 直接断言 cmd 构造逻辑。
// 为此用 helper 抽出 buildCmdForLease（生产代码内联在 SpawnDetached，这里测试 buildChildEnv
// 与 ExtraFiles 约定）。
func TestSpawnDetached_Lease_ExtraFilesIndexComputes3PlusI(t *testing.T) {
	// 模拟：ExtraFiles 已有 2 个占位 fd（如日志 fd），lease read end 放第 3 个位置（index 2）。
	// env fd 应为 3+2=5。
	r1, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r1.Close()
	r2, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r2.Close()
	rLease, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer rLease.Close()

	// 模拟 cmd.ExtraFiles 已有占位 + lease read end 追加在末尾。
	extraFiles := []*os.File{r1, r2, rLease}
	leaseIdx := len(extraFiles) - 1
	env := buildChildEnvWithLease([]string{"PATH=/usr/bin"}, "inst-abc", leaseIdx)
	fdStr := envLookup(env, "TOKEN_USAGE_LEASE_FD")
	if fdStr != "5" {
		t.Errorf("占位 fd 在前 + lease index=2 → env fd=5，实际 %q", fdStr)
	}
	if envLookup(env, "TOKEN_USAGE_START_INSTANCE") != "inst-abc" {
		t.Errorf("instance 写值错 out=%v", env)
	}
}

// TestSpawnDetached_LeaseNil_NoExtraFiles Lease=nil 时不应设置 ExtraFiles 或 lease env。
// 用一个真实但立即退出的 fake bin（echo）验证 cmd 构造不 panic 且 env 不含 lease 变量。
func TestSpawnDetached_LeaseNil_NoLeaseEnv(t *testing.T) {
	// 用 /bin/true 作为 fake bin（立即退出，不依赖 token-usage 二进制）。
	opts := SpawnOptions{BinPath: resolveTrueBin(t), Args: []string{}}
	cmd, err := SpawnDetached(opts)
	if err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	if cmd == nil {
		t.Fatal("cmd 不应为 nil")
	}
	// Lease=nil → 不设置 ExtraFiles。
	if len(cmd.ExtraFiles) != 0 {
		t.Errorf("Lease=nil 时 ExtraFiles 应为空，实际 %d", len(cmd.ExtraFiles))
	}
	// cmd.Env 未被设置（nil）= 继承父 env，不含 lease 变量。
	if cmd.Env != nil {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "TOKEN_USAGE_LEASE_FD=") ||
				strings.HasPrefix(kv, "TOKEN_USAGE_START_INSTANCE=") {
				t.Errorf("Lease=nil 时 env 不应含 lease 变量: %q", kv)
			}
		}
	}
	// 清理：等待子进程退出。
	_ = cmd.Wait()
}

// TestSpawnDetached_Lease_ParentReadEndRemainsCallerOwned Start 后父侧 read end
// 仍由调用方持有；control 层会在确认 child 继承成功后关闭自己的副本。
func TestSpawnDetached_Lease_ParentReadEndRemainsCallerOwned(t *testing.T) {
	rLease, wLease, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// write end 父持有（模拟），测试结束关闭。
	defer wLease.Close()

	opts := SpawnOptions{
		BinPath: resolveTrueBin(t),
		Args:    []string{},
		Lease:   &LeaseSpawnInput{InstanceID: "inst-test", Reader: rLease},
	}
	cmd, err := SpawnDetached(opts)
	if err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// SpawnDetached 不争夺 read end 的关闭所有权；调用方首次关闭必须成功。
	if err := rLease.Close(); err != nil {
		t.Errorf("Start 后父侧 read end 应仍由调用方持有并可关闭: %v", err)
	}
}

// TestSpawnDetached_Lease_WrongReaderTypeReturnsErr Lease.Reader 非 *os.File → 返回错误。
func TestSpawnDetached_Lease_WrongReaderTypeReturnsErr(t *testing.T) {
	opts := SpawnOptions{
		BinPath: resolveTrueBin(t),
		Args:    []string{},
		Lease:   &LeaseSpawnInput{InstanceID: "inst", Reader: "not-a-file"},
	}
	_, err := SpawnDetached(opts)
	if err == nil {
		t.Fatal("Lease.Reader 非 *os.File 应返回错误")
	}
	if !strings.Contains(err.Error(), "*os.File") {
		t.Errorf("错误应提示 *os.File，实际: %v", err)
	}
}

// TestSpawnDetached_Lease_SetsidUnaffected lease 不影响 Setsid。
func TestSpawnDetached_Lease_SetsidUnaffected(t *testing.T) {
	rLease, wLease, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer rLease.Close()
	defer wLease.Close()
	opts := SpawnOptions{
		BinPath: resolveTrueBin(t),
		Args:    []string{},
		Lease:   &LeaseSpawnInput{InstanceID: "inst", Reader: rLease},
	}
	cmd, err := SpawnDetached(opts)
	if err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	defer func() { _ = cmd.Wait() }()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("lease 集成后 Setsid 仍应为 true")
	}
}

// resolveTrueBin 查找系统 true 命令路径（跨平台：macOS=/usr/bin/true，linux=/bin/true）。
// 找不到时 skip 测试（CI 环境理论上总有 true）。
func resolveTrueBin(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"true", "/bin/true", "/usr/bin/true"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("系统找不到 true 命令，跳过 spawn 集成测试")
	return ""
}

// ---- helpers ----

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func envLookup(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// 防 exec 未使用 import（在部分 helper 中可能用到）。
var _ = exec.Command

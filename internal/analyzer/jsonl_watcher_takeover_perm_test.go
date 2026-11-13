//go:build !windows

package analyzer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// skipIfRootOrNoChmod：Linux root 绕过目录读权限（chmod 000 仍可 ReadDir），
// 权限注入夹具在 root 下无区分度，跳过（Docker linux/amd64 验证先例）。
func skipIfRootOrNoChmod(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 权限注入无区分度")
	}
}

// 运行期向可选根内移入含不可读子目录的账号树：接管失败须降级为 deferred，
// 不得使 watcher/analyzer 退出、不得拖停强根监控；恢复权限后经 ticker 重试
// 恢复接管并补采（R6-P2-2 与 R6-P2-3 的移入形态）。
func TestJSONLWatcher_OptionalRootTreeMovedInUnreadable(t *testing.T) {
	skipIfRootOrNoChmod(t)
	tmpDir := t.TempDir()
	mainDir := filepath.Join(tmpDir, "main")
	accounts := filepath.Join(tmpDir, "accounts")
	for _, d := range []string{mainDir, accounts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{mainDir}, []string{accounts}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	watcher.retryInterval = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// staging 预建账号树：projects 内有会话文件、config 下有不可读子目录。
	staging := t.TempDir()
	acct := filepath.Join(staging, "acct-a")
	projects := filepath.Join(acct, "config", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(acct, "config", "denied")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	// denied 树内的文件在不可读期间不可渐进补采，恢复后经 ticker 重试采集——
	// 使末段恢复断言具有独立区分力（区别于中段的 projects 渐进补采）。
	if err := os.WriteFile(filepath.Join(denied, "deep.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	// 整树移入可选根：Create 事件接管在 denied 处失败，须降级不退出。
	// 断言键 = rename 之后的最终路径（submit 携带接管时磁盘上的实际路径）。
	deniedAfterMove := filepath.Join(accounts, "acct-a", "config", "denied")
	sess := filepath.Join(accounts, "acct-a", "config", "projects", "sess.jsonl")
	deep := filepath.Join(deniedAfterMove, "deep.jsonl")
	if err := os.Rename(acct, filepath.Join(accounts, "acct-a")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// watcher 未退出：强根监控仍响应。
	mainFile := filepath.Join(mainDir, "alive.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !rec.saw(mainFile) {
		t.Fatalf("可选根树内接管失败不得拖停强根监控: seen=%v", rec.snapshot())
	}
	if rec.saw(deep) {
		t.Fatalf("denied 不可读期间其内部文件不得被采集: seen=%v", rec.snapshot())
	}
	// 渐进补采语义：denied 之前可读的树内文件即时补采（部分树不可读不拖延迟
	// 可读部分），无需等 denied 恢复。
	if !rec.saw(sess) {
		t.Fatalf("移入树的可读部分应渐进补采: seen=%v", rec.snapshot())
	}
	// R8：故障持续期间（retryInterval=100ms，此处已过约 6 轮 ticker），deferred 只
	// 收敛到 denied 子树——健康兄弟文件不得被周期性重复提交。
	if n := rec.count(sess); n != 1 {
		t.Fatalf("故障持续期间健康文件仅应提交一次（渐进补采），实际 %d 次: seen=%v", n, rec.snapshot())
	}

	// 恢复权限：ticker 重试接管成功并补采树内已有会话。
	if err := os.Chmod(deniedAfterMove, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// 恢复后追加的新文件须经恢复的监听采集（事件路径）。
	appended := filepath.Join(deniedAfterMove, "after-recover.jsonl")
	if err := os.WriteFile(appended, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(deep) {
		t.Fatalf("权限恢复后 ticker 重试应接管 denied 子树并补采其内部文件: seen=%v", rec.snapshot())
	}
	if !rec.saw(appended) {
		t.Fatalf("恢复接管后 denied 子树内的新写入应有监听采集: seen=%v", rec.snapshot())
	}
}

// 启动前可选根内某账号目录不可读：启动注册降级（deferred+锚定）；恢复权限后
// ticker 重试恢复完整监听，此后深层 projects 内的新会话被采集（R6-P2-3 的
// 权限修复形态：不重建目录、无文件事件可用）。
func TestJSONLWatcher_OptionalRootUnreadableSubtreeRecoversAfterChmod(t *testing.T) {
	skipIfRootOrNoChmod(t)
	tmpDir := t.TempDir()
	mainDir := filepath.Join(tmpDir, "main")
	accounts := filepath.Join(tmpDir, "accounts", "claude-accounts")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	acctA := filepath.Join(accounts, "a")
	projects := filepath.Join(acctA, "config", "projects", "proj")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	// 健康兄弟账号 b：启动渐进接管补采一次后，不得随 a 的故障重试被重复提交。
	healthySess := filepath.Join(accounts, "b", "config", "projects", "wb.jsonl")
	if err := os.MkdirAll(filepath.Dir(healthySess), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(healthySess, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(acctA, 0o000); err != nil {
		t.Fatal(err)
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{mainDir}, []string{accounts}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	watcher.retryInterval = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(300 * time.Millisecond) // 启动注册降级（deferred 只记 a；首扫不补采，健康 b 由事件驱动）

	// R8：故障持续期间（约 3 轮 ticker 已过），deferred 只收敛到 a——健康兄弟
	// 零提交（启动首扫不补采，重试范围不含 b）；整根记录的退化形态下 b 会每轮重复。
	if n := rec.count(healthySess); n != 0 {
		t.Fatalf("故障持续期间健康兄弟文件不得被重复提交，实际 %d 次: seen=%v", n, rec.snapshot())
	}

	if err := os.Chmod(acctA, 0o755); err != nil {
		t.Fatal(err)
	}
	// 深层 projects 写新会话：可能落在重试前（existing 补采兜底）或重试后（事件兜底）。
	sess := filepath.Join(projects, "new.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(800 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(sess) {
		t.Fatalf("权限修复后 ticker 重试应恢复监听并采集新会话: seen=%v", rec.snapshot())
	}
}

// T4 原生形态：运行期接管遇到真实权限错误（EACCES）不得被"目录消失"容错
// 吞掉——同批的瞬时消失子树被跳过，权限错误按最小失败子树上抛。
// root 或不支持权限语义的环境按现有规则跳过。
func TestJSONLWatcher_RuntimeTakeoverKeepsPermissionErrors(t *testing.T) {
	skipIfRootOrNoChmod(t)
	tmpDir := t.TempDir()
	parent := filepath.Join(tmpDir, "parent")
	// a-ok 字典序在最前且必然注册成功：在它的 hook（父目录 ReadDir 已完成、
	// 其余兄弟尚未接管）中删除 vanished，确定性制造"同批消失 + 权限错误"并存，
	// 不依赖 denied 的 Add 在各平台上的结果差异。
	aOK := filepath.Join(parent, "a-ok")
	denied := filepath.Join(parent, "denied")
	vanished := filepath.Join(parent, "vanished")
	for _, d := range []string{aOK, denied, vanished} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	watcher.afterWatchHook = func(dir string) {
		if dir == aOK {
			if err := os.RemoveAll(vanished); err != nil {
				t.Errorf("删除 vanished: %v", err)
			}
		}
	}

	_, failed, err := watcher.takeoverDir(context.Background(), parent, false, tolerateVanishedDirs)
	if err == nil {
		t.Fatal("EACCES 不得被消失容错吞掉")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("权限错误不得伪装成目录消失: %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("预期权限错误向上传播: %v", err)
	}
	if !equalStrings(failed, []string{denied}) {
		t.Fatalf("失败列表只应含权限错误子树: %v", failed)
	}
	if _, ok := watcher.watched[vanished]; ok {
		t.Fatalf("消失子树不得登记为已监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[aOK]; !ok {
		t.Fatalf("健康兄弟应正常登记监听: watched=%v", watcher.watched)
	}
}

// Add 成功、ReadDir 遇到真实权限错误（EACCES）的分类点：容错只跳过"目录
// 已消失"，仍存在但不可读的目录必须作为真实失败上抛（不能被 ReadDir 错误
// 吞掉）。用 Add 替身保证本用例只考察 ReadDir 分类点（kqueue 下真实 Add
// 对 000 目录本身就会失败，无法单独命中该分支）。
func TestJSONLWatcher_ReadDirPermissionErrorStaysFailure(t *testing.T) {
	skipIfRootOrNoChmod(t)
	tmpDir := t.TempDir()
	denied := filepath.Join(tmpDir, "denied")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	fake := newPathWatchFake() // Add/Remove 全成功，只让真实 ReadDir 失败
	watcher.addPathWatch = fake.add
	watcher.removePathWatch = fake.remove

	_, failed, err := watcher.takeoverDir(context.Background(), denied, false, tolerateVanishedDirs)
	if err == nil {
		t.Fatal("ReadDir 权限错误不得被消失容错吞掉")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("预期权限错误向上传播: %v", err)
	}
	if !equalStrings(failed, []string{denied}) {
		t.Fatalf("失败列表 = %v, want [%s]", failed, denied)
	}
}

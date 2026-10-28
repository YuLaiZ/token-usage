package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// takeoverRecorder 捕获 submit 的 ChangedFile 及其提交次数。
type takeoverRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func newTakeoverRecorder() *takeoverRecorder {
	return &takeoverRecorder{counts: map[string]int{}}
}

func (r *takeoverRecorder) submit(_ string, req collector.CollectRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[req.ChangedFile]++
}

func (r *takeoverRecorder) saw(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[path] > 0
}

func (r *takeoverRecorder) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[path]
}

func (r *takeoverRecorder) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// 运行期新建子目录并立即写完首个会话文件（建目录与写文件之间零等待）：
// 注册监听前的写入不产生事件，动态接管必须补采树内已有文件。
func TestJSONLWatcher_TakeoverCollectsFilesWrittenBeforeRegistration(t *testing.T) {
	tmpDir := t.TempDir()
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(100 * time.Millisecond) // 等 watcher 就绪

	newDir := filepath.Join(tmpDir, "acct-a", "config", "projects", "p1")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(newDir, "first.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond) // Create 事件 + 接管补采 + debounce
	cancel()
	watcher.Stop()
	if !rec.saw(sess) {
		t.Fatalf("文件在目录接管前写完，接管时应补发采集: seen=%v", rec.snapshot())
	}
}

// 带已有文件的目录整体 rename 移入监控树：移入侧只有目录 Create 事件，
// 树内文件不产生新事件，接管时按现状补采。
func TestJSONLWatcher_TakeoverCollectsFilesInMovedInDir(t *testing.T) {
	tmpDir := t.TempDir()
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	staging := t.TempDir()
	acctDir := filepath.Join(staging, "acct-b")
	movedDir := filepath.Join(acctDir, "config", "projects")
	if err := os.MkdirAll(movedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movedDir, "sess.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(acctDir, filepath.Join(tmpDir, "acct-b")); err != nil {
		t.Fatal(err)
	}
	// 断言键 = rename 之后的最终路径（submit 携带的是接管时磁盘上的实际路径）。
	sess := filepath.Join(tmpDir, "acct-b", "config", "projects", "sess.jsonl")

	time.Sleep(500 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(sess) {
		t.Fatalf("目录带文件移入时应补采树内已有文件: seen=%v", rec.snapshot())
	}
}

// 可选根被同名普通文件占用：强根监控不受影响（文件事件照常触发），随后
// 占用文件被替换为目录树时，经降级锚点的 Create 事件恢复接管并补采。
func TestJSONLWatcher_OptionalRootFailureIsolatedAndRecovers(t *testing.T) {
	tmpDir := t.TempDir()
	mainDir := filepath.Join(tmpDir, "main")
	blocked := filepath.Join(tmpDir, "blocked")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 可选根被同名文件占用（注册必失败形态）。
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{mainDir}, []string{blocked}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	// 强根监控存活：主根文件事件照常触发。
	mainFile := filepath.Join(mainDir, "alive.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !rec.saw(mainFile) {
		t.Fatalf("可选根异常不得拖停强根监控: seen=%v", rec.snapshot())
	}

	// 占用文件替换为目录树：恢复接管并补采树内已有文件。
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	acctProjects := filepath.Join(blocked, "a1", "config", "projects")
	if err := os.MkdirAll(acctProjects, 0o755); err != nil {
		t.Fatal(err)
	}
	recovered := filepath.Join(acctProjects, "back.jsonl")
	if err := os.WriteFile(recovered, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(600 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(recovered) {
		t.Fatalf("可选根修复后应恢复接管并补采: seen=%v", rec.snapshot())
	}
}

// 强根失败语义不变：故障隔离只作用于可选根，强根注册失败仍使 Run 返回错误
// （回归锚定，防止故障隔离改造波及强根合同）。
func TestJSONLWatcher_StrongRootFailureStillFails(t *testing.T) {
	tmpDir := t.TempDir()
	notDir := filepath.Join(tmpDir, "occupier")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewJSONLWatcher([]string{notDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if runErr := watcher.Run(ctx); runErr == nil {
		t.Fatal("强根注册失败应使 Run 返回错误（可选根改造不得放宽强根合同）")
	}
}

// 监听注册先于子项枚举的顺序合同（R6-P2-1）：afterWatchHook 在 addWatch 之后、
// ReadDir 之前注入一次写入——若顺序正确（先 watch 后枚举），该写入经文件事件
// 触发；若实现退回「先枚举后 watch」（如 filepath.Walk 的目录回调时序），写入
// 既不在枚举列表也无监听事件，测试转红。
func TestJSONLWatcher_WatchRegisteredBeforeDirEnumeration(t *testing.T) {
	tmpDir := t.TempDir()
	projects := filepath.Join(tmpDir, "acct", "config", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(projects, "late.jsonl")
	watcher.afterWatchHook = func(dir string) {
		if filepath.Base(dir) == "projects" {
			if err := os.WriteFile(late, []byte("{}\n"), 0o600); err != nil {
				t.Errorf("hook write: %v", err)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(600 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(late) {
		t.Fatalf("监听注册须先于子项枚举：hook 写入应经事件路径触发: seen=%v", rec.snapshot())
	}
}

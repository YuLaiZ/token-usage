package analyzer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YuLaiZ/token-usage/internal/collector"
	"github.com/YuLaiZ/token-usage/internal/config"
)

// recSawResolved 报告记录器是否收到该路径（按解析符号链接后的形态比较：
// 事件路径可能以 /var 或 /private/var 两种写法到达）。
func recSawResolved(r *takeoverRecorder, path string) bool {
	target := resolveSymlinkPath(path)
	for seen := range r.snapshot() {
		if resolveSymlinkPath(seen) == target {
			return true
		}
	}
	return false
}

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

// 符号链接目录下的运行期写入必须触发增量采集：全扫按 os.Stat 跟随链接能采到
// 这类 transcript（会话目录的 .claude 链接到采集根之外），watcher 若只按
// DirEntry.IsDir() 递归就收不到任何事件，两条入口的覆盖范围不一致。
func TestJSONLWatcher_SymlinkedDirWritesTriggerCollection(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	// 目标目录刻意放在采集根之外，只通过符号链接可达。
	target := filepath.Join(base, "outside", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "profile"), link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 只有 Cowork 覆盖范围内的目录链接才跟随（会话目录的 .claude 指向采集面
	// 之外是 Cowork 的落盘形态），因此按 Cowork 根装配。
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	// 等待监听接管完成（该目录无存量文件，静默一段时间即可判定就绪）。
	time.Sleep(500 * time.Millisecond)

	fresh := filepath.Join(target, "sess-a.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 事件按逻辑路径（采集根之下）提交，采集侧据此才能识别 Cowork 受控布局。
	logical := filepath.Join(link, "sess", ".claude", "projects", "session", "sess-a.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("符号链接目录下的写入未被采集（事件应落在采集根内的逻辑路径 %q）: seen=%v", logical, rec.snapshot())
	}
}

// 会话目录的 .claude 链接目标在运行期才创建 projects/ 时，新目录必须经
// 逻辑路径接管，后续写入仍能触发采集。
func TestJSONLWatcher_SymlinkedDirCreatedLaterTriggersCollection(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "profile", "sess", ".claude")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "profile"), link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 运行期在链接目标下逐级创建目录并写入。
	projects := filepath.Join(target, "projects", "session")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	fresh := filepath.Join(projects, "late.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(link, "sess", ".claude", "projects", "session", "late.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("运行期新建的链接目标目录未被接管（应按逻辑路径 %q 上报）: seen=%v", logical, rec.snapshot())
	}
}

// 非 Claude 客户端的目录符号链接不跟随，配置根之外的目标不扩大监听
// 与采集范围（跟随前基线不提交任何请求）。
func TestJSONLWatcher_NonClaudeSymlinkNotFollowed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "sessions")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "codex", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)
	// 目标子目录内写入：链接不被跟随，不应有任何事件。
	sub := filepath.Join(target, "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	cancel()
	w.Stop()
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("非 Claude watcher 不得跟随目录链接扩大范围: seen=%v", rec.snapshot())
	}
}

// 会话目录的 .claude 链接直接挂在 projects/session/ 时，同样要跟随并
// 在写入时触发采集（受控布局的相对层级不变）。
func TestJSONLWatcher_SymlinkedProjectsSubdirTriggersCollection(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "session")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// 逻辑路径：root/acct/profile/sess/.claude/projects/session -> 外部目录
	projectsDir := filepath.Join(root, "acct", "profile", "sess", ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projectsDir, "session")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	fresh := filepath.Join(target, "p.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(projectsDir, "session", "p.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("链接直接挂在 projects/session/ 时写入未被采集（逻辑路径 %q）: seen=%v", logical, rec.snapshot())
	}
}

// 链接目标本身就在另一个监控根内时必须保留原有采集资格：Cowork 会话里的
// uploads/ 链到 Claude projects_dir 时，主根的普通 JSONL 写入不能被改写成
// Cowork 的 uploads 路径（那样会被 Cowork 受控过滤丢弃，主根事件静默消失）。
func TestJSONLWatcher_LinkToOtherRootKeepsNativeEvents(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projects, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Cowork 根：会话目录下 uploads/ 链接到上面的 projects。
	coworkRoot := filepath.Join(base, "cowork")
	sessionDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(projects, filepath.Join(sessionDir, "uploads")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projects}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	mainFile := filepath.Join(projects, "proj", "main.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 事件路径会被归一为解析后的真实形态（macOS 上是 /private/var），按真实
	// 路径判断是否采到即可，不限定字面形态。
	realMain := resolveSymlinkPath(mainFile)
	deadline := time.Now().Add(5 * time.Second)
	for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !recSawResolved(rec, mainFile) {
		t.Fatalf("主根写入被 Cowork 链接截走（期望逻辑归属仍是 %q）: seen=%v", realMain, rec.snapshot())
	}
}

// 链接被删除后，向旧目标写入不得再产生指向原逻辑路径的采集请求。
func TestJSONLWatcher_DeletedLinkStopsOldTargetEvents(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "session")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 确认链接生效：删除前写入能触发采集。
	before := filepath.Join(target, "before.jsonl")
	if err := os.WriteFile(before, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec.snapshot()) == 0 {
		t.Fatalf("链接生效前应能采集: seen=%v", rec.snapshot())
	}

	// 删除链接，等待失效处理生效后向旧目标写入。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	countBefore := len(rec.snapshot())
	if err := os.WriteFile(filepath.Join(target, "after.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	cancel()
	w.Stop()
	if got := rec.snapshot(); len(got) != countBefore {
		t.Fatalf("链接删除后旧目标写入不得触发采集: before=%d after=%v", countBefore, got)
	}
}

// 链接删除后重指到另一目录时，向旧目标写入不得触发采集（否则会以旧逻辑
// 路径提交请求，实际采到的是新目标下的同名文件）。
func TestJSONLWatcher_RetargetedLinkStopsOldTargetEvents(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	oldTarget := filepath.Join(base, "outside-old", "session")
	newTarget := filepath.Join(base, "outside-new", "session")
	for _, d := range []string{oldTarget, newTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "acct", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(oldTarget, "warm.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec.snapshot()) == 0 {
		t.Fatal("链接生效前应能采集")
	}

	// 删除后重指到新目标。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := os.Symlink(newTarget, link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	countBefore := len(rec.snapshot())
	// 旧目标写入：不应再被采集。
	if err := os.WriteFile(filepath.Join(oldTarget, "old-after.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	cancel()
	w.Stop()
	got := rec.snapshot()
	for path := range got {
		if filepath.Base(path) == "old-after.jsonl" {
			t.Fatalf("重指后旧目标写入不得触发采集: seen=%v", got)
		}
	}
	_ = countBefore
}

// 链接撤销后，旧目标下的事件必须被丢弃：既没有根内位置，也没有有效别名。
func TestJSONLWatcher_EventHasAttributionRejectsStaleTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside", "session")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", time.Second, func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)

	// 根外路径 + 无别名：没有归属。
	stale := filepath.Join(outside, "s.jsonl")
	if w.eventHasAttribution(stale, stale) {
		t.Fatal("根外且无别名的事件不应有归属")
	}
	// 根内路径：有归属。
	inside := filepath.Join(root, "a", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if !w.eventHasAttribution(inside, inside) {
		t.Fatal("采集根内的路径应有归属")
	}
}

// 删除 Cowork 别名不得摘掉主项目根的监听：会话里的 uploads/ 链到
// projects_dir 时，两者监听的可能是同一棵真实目录。
func TestJSONLWatcher_DeletingAliasKeepsMainRootWatching(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projects, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	sessionDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sessionDir, "uploads")
	if err := os.Symlink(projects, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projects}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	mainFile := filepath.Join(projects, "proj", "main1.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !recSawResolved(rec, mainFile) {
		t.Fatalf("删除前主根写入应能采集: seen=%v", rec.snapshot())
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2000 * time.Millisecond)
	before := len(rec.snapshot())
	mainFile2 := filepath.Join(projects, "proj", "main2.jsonl")
	if err := os.WriteFile(mainFile2, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for !recSawResolved(rec, mainFile2) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !recSawResolved(rec, mainFile2) {
		t.Fatalf("删除别名后主根仍必须能采集: before=%d seen=%v", before, rec.snapshot())
	}
}

// 链接目标在运行期才创建 projects/：目录创建事件必须经逻辑路径接管，
// 否则通往合法 transcript 的祖先目录被判成根外路径，写入整体漏采。
func TestJSONLWatcher_SymlinkTargetProjectsCreatedLater(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "profile", "sess", ".claude")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "profile"), link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 运行期逐级创建 projects/session 并写入。
	projects := filepath.Join(target, "projects", "session")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond)
	fresh := filepath.Join(projects, "late.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(link, "sess", ".claude", "projects", "session", "late.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("运行期新建的链接目标目录未被接管（逻辑路径 %q）: seen=%v", logical, rec.snapshot())
	}
}

// 链接重指到另一个目录后，新目标里的写入必须触发采集。
func TestJSONLWatcher_RetargetedLinkCollectsNewTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	oldTarget := filepath.Join(base, "old", "session")
	newTarget := filepath.Join(base, "new", "session")
	for _, d := range []string{oldTarget, newTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "acct", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(oldTarget, "warm.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec.snapshot()) == 0 {
		t.Fatal("重指前应能采集")
	}

	// 删除后重指到新目标。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := os.Symlink(newTarget, link); err != nil {
		t.Fatal(err)
	}
	// 触发一次事件让复核发现重指（写入旧目标会被丢弃，但会驱动校验）。
	time.Sleep(300 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(oldTarget, "probe.jsonl"), []byte("{}\n"), 0o600)
	time.Sleep(1200 * time.Millisecond)

	newFile := filepath.Join(newTarget, "fresh.jsonl")
	if err := os.WriteFile(newFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(link, "fresh.jsonl")
	deadline = time.Now().Add(5 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("重指后新目标写入未被采集（逻辑路径 %q）: seen=%v", logical, rec.snapshot())
	}
}

// 重指后新目标里已存在的 transcript 必须补采：这些文件不会再变化，没有事件。
func TestJSONLWatcher_RetargetedLinkCollectsPreexisting(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	oldTarget := filepath.Join(base, "old", "session")
	newTarget := filepath.Join(base, "new", "session")
	for _, d := range []string{oldTarget, newTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	preexisting := filepath.Join(newTarget, "preexisting.jsonl")
	if err := os.WriteFile(preexisting, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 原子替换：删除后重建，并在旧目标写一个事件驱动复核。
	tmpLink := link + ".new"
	if err := os.Symlink(newTarget, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(oldTarget, "probe.jsonl"), []byte("{}\n"), 0o600)

	logical := filepath.Join(link, "preexisting.jsonl")
	deadline := time.Now().Add(6 * time.Second)
	for !rec.saw(logical) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logical) {
		t.Fatalf("重指后存量 transcript 未补采（逻辑路径 %q）: seen=%v", logical, rec.snapshot())
	}
}

// 撤销链接后，只经该链接到达的后代目录监听必须从后端移除。
func TestJSONLWatcher_DeletedLinkReleasesDescendantWatches(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside", "session")
	if err := os.MkdirAll(filepath.Join(target, "projects", "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "acct", "profile", "sess", ".claude")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "session"), link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{root}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(target, "projects", "session", "warm.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	// 触发一次事件让全量复核运行。
	_ = os.WriteFile(filepath.Join(filepath.Dir(link), "probe.jsonl"), []byte("{}\n"), 0o600)
	time.Sleep(1200 * time.Millisecond)

	cancel()
	w.Stop()
	// 用 fsnotify 后端 WatchList 确认真实监听已释放（不只是本地映射）。
	for _, p := range w.watcher.WatchList() {
		if filepath.Clean(p) == filepath.Clean(filepath.Join(link, "projects", "session")) {
			t.Fatalf("撤销链接后代监听仍在后端: %s\nwatchlist=%v", p, w.watcher.WatchList())
		}
	}
}

// 同一真实目录被两个逻辑入口共享时，写入必须经合法入口触发采集：一个会话的
// 合法 projects/session/ 链接与另一个会话的 uploads/ 链接指向同一个根外目录。
// linkMap 只保留最后登记的入口时，uploads/ 会覆盖合法入口，随后被 Cowork
// 受控过滤丢弃，整份 transcript 连续多轮漏采。合法入口排在前、uploads/ 排在
// 后（字母序），正好复现「后登记覆盖」的时序。
func TestJSONLWatcher_SharedLinkTargetKeepsLegalEntry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	shared := filepath.Join(base, "outside", "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	// 合法入口：会话 sessA 的 .claude/projects/session -> shared。
	legalLink := filepath.Join(coworkRoot, "acct", "profile", "sessA", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(legalLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, legalLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	// 非法入口：另一个会话 sessB 的 uploads/ -> 同一 shared。
	uploadsLink := filepath.Join(coworkRoot, "acct", "profile", "sessB", "uploads")
	if err := os.MkdirAll(filepath.Dir(uploadsLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, uploadsLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 共享目标写入：只能经合法入口提交。
	if err := os.WriteFile(filepath.Join(shared, "sess.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legalFile := filepath.Join(legalLink, "sess.jsonl")
	uploadsFile := filepath.Join(uploadsLink, "sess.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(legalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// 主项目根的对照事件必须仍然正常（共享链接不能截走主根归属）。
	mainFile := filepath.Join(projectsRoot, "proj", "main.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(legalFile) {
		t.Fatalf("共享链接目标的写入必须经合法入口触发采集（逻辑路径 %q）: seen=%v", legalFile, rec.snapshot())
	}
	if rec.saw(uploadsFile) {
		t.Fatalf("uploads/ 入口不得触发采集: seen=%v", rec.snapshot())
	}
	if !recSawResolved(rec, mainFile) {
		t.Fatalf("主项目根写入必须正常采集: seen=%v", rec.snapshot())
	}
}

// 不同层级的链接重叠：合法的 .claude 链接指向根外目录，另一个会话的 uploads/
// 指向该目标下的 projects/。事件先命中更深的 projects/ 层，但该层只有不可采集
// 的 uploads/ 入口；必须回退到较浅的 .claude 层经合法入口上报。启动后新建
// session/ 并写入 transcript，文件事件与新目录接管都要走合法入口。
func TestJSONLWatcher_NestedSharedLinkFallsBackToShallowLegalEntry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	realRoot := filepath.Join(base, "outside", "real")
	if err := os.MkdirAll(filepath.Join(realRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 较浅的合法入口：会话 sessA 的 .claude -> realRoot。
	legalLink := filepath.Join(coworkRoot, "acct", "profileA", "sessA", ".claude")
	if err := os.MkdirAll(filepath.Dir(legalLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, legalLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	// 较深的非采集入口：会话 sessB 的 uploads/ -> realRoot/projects。
	uploadsLink := filepath.Join(coworkRoot, "acct", "profileB", "sessB", "uploads")
	if err := os.MkdirAll(filepath.Dir(uploadsLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realRoot, "projects"), uploadsLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	// 记录新目录接管实际使用的逻辑路径：必须落在合法入口下，不能是 uploads/。
	legalDir := filepath.Join(legalLink, "projects", "session")
	var hookMu sync.Mutex
	sawLegalDir := false
	w.afterWatchHook = func(dir string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		if filepath.Clean(dir) == filepath.Clean(legalDir) {
			sawLegalDir = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 启动后在真实 projects/ 下新建 session/ 并写入 JSONL（目录与文件事件）。
	sessionDir := filepath.Join(realRoot, "projects", "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sessionDir, "nested.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legalFile := filepath.Join(legalLink, "projects", "session", "nested.jsonl")
	uploadsFile := filepath.Join(uploadsLink, "session", "nested.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(legalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// 主项目根的对照事件仍正常。
	mainFile := filepath.Join(projectsRoot, "proj", "main.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(legalFile) {
		t.Fatalf("深层 uploads/ 不得遮蔽浅层合法入口（期望逻辑路径 %q）: seen=%v", legalFile, rec.snapshot())
	}
	if rec.saw(uploadsFile) {
		t.Fatalf("uploads/ 入口不得触发采集: seen=%v", rec.snapshot())
	}
	if !recSawResolved(rec, mainFile) {
		t.Fatalf("主项目根写入必须正常采集: seen=%v", rec.snapshot())
	}
	// 新目录接管必须走合法入口，否则接管时已存在的存量文件补采会被
	// Cowork 受控过滤丢掉。
	hookMu.Lock()
	defer hookMu.Unlock()
	if !sawLegalDir {
		t.Fatalf("新目录接管必须经合法入口 %q: seen=%v", legalDir, rec.snapshot())
	}
}

// 链接重叠发生在 .claude 的上一级：合法 profile 链接指向根外目录，另一会话的
// uploads/ 指向该目录下已有的会话子目录。watcher 启动后才在会话子目录里逐级
// 创建 .claude/projects/session/ 并写入 JSONL。新建的 .claude 下面还没有
// projects/，目录资格必须按受控布局的合法祖先判定，否则目录接管会回退到
// uploads/ 入口，后续事件沿 uploads 逻辑路径传播并整份漏采。
func TestJSONLWatcher_ProfileLinkNewLayoutGoesToLegalEntry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	realRoot := filepath.Join(base, "outside", "real")
	sessX := filepath.Join(realRoot, "sessX")
	if err := os.MkdirAll(sessX, 0o755); err != nil {
		t.Fatal(err)
	}
	// 合法 profile 链接：acct/profileA -> realRoot。
	legalProfile := filepath.Join(coworkRoot, "acct", "profileA")
	if err := os.MkdirAll(filepath.Dir(legalProfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, legalProfile); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	// 较深的非采集入口：acct/profileB/sessB/uploads -> realRoot/sessX。
	uploadsLink := filepath.Join(coworkRoot, "acct", "profileB", "sessB", "uploads")
	if err := os.MkdirAll(filepath.Dir(uploadsLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sessX, uploadsLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	legalDir := filepath.Join(legalProfile, "sessX", ".claude")
	var hookMu sync.Mutex
	seenDirs := map[string]bool{}
	w.afterWatchHook = func(dir string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		seenDirs[filepath.Clean(dir)] = true
	}
	sawLegalDir := func() bool {
		hookMu.Lock()
		defer hookMu.Unlock()
		return seenDirs[filepath.Clean(legalDir)]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 启动后逐级创建受控布局并写入 JSONL。
	for _, rel := range []string{".claude", ".claude/projects", ".claude/projects/session"} {
		if err := os.MkdirAll(filepath.Join(sessX, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for !sawLegalDir() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(sessX, ".claude", "projects", "session", "profile.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legalFile := filepath.Join(legalDir, "projects", "session", "profile.jsonl")
	uploadsFile := filepath.Join(uploadsLink, ".claude", "projects", "session", "profile.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(legalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// 主项目根的对照事件仍正常。
	mainFile := filepath.Join(projectsRoot, "proj", "main.jsonl")
	if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(legalFile) {
		t.Fatalf("新建受控布局必须经浅层合法入口触发采集（期望 %q）: seen=%v", legalFile, rec.snapshot())
	}
	if rec.saw(uploadsFile) {
		t.Fatalf("uploads/ 入口不得触发采集: seen=%v", rec.snapshot())
	}
	if !recSawResolved(rec, mainFile) {
		t.Fatalf("主项目根写入必须正常采集: seen=%v", rec.snapshot())
	}
	if !sawLegalDir() {
		t.Fatalf("新建 .claude 的目录接管必须走合法入口 %q", legalDir)
	}
}

// Cowork 别名不得截走 Claude 主项目根的新目录接管：uploads/ 链接指向
// projects_dir 时，把一个已含 transcript 的目录移入主根。目录事件必须保留
// 原生 projects_dir 入口（否则接管沿 uploads/ 枚举，存量文件被 Cowork 受控
// 过滤丢弃，而文件不再写入时永远等不到下一次事件）。
func TestJSONLWatcher_MovedInDirKeepsNativeRootOverCoworkAlias(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(projectsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	sessionDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cowork 会话的 uploads/ 指向主项目根（同一条真实目录被两个入口到达）。
	uploadsLink := filepath.Join(sessionDir, "uploads")
	if err := os.Symlink(projectsRoot, uploadsLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 把已含 transcript 的目录整体 rename 移入主根：移入侧只有目录事件，
	// 树内文件不会产生新写入事件，必须由接管补采。
	staging := t.TempDir()
	movedDir := filepath.Join(staging, "moved")
	if err := os.MkdirAll(movedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	movedFile := filepath.Join(movedDir, "moved.jsonl")
	if err := os.WriteFile(movedFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedDir, filepath.Join(projectsRoot, "moved")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(projectsRoot, "moved", "moved.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !recSawResolved(rec, target) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !recSawResolved(rec, target) {
		t.Fatalf("主根移入目录的存量 transcript 必须补采（期望 %q）: seen=%v", target, rec.snapshot())
	}
	// 不得以 uploads/ 逻辑路径提交同一文件。
	uploadsFile := filepath.Join(uploadsLink, "moved", "moved.jsonl")
	if rec.saw(uploadsFile) {
		t.Fatalf("uploads/ 入口不得截走主根目录接管: seen=%v", rec.snapshot())
	}
}

// 反向用例：合法 .claude/projects/session/ 链接指向同一 Cowork 根下另一会话的
// uploads/target/。把已含 transcript 的目录整体移入 target/ 时，事件真实路径
// 虽然在根内，却处于受控排除的 uploads/，不具备目录接管资格；必须继续查找
// 合法别名并经 .claude/ 入口补采，不能让「根内即保留原生路径」挡住合法入口。
func TestJSONLWatcher_MovedInDirUnderCoworkUploadsUsesLegalEntry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(projectsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	uploadsTarget := filepath.Join(coworkRoot, "acct", "profileB", "sessB", "uploads", "target")
	if err := os.MkdirAll(uploadsTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	legalLink := filepath.Join(coworkRoot, "acct", "profileA", "sessA", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(legalLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(uploadsTarget, legalLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 把已含 transcript 的目录整体 rename 移入 uploads/target/。
	staging := t.TempDir()
	movedDir := filepath.Join(staging, "moved")
	if err := os.MkdirAll(movedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movedDir, "moved.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedDir, filepath.Join(uploadsTarget, "moved")); err != nil {
		t.Fatal(err)
	}
	legalFile := filepath.Join(legalLink, "moved", "moved.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(legalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(legalFile) {
		t.Fatalf("Cowork uploads/ 下移入的目录必须经合法 .claude/ 入口补采（期望 %q）: seen=%v", legalFile, rec.snapshot())
	}
	realFile := filepath.Join(uploadsTarget, "moved", "moved.jsonl")
	if rec.saw(realFile) {
		t.Fatalf("uploads/ 真实路径不具备采集资格，不得提交: seen=%v", rec.snapshot())
	}
}

// 原生 Claude 子代理目录不能被 Cowork 别名接管：合法的 .claude/projects/
// session/ 链接指向主根 subagents/workflows/，移入一个含 agent-*.jsonl 与
// meta.json 的 run1/。workflows/<runId>/ 只放行 agent-*.jsonl，合成 probe.jsonl
// 会误判；必须按原生 subagents 入口接管，否则采集器按别名路径识别不到子代理，
// 父会话与标题都会丢失。
func TestJSONLWatcher_MovedInAgentRunKeepsNativeSubagentsDir(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	workflowsDir := filepath.Join(projectsRoot, "proj", "sess-1", "subagents", "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	aliasLink := filepath.Join(coworkRoot, "acct", "profile", "sess", ".claude", "projects", "session")
	if err := os.MkdirAll(filepath.Dir(aliasLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workflowsDir, aliasLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeAgentRoots([]string{projectsRoot}, nil)
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	// 移入一个已含 agent transcript 与 meta 的 run1/ 目录。
	staging := t.TempDir()
	runDir := filepath.Join(staging, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentLine := `{"type":"assistant","sessionId":"parent-session","timestamp":"2026-11-16T10:00:00+08:00","message":{"id":"agent-msg","role":"assistant","model":"m","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(runDir, "agent-123.jsonl"), []byte(agentLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "agent-123.meta.json"), []byte(`{"description":"子代理标题"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(runDir, filepath.Join(workflowsDir, "run1")); err != nil {
		t.Fatal(err)
	}
	nativeFile := filepath.Join(workflowsDir, "run1", "agent-123.jsonl")
	aliasFile := filepath.Join(aliasLink, "run1", "agent-123.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(nativeFile) && !rec.saw(resolveSymlinkPath(nativeFile)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if rec.saw(aliasFile) {
		t.Fatalf("子代理目录不得经 Cowork 别名提交: seen=%v", rec.snapshot())
	}
	if !rec.saw(nativeFile) && !rec.saw(resolveSymlinkPath(nativeFile)) {
		t.Fatalf("移入的子代理目录必须按原生入口补采（期望 %q）: seen=%v", nativeFile, rec.snapshot())
	}

	// 把原生路径交给实际采集器，验证子代理父会话与标题仍能恢复。
	cfg := &config.Config{Clients: map[string]config.Client{
		"claude": {Enabled: true, Paths: map[string]string{"projects_dir": projectsRoot}},
	}}
	c := collector.NewClaudeCollector(cfg)
	res, err := c.Collect(context.Background(), collector.CollectRequest{ChangedFile: nativeFile},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("采集子代理失败: %v", err)
	}
	if len(res.Messages) != 1 || len(res.Sessions) != 1 {
		t.Fatalf("子代理采集结果 messages=%d sessions=%d", len(res.Messages), len(res.Sessions))
	}
	if res.Messages[0].AgentParentID != "parent-session" {
		t.Fatalf("消息父会话 = %q, want parent-session", res.Messages[0].AgentParentID)
	}
	if res.Sessions[0].ID != "agent-123" || res.Sessions[0].AgentParentID != "parent-session" {
		t.Fatalf("子代理会话 = %+v", res.Sessions[0])
	}
	if res.Sessions[0].Title != "子代理标题" {
		t.Fatalf("子代理标题 = %q, want 子代理标题", res.Sessions[0].Title)
	}
}

// 运行期首次创建 Cowork 的 .claude 链接必须复用启动递归的链接规则：监听挂在
// 链接目标的真实路径、枚举与上报走逻辑路径、登记别名。否则目标在根外，后续
// 在目标里新建的 projects/session/ 与 transcript 事件无法还原逻辑路径，整份
// 漏采。普通目录对照组按原路径接管并正常采集。
func TestJSONLWatcher_RuntimeCreatedCoworkLinkFollowsTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		useLink bool
	}{
		{"symlink", true},
		{"plain-dir", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			projectsRoot := filepath.Join(base, "projects")
			if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
				t.Fatal(err)
			}
			coworkRoot := filepath.Join(base, "cowork")
			sessDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
			if err := os.MkdirAll(sessDir, 0o755); err != nil {
				t.Fatal(err)
			}
			var target string
			if tc.useLink {
				target = filepath.Join(base, "outside", "target")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			rec := newTakeoverRecorder()
			w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
			if err != nil {
				t.Fatal(err)
			}
			w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			go w.Run(ctx)
			time.Sleep(500 * time.Millisecond)

			// 运行期首次创建 .claude（链接或普通目录）。
			logicalLink := filepath.Join(sessDir, ".claude")
			if tc.useLink {
				if err := os.Symlink(target, logicalLink); err != nil {
					t.Skipf("symlink 不可用: %v", err)
				}
			} else {
				if err := os.MkdirAll(logicalLink, 0o755); err != nil {
					t.Fatal(err)
				}
				target = logicalLink
			}
			// 等 watcher 处理链接/目录创建事件并完成接管。
			time.Sleep(600 * time.Millisecond)

			// 再在目标下创建受控布局并写入 transcript。
			projectsDir := filepath.Join(target, "projects", "session")
			if err := os.MkdirAll(projectsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
			if err := os.WriteFile(filepath.Join(projectsDir, "new.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logicalFile := filepath.Join(logicalLink, "projects", "session", "new.jsonl")
			deadline := time.Now().Add(5 * time.Second)
			for !rec.saw(logicalFile) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			// 主项目根的对照事件仍正常。
			mainFile := filepath.Join(projectsRoot, "proj", "main.jsonl")
			if err := os.WriteFile(mainFile, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for !recSawResolved(rec, mainFile) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			w.Stop()
			if !rec.saw(logicalFile) {
				t.Fatalf("运行期新建 .claude 后目标内的 transcript 必须经逻辑入口采集（期望 %q）: seen=%v", logicalFile, rec.snapshot())
			}
			if !recSawResolved(rec, mainFile) {
				t.Fatalf("主项目根写入必须正常采集: seen=%v", rec.snapshot())
			}
		})
	}
}

// 首次创建 Cowork 链接时，真实目标的第一次 Add 失败：deferred 重试必须以
// 真实目标为监听路径（不能退回逻辑路径），真实监听成功后才恢复；随后目标内
// 新建的 transcript 必须触发采集。
func TestJSONLWatcher_LinkTargetAddFailureRecoversOnRetry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	sessDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside", "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	w.retryInterval = 100 * time.Millisecond
	var mu sync.Mutex
	attempts := 0
	cleanTarget := filepath.Clean(target)
	resolvedTarget := filepath.Clean(resolveSymlinkPath(target))
	realAdd := w.addPathWatch
	w.addPathWatch = func(p string) error {
		mu.Lock()
		pc := filepath.Clean(p)
		if pc == cleanTarget || pc == resolvedTarget {
			attempts++
			if attempts == 1 {
				mu.Unlock()
				return os.ErrPermission
			}
		}
		mu.Unlock()
		return realAdd(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 首次创建 .claude 链接：第一次 Add(真实目标) 失败，进入 deferred。
	if err := os.Symlink(target, filepath.Join(sessDir, ".claude")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n < 2 {
		t.Fatalf("真实目标 Add 至少应尝试两次（deferred 重试仍走逻辑路径）: attempts=%d", n)
	}

	// 恢复后在目标下新建受控布局并写入 transcript。
	projectsDir := filepath.Join(target, "projects", "session")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(projectsDir, "new.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logicalFile := filepath.Join(sessDir, ".claude", "projects", "session", "new.jsonl")
	for !rec.saw(logicalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logicalFile) {
		t.Fatalf("重试恢复后目标内的 transcript 必须触发采集（期望 %q）: seen=%v", logicalFile, rec.snapshot())
	}
}

// 链接重指后新目标的第一次 Add 失败：重指失败子树必须进入 deferred 并以真实
// 目标重试；恢复后目标内的 transcript 必须触发采集。
func TestJSONLWatcher_RetargetAddFailureRecoversOnRetry(t *testing.T) {
	base := t.TempDir()
	projectsRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(filepath.Join(projectsRoot, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	coworkRoot := filepath.Join(base, "cowork")
	sessDir := filepath.Join(coworkRoot, "acct", "profile", "sess")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetA := filepath.Join(base, "outside", "a")
	targetB := filepath.Join(base, "outside", "b")
	for _, d := range []string{targetA, targetB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logicalLink := filepath.Join(sessDir, ".claude")
	if err := os.Symlink(targetA, logicalLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	rec := newTakeoverRecorder()
	w, err := NewJSONLWatcher([]string{projectsRoot}, []string{coworkRoot}, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClaudeCoworkRoots([]string{coworkRoot}, nil)
	w.retryInterval = 100 * time.Millisecond
	var mu sync.Mutex
	attemptsB := 0
	cleanB := filepath.Clean(targetB)
	resolvedB := filepath.Clean(resolveSymlinkPath(targetB))
	realAdd := w.addPathWatch
	w.addPathWatch = func(p string) error {
		mu.Lock()
		pc := filepath.Clean(p)
		if pc == cleanB || pc == resolvedB {
			attemptsB++
			if attemptsB == 1 {
				mu.Unlock()
				return os.ErrPermission
			}
		}
		mu.Unlock()
		return realAdd(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)

	// 重指链接到 targetB，并写主根文件触发 revalidateAllLinkAliases。
	if err := os.Remove(logicalLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetB, logicalLink); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	trigger := filepath.Join(projectsRoot, "proj", "trigger.jsonl")
	if err := os.WriteFile(trigger, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attemptsB
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n := attemptsB
	mu.Unlock()
	if n < 2 {
		t.Fatalf("重指后新目标 Add 至少应尝试两次（失败子树未进入 deferred）: attempts=%d", n)
	}

	projectsDir := filepath.Join(targetB, "projects", "session")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(projectsDir, "new.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logicalFile := filepath.Join(logicalLink, "projects", "session", "new.jsonl")
	for !rec.saw(logicalFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	w.Stop()
	if !rec.saw(logicalFile) {
		t.Fatalf("重指重试恢复后目标内的 transcript 必须触发采集（期望 %q）: seen=%v", logicalFile, rec.snapshot())
	}
}

func TestJSONLWatcher_CoworkDirectoryCycle(t *testing.T) {
	for _, entry := range []string{"startup", "runtime"} {
		t.Run(entry, func(t *testing.T) {
			base := t.TempDir()
			main, root := filepath.Join(base, "main"), filepath.Join(base, "cowork")
			projects := filepath.Join(root, "account", "profile", "session", ".claude", "projects")
			work := filepath.Join(projects, "work")
			for _, dir := range []string{main, work} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			loop := filepath.Join(projects, "self")
			addLoop := func() {
				if err := os.Symlink(".", loop); err != nil {
					t.Skipf("无法创建目录链接: %v", err)
				}
			}
			if entry == "startup" {
				addLoop()
			}
			w, err := NewJSONLWatcher([]string{main}, []string{root}, "claude", time.Millisecond, newTakeoverRecorder().submit, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			w.SetClaudeCoworkRoots([]string{root}, nil)

			if err := w.registerRoots(t.Context(), false, strictVanishedDirs); err != nil {
				t.Fatal(err)
			}
			if entry == "runtime" {
				addLoop()
				if err := w.registerCreatedDir(t.Context(), loop); err != nil {
					t.Fatal(err)
				}
			}
			if len(w.linkMap) != 0 {
				t.Fatalf("回指链接不应登记别名，得到 %d 个目标", len(w.linkMap))
			}
			for _, path := range w.watcher.WatchList() {
				if pathWithin(loop, path) {
					t.Fatalf("回指链接不应注册后代监听：%s", path)
				}
			}
			if !w.rootWatchEffective(work) {
				t.Fatal("正常项目子目录应保留监听")
			}
		})
	}
}

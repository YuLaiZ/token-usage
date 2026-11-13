package analyzer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/YuLaiZ/token-usage/internal/collector"
)

// pathWatchFake 是 addPathWatch/removePathWatch 的测试替身：只记录调用路径与
// 返回结果，不直接修改 watcher 状态。watched/unconfirmed 维护、清理排序和
// 重建判断全部由真实生产逻辑执行，断言依据来自运行结果而非替身预写。
type pathWatchFake struct {
	mu             sync.Mutex
	addErr         map[string]error   // 按路径固定返回的 Add 错误
	addCalls       []string           // 按调用顺序记录
	removeSeq      map[string][]error // 按路径的 Remove 返回队列，耗尽后回落 removeFallback
	removeFallback error
	removeCalls    []string
	events         []string // 按调用顺序记录 "add:<path>" / "remove:<path>"，供顺序断言
}

func newPathWatchFake() *pathWatchFake {
	return &pathWatchFake{addErr: map[string]error{}, removeSeq: map[string][]error{}}
}

func (f *pathWatchFake) add(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls = append(f.addCalls, path)
	f.events = append(f.events, "add:"+path)
	return f.addErr[path]
}

func (f *pathWatchFake) remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, path)
	f.events = append(f.events, "remove:"+path)
	if q := f.removeSeq[path]; len(q) > 0 {
		f.removeSeq[path] = q[1:]
		return q[0]
	}
	return f.removeFallback
}

func (f *pathWatchFake) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *pathWatchFake) setRemoveFallback(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeFallback = err
}

// removeCountBeforeLastAdd 返回 path 在最后一次 Add 之前出现的 Remove 次数，
// 用于断言「先确认旧监听释放、再重新登记」的顺序。
func removeCountBeforeLastAdd(events []string, path string) int {
	lastAdd := -1
	for i, ev := range events {
		if ev == "add:"+path {
			lastAdd = i
		}
	}
	n := 0
	for _, ev := range events[:lastAdd] {
		if ev == "remove:"+path {
			n++
		}
	}
	return n
}

func (f *pathWatchFake) addedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addCalls...)
}

func (f *pathWatchFake) removedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removeCalls...)
}

func (f *pathWatchFake) addCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.addCalls {
		if p == path {
			n++
		}
	}
	return n
}

func equalStrings(a, b []string) bool {
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

// T1：父目录枚举后、子目录 Add 前消失（运行期容错策略）——失效子树跳过，
// 健康兄弟继续接管，其树内已有会话文件进入补采列表。用真实 Add 记录竞态
// 命中点：显式断言 b-vanish 的 Add 确实执行且返回 ENOENT，排除"目录从未
// 进入枚举"的假通过。
func TestJSONLWatcher_VanishedBeforeAddSkipsSubtreeOnly(t *testing.T) {
	tmpDir := t.TempDir()
	parent := filepath.Join(tmpDir, "parent")
	aFirst := filepath.Join(parent, "a-first")   // 字典序在前，先被接管
	bVanish := filepath.Join(parent, "b-vanish") // 接管 a-first 时被删除
	for _, d := range []string{aFirst, bVanish} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	healthy := filepath.Join(aFirst, "healthy.jsonl")
	if err := os.WriteFile(healthy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)

	// 包装真实 Add 记录 b-vanish 的结果：父目录 ReadDir 已取得 entries（含
	// b-vanish），在接管 a-first 的 hook 中删除它，使 b-vanish 的 Add 命中 ENOENT。
	realAdd := watcher.addPathWatch
	var mu sync.Mutex
	var vanishAddErr error
	watcher.addPathWatch = func(p string) error {
		err := realAdd(p)
		mu.Lock()
		defer mu.Unlock()
		if p == bVanish {
			vanishAddErr = err
		}
		return err
	}
	watcher.afterWatchHook = func(dir string) {
		if dir == aFirst {
			if err := os.RemoveAll(bVanish); err != nil {
				t.Errorf("删除 b-vanish: %v", err)
			}
		}
	}

	existing, failed, err := watcher.takeoverDir(context.Background(), parent, true, tolerateVanishedDirs)
	if err != nil {
		t.Fatalf("瞬时消失不得作为接管错误返回: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("消失子树不得进入失败列表: %v", failed)
	}
	mu.Lock()
	gotAddErr := vanishAddErr
	mu.Unlock()
	if !errors.Is(gotAddErr, fs.ErrNotExist) {
		t.Fatalf("b-vanish 的 Add 应被执行并返回 ENOENT（竞态须真实命中）: %v", gotAddErr)
	}
	found := false
	for _, f := range existing {
		if f == healthy {
			found = true
		}
	}
	if !found {
		t.Fatalf("健康兄弟树内已有文件应进入补采列表: %v", existing)
	}
	if _, ok := watcher.watched[bVanish]; ok {
		t.Fatalf("消失路径不得登记为已监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[aFirst]; !ok {
		t.Fatalf("健康兄弟应正常登记监听: watched=%v", watcher.watched)
	}
}

// T2：Add（含此前已登记、快速返回）成功后、ReadDir 前整树消失——释放失效
// 子树已登记监听。用包内 Add/Remove 替身覆盖三种 Remove 结果分支，断言真实
// 逻辑产生的清理顺序、watched/unconfirmed 变化和重建时的新 Add。
func TestJSONLWatcher_VanishedAfterAddReleasesSubtree(t *testing.T) {
	type testCase struct {
		name string
		// configure 预置 Remove 替身行为（首次清理与重建重试的返回序列）。
		configure func(f *pathWatchFake, tree, sub1, sub2 string)
		// wantUnconfirmed 是首次清理后 unconfirmed 的期望条目数。
		wantUnconfirmed int
		// wantRetakeoverErr 表示重建后的接管应因旧监听无法确认释放而失败。
		wantRetakeoverErr bool
	}
	cases := []testCase{
		{
			name:      "remove-success",
			configure: func(f *pathWatchFake, _, _, _ string) {},
		},
		{
			name: "remove-nonexistent-watch",
			configure: func(f *pathWatchFake, _, _, _ string) {
				f.removeFallback = fmt.Errorf("wrapped: %w", fsnotify.ErrNonExistentWatch)
			},
		},
		{
			name: "release-failed-then-retry-succeeds",
			configure: func(f *pathWatchFake, tree, sub1, sub2 string) {
				boom := errors.New("release failed")
				for _, p := range []string{tree, sub1, sub2} {
					f.removeSeq[p] = []error{boom, nil}
				}
			},
			wantUnconfirmed: 3,
		},
		{
			name: "release-still-fails-on-retakeover",
			configure: func(f *pathWatchFake, tree, sub1, sub2 string) {
				boom := errors.New("release failed")
				boom2 := errors.New("release still failing")
				f.removeSeq[tree] = []error{boom, boom2}
				for _, p := range []string{sub1, sub2} {
					f.removeSeq[p] = []error{boom}
				}
			},
			wantUnconfirmed:   3,
			wantRetakeoverErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tree := filepath.Join(tmpDir, "tree")
			sub1 := filepath.Join(tree, "sub1")
			sub2 := filepath.Join(tree, "sub2")
			for _, d := range []string{sub1, sub2} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(sub1, "s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
				func(string, collector.CollectRequest) {}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(watcher.Stop)
			fake := newPathWatchFake()
			watcher.addPathWatch = fake.add
			watcher.removePathWatch = fake.remove
			tc.configure(fake, tree, sub1, sub2)

			ctx := context.Background()
			// 第一次接管：真实逻辑登记 tree/sub1/sub2 三条监听。
			if _, _, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs); err != nil {
				t.Fatalf("初始接管失败: %v", err)
			}
			if len(watcher.watched) != 3 {
				t.Fatalf("初始接管应登记 3 条监听: watched=%v", watcher.watched)
			}

			// Add（快速返回）后、ReadDir 前整树消失：释放失效子树并跳过。
			if err := os.RemoveAll(tree); err != nil {
				t.Fatal(err)
			}
			if _, failed, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs); err != nil {
				t.Fatalf("消失竞态不得作为接管错误返回: %v", err)
			} else if len(failed) != 0 {
				t.Fatalf("消失竞态不得进入失败列表: %v", failed)
			}

			// 清理顺序：子路径先于父路径（深度降序），同深度按路径名升序。
			wantOrder := []string{sub1, sub2, tree}
			if got := fake.removedPaths(); !equalStrings(got, wantOrder) {
				t.Fatalf("失效监听清理顺序 = %v, want %v", got, wantOrder)
			}
			if got := len(watcher.unconfirmed); got != tc.wantUnconfirmed {
				t.Fatalf("首次清理后 unconfirmed = %d 条, want %d: %v", got, tc.wantUnconfirmed, watcher.unconfirmed)
			}
			wantWatched := 0
			if tc.wantUnconfirmed > 0 {
				wantWatched = 3 // 释放未确认时本地记录保留，等待重试
			}
			if got := len(watcher.watched); got != wantWatched {
				t.Fatalf("首次清理后 watched = %d 条, want %d: %v", got, wantWatched, watcher.watched)
			}

			// 同名目录树重建：恢复遍历必须能重新 Add（或按策略报真实错误）。
			for _, d := range []string{sub1, sub2} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, failed, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs)
			if tc.wantRetakeoverErr {
				if err == nil {
					t.Fatal("旧监听释放仍无法确认时必须报错，不得把同名目录当作已恢复")
				}
				if !equalStrings(failed, []string{tree}) {
					t.Fatalf("失败子树 = %v, want [%s]", failed, tree)
				}
				if got := fake.addCount(tree); got != 1 {
					t.Fatalf("释放未确认时不得执行新的 Add，Add(%s) 调用 %d 次", tree, got)
				}
				if got := len(watcher.unconfirmed); got != 3 {
					t.Fatalf("重试失败后 unconfirmed 应保留 3 条: %v", watcher.unconfirmed)
				}
				return
			}
			if err != nil {
				t.Fatalf("重建后接管失败: %v", err)
			}
			if len(failed) != 0 {
				t.Fatalf("重建后接管失败子树 = %v, want none", failed)
			}
			if len(watcher.unconfirmed) != 0 {
				t.Fatalf("释放确认后 unconfirmed 应清空: %v", watcher.unconfirmed)
			}
			// 重建后每条路径都应执行过第二次（新的）Add。
			for _, p := range []string{tree, sub1, sub2} {
				if got := fake.addCount(p); got != 2 {
					t.Fatalf("重建后应执行新的 Add(%s)，累计次数 = %d, want 2（addCalls=%v）", p, got, fake.addedPaths())
				}
				if _, ok := watcher.watched[p]; !ok {
					t.Fatalf("重建接管后 %s 应重新登记监听: watched=%v", p, watcher.watched)
				}
			}
			// 释放未确认的分支：重建接管必须「先重试释放、确认后才重新 Add」，
			// 不能跳过 Remove 直接登记同名路径。
			if tc.wantUnconfirmed > 0 {
				events := fake.eventLog()
				for _, p := range []string{tree, sub1, sub2} {
					if got := removeCountBeforeLastAdd(events, p); got < 2 {
						t.Fatalf("重建时 %s 应先重试释放（最后一次 Add 之前应有 ≥2 次 Remove），实际 %d 次（events=%v）", p, got, events)
					}
				}
			}
		})
	}
}

// T3：事件循环的三个运行期入口——Remove 后恢复监听、Create/移入接管、
// 目标根祖先逐级创建触发的根接管。瞬时消失不退出，健康目录的已有文件补采、
// 后续写入经事件采集。
func TestJSONLWatcher_RuntimeEntriesTolerateVanishing(t *testing.T) {
	setupWatcher := func(t *testing.T, dirs []string, rec *takeoverRecorder) *JSONLWatcher {
		t.Helper()
		watcher, err := NewJSONLWatcher(dirs, nil, "claude", 50*time.Millisecond, rec.submit, nil)
		if err != nil {
			t.Fatal(err)
		}
		return watcher
	}

	t.Run("remove-triggers-tolerant-restore", func(t *testing.T) {
		tmpDir := t.TempDir()
		aFirst := filepath.Join(tmpDir, "a-first")
		vanish := filepath.Join(tmpDir, "vanish")
		trigger := filepath.Join(tmpDir, "trigger")
		for _, d := range []string{aFirst, vanish, trigger} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		rec := newTakeoverRecorder()
		watcher := setupWatcher(t, []string{tmpDir}, rec)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// 首扫完成后武装 hook：恢复遍历触达 a-first 时删除兄弟 vanish
		// （父目录枚举已完成、vanish 尚未重新枚举），watcher 必须跳过而不是退出。
		var armed atomic.Bool
		watcher.afterWatchHook = func(dir string) {
			if armed.Load() && dir == aFirst {
				if err := os.RemoveAll(vanish); err != nil {
					t.Errorf("删除 vanish: %v", err)
				}
			}
		}
		go watcher.Run(ctx)
		time.Sleep(150 * time.Millisecond)
		armed.Store(true)

		// 删除被监听目录 trigger 触发 Remove 恢复遍历；hook 在遍历中删除 vanish。
		if err := os.RemoveAll(trigger); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)

		// watcher 未退出：重建 trigger 并写入会话，须被事件链路接管采集。
		if err := os.MkdirAll(trigger, 0o755); err != nil {
			t.Fatal(err)
		}
		sess := filepath.Join(trigger, "after-restore.jsonl")
		if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		cancel()
		watcher.Stop()
		if !rec.saw(sess) {
			t.Fatalf("恢复遍历中兄弟目录消失不得拖停 watcher，重建目录的新写入应被采集: seen=%v", rec.snapshot())
		}
	})

	t.Run("create-move-in-takeover", func(t *testing.T) {
		tmpDir := t.TempDir()
		rec := newTakeoverRecorder()
		watcher := setupWatcher(t, []string{tmpDir}, rec)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		staging := t.TempDir()
		moved := filepath.Join(staging, "moved")
		aFirst := filepath.Join(moved, "a-first")
		bVanish := filepath.Join(moved, "b-vanish")
		for _, d := range []string{aFirst, bVanish} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(aFirst, "pre.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		realAdd := watcher.addPathWatch
		var vanishCalled atomic.Bool
		var vanishErr atomic.Value // error：记录 b-vanish 的 Add 实际结果（nil/ENOENT/后端差异错误）
		watcher.addPathWatch = func(p string) error {
			err := realAdd(p)
			if filepath.Base(p) == "b-vanish" {
				vanishCalled.Store(true)
				vanishErr.Store(err)
			}
			return err
		}
		watcher.afterWatchHook = func(dir string) {
			if dir == filepath.Join(tmpDir, "moved", "a-first") {
				if err := os.RemoveAll(filepath.Join(tmpDir, "moved", "b-vanish")); err != nil {
					t.Errorf("删除 b-vanish: %v", err)
				}
			}
		}
		go watcher.Run(ctx)
		time.Sleep(150 * time.Millisecond)

		// 整树移入：Create 事件接管过程中 b-vanish 消失。
		if err := os.Rename(moved, filepath.Join(tmpDir, "moved")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)

		sess := filepath.Join(tmpDir, "moved", "a-first", "pre.jsonl")
		later := filepath.Join(tmpDir, "moved", "a-first", "later.jsonl")
		if err := os.WriteFile(later, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		cancel()
		watcher.Stop()
		// b-vanish 的 Add 必须被实际执行（竞态真实命中）。错误形态随 fsnotify
		// 后端与删除事件的交错而变化：Lstat 先于删除 → ENOENT；后端清理与 Add
		// 并发 → kqueue 可能返回 EBADF 等实现相关错误；Add 早于清理 → 假成功，
		// 由随后的 ReadDir ENOENT 释放路径兜底。只要容错后 watcher 存活且健康
		// 路径照常采集，即证明该竞态类别已被容错覆盖。
		if !vanishCalled.Load() {
			t.Fatal("移入接管中 b-vanish 的 Add 未被实际执行（竞态未命中）")
		}
		gotVanishErr, _ := vanishErr.Load().(error)
		if gotVanishErr == nil {
			t.Logf("b-vanish 的 Add 返回 nil（后端清理滞后），由 ReadDir 阶段兜底")
		}
		if !rec.saw(sess) {
			t.Fatalf("接管前已写入的文件应补采: seen=%v", rec.snapshot())
		}
		if !rec.saw(later) {
			t.Fatalf("接管后的新写入应经事件采集: seen=%v", rec.snapshot())
		}
	})

	t.Run("ancestor-created-stepwise", func(t *testing.T) {
		tmpDir := t.TempDir()
		root := filepath.Join(tmpDir, "root", "projects") // 首扫时不存在，锚定 tmpDir/root
		rec := newTakeoverRecorder()
		watcher := setupWatcher(t, []string{root}, rec)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		go watcher.Run(ctx)
		time.Sleep(150 * time.Millisecond)

		// 逐级创建目标根并在树内写入会话：创建事件应触发根接管并补采。
		sub := filepath.Join(root, "proj-a")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		sess := filepath.Join(sub, "first.jsonl")
		if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		cancel()
		watcher.Stop()
		if !rec.saw(sess) {
			t.Fatalf("祖先逐级创建后目标根应被接管并补采: seen=%v", rec.snapshot())
		}
	})
}

// T4：同一批接管中瞬时消失与真实错误并存——只过滤消失项，真实错误按现状
// 进入失败列表并向上传播；strict 策略（首次启动）对 ENOENT 也不过过滤。
func TestJSONLWatcher_TakeoverFiltersVanishButNotRealErrors(t *testing.T) {
	tmpDir := t.TempDir()
	parent := filepath.Join(tmpDir, "parent")
	vanish := filepath.Join(parent, "sibling-vanish")
	denied := filepath.Join(parent, "sibling-denied")
	for _, d := range []string{vanish, denied} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	newWatcher := func(t *testing.T, fake *pathWatchFake) *JSONLWatcher {
		t.Helper()
		watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
			func(string, collector.CollectRequest) {}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(watcher.Stop)
		fake.addErr[vanish] = fmt.Errorf("watch directory %s: %w", vanish, fs.ErrNotExist)
		fake.addErr[denied] = fmt.Errorf("watch directory %s: %w", denied, fs.ErrPermission)
		watcher.addPathWatch = fake.add
		watcher.removePathWatch = fake.remove
		return watcher
	}

	// 运行期策略：消失项被过滤，权限错误保留。
	watcher := newWatcher(t, newPathWatchFake())
	_, failed, err := watcher.takeoverDir(context.Background(), parent, false, tolerateVanishedDirs)
	if err == nil {
		t.Fatal("同批真实错误必须继续向上传播")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("真实错误不得被消失项掩埋: %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("预期权限错误向上传播: %v", err)
	}
	if !equalStrings(failed, []string{denied}) {
		t.Fatalf("失败列表只应含真实错误子树: %v", failed)
	}

	// strict 策略（首次启动语义）：ENOENT 与真实错误都按失败处理。
	strictWatcher := newWatcher(t, newPathWatchFake())
	_, failed, err = strictWatcher.takeoverDir(context.Background(), parent, false, strictVanishedDirs)
	if err == nil {
		t.Fatal("strict 策略下 Add 失败必须作为错误返回")
	}
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("strict 策略应保留全部失败原因: %v", err)
	}
	if !equalStrings(failed, []string{denied, vanish}) {
		t.Fatalf("strict 失败列表 = %v, want [%s %s]", failed, denied, vanish)
	}
}

// T2 原生形态：真实 fsnotify 后端下目录消失后同名重建——恢复监听后补采
// 树内已有文件，并继续采集后续独立写入（不按平台写死 Remove 的 errno）。
func TestJSONLWatcher_VanishedThenRebuiltNativeRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	proj := filepath.Join(tmpDir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// 删除被监听目录，待 Remove 恢复遍历处理完毕。
	if err := os.RemoveAll(proj); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	// 同名重建并立即写入：接管时补采已存在文件。
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	already := filepath.Join(proj, "already.jsonl")
	if err := os.WriteFile(already, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)

	// 重建后的独立写入：恢复的监听应收到文件事件。
	later := filepath.Join(proj, "later.jsonl")
	if err := os.WriteFile(later, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(already) {
		t.Fatalf("重建接管应补采树内已有文件（实际平台见运行记录）: seen=%v", rec.snapshot())
	}
	if !rec.saw(later) {
		t.Fatalf("重建接管后的新写入应经事件采集: seen=%v", rec.snapshot())
	}
}

// 后端在删除竞态下可能返回非 ENOENT 的实现相关错误（如 kqueue 陈旧 fd 的
// EBADF）：目录确已消失时按竞态跳过（二次 Stat 确认），目录仍存在时真实
// 错误照旧进入失败列表并向上传播。gone 必须在父目录枚举之后、其自身接管
// 之前消失（字典序在前的 a-first 的 hook 中删除），确保命中 Add 失败处的
// 二次确认分支，而不是"目录从未进入枚举"的假通过。
func TestJSONLWatcher_TakeoverTreatsBackendErrorsAsVanishOnlyWhenGone(t *testing.T) {
	tmpDir := t.TempDir()
	parent := filepath.Join(tmpDir, "parent")
	aFirst := filepath.Join(parent, "a-first")
	gone := filepath.Join(parent, "gone")
	kept := filepath.Join(parent, "kept")
	for _, d := range []string{aFirst, gone, kept} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	fake := newPathWatchFake()
	backendErr := fmt.Errorf("register watch: %w", syscall.EBADF)
	fake.addErr[gone] = backendErr
	fake.addErr[kept] = backendErr
	watcher.addPathWatch = fake.add
	watcher.removePathWatch = fake.remove
	watcher.afterWatchHook = func(dir string) {
		if dir == aFirst {
			// 父目录 ReadDir 已取得 entries（含 gone），此刻删除 gone。
			if err := os.RemoveAll(gone); err != nil {
				t.Errorf("删除 gone: %v", err)
			}
		}
	}

	// gone 已消失（后端错误非 ENOENT）：按竞态跳过；kept 目录仍在的同一错误保留。
	_, failed, err := watcher.takeoverDir(context.Background(), parent, false, tolerateVanishedDirs)
	if err == nil {
		t.Fatal("目录仍存在时同一后端错误必须继续向上传播")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("消失项不得进入聚合错误: %v", err)
	}
	if !equalStrings(failed, []string{kept}) {
		t.Fatalf("失败列表 = %v, want [%s]（目录仍存在的后端错误不得被吞）", failed, kept)
	}
	if got := fake.addCount(gone); got != 1 {
		t.Fatalf("gone 的 Add 必须被实际执行（二次确认分支须真实命中），调用 %d 次", got)
	}
	if _, ok := watcher.watched[gone]; ok {
		t.Fatalf("消失子树不得登记为已监听: watched=%v", watcher.watched)
	}

	// 目录仍存在时同一错误按真实失败处理。
	if _, _, err := watcher.takeoverDir(context.Background(), kept, false, tolerateVanishedDirs); err == nil {
		t.Fatal("目录仍存在的 Add 错误必须作为真实失败返回")
	}
}

// 可选根 deferred 重试保持 strict 语义：目录暂缺时 deferred 必须保留（锚点
// 注册失败后 ticker 是唯一恢复途径），同名重建后的下一次重试成功接管并补采。
// 回退为 tolerate 会把"未恢复"当已恢复清空 deferred，使本测试转红。
func TestJSONLWatcher_RetryDeferredKeepsMissingRoot(t *testing.T) {
	tmpDir := t.TempDir()
	accounts := filepath.Join(tmpDir, "accounts")
	mainDir := filepath.Join(tmpDir, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{mainDir}, []string{accounts}, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	// 模拟可选根注册真实失败后的降级状态：deferred 含整根、目录当前不存在。
	watcher.deferred[accounts] = struct{}{}
	ctx := context.Background()

	watcher.retryDeferred(ctx)
	if _, ok := watcher.deferred[accounts]; !ok {
		t.Fatalf("目录暂缺时必须保留 deferred（strict 重试语义），不能当作已恢复: deferred=%v", watcher.deferred)
	}

	// 同名重建：下一次重试成功接管并补采树内已有文件。
	if err := os.MkdirAll(accounts, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(accounts, "sess.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	watcher.retryDeferred(ctx)
	if _, ok := watcher.deferred[accounts]; ok {
		t.Fatalf("重建后重试成功应从 deferred 移除: deferred=%v", watcher.deferred)
	}
	if _, ok := watcher.watched[accounts]; !ok {
		t.Fatalf("重建后应登记监听: watched=%v", watcher.watched)
	}
	deadline := time.Now().Add(time.Second)
	for !rec.saw(sess) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(sess) {
		t.Fatalf("重建接管应补采树内已有文件: seen=%v", rec.snapshot())
	}
}

// 祖先逐级创建触发的根接管必须使用运行期容错：接管遍历中兄弟目录消失
// 时跳过而非向事件循环返回错误（该分支回退 strict 会使本测试转红）。
func TestJSONLWatcher_CreatedAncestorBranchToleratesVanishing(t *testing.T) {
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "root", "projects")
	aFirst := filepath.Join(root, "a-first")
	bVanish := filepath.Join(root, "b-vanish")
	for _, d := range []string{aFirst, bVanish} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	healthy := filepath.Join(aFirst, "healthy.jsonl")
	if err := os.WriteFile(healthy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)

	realAdd := watcher.addPathWatch
	var mu sync.Mutex
	var vanishAddErr error
	watcher.addPathWatch = func(p string) error {
		err := realAdd(p)
		if p == bVanish {
			mu.Lock()
			vanishAddErr = err
			mu.Unlock()
		}
		return err
	}
	watcher.afterWatchHook = func(dir string) {
		if dir == aFirst {
			if err := os.RemoveAll(bVanish); err != nil {
				t.Errorf("删除 b-vanish: %v", err)
			}
		}
	}

	// 模拟祖先（tmp/root）被逐级创建的事件：path 是配置根 root 的祖先。
	if err := watcher.registerCreatedDir(context.Background(), filepath.Join(tmpDir, "root")); err != nil {
		t.Fatalf("祖先逐级创建触发的根接管不得因兄弟目录消失报错: %v", err)
	}
	mu.Lock()
	gotAddErr := vanishAddErr
	mu.Unlock()
	if !errors.Is(gotAddErr, fs.ErrNotExist) {
		t.Fatalf("b-vanish 的 Add 应被执行并返回 ENOENT（竞态须真实命中）: %v", gotAddErr)
	}
	// 接管补采：健康兄弟树内已有文件应经 debounce 提交。
	deadline := time.Now().Add(time.Second)
	for !rec.saw(healthy) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(healthy) {
		t.Fatalf("祖先分支接管应补采健康兄弟树内文件: seen=%v", rec.snapshot())
	}
}

// 事件驱动的失效清理不得清掉未确认标记：释放失败的路径必须在本地记录被
// 事件清理后仍保留 unconfirmed，供同名重建时先重试释放再登记。
func TestJSONLWatcher_InvalidatedTreeReleaseKeepsUnconfirmed(t *testing.T) {
	tmpDir := t.TempDir()
	tree := filepath.Join(tmpDir, "tree")
	sub1 := filepath.Join(tree, "sub1")
	sub2 := filepath.Join(tree, "sub2")
	for _, d := range []string{sub1, sub2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	fake := newPathWatchFake()
	fake.removeFallback = errors.New("release failed")
	watcher.addPathWatch = fake.add
	watcher.removePathWatch = fake.remove

	ctx := context.Background()
	if _, _, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs); err != nil {
		t.Fatalf("初始接管失败: %v", err)
	}
	// Add 后 ReadDir 前消失：释放全部失败 → unconfirmed 3 条，watched 保留。
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, _, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs); err != nil {
		t.Fatalf("消失竞态不得作为接管错误: %v", err)
	}
	if len(watcher.unconfirmed) != 3 {
		t.Fatalf("首次清理后 unconfirmed = %d 条, want 3: %v", len(watcher.unconfirmed), watcher.unconfirmed)
	}
	// Remove/Rename 事件到达：本地记录清除，但未确认标记必须保留。
	if !watcher.releaseInvalidatedTree(tree) {
		t.Fatal("事件清理应报告条目录被移除")
	}
	if len(watcher.watched) != 0 {
		t.Fatalf("事件清理应清除本地记录: %v", watcher.watched)
	}
	if len(watcher.unconfirmed) != 3 {
		t.Fatalf("事件清理不得清掉未确认标记: %v", watcher.unconfirmed)
	}
	// 释放恢复后同名重建：先重试 Remove 确认释放，再重新 Add。
	fake.setRemoveFallback(nil)
	for _, d := range []string{sub1, sub2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, failed, err := watcher.takeoverDir(ctx, tree, false, tolerateVanishedDirs); err != nil || len(failed) != 0 {
		t.Fatalf("重建接管失败: err=%v failed=%v", err, failed)
	}
	if len(watcher.unconfirmed) != 0 {
		t.Fatalf("释放确认后 unconfirmed 应清空: %v", watcher.unconfirmed)
	}
	for _, p := range []string{tree, sub1, sub2} {
		if _, ok := watcher.watched[p]; !ok {
			t.Fatalf("重建接管后 %s 应重新登记监听: watched=%v", p, watcher.watched)
		}
	}
}

// 原生形态：整树 rename 移走后原地同名重建，重建树内的新写入必须被采集。
// kqueue 后端对 rename 只清理事件路径自身、子孙 watch 以旧路径残留；事件
// 驱动的失效清理必须先逐路径释放后端监听，否则重建时 Add 命中陈旧条目，
// 本地误判已恢复，新写入静默丢失（HEAD 基线 2/2 复现）。重建后先等待恢复
// 接管完成再写文件，避免"写入早于接管、由补采兜底"掩盖假 watch。
func TestJSONLWatcher_RenameAwayThenRebuildNativeRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	proj := filepath.Join(tmpDir, "proj")
	sub := filepath.Join(proj, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// 整树 rename 移走（不删除 inode），待失效事件处理完毕，再原地同名重建。
	if err := os.Rename(proj, filepath.Join(tmpDir, "proj-old")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 等恢复接管（Create 事件 → takeover）完成后再写入：若监听是假恢复，
	// 文件事件不会发生，且此时已错过接管补采窗口。
	time.Sleep(250 * time.Millisecond)
	sess := filepath.Join(sub, "new.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	watcher.Stop()
	if !rec.saw(sess) {
		t.Fatalf("整树 rename 移走后同名重建，新写入必须被采集（不得假恢复）: seen=%v", rec.snapshot())
	}
}

// 运行期配置根接管：Stat(root) 成功后父目录（连同整树）被删，父目录 Add 的
// ENOENT 必须回到缺根锚定语义、监听仍存活的祖先，而不是上抛拖停 watcher；
// strict（启动期）保持既有失败语义。
func TestJSONLWatcher_RuntimeAncestorVanishAnchorsAlive(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	vanished := false
	watcher.addPathWatch = func(p string) error {
		if p == treeDir && !vanished {
			vanished = true
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
		}
		return realAdd(p)
	}
	if _, err := watcher.registerRoot(context.Background(), root, false, tolerateVanishedDirs); err != nil {
		t.Fatalf("运行期祖先消失不得拖停 watcher: %v", err)
	}
	if !vanished {
		t.Fatal("竞态未命中（父目录 Add 未被调用）")
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("应锚定仍存活的祖先: watched=%v", watcher.watched)
	}

	// strict 对照：同一竞态按既有语义上抛。
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	strictWatcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(strictWatcher.Stop)
	realAdd2 := strictWatcher.addPathWatch
	vanished2 := false
	strictWatcher.addPathWatch = func(p string) error {
		if p == treeDir && !vanished2 {
			vanished2 = true
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
		}
		return realAdd2(p)
	}
	if _, err := strictWatcher.registerRoot(context.Background(), root, false, strictVanishedDirs); err == nil {
		t.Fatal("strict 下父目录 Add 的 ENOENT 必须按真实错误上抛")
	}
}

// 真实错误（权限）即使目录随后消失也不得被二次 Stat 兜底吞掉：二次确认只
// 适用于已证实的后端竞态错误（EBADF），不把"事后 ENOENT"用作所有错误的通用
// 兜底。
func TestJSONLWatcher_TakeoverKeepsRealErrorsEvenAfterVanish(t *testing.T) {
	tmpDir := t.TempDir()
	parent := filepath.Join(tmpDir, "parent")
	aFirst := filepath.Join(parent, "a-first")
	gone := filepath.Join(parent, "gone")
	for _, d := range []string{aFirst, gone} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := NewJSONLWatcher([]string{tmpDir}, nil, "claude", 50*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	fake := newPathWatchFake()
	fake.addErr[gone] = fmt.Errorf("open %s: %w", gone, fs.ErrPermission)
	watcher.addPathWatch = fake.add
	watcher.removePathWatch = fake.remove
	watcher.afterWatchHook = func(dir string) {
		if dir == aFirst {
			if err := os.RemoveAll(gone); err != nil {
				t.Errorf("删除 gone: %v", err)
			}
		}
	}
	_, failed, err := watcher.takeoverDir(context.Background(), parent, false, tolerateVanishedDirs)
	if err == nil {
		t.Fatal("权限错误即使目录随后消失也必须上抛")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("预期权限错误: %v", err)
	}
	if !equalStrings(failed, []string{gone}) {
		t.Fatalf("失败列表 = %v, want [%s]", failed, gone)
	}
}

// 运行期父目录 Add 返回 ENOENT 后、锚定前根已快速重建：必须按完整接管处理
// （递归监听子目录 + 补采存量文件），只挂顶层监听会静默漏采——父目录未监听、
// 子目录创建事件早于监听，没有另一个 Create 事件可以补救。
func TestJSONLWatcher_RebuiltRootBeforeAnchorTakesOverFully(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(sub, "old.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	rebuilt := false
	watcher.addPathWatch = func(p string) error {
		if p == treeDir && !rebuilt {
			rebuilt = true
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
			// 父目录 Add 命中 ENOENT（整树已不存在）。
			err := realAdd(p)
			// 锚定执行前根已快速重建（含子目录与存量文件）。
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建树: %v", mkErr)
			}
			if wErr := os.WriteFile(old, []byte("{}\n"), 0o600); wErr != nil {
				t.Errorf("重建存量文件: %v", wErr)
			}
			return err
		}
		return realAdd(p)
	}

	if _, err := watcher.registerRoot(context.Background(), root, true, tolerateVanishedDirs); err != nil {
		t.Fatalf("根快速重建时应完整接管而非报错: %v", err)
	}
	if !rebuilt {
		t.Fatal("竞态未命中（父目录 Add 未被调用）")
	}
	if _, ok := watcher.watched[root]; !ok {
		t.Fatalf("重建根应登记监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[sub]; !ok {
		t.Fatalf("重建树内子目录必须被递归接管: watched=%v", watcher.watched)
	}
	deadline := time.Now().Add(time.Second)
	for !rec.saw(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(old) {
		t.Fatalf("重建树内已有文件必须补采: seen=%v", rec.snapshot())
	}
}

// 根在选出存活祖先之后、addWatch(ancestor) 生效之前重建：Create 事件早于
// 监听而丢失，只挂祖先监听会让整个子树在无监听、无事件的状态下静默漏采。
// 挂上祖先后必须回查根：已出现就继续完整接管（递归监听 + 补采存量）。
func TestJSONLWatcher_RootRebuiltBeforeAncestorWatchCovered(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	old := filepath.Join(sub, "old.jsonl")

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	built := false
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir && !built {
			built = true
			// 祖先监听真正生效之前根已重建：创建事件永远不会到达。
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
				return realAdd(p)
			}
			if wErr := os.WriteFile(old, []byte("{}\n"), 0o600); wErr != nil {
				t.Errorf("重建存量文件: %v", wErr)
			}
		}
		return realAdd(p)
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err != nil {
		t.Fatalf("根在祖先监听生效前重建时不应报错: %v", err)
	}
	if !built {
		t.Fatal("竞态未命中（祖先 Add 未被调用）")
	}
	if _, ok := watcher.watched[filepath.Clean(root)]; !ok {
		t.Fatalf("重建根必须登记监听，否则子树后续写入无事件: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[sub]; !ok {
		t.Fatalf("重建树内子目录必须递归接管: watched=%v", watcher.watched)
	}
	deadline := time.Now().Add(time.Second)
	for !rec.saw(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(old) {
		t.Fatalf("重建树内已有文件必须补采: seen=%v", rec.snapshot())
	}
}

// 同一竞态的事件循环形态：根在祖先监听生效前建好后，除存量补采外，随后
// 独立写入的新 JSONL 也必须经新登记的监听触发采集（修复前只有祖先监听，
// 子树内的新文件事件永远收不到）。
func TestJSONLWatcher_RootRebuiltBeforeAncestorWatchNewWritesCollected(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	old := filepath.Join(sub, "old.jsonl")

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	realAdd := watcher.addPathWatch
	built := false
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir && !built {
			built = true
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
				return realAdd(p)
			}
			if wErr := os.WriteFile(old, []byte("{}\n"), 0o600); wErr != nil {
				t.Errorf("重建存量文件: %v", wErr)
			}
		}
		return realAdd(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(old) {
		t.Fatalf("竞态窗口内建好的树必须补采存量: seen=%v", rec.snapshot())
	}

	fresh := filepath.Join(sub, "fresh.jsonl")
	if werr := os.WriteFile(fresh, []byte("{}\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !rec.saw(fresh) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	watcher.Stop()
	if !rec.saw(fresh) {
		t.Fatalf("竞态后子树内新写入必须被监听并采集: seen=%v", rec.snapshot())
	}
}

// 中间目录在祖先监听生效之前出现、配置根在之后才创建：只回查最终根会报告
// 锚定成功，但根在未被监听的中间目录下创建，祖先收不到该事件——存量与后续
// 新写入都静默漏采。逐段回查必须挂上已出现的每一级中间目录（用两级验证
// 不是只补挂根的直接父目录）。
func TestJSONLWatcher_IntermediateDirBuiltBeforeAncestorWatchCovered(t *testing.T) {
	tmpDir := t.TempDir()
	midA := filepath.Join(tmpDir, "a")
	midB := filepath.Join(midA, "b")
	root := filepath.Join(midB, "projects")
	sub := filepath.Join(root, "proj-x")
	old := filepath.Join(sub, "old.jsonl")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	built := false
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir && !built {
			built = true
			// 两级中间目录在祖先监听生效前出现；配置根尚未创建。
			if mkErr := os.MkdirAll(midB, 0o755); mkErr != nil {
				t.Errorf("创建中间目录: %v", mkErr)
			}
		}
		return realAdd(p)
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err != nil {
		t.Fatalf("中间目录已出现时不应报错: %v", err)
	}
	if !built {
		t.Fatal("竞态未命中（祖先 Add 未被调用）")
	}
	if _, ok := watcher.watched[midA]; !ok {
		t.Fatalf("已出现的第一级中间目录必须登记监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[midB]; !ok {
		t.Fatalf("已出现的第二级中间目录必须登记监听（逐段回查）: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[cleanRoot]; ok {
		t.Fatalf("根尚未出现，不应登记: watched=%v", watcher.watched)
	}

	// 模拟中间目录上的 Create 事件：根随后创建，接管并补采存量。
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := watcher.registerCreatedDir(context.Background(), cleanRoot); err != nil {
		t.Fatalf("根创建事件应恢复接管: %v", err)
	}
	if _, ok := watcher.watched[cleanRoot]; !ok {
		t.Fatalf("根出现后应登记监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[sub]; !ok {
		t.Fatalf("根内子目录必须递归接管: watched=%v", watcher.watched)
	}
	deadline := time.Now().Add(time.Second)
	for !rec.saw(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(old) {
		t.Fatalf("根内已有文件必须补采: seen=%v", rec.snapshot())
	}
}

// 同一竞态的事件循环形态：中间目录在祖先监听生效前出现、根在之后创建；
// 根创建事件必须落在中间目录监听上，存量补采与随后新写入都要被采集。
func TestJSONLWatcher_IntermediateDirBuiltBeforeAncestorWatchEvents(t *testing.T) {
	tmpDir := t.TempDir()
	mid := filepath.Join(tmpDir, "a")
	root := filepath.Join(mid, "projects")
	sub := filepath.Join(root, "proj-x")
	old := filepath.Join(sub, "old.jsonl")

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 50*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	realAdd := watcher.addPathWatch
	midWatched := make(chan struct{})
	var midOnce sync.Once
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir {
			if mkErr := os.MkdirAll(mid, 0o755); mkErr != nil {
				t.Errorf("创建中间目录: %v", mkErr)
			}
		}
		err := realAdd(p)
		if p == mid && err == nil {
			midOnce.Do(func() { close(midWatched) })
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go watcher.Run(ctx)
	select {
	case <-midWatched:
	case <-time.After(5 * time.Second):
		cancel()
		watcher.Stop()
		t.Fatal("中间目录监听未生效")
	}

	// 根在 Add 之后创建：创建事件由中间目录监听捕获。
	if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(old, []byte("{}\n"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !rec.saw(old) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(old) {
		cancel()
		watcher.Stop()
		t.Fatalf("根内已有文件必须补采: seen=%v", rec.snapshot())
	}

	fresh := filepath.Join(sub, "fresh.jsonl")
	if wErr := os.WriteFile(fresh, []byte("{}\n"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !rec.saw(fresh) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	watcher.Stop()
	if !rec.saw(fresh) {
		t.Fatalf("根内新写入必须被监听并采集: seen=%v", rec.snapshot())
	}
}

// 中间目录在 Stat 与 Add 之间消失：上一段监听已生效，重建事件可恢复——
// 明确消失按锚定成功回退，不新增启动失败路径（strict/tolerate 一致）。
func TestJSONLWatcher_IntermediateDirVanishesBeforeAddAnchors(t *testing.T) {
	tmpDir := t.TempDir()
	mid := filepath.Join(tmpDir, "a")
	root := filepath.Join(mid, "projects")

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	built := false
	watcher.addPathWatch = func(p string) error {
		switch {
		case p == tmpDir && !built:
			built = true
			if mkErr := os.MkdirAll(mid, 0o755); mkErr != nil {
				t.Errorf("创建中间目录: %v", mkErr)
			}
			return realAdd(p)
		case p == mid:
			// 中间目录在 Stat 与 Add 之间消失。
			if rmErr := os.RemoveAll(mid); rmErr != nil {
				t.Errorf("删除中间目录: %v", rmErr)
			}
			return realAdd(p)
		}
		return realAdd(p)
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, strictVanishedDirs); err != nil {
		t.Fatalf("中间目录明确消失应按锚定成功回退: %v", err)
	}
	if !built {
		t.Fatal("竞态未命中（祖先 Add 未被调用）")
	}
	if _, ok := watcher.watched[mid]; ok {
		t.Fatalf("已消失的中间目录不应登记: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("祖先监听应保留: watched=%v", watcher.watched)
	}
}

// 两轮回查接管都因根短暂消失而跳过时，父目录监听仍然有效：重试耗尽必须
// 保留锚定状态（等待下一次创建事件），不能把"等待重建"误报为致命失败。
func TestJSONLWatcher_AnchoredAfterRepeatedRecheckVanishing(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir {
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
		}
		return realAdd(p)
	}
	// 第一轮接管跳过后的监听释放点：重建根树，让第二轮回查仍看到根。
	rebuilt := false
	watcher.removePathWatch = func(p string) error {
		if p == cleanRoot && !rebuilt {
			rebuilt = true
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
		}
		return nil
	}
	hooks := 0
	watcher.afterWatchHook = func(dir string) {
		if dir == cleanRoot {
			hooks++
			if rmErr := os.RemoveAll(treeDir); rmErr != nil {
				t.Errorf("删除整树: %v", rmErr)
			}
		}
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err != nil {
		t.Fatalf("父监听仍有效时重试耗尽不得报错: %v", err)
	}
	if hooks != 2 {
		t.Fatalf("两轮接管都应命中根消失（hooks=%d）", hooks)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("有效父监听必须保留: watched=%v", watcher.watched)
	}
}

// 一次有效监听都没挂上时重试耗尽必须报错（"持续消失"的合成错误）；不能因为
// 目录消失按容错规则跳过就报告锚定成功。
func TestJSONLWatcher_AllAncestorAddsVanishReportsError(t *testing.T) {
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "a", "projects")

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	watcher.addPathWatch = func(string) error { return fs.ErrNotExist }

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err == nil {
		t.Fatal("一次监听也没挂上时必须报错")
	} else if !strings.Contains(err.Error(), "kept vanishing") {
		t.Fatalf("应是持续消失的合成错误: %v", err)
	}
	if len(watcher.watched) != 0 {
		t.Fatalf("不应登记任何监听: watched=%v", watcher.watched)
	}
}

// 第一轮已挂上有效父监听、第二轮选出的新祖先在 Stat 与 Add 间消失：耗尽时
// 仍应保留锚定状态返回成功，不能因 vanish 型 lastErr 误报致命失败。
func TestJSONLWatcher_AnchoredAncestorVanishOnRetryStillSucceeds(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "x")
	root := filepath.Join(ancestor, "a", "projects")
	sub := filepath.Join(root, "proj-x")
	cleanRoot := filepath.Clean(root)
	if err := os.MkdirAll(ancestor, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	watcher.addPathWatch = func(p string) error {
		switch {
		case p == ancestor:
			// 第一轮：祖先监听生效前根已重建。
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
			return realAdd(p)
		case p == base:
			// 第二轮：缩小后的整树已消失，祖先 Add 命中 ENOENT。
			return fs.ErrNotExist
		}
		return realAdd(p)
	}
	watcher.afterWatchHook = func(dir string) {
		if dir == cleanRoot {
			// 第一轮接管中根（连同中间目录 x）消失，回到锚定流程。
			if rmErr := os.RemoveAll(ancestor); rmErr != nil {
				t.Errorf("删除整树: %v", rmErr)
			}
		}
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err != nil {
		t.Fatalf("首轮已有有效锚点时第二轮的目录消失不得报错: %v", err)
	}
	if _, ok := watcher.watched[ancestor]; !ok {
		t.Fatalf("首轮登记的祖先监听必须保留: watched=%v", watcher.watched)
	}
}

// 回查接管中同轮的真实错误不能被根消失掩盖：tolerate 只过滤消失项，权限
// 等已确认的错误必须原样上抛（不能用事后 Stat 把整轮当作锚定成功）。
func TestJSONLWatcher_AncestorRecheckKeepsRealErrorsAfterRootVanishes(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	projA := filepath.Join(root, "proj-a")
	projB := filepath.Join(root, "proj-b")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	built := false
	watcher.addPathWatch = func(p string) error {
		switch {
		case p == tmpDir && !built:
			built = true
			if mkErr := os.MkdirAll(projA, 0o755); mkErr != nil {
				t.Errorf("创建子目录: %v", mkErr)
			}
			if mkErr := os.MkdirAll(projB, 0o755); mkErr != nil {
				t.Errorf("创建子目录: %v", mkErr)
			}
			return realAdd(p)
		case p == projA:
			return fs.ErrPermission
		case p == projB:
			// 第二个子目录随根一起消失：遍历已保留的权限错误不得被整轮吞掉。
			if rmErr := os.RemoveAll(cleanRoot); rmErr != nil {
				t.Errorf("删除整树: %v", rmErr)
			}
			return realAdd(p)
		}
		return realAdd(p)
	}

	failed, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("同轮根消失不得吞掉已确认的权限错误: err=%v failed=%v", err, failed)
	}
}

// 回查接管窗口内根再次消失（tolerate）：跳过并回到锚定流程，最终锚定成功
// （只登记存活祖先）而不报错；随后的重建仍经 Create 事件恢复。
func TestJSONLWatcher_RootVanishesDuringAncestorRecheckTakeover(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	stage := 0
	watcher.addPathWatch = func(p string) error {
		if stage == 0 && p == tmpDir {
			// 选出祖先之后、监听生效之前根已重建。
			stage = 1
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
		}
		return realAdd(p)
	}
	// 根已完整出现、回查接管遍历前再次消失：takeoverDir 按 tolerate 跳过。
	watcher.afterWatchHook = func(dir string) {
		if stage == 1 && dir == cleanRoot {
			stage = 2
			if rmErr := os.RemoveAll(treeDir); rmErr != nil {
				t.Errorf("删除整树: %v", rmErr)
			}
		}
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs); err != nil {
		t.Fatalf("回查接管中根再次消失时应回到锚定流程而不是报错: %v", err)
	}
	if stage != 2 {
		t.Fatalf("竞态未完整命中（stage=%d）", stage)
	}
	// 释放已删除目录的监听可能无法确认（后端保留 watch 并标记 unconfirmed，
	// 语义上同样不是有效监听）：断言有效监听状态，而非 watched 键不存在。
	if watcher.rootWatchEffective(cleanRoot) {
		t.Fatalf("根已消失，不得有有效监听: watched=%v unconfirmed=%v", watcher.watched, watcher.unconfirmed)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("应锚定仍存活的祖先: watched=%v", watcher.watched)
	}
}

// strict 启动期同一竞态：回查接管中根再次消失时不得新增启动失败路径——
// 祖先监听已生效，重建可经 Create 事件恢复，按锚定成功返回。
func TestJSONLWatcher_RootVanishesDuringAncestorRecheckStrictAnchors(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	stage := 0
	watcher.addPathWatch = func(p string) error {
		if stage == 0 && p == tmpDir {
			stage = 1
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
		}
		return realAdd(p)
	}
	// 根已完整出现、回查接管遍历前再次消失：tolerate 跳过接管，strict 按
	// 明确的根消失回退锚定成功。
	watcher.afterWatchHook = func(dir string) {
		if stage == 1 && dir == cleanRoot {
			stage = 2
			if rmErr := os.RemoveAll(treeDir); rmErr != nil {
				t.Errorf("删除整树: %v", rmErr)
			}
		}
	}

	if _, err := watcher.anchorMissingRoot(context.Background(), root, strictVanishedDirs); err != nil {
		t.Fatalf("回查接管中根消失应按锚定成功返回，不得新增启动失败路径: %v", err)
	}
	if stage != 2 {
		t.Fatalf("竞态未完整命中（stage=%d）", stage)
	}
	// 释放已删除目录的监听可能无法确认（后端保留 watch 并标记 unconfirmed，
	// 语义上同样不是有效监听）：断言有效监听状态，而非 watched 键不存在。
	if watcher.rootWatchEffective(cleanRoot) {
		t.Fatalf("根已消失，不得有有效监听: watched=%v unconfirmed=%v", watcher.watched, watcher.unconfirmed)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("应锚定仍存活的祖先: watched=%v", watcher.watched)
	}
}

// 回查接管发现根仍存在、但子树接管失败（权限等真实错误）：不得按"根消失"
// 回退成锚定成功，必须保留既有失败语义（返回错误与最小失败子树）。
func TestJSONLWatcher_AncestorRecheckKeepsRealErrors(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	cleanRoot := filepath.Clean(root)

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	fake := newPathWatchFake()
	fake.addErr[sub] = fs.ErrPermission
	built := false
	watcher.addPathWatch = func(p string) error {
		if p == tmpDir && !built {
			built = true
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建整树: %v", mkErr)
			}
		}
		return fake.add(p)
	}
	watcher.removePathWatch = fake.remove

	failed, err := watcher.anchorMissingRoot(context.Background(), root, tolerateVanishedDirs)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("根仍存在时子树接管失败必须保留真实错误: err=%v", err)
	}
	if !built {
		t.Fatal("竞态未命中（祖先 Add 未被调用）")
	}
	if len(failed) != 1 || failed[0] != sub {
		t.Fatalf("失败子树应是最小目录 %s: failed=%v", sub, failed)
	}
	if _, ok := watcher.watched[cleanRoot]; !ok {
		t.Fatalf("根监听已登记，不应回退: watched=%v", watcher.watched)
	}
}

// 根在锚定接管中再次消失：tolerate 会跳过并不登记任何监听，此时必须回到
// 锚定流程找到存活祖先；随后的重建（Create 事件）仍要能接管并补采存量与
// 新增文件（缺根锚定语义的核心保证）。
func TestJSONLWatcher_RootVanishesAgainDuringAnchorTakeover(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(sub, "old.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := newTakeoverRecorder()
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond, rec.submit, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	cleanRoot := filepath.Clean(root)
	stage := 0
	watcher.addPathWatch = func(p string) error {
		switch {
		case stage == 0 && p == treeDir:
			// 父目录 Add 命中 ENOENT，随后根快速重建。
			stage = 1
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
			err := realAdd(p)
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建树: %v", mkErr)
			}
			if wErr := os.WriteFile(old, []byte("{}\n"), 0o600); wErr != nil {
				t.Errorf("重建存量文件: %v", wErr)
			}
			return err
		case stage == 1 && p == cleanRoot:
			// 接管根时根再次消失：takeoverDir 会按 tolerate 跳过。
			stage = 2
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("再次删除整树: %v", err)
			}
			return realAdd(p)
		}
		return realAdd(p)
	}

	if _, err := watcher.registerRoot(context.Background(), root, true, tolerateVanishedDirs); err != nil {
		t.Fatalf("根在锚定接管中再次消失时应回锚定而不是报错: %v", err)
	}
	if stage != 2 {
		t.Fatalf("竞态未完整命中（stage=%d）", stage)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("应锚定仍存活的祖先: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[cleanRoot]; ok {
		t.Fatalf("根已消失，不应登记: watched=%v", watcher.watched)
	}

	// 模拟祖先上的 Create 事件：根再次重建，接管应补采存量文件。
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rebuilt := filepath.Join(sub, "rebuilt.jsonl")
	if err := os.WriteFile(rebuilt, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := watcher.registerCreatedDir(context.Background(), cleanRoot); err != nil {
		t.Fatalf("重建后的 Create 事件应恢复接管: %v", err)
	}
	if _, ok := watcher.watched[cleanRoot]; !ok {
		t.Fatalf("重建后应登记根监听: watched=%v", watcher.watched)
	}
	if _, ok := watcher.watched[sub]; !ok {
		t.Fatalf("重建后应递归接管子目录: watched=%v", watcher.watched)
	}
	deadline := time.Now().Add(time.Second)
	for !rec.saw(rebuilt) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.saw(rebuilt) {
		t.Fatalf("重建后的存量文件必须补采: seen=%v", rec.snapshot())
	}
}

// 根在 Add 成功后、ReadDir 前消失且旧监听释放无法确认（unconfirmed）时，
// watched 里的旧登记不能当作接管成功：必须回到锚定流程挂上存活祖先。
func TestJSONLWatcher_UnconfirmedOldWatchDoesNotCountAsTakeover(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	sub := filepath.Join(root, "proj-x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	realAdd := watcher.addPathWatch
	cleanRoot := filepath.Clean(root)
	rebuilt := false
	watcher.addPathWatch = func(p string) error {
		if p == treeDir && !rebuilt {
			rebuilt = true
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
			err := realAdd(p)
			if mkErr := os.MkdirAll(sub, 0o755); mkErr != nil {
				t.Errorf("重建树: %v", mkErr)
			}
			return err
		}
		return realAdd(p)
	}
	watcher.removePathWatch = func(p string) error { return errors.New("release failed") }
	watcher.afterWatchHook = func(dir string) {
		if dir == cleanRoot {
			// Add 成功后、ReadDir 前整树再次消失：释放失败会留下 unconfirmed。
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("接管中删除整树: %v", err)
			}
		}
	}

	if _, err := watcher.registerRoot(context.Background(), root, true, tolerateVanishedDirs); err != nil {
		t.Fatalf("旧监听未确认时不得报错或报成功，应回锚定: %v", err)
	}
	if _, ok := watcher.unconfirmed[cleanRoot]; !ok {
		t.Fatalf("旧监听释放失败应留下 unconfirmed: %v", watcher.unconfirmed)
	}
	if _, ok := watcher.watched[tmpDir]; !ok {
		t.Fatalf("unconfirmed 的旧登记不得当作接管成功，应锚定存活祖先: watched=%v", watcher.watched)
	}
}

// 连续两次接管都因目录消失被跳过（一次监听也没挂上）：必须返回错误而不是
// 报告成功——上层据此决定强根失败/可选根降级，不能让失效根静默存活。
func TestJSONLWatcher_AllTakeoversSkippedReportsError(t *testing.T) {
	tmpDir := t.TempDir()
	treeDir := filepath.Join(tmpDir, "a")
	root := filepath.Join(treeDir, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewJSONLWatcher([]string{root}, nil, "claude", 30*time.Millisecond,
		func(string, collector.CollectRequest) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watcher.Stop)
	watcher.addPathWatch = func(p string) error {
		if p == treeDir {
			// 父目录消失后根随之消失；随后连存活祖先的监听也持续失败。
			if err := os.RemoveAll(treeDir); err != nil {
				t.Errorf("删除整树: %v", err)
			}
		}
		if p == treeDir || p == tmpDir {
			return fmt.Errorf("watch: %w", fs.ErrNotExist)
		}
		return nil
	}
	if _, err := watcher.registerRoot(context.Background(), root, true, tolerateVanishedDirs); err == nil {
		t.Fatal("一次监听都没挂上时必须返回错误，不能报告接管成功")
	}
	if len(watcher.watched) != 0 {
		t.Fatalf("不应留下任何登记: watched=%v", watcher.watched)
	}
}

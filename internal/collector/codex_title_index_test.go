package collector

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCodexIndex 写入索引 fixture（行以 \n 终结；trailingNewline=false 时不补
// 最后一个 \n，模拟正在写入的半行形态）。
func writeCodexIndex(t *testing.T, lines []string, trailingNewline bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, codexSessionIndexFile)
	content := strings.Join(lines, "\n")
	if trailingNewline && len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexDirOf(path string) string { return filepath.Dir(path) }

func TestReadCodexTitleIndexBasics(t *testing.T) {
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"标题 A","updated_at":"2026-05-13T02:25:03.502412Z"}`,
		`{"id":"a-2","thread_name":"标题 B","updated_at":"2026-05-13T03:00:00Z"}`,
	}, true)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a-1"].Name != "标题 A" || got["a-2"].Name != "标题 B" {
		t.Fatalf("got %v", got)
	}
	// 记录携带 updated_at（UnixNano）：落库侧防倒退比较依赖该值。
	want1, _ := time.Parse(time.RFC3339, "2026-05-13T02:25:03.502412Z")
	if got["a-1"].UpdatedAtUnixNano != want1.UnixNano() {
		t.Fatalf("a-1 ts = %d, want %d", got["a-1"].UpdatedAtUnixNano, want1.UnixNano())
	}
}

// TestReadCodexTitleIndexRenameTakesLatestTs：改名行胜出的同时 ts 也是最新行的。
func TestReadCodexTitleIndexRenameTakesLatestTs(t *testing.T) {
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"旧名字","updated_at":"2026-05-01T00:00:00Z"}`,
		`{"id":"a-1","thread_name":"新名字","updated_at":"2026-06-01T00:00:00Z"}`,
	}, true)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-06-01T00:00:00Z")
	if got["a-1"].Name != "新名字" || got["a-1"].UpdatedAtUnixNano != want.UnixNano() {
		t.Fatalf("got %+v", got["a-1"])
	}
}

func TestReadCodexTitleIndexRenameTakesLatest(t *testing.T) {
	// 同 id 改名：updated_at 更新的行胜出（文件顺序与新旧无关）。
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"新名字","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"id":"a-1","thread_name":"旧名字","updated_at":"2026-05-01T00:00:00Z"}`,
	}, true)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got["a-1"].Name != "新名字" {
		t.Fatalf("应取最新 updated_at: %v", got)
	}
}

func TestReadCodexTitleIndexSameTimestampLaterLineWins(t *testing.T) {
	// 同时间戳：文件中后出现的有效记录胜出。
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"前行","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"id":"a-1","thread_name":"后行","updated_at":"2026-06-01T00:00:00Z"}`,
	}, true)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got["a-1"].Name != "后行" {
		t.Fatalf("同时间应后行胜出: %v", got)
	}
}

func TestReadCodexTitleIndexSkipsBadLines(t *testing.T) {
	// 坏 JSON、缺 id、空 thread_name、不可解析 updated_at：逐行跳过，
	// 有效行不受影响；末尾正在写入的半行（无 \n + 坏 JSON）同样只跳过该行。
	path := writeCodexIndex(t, []string{
		`{"id":"good","thread_name":"有效","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"broken json`,
		`{"id":"","thread_name":"缺 id","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"id":"empty-name","thread_name":"","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"id":"bad-ts","thread_name":"坏时间","updated_at":"not-a-time"}`,
	}, false)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["good"].Name != "有效" {
		t.Fatalf("坏行应跳过、有效行保留: %v", got)
	}
}

func TestReadCodexTitleIndexInvalidDoesNotOverrideValid(t *testing.T) {
	// 无效记录（空标题/坏时间）不得覆盖同 id 的有效记录。
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"有效","updated_at":"2026-06-01T00:00:00Z"}`,
		`{"id":"a-1","thread_name":"","updated_at":"2026-07-01T00:00:00Z"}`,
		`{"id":"a-1","thread_name":"更晚但坏时间","updated_at":"bad"}`,
	}, true)
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got["a-1"].Name != "有效" {
		t.Fatalf("无效记录不得覆盖有效记录: %v", got)
	}
}

func TestReadCodexTitleIndexMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadCodexTitleIndex(context.Background(), dir, slog.Default())
	if err != nil {
		t.Fatalf("缺文件应返回空与 nil 错误: %v", err)
	}
	if got != nil {
		t.Fatalf("缺文件应返回 nil map,实际 %v", got)
	}
	// state_dir 本身不存在同样视为无索引。
	got, err = ReadCodexTitleIndex(context.Background(), filepath.Join(dir, "nope"), slog.Default())
	if err != nil || got != nil {
		t.Fatalf("不存在的 state_dir: got=%v err=%v", got, err)
	}
}

func TestReadCodexTitleIndexEmptyStateDir(t *testing.T) {
	got, err := ReadCodexTitleIndex(context.Background(), "", slog.Default())
	if err != nil || got != nil {
		t.Fatalf("空 state_dir 应静默跳过: got=%v err=%v", got, err)
	}
}

func TestReadCodexTitleIndexCanceled(t *testing.T) {
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"标题","updated_at":"2026-06-01T00:00:00Z"}`,
	}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadCodexTitleIndex(ctx, indexDirOf(path), slog.Default())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context error,实际 %v", err)
	}
}

func TestReadCodexTitleIndexUnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 对 0o000 文件仍可读，无法构造不可读形态")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, codexSessionIndexFile)
	if err := os.WriteFile(path, []byte("{}\n"), 0o000); err != nil {
		t.Skipf("无法构造不可读文件: %v", err)
	}
	if _, err := ReadCodexTitleIndex(context.Background(), dir, slog.Default()); err == nil {
		t.Fatal("不可读文件应返回错误供诊断")
	}
}

func TestReadCodexTitleIndexMidReadChangeRereads(t *testing.T) {
	// 读中变化：注入 hook 在首遍读取后、读后指纹 stat 前把文件改写成 v2
	//（size 与 mtime 均拉开），指纹不一致触发有界重读一次，最终取重读结果 v2。
	// 错误实现（丢弃重试结果或漏判指纹）会返回 v1 而被断言抓住。
	path := writeCodexIndex(t, []string{
		`{"id":"a-1","thread_name":"v1","updated_at":"2026-06-01T00:00:00Z"}`,
	}, true)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	codexIndexPostReadHook = func() {
		v2 := []byte("{\"id\":\"a-1\",\"thread_name\":\"v2-reread\",\"updated_at\":\"2026-06-02T00:00:00Z\"}\n")
		if err := os.WriteFile(path, v2, 0o644); err != nil {
			t.Errorf("hook 改写失败: %v", err)
		}
	}
	t.Cleanup(func() { codexIndexPostReadHook = nil })
	got, err := ReadCodexTitleIndex(context.Background(), indexDirOf(path), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got["a-1"].Name != "v2-reread" {
		t.Fatalf("读中变化应经重读取新值: %v", got)
	}
}

func TestCodexTitleIndexPath(t *testing.T) {
	if got := CodexTitleIndexPath(""); got != "" {
		t.Fatalf("空 state_dir 应返回空串,实际 %q", got)
	}
	want := filepath.Join("/x/y", codexSessionIndexFile)
	if got := CodexTitleIndexPath("/x/y"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

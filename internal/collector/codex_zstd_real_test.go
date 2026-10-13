package collector

// Codex 压缩 rollout 适配的真实样本验收：用真实 ~/.codex 下产生的 .jsonl.zst
// 与压缩前备份对账，全程只读源数据、不触达 ~/.token-usage（无 DB 落库；
// engine 门路径见 engine 包同名测试）。
//
// 显式开关（CODEX_ZST_REAL_ACCEPTANCE=1）+ 样本探测双守卫：CI 与无样本环境
// 跳过，样本齐备时显式发起才执行。前置条件：存在 Codex state DB；压缩前的
// sessions 备份已解包到 /tmp/codex-zst-recheck（顶层为 sessions/ 与
// archived_sessions/，与 ~/.codex 相对结构一致）。

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const realBackupRoot = "/tmp/codex-zst-recheck"

// realCodexEnv 探测并返回真实样本环境（sessions 根、state 目录、备份根）；
// 任一前提不满足时 Skip（含未显式开启开关）。
func realCodexEnv(t *testing.T) (sessionsDir, stateDir string) {
	t.Helper()
	if os.Getenv("CODEX_ZST_REAL_ACCEPTANCE") != "1" {
		t.Skip("未设置 CODEX_ZST_REAL_ACCEPTANCE=1（真实样本验收按需显式执行）")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sessionsDir = filepath.Join(home, ".codex", "sessions")
	stateDir = filepath.Join(home, ".codex")
	if _, err := os.Stat(filepath.Join(stateDir, "state_5.sqlite")); err != nil {
		t.Skipf("无 Codex state DB: %v", err)
	}
	if _, err := os.Stat(realBackupRoot); err != nil {
		t.Skipf("压缩前备份未解包到 %s: %v", realBackupRoot, err)
	}
	return sessionsDir, stateDir
}

// countForm 统计目录树下两种 rollout 形态的文件数。
func countForm(t *testing.T, root, suffix string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), suffix) {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A. daemon 重启全扫（ScanExistingJSONL）：磁盘双形态全部采到、无 partialErr、
// 每个压缩文件帧完整解码（Err==nil 且 TrailingNewline==true）。FullyParsed
// 另要求 BadLines==0：真实历史数据存在压缩前即有的存量坏行（与备份 .jsonl
// 双侧同坏，由诊断用例锚定），不构成适配缺陷，零坏行数仅作日志观察值。
func TestCodexReal_ScanExistingCollectsAllCompressed(t *testing.T) {
	sessionsDir, stateDir := realCodexEnv(t)
	wantZst := countForm(t, sessionsDir, ".jsonl.zst")
	wantPlain := countForm(t, sessionsDir, ".jsonl")
	archived := filepath.Join(filepath.Dir(sessionsDir), "archived_sessions")
	if st, err := os.Stat(archived); err == nil && st.IsDir() {
		wantZst += countForm(t, archived, ".jsonl.zst")
		wantPlain += countForm(t, archived, ".jsonl")
	}
	if wantZst == 0 {
		t.Skip("无压缩样本")
	}

	cfg := testCodexConfig(stateDir, sessionsDir)
	c := NewCodexCollector(cfg)
	start := time.Now()
	result, err := c.Collect(context.Background(), CollectRequest{
		Source:            CollectSourceClient,
		ScanExistingJSONL: true,
	}, slog.Default())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("全扫终止性失败: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("全扫不得有 partialErr: %v", result.PartialErr)
	}
	var zstStatus, zstFrameOK, zstClean int
	for _, fs := range result.FileStatuses {
		if !strings.HasSuffix(fs.Path, ".zst") {
			continue
		}
		zstStatus++
		if fs.Err == nil && fs.TrailingNewline {
			zstFrameOK++
			if fs.BadLines == 0 {
				zstClean++
			}
		} else {
			t.Errorf("%s: 帧完整性破坏（Err=%v TrailingNewline=%v）", fs.Path, fs.Err, fs.TrailingNewline)
		}
	}
	if zstStatus != wantZst || zstFrameOK != wantZst {
		t.Fatalf("压缩文件: 采集 %d/%d, 帧完整 %d/%d", zstStatus, wantZst, zstFrameOK, wantZst)
	}
	t.Logf("全扫: 文件 %d（.zst %d + .jsonl %d）, 消息 %d, 会话 %d, 耗时 %s; 零坏行 .zst %d（存量坏行 .zst %d 不写门, 与 .jsonl 同语义）",
		len(result.FileStatuses), wantZst, wantPlain, len(result.Messages), len(result.Sessions), elapsed, zstClean, wantZst-zstClean)
}

// B. state threads 路径（collect all 形态，Dates=nil）：全部 threads 无 ENOENT
// partialErr；已压缩 thread（rollout_path 的 .jsonl 已不存在）逐一经兄弟映射
// 采到（FileStatuses 含对应 .zst 且无 Err）。
func TestCodexReal_ThreadsPathResolvesAllSiblings(t *testing.T) {
	sessionsDir, stateDir := realCodexEnv(t)
	cfg := testCodexConfig(stateDir, sessionsDir)
	c := NewCodexCollector(cfg)

	result, err := c.Collect(context.Background(), CollectRequest{Dates: nil}, slog.Default())
	if err != nil {
		t.Fatalf("threads 路径终止性失败: %v", err)
	}
	if result.PartialErr != nil {
		t.Fatalf("threads 路径不得有 partialErr（ENOENT 即兄弟映射缺口）: %v", result.PartialErr)
	}

	// 逐 thread 核对：rollout_path 指向的 .jsonl 已不存在者，必须以 .zst 兄弟
	// 形态出现在 FileStatuses 且无 Err。
	byPath := make(map[string]FileScanStatus, len(result.FileStatuses))
	for _, fs := range result.FileStatuses {
		byPath[fs.Path] = fs
	}
	compressed, resolved := 0, 0
	for _, dbPath := range mustFindStateDBs(t, stateDir) {
		threads, err := readThreads(context.Background(), dbPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range threads {
			if th.RolloutPath == "" {
				continue
			}
			if _, statErr := os.Stat(th.RolloutPath); statErr == nil {
				continue // 未压缩或已解压回写，非本用例对象
			}
			compressed++
			sib := codexRolloutSibling(th.RolloutPath)
			fs, ok := byPath[sib]
			if ok && fs.Err == nil {
				resolved++
			} else {
				t.Errorf("thread %s 的压缩兄弟 %s 未被无错采集: %+v", th.ID, sib, fs)
			}
		}
	}
	if compressed == 0 {
		t.Skip("threads 中无已压缩会话")
	}
	if resolved != compressed {
		t.Fatalf("已压缩 thread 兄弟映射 %d/%d", resolved, compressed)
	}
	t.Logf("threads 路径: 已压缩 thread %d 全部经兄弟映射采到, 消息 %d", compressed, len(result.Messages))
}

func mustFindStateDBs(t *testing.T, stateDir string) []string {
	t.Helper()
	dbs, err := findStateDBs(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return dbs
}

// C. 压缩前后等价对账：磁盘上每个 .zst 解压解析的结果，与压缩前备份同名
// .jsonl 的解析结果逐消息比较（ID/TS/token 七字段与 DurationMS 均判失败）。
// 备份顶层布局（sessions/ + archived_sessions/）与 ~/.codex 相对结构一致，
// 映射 = 磁盘路径的 ~/.codex 前缀替换为备份根、剥 .zst。
func TestCodexReal_CompressedMatchesPreCompressionBackup(t *testing.T) {
	sessionsDir, _ := realCodexEnv(t)
	codexRoot := filepath.Dir(sessionsDir) // ~/.codex

	var checked, msgMismatch, durMismatch, missing int
	err := filepath.WalkDir(codexRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl.zst") {
			return err
		}
		if !strings.HasPrefix(path, filepath.Join(codexRoot, "sessions")) &&
			!strings.HasPrefix(path, filepath.Join(codexRoot, "archived_sessions")) {
			return nil
		}
		rel, rerr := filepath.Rel(codexRoot, path)
		if rerr != nil {
			return rerr
		}
		backup := filepath.Join(realBackupRoot, strings.TrimSuffix(rel, ".zst"))
		if _, berr := os.Stat(backup); berr != nil {
			missing++
			return nil // 压缩后才产生、不在备份内的样本：计数不对账
		}
		zstRes, _, zerr := parseCodexRollout(path, codexThread{}, nil)
		if zerr != nil {
			t.Errorf("解析 %s 失败: %v", path, zerr)
			return nil
		}
		backupRes, _, berr2 := parseCodexRollout(backup, codexThread{}, nil)
		if berr2 != nil {
			t.Errorf("解析备份 %s 失败: %v", backup, berr2)
			return nil
		}
		if len(zstRes.Messages) != len(backupRes.Messages) {
			t.Errorf("%s: 消息数 压缩=%d 备份=%d", rel, len(zstRes.Messages), len(backupRes.Messages))
			msgMismatch++
			return nil
		}
		for i := range zstRes.Messages {
			z, b := zstRes.Messages[i], backupRes.Messages[i]
			if z.ID != b.ID || z.TS != b.TS ||
				z.InputTokens != b.InputTokens || z.FreshInputTokens != b.FreshInputTokens ||
				z.OutputTokens != b.OutputTokens || z.CacheReadTokens != b.CacheReadTokens ||
				z.ReasoningTokens != b.ReasoningTokens || z.TotalTokens != b.TotalTokens {
				t.Errorf("%s: 消息 %s token 字段不一致\n压缩: %+v\n备份: %+v", rel, z.ID, z, b)
				msgMismatch++
				break
			}
			if z.DurationMS != b.DurationMS {
				if durMismatch == 0 {
					t.Errorf("%s: 消息 %s DurationMS 压缩=%d 备份=%d（首个差异）", rel, z.ID, z.DurationMS, b.DurationMS)
				}
				durMismatch++
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Skip("无可对账样本（备份内无对应 .jsonl）")
	}
	t.Logf("对账: 会话 %d 核对一致, 消息不一致 %d, DurationMS 差异 %d, 备份缺失(跳过) %d",
		checked, msgMismatch, durMismatch, missing)
	if msgMismatch != 0 {
		t.Fatalf("存在消息级不一致（压缩过程内容损失或解析漂移）: %d", msgMismatch)
	}
	if durMismatch != 0 {
		t.Fatalf("存在 DurationMS 不一致（时长估算漂移）: %d", durMismatch)
	}
}

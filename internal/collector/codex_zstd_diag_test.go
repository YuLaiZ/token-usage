package collector

// 真实样本的存量坏行锚定：比较磁盘 .zst 与压缩前备份 .jsonl 的坏行构成
//（数量/首行号）。真实历史数据存在压缩前即有的行级异常（token_count 缺前置
// assistant 关联等），此类文件不含坏行的 FullyParsed 判定本就不成立、不写
// 跳过门——与 .jsonl 形态同语义。本用例证明这些坏行是压缩前已存在的存量
// 数据异常而非压缩适配引入：要求「仅压缩侧坏」与「构成不同」恒为零。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRealDiag_BadLinesParity(t *testing.T) {
	if os.Getenv("CODEX_ZST_REAL_ACCEPTANCE") != "1" {
		t.Skip("diagnostic only")
	}
	sessionsDir, _ := realCodexEnv(t)
	codexRoot := filepath.Dir(sessionsDir)

	var sameBad, diffBad, zstBadOnly int
	var examples []string
	err := filepath.WalkDir(codexRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl.zst") {
			return err
		}
		if !strings.HasPrefix(path, filepath.Join(codexRoot, "sessions")) &&
			!strings.HasPrefix(path, filepath.Join(codexRoot, "archived_sessions")) {
			return nil
		}
		rel, _ := filepath.Rel(codexRoot, path)
		backup := filepath.Join(realBackupRoot, strings.TrimSuffix(rel, ".zst"))
		if _, berr := os.Stat(backup); berr != nil {
			return nil
		}
		_, zs, zerr := parseCodexRollout(path, codexThread{}, nil)
		if zerr != nil {
			t.Errorf("zst %s: %v", rel, zerr)
			return nil
		}
		_, bs, berr2 := parseCodexRollout(backup, codexThread{}, nil)
		if berr2 != nil {
			t.Errorf("backup %s: %v", rel, berr2)
			return nil
		}
		switch {
		case zs.BadLines == 0 && bs.BadLines == 0:
		case zs.BadLines == bs.BadLines && zs.FirstBadLine == bs.FirstBadLine:
			sameBad++
			if len(examples) < 5 {
				examples = append(examples, rel)
			}
		case zs.BadLines > 0 && bs.BadLines == 0:
			zstBadOnly++
			examples = append(examples, "ZST-ONLY "+rel)
		default:
			diffBad++
			examples = append(examples, "DIFF "+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("坏行构成: 双侧同坏 %d, 仅压缩侧坏 %d, 构成不同 %d", sameBad, zstBadOnly, diffBad)
	for _, e := range examples {
		t.Logf("  样例: %s", e)
	}
	if zstBadOnly > 0 || diffBad > 0 {
		t.Fatalf("存在压缩侧特有坏行（适配引入缺陷嫌疑）: zstOnly=%d diff=%d", zstBadOnly, diffBad)
	}
}

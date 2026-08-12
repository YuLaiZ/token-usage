// cmd/release-body 定制生成 GitHub Release 的 body 文件，替代 gh 的
// --generate-notes。它读取手写的中英版本说明，结合按 tag 自动判定的版本性质
// （候选/稳定）与自动提取的外部贡献者致谢，拼成英文在前、中文在后、含资产与
// 校验指引的完整 body，写入 -out 指定文件，供后续 `gh release create --notes-file` 使用。
//
// 用法：
//
//	go run ./cmd/release-body -tag <tag> -notes <path> -repo <owner/repo> -prev-tag <prev> -out <path>
//
// 其中 -prev-tag 为前一个 release tag，首发版本可留空（取仓库首个 commit 日期作为
// 致谢提取的时间下界）。任一步失败以退出码 1 终止。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/YuLaiZ/token-usage/internal/releasenotes"
)

func main() {
	tag := flag.String("tag", "", "发布的版本 tag（必填，如 v0.1.0-rc.1）")
	notes := flag.String("notes", "", "手写版本说明文件路径（必填，含 <!-- en --> / <!-- zh --> 标记）")
	repo := flag.String("repo", "", "owner/repo（必填，对应 GITHUB_REPOSITORY）")
	prevTag := flag.String("prev-tag", "", "前一个 release tag（首发留空）")
	out := flag.String("out", "", "输出 body 文件路径（必填）")
	flag.Parse()

	if err := run(*tag, *notes, *repo, *prevTag, *out); err != nil {
		fmt.Fprintf(os.Stderr, "release-body 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("release-body 已生成: %s\n", *out)
}

// run 执行完整生成流程，任一步失败返回非 nil 错误。它只做组装与编排，
// 纯逻辑由 internal/releasenotes 提供，本函数负责文件读写与外部命令（git/gh）调用。
func run(tag, notes, repo, prevTag, out string) error {
	if tag == "" {
		return errors.New("-tag 必填")
	}
	if notes == "" {
		return errors.New("-notes 必填")
	}
	if repo == "" {
		return errors.New("-repo 必填")
	}
	if out == "" {
		return errors.New("-out 必填")
	}

	// 1. 读取手写版本说明并拆为中英两段。
	raw, err := os.ReadFile(notes)
	if err != nil {
		return fmt.Errorf("读取 notes 文件 %q 失败: %w", notes, err)
	}
	contentEN, contentZH, err := releasenotes.SplitNotes(string(raw))
	if err != nil {
		return fmt.Errorf("拆分 notes 失败: %w", err)
	}

	// 2. 取 tag 指向 commit 的 committer date 作为致谢提取的时间上界。
	tagDate, err := commitDate(tag)
	if err != nil {
		return fmt.Errorf("取 tag %s 的 commit 日期失败: %w", tag, err)
	}

	// 3. 取时间下界：有前序 tag 则用其 commit 日期，否则用仓库首个 commit 日期。
	var prevDate string
	if prevTag != "" {
		prevDate, err = commitDate(prevTag)
		if err != nil {
			return fmt.Errorf("取 prev-tag %s 的 commit 日期失败: %w", prevTag, err)
		}
	} else {
		prevDate, err = firstCommitDate()
		if err != nil {
			return fmt.Errorf("取仓库首个 commit 日期失败: %w", err)
		}
	}

	// 4. 提取外部贡献者（真实调用 gh；owner 命中则排除）。
	owner := ownerFromRepo(repo)
	thanks, err := releasenotes.ExtractCredits(repo, prevDate, tagDate, owner, realGHRunner)
	if err != nil {
		return fmt.Errorf("提取致谢失败: %w", err)
	}

	// 5. 拼接完整 body 并写出。
	body := releasenotes.BuildBody(releasenotes.Options{
		Tag:       tag,
		ContentEN: contentEN,
		ContentZH: contentZH,
		Thanks:    thanks,
	})
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return fmt.Errorf("写入 body 文件 %q 失败: %w", out, err)
	}
	return nil
}

// commitDate 返回 ref（tag 或 commit）指向 commit 的 committer date（ISO 8601）。
// 用 %cI 而非 %ci 以获得可被 search 谓词解析的严格 ISO 8601 格式。
func commitDate(ref string) (string, error) {
	out, err := exec.Command("git", "log", "-1", "--format=%cI", ref).Output()
	if err != nil {
		return "", fmt.Errorf("git log -1 --format=%%cI %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// firstCommitDate 返回仓库最早根提交的 committer date，作为首发版本致谢提取的下界。
// --reverse 使最早提交排在最前，取第一行即仓库首个 commit 的日期。
func firstCommitDate() (string, error) {
	out, err := exec.Command("git", "log", "--max-parents=0", "--reverse", "--format=%cI").Output()
	if err != nil {
		return "", fmt.Errorf("git log --max-parents=0: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", errors.New("仓库无提交记录")
	}
	// 多个根提交时取最早的一个（reverse 后首行）。
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s, nil
}

// ownerFromRepo 从 "owner/repo" 取斜杠前的 owner 部分，用于致谢排除。
// 无斜杠时原样返回（此时不做 owner 排除）。
func ownerFromRepo(repo string) string {
	if i := strings.Index(repo, "/"); i >= 0 {
		return repo[:i]
	}
	return repo
}

// realGHRunner 是 gh 的真实 os/exec 实现，供 ExtractCredits 在生产环境使用。
// 用 CombinedOutput 保留 gh 的 stderr 细节（如 HTTP 状态码），失败时错误信息
// 可完整带出，便于排障。
func realGHRunner(args ...string) ([]byte, error) {
	return exec.Command("gh", args...).CombinedOutput()
}

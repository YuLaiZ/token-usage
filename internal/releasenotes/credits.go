package releasenotes

import (
	"fmt"
	"strconv"
	"strings"
)

// ghRunner 封装一次 gh 命令调用：接受命令行参数，返回 stdout 字节。
// 真实实现由调用方（cmd/release-body）注入 os/exec；测试注入 mock 以避免联网。
type ghRunner func(args ...string) ([]byte, error)

// ExtractCredits 用 GitHub search issues 端点提取一个版本范围内的外部贡献者：
// 已合并 PR（代码贡献）与已关闭 issue（问题反馈）。
//
//   - repo 形如 "owner/repo"，用于把查询限定到本仓库；
//   - prevTagDate / tagDate 为 tag 指向 commit 的 committer date（ISO 8601），
//     作为 PR merged / issue closed 的时间范围下界与上界；
//   - owner 为仓库所有者 login，命中则排除（仅谢外部贡献）；
//   - run 为 gh 调用的注入实现，nil 时报错。
//
// 返回的 ThanksData 中，PRs 与 Issues 各自按 gh 返回顺序排列、不做去重
// （每个 PR/issue 由 number 唯一标识，同一 login 多项分别列出）。
func ExtractCredits(repo, prevTagDate, tagDate, owner string, run ghRunner) (ThanksData, error) {
	if run == nil {
		return ThanksData{}, fmt.Errorf("gh runner 未注入")
	}

	prs, err := searchContributors("pr", repo, prevTagDate, tagDate, run)
	if err != nil {
		return ThanksData{}, err
	}
	issues, err := searchContributors("issue", repo, prevTagDate, tagDate, run)
	if err != nil {
		return ThanksData{}, err
	}

	prs = excludeOwner(prs, owner)
	issues = excludeOwner(issues, owner)
	return ThanksData{PRs: prs, Issues: issues}, nil
}

// searchContributors 调 gh search/issues 查询指定类型的贡献者。
// kind 为 "pr"（已合并 PR）或 "issue"（已关闭 issue），决定查询谓词。
// gh 经 --jq 把每条结果格式化为 "number\tlogin" 一行，便于解析。
//
// 注意：search 端点只接受 GET，而 gh api 在带 -f 参数时会自动切换为 POST，
// 必须显式 -X GET，否则 GitHub 返回 404。
//
// 分页限制：per_page=100 无 --paginate，超 100 条会静默截断；当前单项目规模
// 远低于该上限，可接受。如需扩展可加 --paginate（search 接口总上限 1000）。
func searchContributors(kind, repo, prevDate, tagDate string, run ghRunner) ([]Contributor, error) {
	q, err := buildQuery(kind, repo, prevDate, tagDate)
	if err != nil {
		return nil, err
	}
	args := []string{
		"api", "-X", "GET", "search/issues",
		"-f", "q=" + q,
		"-f", "per_page=100",
		// 已删除用户的 user 为 null，select 过滤掉，避免渲染成 "@null"。
		"--jq", `.items[] | select(.user != null) | "\(.number)\t\(.user.login)"`,
	}
	out, err := run(args...)
	if err != nil {
		return nil, fmt.Errorf("gh 查询 %s 贡献者失败: %w", kind, err)
	}
	return parseContributors(out), nil
}

// buildQuery 构造 search/issues 的 q 字符串。
func buildQuery(kind, repo, prevDate, tagDate string) (string, error) {
	switch kind {
	case "pr":
		return fmt.Sprintf("repo:%s is:pr is:merged merged:%s..%s", repo, prevDate, tagDate), nil
	case "issue":
		return fmt.Sprintf("repo:%s is:issue is:closed closed:%s..%s", repo, prevDate, tagDate), nil
	default:
		return "", fmt.Errorf("未知贡献类型 %q（仅支持 pr/issue）", kind)
	}
}

// parseContributors 解析 gh --jq 输出的 TSV：每行 "number\tlogin"。
// 空行、缺列、number 非数字或 login 为空的行被跳过。
func parseContributors(out []byte) []Contributor {
	var cs []Contributor
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		num, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		login := strings.TrimSpace(parts[1])
		if login == "" {
			continue
		}
		cs = append(cs, Contributor{Login: login, Number: num})
	}
	return cs
}

// excludeOwner 返回 login 不等于 owner 的贡献者；owner 为空时原样返回。
// GitHub login 大小写不敏感，用 EqualFold 比较。
func excludeOwner(cs []Contributor, owner string) []Contributor {
	if owner == "" {
		return cs
	}
	out := make([]Contributor, 0, len(cs))
	for _, c := range cs {
		if strings.EqualFold(c.Login, owner) {
			continue
		}
		out = append(out, c)
	}
	return out
}

package releasenotes

import (
	"errors"
	"strings"
	"testing"
)

var errSentinel = errors.New("sentinel failure")

// mockGHRunner 根据 gh 调用参数中的 q= 字段区分 pr/issue，
// 返回预设的 TSV（每行 number\tlogin）。不触发任何真实网络。
func mockGHRunner(t *testing.T, prOut, issueOut string) ghRunner {
	t.Helper()
	return func(args ...string) ([]byte, error) {
		q := ""
		for i, a := range args {
			if a == "-f" && i+1 < len(args) && strings.HasPrefix(args[i+1], "q=") {
				q = args[i+1]
			}
		}
		if strings.Contains(q, "is:pr") {
			return []byte(prOut), nil
		}
		return []byte(issueOut), nil
	}
}

func TestExtractCredits(t *testing.T) {
	t.Run("PR 与 issue 均提取并排除 owner", func(t *testing.T) {
		// owner 为 ownerlogin，应被排除；alice/bob/carol 保留。
		prOut := "12\talice\n13\tbob\n14\townerlogin\n"
		issueOut := "8\tcarol\n9\townerlogin\n10\talice\n"
		runner := mockGHRunner(t, prOut, issueOut)

		td, err := ExtractCredits("ownerlogin/repo", "2026-01-01", "2026-08-12", "ownerlogin", runner)
		if err != nil {
			t.Fatalf("非期望错误: %v", err)
		}

		wantPRs := []Contributor{{"alice", 12}, {"bob", 13}}
		wantIssues := []Contributor{{"carol", 8}, {"alice", 10}}
		if !equalContribs(td.PRs, wantPRs) {
			t.Errorf("PRs 不符: got %+v want %+v", td.PRs, wantPRs)
		}
		if !equalContribs(td.Issues, wantIssues) {
			t.Errorf("Issues 不符: got %+v want %+v", td.Issues, wantIssues)
		}
		// owner 必须被排除。
		for _, c := range td.PRs {
			if c.Login == "ownerlogin" {
				t.Errorf("PRs 不应包含 owner: %+v", c)
			}
		}
		for _, c := range td.Issues {
			if c.Login == "ownerlogin" {
				t.Errorf("Issues 不应包含 owner: %+v", c)
			}
		}
	})

	t.Run("空结果返回空 ThanksData", func(t *testing.T) {
		runner := mockGHRunner(t, "", "")
		td, err := ExtractCredits("ownerlogin/repo", "2026-01-01", "2026-08-12", "ownerlogin", runner)
		if err != nil {
			t.Fatalf("非期望错误: %v", err)
		}
		if len(td.PRs) != 0 || len(td.Issues) != 0 {
			t.Errorf("空结果应为空 ThanksData: got %+v", td)
		}
	})

	t.Run("PR 与 issue 的查询分别用 merged/closed", func(t *testing.T) {
		var seen []string
		runner := func(args ...string) ([]byte, error) {
			for i, a := range args {
				if a == "-f" && i+1 < len(args) && strings.HasPrefix(args[i+1], "q=") {
					seen = append(seen, args[i+1])
				}
			}
			return []byte(""), nil
		}
		if _, err := ExtractCredits("acme/r", "2026-01-01T00:00:00Z", "2026-08-12T00:00:00Z", "acme", runner); err != nil {
			t.Fatalf("非期望错误: %v", err)
		}
		if len(seen) != 2 {
			t.Fatalf("应发起 2 次 gh 查询, got %d: %v", len(seen), seen)
		}
		if !strings.Contains(seen[0], "is:pr") || !strings.Contains(seen[0], "is:merged") || !strings.Contains(seen[0], "merged:") {
			t.Errorf("PR 查询应含 is:pr is:merged merged: , got %q", seen[0])
		}
		if !strings.Contains(seen[1], "is:issue") || !strings.Contains(seen[1], "is:closed") || !strings.Contains(seen[1], "closed:") {
			t.Errorf("issue 查询应含 is:issue is:closed closed: , got %q", seen[1])
		}
		// 查询应限定到 repo，且日期值实际拼入谓词。
		for _, q := range seen {
			if !strings.Contains(q, "repo:acme/r") {
				t.Errorf("查询应限定 repo: %q", q)
			}
			if !strings.Contains(q, "2026-01-01T00:00:00Z..2026-08-12T00:00:00Z") {
				t.Errorf("查询应包含传入的日期窗口: %q", q)
			}
		}
	})

	t.Run("runner 报错时向上传递", func(t *testing.T) {
		runner := func(args ...string) ([]byte, error) {
			return nil, errSentinel
		}
		if _, err := ExtractCredits("acme/r", "d1", "d2", "acme", runner); err == nil {
			t.Fatalf("期望报错")
		}
	})

	t.Run("gh 调用显式 GET 方法", func(t *testing.T) {
		// search 端点只接受 GET；gh api 带 -f 时会自动切 POST（返回 404），
		// 必须显式 -X GET。此用例回归保护该参数不被再次删掉。
		var allArgs [][]string
		runner := func(args ...string) ([]byte, error) {
			allArgs = append(allArgs, args)
			return []byte(""), nil
		}
		if _, err := ExtractCredits("acme/r", "d1", "d2", "acme", runner); err != nil {
			t.Fatalf("非期望错误: %v", err)
		}
		if len(allArgs) != 2 {
			t.Fatalf("应发起 2 次 gh 查询, got %d", len(allArgs))
		}
		for _, args := range allArgs {
			if len(args) < 3 || args[0] != "api" {
				t.Errorf("gh 调用应以 api 开头, got %v", args)
			}
			var hasX, hasGET bool
			var jqArg string
			for i, a := range args {
				if a == "-X" {
					hasX = true
				}
				if a == "GET" {
					hasGET = true
				}
				if a == "--jq" && i+1 < len(args) {
					jqArg = args[i+1]
				}
			}
			if !hasX || !hasGET {
				t.Errorf("gh 调用应含 -X GET（search 端点仅接受 GET）, got %v", args)
			}
			// jq 必须过滤 user 为 null 的已删除用户（回归 select 过滤）。
			if !strings.Contains(jqArg, "select(.user != null)") {
				t.Errorf("jq 应过滤 null user, got %q", jqArg)
			}
		}
	})

	t.Run("runner 未注入时报错", func(t *testing.T) {
		if _, err := ExtractCredits("acme/r", "d1", "d2", "acme", nil); err == nil {
			t.Fatalf("未注入 runner 应报错")
		}
	})

	t.Run("owner 为空时不做排除", func(t *testing.T) {
		// owner="" 时所有 login 保留（边界场景）。
		prOut := "1\tanyone\n"
		runner := mockGHRunner(t, prOut, "")
		td, err := ExtractCredits("acme/r", "d1", "d2", "", runner)
		if err != nil {
			t.Fatalf("非期望错误: %v", err)
		}
		if len(td.PRs) != 1 || td.PRs[0].Login != "anyone" {
			t.Errorf("owner 为空时应保留全部, got %+v", td.PRs)
		}
	})

	t.Run("owner 排除大小写不敏感", func(t *testing.T) {
		// GitHub login 大小写不敏感，OwnerLogin 也应被排除（回归 EqualFold）。
		prOut := "1\tOwnerLogin\n2\tbob\n"
		runner := mockGHRunner(t, prOut, "")
		td, err := ExtractCredits("ownerlogin/repo", "d1", "d2", "ownerlogin", runner)
		if err != nil {
			t.Fatalf("非期望错误: %v", err)
		}
		if len(td.PRs) != 1 || td.PRs[0].Login != "bob" {
			t.Errorf("owner 排除应大小写不敏感, got %+v", td.PRs)
		}
	})
}

func equalContribs(a, b []Contributor) bool {
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

// TestParseContributors 覆盖 TSV 解析的容错分支：空行、CRLF、缺列、
// 非数字 number、空 login。断言有区分度：错误解析必须让期望的贡献者集合失配。
func TestParseContributors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Contributor
	}{
		{name: "正常行", in: "12\talice\n13\tbob\n", want: []Contributor{{"alice", 12}, {"bob", 13}}},
		{name: "空行与CRLF行尾", in: "12\talice\r\n\r\n13\tbob\r\n", want: []Contributor{{"alice", 12}, {"bob", 13}}},
		{name: "缺列行跳过", in: "12\talice\nbroken\n13\tbob\n", want: []Contributor{{"alice", 12}, {"bob", 13}}},
		{name: "非数字number跳过", in: "abc\talice\n12\tbob\n", want: []Contributor{{"bob", 12}}},
		{name: "空login跳过", in: "12\t\n13\tbob\n", want: []Contributor{{"bob", 13}}},
		{name: "空输入", in: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseContributors([]byte(tt.in))
			if !equalContribs(got, tt.want) {
				t.Errorf("parseContributors(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

package querier

import (
	"strings"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// longProjectFixture 是 ZCode 临时工作区目录形态（13 位毫秒时间戳 + UUID）：
// project 是目录末段原值，长度 50 列，必须被 Project 列上限截断。
const longProjectFixture = "1793859963949-ff02f9b3-74c7-44f5-8e52-6901002ae048"

// TestRenderSessions_ProjectColumnLimited 锁定会话明细的 Project 列限宽：
// 超长自由文本截断加省略号，避免单个临时目录名把整张表拉宽；短项目名
// 原样保留，不因限宽机制被误截。
func TestRenderSessions_ProjectColumnLimited(t *testing.T) {
	q := &Querier{}
	out := q.RenderSessions([]SessionRow{
		{Client: model.ClientZCode, Project: longProjectFixture, Title: "长项目会话", Agg: GroupAggregate{Requests: 1, TotalTokens: 10}},
		{Client: model.ClientClaudeCode, Project: "app", Title: "短项目会话", Agg: GroupAggregate{Requests: 1, TotalTokens: 5}},
	})

	if strings.Contains(out, longProjectFixture) {
		t.Fatalf("Project 列应截断超长值，输出仍含完整值:\n%s", out)
	}
	if !strings.Contains(out, "1793859963949-ff02f9b3-74c7...") {
		t.Fatalf("Project 列应显示截断前缀与省略号:\n%s", out)
	}
	if !strings.Contains(out, "│ app ") {
		t.Fatalf("短 Project 应原样输出:\n%s", out)
	}
}

// TestRenderDimensionView_ProjectColumnLimited 锁定 query project 视图同一上限：
// 维度表与会话明细表共用 Project 限宽规则，避免两处渲染口径分叉。
func TestRenderDimensionView_ProjectColumnLimited(t *testing.T) {
	q := &Querier{}
	view := DimensionView{
		Dimensions: []string{"project"},
		TitleEn:    "Group by project", TitleZh: "按项目分组",
	}
	out := q.RenderDimensionView(view, []string{"2026-11-05"}, []DimensionRow{
		{Keys: []string{longProjectFixture}, Agg: GroupAggregate{Requests: 1, TotalTokens: 10}},
		{Keys: []string{"app"}, Agg: GroupAggregate{Requests: 1, TotalTokens: 5}},
	}, GroupAggregate{Requests: 2, TotalTokens: 15})

	if strings.Contains(out, longProjectFixture) {
		t.Fatalf("project 维度列应截断超长值，输出仍含完整值:\n%s", out)
	}
	if !strings.Contains(out, "1793859963949-ff02f9b3-74c7...") {
		t.Fatalf("project 维度列应显示截断前缀与省略号:\n%s", out)
	}
	if !strings.Contains(out, "│ app ") {
		t.Fatalf("短 project 键应原样输出:\n%s", out)
	}
}

// TestRenderDimensionView_CombinedProjectColumnLimited 锁定维度列的限宽映射
// 按 view.Dimensions 下标对齐：组合视图（client + project）里 project 位于
// 第 2 维时同样截断，而第 1 维 client 的长值保持完整。若实现误按首维或错位
// 判断，project 会漏截断、client 会被误截，本用例两个方向都会失败。
func TestRenderDimensionView_CombinedProjectColumnLimited(t *testing.T) {
	q := &Querier{}
	longClient := "Open Design / " + strings.Repeat("x", 40)
	view := DimensionView{
		Dimensions: []string{"client", "project"},
		TitleEn:    "Group by client and project",
		TitleZh:    "按客户端与项目分组",
	}
	out := q.RenderDimensionView(view, []string{"2026-11-05"}, []DimensionRow{
		{Keys: []string{longClient, longProjectFixture}, Agg: GroupAggregate{Requests: 1, TotalTokens: 10}},
	}, GroupAggregate{Requests: 1, TotalTokens: 10})

	if !strings.Contains(out, longClient) {
		t.Fatalf("client 列不应限宽，长值应完整输出:\n%s", out)
	}
	if strings.Contains(out, longProjectFixture) {
		t.Fatalf("project 位于第 2 维时也应截断:\n%s", out)
	}
	if !strings.Contains(out, "1793859963949-ff02f9b3-74c7...") {
		t.Fatalf("project 第 2 维应显示截断前缀与省略号:\n%s", out)
	}
}

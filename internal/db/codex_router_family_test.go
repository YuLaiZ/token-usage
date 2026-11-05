package db

import (
	"context"
	"testing"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// === codex 归因家族匹配 + 模型校验合同 ===
//
// 合同：子代理 message 通过 AgentParentID 与父会话共桶；proxy 行 session 恒为
// 主会话，经 CodexFamilyRoot 归一后进入同一家族桶；配对前做模型归一比较
// （ccs-<provider>/ 前缀、[..] 后缀、大小写），异模型行不得消费他模型 message。
// 背景：Codex 允许子代理使用与主会话不同的模型（真实用法），缺模型校验会把
// 子代理 proxy 行配给主会话 message，造成 router_model 归因错误。

func TestNormalizeCodexModelID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ccs-opencode/deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"  GPT-6.1-Terra  ", "gpt-6.1-terra"},
		{"deepseek-v4.1-flash[1M]", "deepseek-v4.1-flash"},
		{"ccs-opencode/deepseek-v4.1-flash[1M]", "deepseek-v4.1-flash"},
		{"", ""},
		{"ccs-opencode/", ""},
	}
	for _, c := range cases {
		if got := NormalizeCodexModelID(c.in); got != c.want {
			t.Errorf("NormalizeCodexModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCodexFamilyRoot(t *testing.T) {
	parents := map[string]string{
		"child":   "root",
		"grand":   "child",
		"cycle-a": "cycle-b",
		"cycle-b": "cycle-a",
	}
	cases := []struct{ in, want string }{
		{"root", "root"},
		{"child", "root"},
		{"grand", "root"},
		{"unknown", "unknown"},
		{"cycle-a", "cycle-a"}, // 32 跳防御：成环时停在起点而非死循环
	}
	for _, c := range cases {
		if got := CodexFamilyRoot(c.in, parents); got != c.want {
			t.Errorf("CodexFamilyRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// attributionMap 把归因列表转 map[MessageID]RouterAttribution（同一 message
// 单次消费，键唯一）。
func attributionMap(infos []model.RouterAttribution) map[string]model.RouterAttribution {
	out := make(map[string]model.RouterAttribution, len(infos))
	for _, info := range infos {
		out[info.MessageID] = info
	}
	return out
}

// TestMatchCodexRouterAttributions_ModelGuard 复现真实错配场景（01a1aab3/
// 01a1aad3）：主会话 gpt-6.1-terra 与子代理 deepseek 异模型并发，子代理请求
// 与主会话某条消息同秒。断言：
//   - 子代理消息拿到 deepseek 归因；
//   - 主会话所有归因都是 terra（不得出现 model=terra 而 router_model=deepseek
//     的自相矛盾）；
//   - 与子代理请求同秒的主会话消息不被 deepseek 行消费。
func TestMatchCodexRouterAttributions_ModelGuard(t *testing.T) {
	const baseSec = int64(1781092800)
	log := func(req, session, modelName string, deltaSec int64) model.RouterLog {
		return model.RouterLog{RequestID: req, RouterName: "cc_switch", SessionID: session,
			AppType: "codex", Model: modelName, ProviderName: "P", DataSource: "proxy",
			CreatedAt: baseSec + deltaSec}
	}
	msg := func(id, session, parent, modelName string, deltaSec int64) model.Message {
		return model.Message{ID: id, SessionID: session, AgentParentID: parent,
			Client: "Codex App", TS: (baseSec + deltaSec) * 1000, Model: modelName}
	}

	logs := []model.RouterLog{
		log("r1", "codex_s1", "gpt-6.1-terra", 0),
		log("r2", "codex_s1", "gpt-6.1-terra", 5),
		log("r3", "codex_s1", "gpt-6.1-terra", 10),
		log("rd", "codex_s1", "deepseek-v4.1-flash", 15), // 子代理请求：记在主会话 id 下
		log("r4", "codex_s1", "gpt-6.1-terra", 20),
		log("r5", "codex_s1", "gpt-6.1-terra", 25),
	}
	messages := []model.Message{
		msg("m1", "s1", "", "ccs-opencode/gpt-6.1-terra", 1),
		msg("m2", "s1", "", "ccs-opencode/gpt-6.1-terra", 6),
		msg("m3", "s1", "", "ccs-opencode/gpt-6.1-terra", 11),
		msg("m4", "s1", "", "ccs-opencode/gpt-6.1-terra", 16), // 与 rd 同窗：无校验时会被抢
		msg("m5", "s1", "", "ccs-opencode/gpt-6.1-terra", 21),
		msg("m6", "s1", "", "ccs-opencode/gpt-6.1-terra", 26),
		msg("cm1", "s2", "s1", "ccs-opencode/deepseek-v4.1-flash", 15),
	}

	got := attributionMap(MatchCodexRouterAttributions(logs, messages, CodexRouterMatchWindowSec))

	child, ok := got["cm1"]
	if !ok {
		t.Fatalf("子代理消息 cm1 应获得归因，实际 %+v", got)
	}
	if child.Model != "deepseek-v4.1-flash" || child.RequestID != "rd" {
		t.Fatalf("cm1 归因 = %+v, want model=deepseek-v4.1-flash request=rd", child)
	}
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		info, ok := got[id]
		if !ok {
			continue // 6 条消息 5 条 terra 行：允许一条未配（token_count 与请求数不一一对应）
		}
		if NormalizeCodexModelID(info.Model) != "gpt-6.1-terra" {
			t.Fatalf("%s（model=terra）被错配为 model=%q request=%s", id, info.Model, info.RequestID)
		}
	}
	if len(got) != 6 {
		t.Fatalf("归因数量 = %d, want 6（5 条主会话 + 1 条子代理）: %+v", len(got), got)
	}
}

// TestMatchCodexRouterAttributions_MultiLevelFamily 覆盖多层子代理（depth 2）：
// 孙代理消息经父链归一到根会话桶，proxy 行（session=根）可匹配。
func TestMatchCodexRouterAttributions_MultiLevelFamily(t *testing.T) {
	const baseSec = int64(1781092800)
	logs := []model.RouterLog{{
		RequestID: "r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "deepseek-v4.1-flash", ProviderName: "P",
		DataSource: "proxy", CreatedAt: baseSec,
	}}
	messages := []model.Message{
		{ID: "gm1", SessionID: "grand", AgentParentID: "child", Client: "Codex App",
			TS: (baseSec + 1) * 1000, Model: "ccs-opencode/deepseek-v4.1-flash"},
	}
	// parents 需包含 child→root 关系才能把 grand 归到 root：由 child 的其它
	// message 行提供（真实查询返回全家族，含中间层会话）。
	messages = append(messages, model.Message{
		ID: "cm0", SessionID: "child", AgentParentID: "root", Client: "Codex App",
		TS: (baseSec + 400) * 1000, Model: "ccs-opencode/deepseek-v4.1-flash",
	})
	got := attributionMap(MatchCodexRouterAttributions(logs, messages, CodexRouterMatchWindowSec))
	if info, ok := got["gm1"]; !ok || info.RequestID != "r1" {
		t.Fatalf("孙代理消息应经父链归因到根会话的 proxy 行，got %+v", got)
	}
}

// TestQueryCodexSessionParents 覆盖家族关系查询：成员自身 ∪ 父会话在集合内的
// 直接子代；agent_parent_id 为空/自环的行不返回。
func TestQueryCodexSessionParents(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	rows := []model.Message{
		{ID: "p1#1", SessionID: "root", Client: model.ClientCodexCLI, Date: "2026-11-05", TS: 1000, Model: "m"},
		{ID: "c1#1", SessionID: "child-a", AgentParentID: "root", Client: model.ClientCodexCLI, Date: "2026-11-05", TS: 1000, Model: "m"},
		{ID: "c2#1", SessionID: "child-b", AgentParentID: "root", Client: model.ClientCodexCLI, Date: "2026-11-05", TS: 1000, Model: "m"},
		{ID: "g1#1", SessionID: "grand", AgentParentID: "child-a", Client: model.ClientCodexCLI, Date: "2026-11-05", TS: 1000, Model: "m"},
		{ID: "x1#1", SessionID: "self-loop", AgentParentID: "self-loop", Client: model.ClientCodexCLI, Date: "2026-11-05", TS: 1000, Model: "m"},
	}
	if _, err := UpsertMessages(context.Background(), d, rows); err != nil {
		t.Fatal(err)
	}
	got, err := QueryCodexSessionParents(context.Background(), d, []string{"root"})
	if err != nil {
		t.Fatalf("QueryCodexSessionParents failed: %v", err)
	}
	want := map[string]string{"child-a": "root", "child-b": "root"}
	if len(got) != len(want) {
		t.Fatalf("parents = %+v, want %+v", got, want)
	}
	for sid, parent := range want {
		if got[sid] != parent {
			t.Fatalf("parents[%q] = %q, want %q（全量 %+v）", sid, got[sid], parent, got)
		}
	}
	// 反向：只给子代理 id，也返回其父关系（入口只触达子代理的场景）。
	got, err = QueryCodexSessionParents(context.Background(), d, []string{"grand"})
	if err != nil {
		t.Fatal(err)
	}
	if got["grand"] != "child-a" {
		t.Fatalf("parents[grand] = %q, want child-a（全量 %+v）", got["grand"], got)
	}
}

// TestQueryCodexSessionParents_SessionsPreferred：家族关系的权威来源是
// sessions 表——中间层代理没有可入库消息（messages 无行）时，其父关系仍可被
// 查到，多层父链不断裂。messages 只在旧库/异常路径兜底。
func TestQueryCodexSessionParents_SessionsPreferred(t *testing.T) {
	d := openFreshDB(t)
	defer d.Close()
	// sessions 侧：mid ← root、grand ← mid；mid 无任何消息行。
	if _, err := UpsertSessionMeta(context.Background(), d, []model.Session{
		{ID: "mid", Client: model.ClientCodexCLI, AgentParentID: "root", FirstTS: 1, LastTS: 2},
		{ID: "grand", Client: model.ClientCodexCLI, AgentParentID: "mid", FirstTS: 1, LastTS: 2},
	}); err != nil {
		t.Fatal(err)
	}
	// messages 侧只有孙代理的一张消息（不携带中间层关系）。
	if _, err := UpsertMessages(context.Background(), d, []model.Message{{
		ID: "gm1#1", SessionID: "grand", Client: model.ClientCodexCLI,
		Date: "2026-11-05", TS: 1000, Model: "ccs-opencode/deepseek-v4.1-flash",
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := QueryCodexSessionParents(context.Background(), d, []string{"grand", "mid"})
	if err != nil {
		t.Fatalf("QueryCodexSessionParents failed: %v", err)
	}
	if got["mid"] != "root" {
		t.Fatalf("parents[mid] = %q, want root（sessions 权威来源，全量 %+v）", got["mid"], got)
	}
	if got["grand"] != "mid" {
		t.Fatalf("parents[grand] = %q, want mid（全量 %+v）", got["grand"], got)
	}
}

// TestMatchCodexRouterAttributions_CrossProviderSameModel：同模型不同供应商——
// 双方都带聚合供应商前缀且段不一致时不得配对。反证设计：错误供应商的消息在
// 时间上**更近**，若没有前缀校验，最近邻会选中它（测试必红）。
func TestMatchCodexRouterAttributions_CrossProviderSameModel(t *testing.T) {
	const baseSec = int64(1781092800)
	logWithRequest := func(reqID string, createdAt int64, requestModel string) model.RouterLog {
		return model.RouterLog{RequestID: reqID, RouterName: "cc_switch", SessionID: "codex_root",
			AppType: "codex", Model: "deepseek-v4.1-flash", RequestModel: requestModel,
			ProviderName: "P", DataSource: "proxy", CreatedAt: createdAt}
	}
	// 反证：只有供应商 A 的日志行；供应商 B 的消息更近（Δ2s vs Δ50s）。
	logsA := []model.RouterLog{logWithRequest("rA", baseSec, "ccs-provider-a/deepseek-v4.1-flash")}
	messages := []model.Message{
		{ID: "mB-near", SessionID: "root", Client: "Codex App",
			TS: (baseSec + 2) * 1000, Model: "ccs-provider-b/deepseek-v4.1-flash"},
		{ID: "mA-far", SessionID: "root", Client: "Codex App",
			TS: (baseSec + 50) * 1000, Model: "ccs-provider-a/deepseek-v4.1-flash"},
	}
	got := attributionMap(MatchCodexRouterAttributions(logsA, messages, CodexRouterMatchWindowSec))
	if got["mA-far"].RequestID != "rA" {
		t.Fatalf("供应商 A 的日志必须配给供应商 A 的消息（mB-near 更近但前缀不同须拒绝），got %+v", got)
	}
	if _, ok := got["mB-near"]; ok {
		t.Fatalf("供应商 B 的消息不得被供应商 A 的日志归因，got %+v", got)
	}

	// 各归各家：A/B 两条日志与两条消息。
	logsAB := []model.RouterLog{
		logWithRequest("rA", baseSec, "ccs-provider-a/deepseek-v4.1-flash"),
		logWithRequest("rB", baseSec+100, "ccs-provider-b/deepseek-v4.1-flash"),
	}
	eachOwn := []model.Message{
		{ID: "mA", SessionID: "root", Client: "Codex App",
			TS: (baseSec + 2) * 1000, Model: "ccs-provider-a/deepseek-v4.1-flash"},
		{ID: "mB", SessionID: "root", Client: "Codex App",
			TS: (baseSec + 100) * 1000, Model: "ccs-provider-b/deepseek-v4.1-flash"},
	}
	gotEach := attributionMap(MatchCodexRouterAttributions(logsAB, eachOwn, CodexRouterMatchWindowSec))
	if gotEach["mA"].RequestID != "rA" || gotEach["mB"].RequestID != "rB" {
		t.Fatalf("A/B 应各归各家，got %+v", gotEach)
	}

	// 降级边界：旧数据无 request_model 时仅按模型名匹配（无法区分供应商）。
	legacy := []model.RouterLog{logWithRequest("rLegacy", baseSec, "")}
	if len(MatchCodexRouterAttributions(legacy, messages, CodexRouterMatchWindowSec)) == 0 {
		t.Fatal("无 request_model 的旧数据应按模型名降级匹配（保守放行）")
	}
}

// TestMatchCodexRouterAttributions_MissingIntermediateSession：多层家族中
// 中间层代理没有消息时，父链依赖传入的 parents（sessions 表来源），孙代理仍
// 能归因到根会话的 proxy 行。
func TestMatchCodexRouterAttributions_MissingIntermediateSession(t *testing.T) {
	const baseSec = int64(1781092800)
	parents := map[string]string{"mid": "root", "grand": "mid"}
	logs := []model.RouterLog{{
		RequestID: "r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "deepseek-v4.1-flash", ProviderName: "OpenCode Go",
		DataSource: "proxy", CreatedAt: baseSec,
	}}
	messages := []model.Message{{
		ID: "gm1", SessionID: "grand", AgentParentID: "mid", Client: "Codex App",
		TS: (baseSec + 1) * 1000, Model: "ccs-opencode/deepseek-v4.1-flash",
	}}
	got := attributionMap(MatchCodexRouterAttributionsWithParents(logs, messages, parents, CodexRouterMatchWindowSec))
	if got["gm1"].RequestID != "r1" {
		t.Fatalf("孙代理应经完整父链归因到根会话 proxy 行，got %+v", got)
	}
	// 反证：父链缺 mid→root 这一跳（仅靠 messages 构建）时无法归因——说明
	// sessions 权威来源的必要性。
	gotWithout := attributionMap(MatchCodexRouterAttributions(logs, messages, CodexRouterMatchWindowSec))
	if _, ok := gotWithout["gm1"]; ok {
		t.Fatalf("缺少中间层关系时不应能归因（反证），got %+v", gotWithout)
	}
}

// TestMatchCodexRouterAttributions_RequestModelMapping：cc-switch 把请求模型映射
// 为不同上游模型时（request gpt-5.6 → upstream gpt-5.6-terra），必须按请求模型
// 比较——先比上游模型会错误拒绝合法归因。旧数据无 request_model 时仍按上游
// 模型比较（映射场景无法识别，属历史降级行为）。
func TestMatchCodexRouterAttributions_RequestModelMapping(t *testing.T) {
	const baseSec = int64(1781092800)
	logs := []model.RouterLog{{
		RequestID: "r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "gpt-5.6-terra", RequestModel: "gpt-5.6",
		ProviderName: "P", DataSource: "proxy", CreatedAt: baseSec,
	}}
	messages := []model.Message{{
		ID: "m1", SessionID: "root", Client: "Codex App",
		TS: (baseSec + 1) * 1000, Model: "gpt-5.6",
	}}
	got := attributionMap(MatchCodexRouterAttributions(logs, messages, CodexRouterMatchWindowSec))
	if got["m1"].RequestID != "r1" {
		t.Fatalf("请求模型映射场景应匹配（message.model=request_model=gpt-5.6，upstream=gpt-5.6-terra），got %+v", got)
	}

	// 反证一：请求模型不一致 → 拒绝（message.model 是 terra，但 proxy 请求是 5.6）。
	mismatch := []model.Message{{
		ID: "m2", SessionID: "root", Client: "Codex App",
		TS: (baseSec + 2) * 1000, Model: "gpt-5.6-terra",
	}}
	if got := attributionMap(MatchCodexRouterAttributions(logs, mismatch, CodexRouterMatchWindowSec)); len(got) != 0 {
		t.Fatalf("请求模型不一致时不应匹配，got %+v", got)
	}

	// 反证二：旧数据无 request_model 时按上游模型比较——message=gpt-5.6 与
	// 上游 gpt-5.6-terra 不同 → 拒绝（映射场景在旧数据上无法识别）。
	legacyLogs := []model.RouterLog{{
		RequestID: "r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "gpt-5.6-terra", ProviderName: "P",
		DataSource: "proxy", CreatedAt: baseSec,
	}}
	if got := attributionMap(MatchCodexRouterAttributions(legacyLogs, messages, CodexRouterMatchWindowSec)); len(got) != 0 {
		t.Fatalf("无 request_model 的旧数据按上游模型比较应拒绝，got %+v", got)
	}
	// 旧数据且上游与消息一致 → 匹配。
	legacySame := []model.RouterLog{{
		RequestID: "r1", RouterName: "cc_switch", SessionID: "codex_root",
		AppType: "codex", Model: "gpt-5.6", ProviderName: "P",
		DataSource: "proxy", CreatedAt: baseSec,
	}}
	if got := attributionMap(MatchCodexRouterAttributions(legacySame, messages, CodexRouterMatchWindowSec)); len(got) != 1 {
		t.Fatalf("旧数据且上游一致时应匹配，got %+v", got)
	}
}

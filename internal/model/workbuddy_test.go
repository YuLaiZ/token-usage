package model

import "testing"

// 编码可逆：非空合法 expert_id 经 WorkBuddyExpertClientKey 编码后可由
// ParseWorkBuddyExpertClientKey 还原原值；大小写敏感、Unicode 安全。
func TestWorkBuddyExpertClientKeyRoundTrip(t *testing.T) {
	ids := []string{
		"A",
		"a", // 仅大小写不同：不同身份，不得归并
		"MeituanLivingAssistant",
		"美团专家",
		"expert with spaces",
		"emoji-🤖",
	}
	for _, id := range ids {
		key := WorkBuddyExpertClientKey(id)
		if key == "" {
			t.Fatalf("WorkBuddyExpertClientKey(%q) 不应为空", id)
		}
		got, ok := ParseWorkBuddyExpertClientKey(key)
		if !ok || got != id {
			t.Errorf("round-trip 失败: %q -> %q -> (%q, %v)", id, key, got, ok)
		}
	}
	if WorkBuddyExpertClientKey("A") == WorkBuddyExpertClientKey("a") {
		t.Error("仅大小写不同的 expert id 编码后必须不同")
	}
	// 空值与非有效 UTF-8 不编码
	if WorkBuddyExpertClientKey("") != "" {
		t.Error("空 expert_id 应返回空串")
	}
	if WorkBuddyExpertClientKey(string([]byte{0xff, 0xfe})) != "" {
		t.Error("非有效 UTF-8 应返回空串")
	}
}

// 家族严格校验：固定前缀、非空偶数位小写 hex、解码非空且有效 UTF-8；非法
// 形态（大写 hex、奇数位、空段、前缀变体）都不是家族成员。
func TestWorkBuddyFamilyClientValidation(t *testing.T) {
	cases := []struct {
		client string
		want   bool
	}{
		{ClientWorkBuddy, true},
		{WorkBuddyExpertClientKey("A"), true},  // WorkBuddy Expert:41
		{WorkBuddyExpertClientKey("美团"), true}, // 多字节 UTF-8 hex
		{"WorkBuddy Expert:41", true},          // "A"
		{"WorkBuddy Expert:4", false},          // 奇数位
		{"WorkBuddy Expert:", false},           // 空段
		{"WorkBuddy Expert:41A", false},        // 奇数位混大写
		{"WorkBuddy Expert:4G", false},         // 非法字符
		{"workbuddy expert:41", false},         // 前缀大小写不同
		{"WorkBuddy ExpertX:41", false},        // 前缀变体
		{"WorkBuddyExpert:41", false},          // 缺分隔空格
		{"WorkBuddy Expert:00", true},          // 解码为单个 NUL 字节：非空且合法 UTF-8，按不透明身份处理
		{"", false},
		{"WorkBuddy Experts", false},
	}
	for _, tc := range cases {
		if got := IsWorkBuddyFamilyClient(tc.client); got != tc.want {
			t.Errorf("IsWorkBuddyFamilyClient(%q) = %v, want %v", tc.client, got, tc.want)
		}
	}
}

// 显示名：合法 expert 键解码为 WorkBuddy [<原值>]，保留大小写；普通 client
// 与非家族键原样返回；legacy mimocode 兜底语义不变。
func TestClientDisplayNameWorkBuddyExpert(t *testing.T) {
	if got := ClientDisplayName(WorkBuddyExpertClientKey("A")); got != "WorkBuddy [A]" {
		t.Errorf("display = %q, want WorkBuddy [A]", got)
	}
	if got := ClientDisplayName(WorkBuddyExpertClientKey("a")); got != "WorkBuddy [a]" {
		t.Errorf("display = %q, want WorkBuddy [a]（大小写保留）", got)
	}
	if got := ClientDisplayName(WorkBuddyExpertClientKey("MeituanLivingAssistant")); got != "WorkBuddy [MeituanLivingAssistant]" {
		t.Errorf("display = %q", got)
	}
	if got := ClientDisplayName(ClientWorkBuddy); got != "WorkBuddy" {
		t.Errorf("普通 client 显示名 = %q", got)
	}
	if got := ClientDisplayName("SomeOtherClient"); got != "SomeOtherClient" {
		t.Errorf("非家族键应原样返回, got %q", got)
	}
	if got := ClientDisplayName(LegacyClientXiaomiMiMoCode); got != ClientMiMoCode {
		t.Errorf("legacy 兜底语义不应回归, got %q", got)
	}
}

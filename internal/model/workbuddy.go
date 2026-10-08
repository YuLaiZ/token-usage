// internal/model/workbuddy.go
package model

import (
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// WorkBuddyExpertClientPrefix 是 WorkBuddy expert 会话存储 client 身份键的固定
// 前缀。身份键形如 "WorkBuddy Expert:<hex>"，<hex> 是 expert_id 原始 UTF-8 字节
// 的小写十六进制编码；编码可逆且无分隔符歧义。可读显示名
// "WorkBuddy [<expert_id>]" 只由 ClientDisplayName 在渲染侧派生，不落库。
const WorkBuddyExpertClientPrefix = "WorkBuddy Expert:"

// WorkBuddyExpertClientKey 把非空 expert_id 编码为存储身份键。
// expert_id 按不透明、大小写敏感的身份处理（不 trim、不折叠）；非有效 UTF-8
// 或空值返回空串，调用方须将该会话按元数据无效暂缓并报告错误。
func WorkBuddyExpertClientKey(expertID string) string {
	if expertID == "" || !utf8.ValidString(expertID) {
		return ""
	}
	return WorkBuddyExpertClientPrefix + hex.EncodeToString([]byte(expertID))
}

// ParseWorkBuddyExpertClientKey 严格校验并解码 expert 身份键：固定前缀 + 非空
// 偶数位小写十六进制 + 解码后非空且为有效 UTF-8。合法时返回 expert_id 原值；
// 不是合法 expert 键（含普通 "WorkBuddy"、前缀大小写变体、大写 hex、奇数位、
// 空段、非法 UTF-8）返回空串与 false。家族选择不得用宽泛前缀匹配替代本校验。
func ParseWorkBuddyExpertClientKey(client string) (string, bool) {
	if !strings.HasPrefix(client, WorkBuddyExpertClientPrefix) {
		return "", false
	}
	rest := client[len(WorkBuddyExpertClientPrefix):]
	if rest == "" || len(rest)%2 != 0 {
		return "", false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	raw, err := hex.DecodeString(rest)
	if err != nil || len(raw) == 0 || !utf8.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

// IsWorkBuddyFamilyClient 报告 client 是否 WorkBuddy 家族成员：精确的 "WorkBuddy"
// 或严格合法的 expert 编码。非法编码（大写 hex、奇数位、非前缀形态等）不是家族
// 成员，按异常数据处理并报告，不得静默纳入家族迁移或删除。
func IsWorkBuddyFamilyClient(client string) bool {
	if client == ClientWorkBuddy {
		return true
	}
	_, ok := ParseWorkBuddyExpertClientKey(client)
	return ok
}

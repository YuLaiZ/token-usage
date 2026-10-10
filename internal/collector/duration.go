package collector

// 会话日志请求时长估算的共享过滤门。
//
// 估算时长含等待首 token 的全程：output 越少，首字等待与传输突发占比越大，
// 算出的「速度」噪音大于信号；时长过短同理。两阈值均含边界（>=），
// 不满足任一条件的估算落 0（未记录）。

const (
	// minDurationOutputTokens 是计入时长的最低输出 token 数。
	minDurationOutputTokens = 200
	// minDurationEstimateMS 是计入时长的最低估算毫秒数。
	minDurationEstimateMS = 1000
	// maxDurationEstimateMS 是计入时长的上限（1 小时）：估算含等待首 token
	// 的全程，长于该值的「时长」多半夹了用户离开后的长间隔或跨段 resume，
	// 不再代表单次请求。
	maxDurationEstimateMS = 60 * 60 * 1000
)

// estimateDurationMS 应用时长过滤门：output 与时长双阈值内返回原时长，
// 否则 0。负时长（数据乱序）与非正值一律 0。
func estimateDurationMS(outputTokens int64, durationMS int64) int64 {
	if outputTokens >= minDurationOutputTokens &&
		durationMS >= minDurationEstimateMS && durationMS <= maxDurationEstimateMS {
		return durationMS
	}
	return 0
}

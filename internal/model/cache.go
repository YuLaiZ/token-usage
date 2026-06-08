// internal/model/cache.go
package model

// SubtractCache 从 input 中扣除 cacheRead 与 cacheCreate，得到真正的 fresh input；
// 结果非负（小于 0 时 clamp 到 0）。
func SubtractCache(input, cacheRead, cacheCreate int64) int64 {
	fresh := input - cacheRead - cacheCreate
	if fresh < 0 {
		return 0
	}
	return fresh
}

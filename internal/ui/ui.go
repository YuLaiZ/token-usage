// Package ui 提供用户可见输出的统一双语格式。
package ui

import "fmt"

// Bi 返回「English / 中文」形式的双语文本：英文在前，空格-斜杠-空格分隔，
// 与命令 Short 字段的既有先例一致。双语恒显是明确产品形态，不做语言
// 检测与切换；全仓分隔符经此函数保持一致。
func Bi(en, zh string) string {
	return fmt.Sprintf("%s / %s", en, zh)
}

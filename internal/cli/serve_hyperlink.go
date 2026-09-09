package cli

// serve_hyperlink.go 为 serve 命令的启动行提供终端超链接渲染：在支持 OSC 8
// 的交互终端（iTerm2、VS Code、Warp 等）里把仪表板 URL 渲染为可点击链接，
// 其余场景原样输出纯文本。

// hyperlinkURL 把 URL 包进 OSC 8 超链接转义序列（BEL 终止符形态）:
// "\x1b]8;;URL\x07TEXT\x1b]8;;\x07"，可见文本仍是 URL 本身；tty=false 或
// url 为空时原样返回。不支持 OSC 8 的终端会忽略未知转义序列，可见文本
// 不受影响，属安全降级。
func hyperlinkURL(url string, tty bool) string {
	if !tty || url == "" {
		return url
	}
	return "\x1b]8;;" + url + "\x07" + url + "\x1b]8;;\x07"
}

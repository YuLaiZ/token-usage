package web

import (
	"embed"
	"io/fs"
)

// 静态仪表板资产经 go:embed 编入二进制:单文件交付、零外部资源
// (无 CDN/字体/图片),目录内容由 index.html/app.css/app.js 组成。
//
//go:embed all:static
var staticRoot embed.FS

// assetFS 是 static 目录的子文件系统,供 /assets/ 文件服务直接挂载。
var assetFS = func() fs.FS {
	sub, err := fs.Sub(staticRoot, "static")
	if err != nil {
		// embed 根是编译期常量,失败仅可能是构建产物被破坏,直接 panic 早暴露。
		panic(err)
	}
	return sub
}()

// indexHTML 在包初始化时读入内嵌首页,请求路径零 IO。
var indexHTML = func() []byte {
	data, err := staticRoot.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	return data
}()

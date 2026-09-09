// cmd/devdashboard 本地调试专用的仪表板服务：绕开 `serve` 的单实例守卫与
// serve.json 生命周期，把仪表板直接架在指定的 SQLite 库上，供开发期在
// 不干扰正式实例（默认 127.0.0.1:8619）的前提下随时起停。
//
// 与 `serve` 的差异（均为有意为之，勿用于生产）：
//   - 无单实例守卫：可与其他实例（含正式 8619）并行运行，互不探活；
//   - 无 serve.json/serve.log 生命周期状态：起停全凭本进程，Ctrl+C 即退出；
//   - 数据面同样只读：不写数据库与配置。
//
// 用法：go run ./cmd/devdashboard --db /path/to/usage.db --addr 127.0.0.1:8630
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/YuLaiZ/token-usage/internal/buildinfo"
	"github.com/YuLaiZ/token-usage/internal/db"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/web"
)

// defaultDevAddr 刻意与正式 serve 的 8619 错开，避免开发实例与正式实例抢端口。
const defaultDevAddr = "127.0.0.1:8630"

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析用户主目录失败:", err)
		os.Exit(1)
	}
	defaultDB := filepath.Join(home, ".token-usage", "usage.db")

	addr := flag.String("addr", defaultDevAddr, "监听地址（开发实例，默认与正式 8619 错开）")
	dbPath := flag.String("db", defaultDB, "SQLite 库路径（默认 ~/.token-usage/usage.db；可指向测试库/副本库）")
	flag.Parse()

	usageDB, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devdashboard 打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer usageDB.Close()

	version := buildinfo.Current().Short()
	mux := http.NewServeMux()
	mux.Handle("/", web.NewServer(querier.New(usageDB), version))

	fmt.Printf("devdashboard 已启动 %s（db %s，版本 %s；Ctrl+C 退出）\n", *addr, *dbPath, version)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "devdashboard 退出: %v\n", err)
		os.Exit(1)
	}
}

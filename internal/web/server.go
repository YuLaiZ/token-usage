// Package web 提供 serve 命令的本地 HTTP 服务:为仪表板提供 JSON
// (/api/meta、/api/dashboard、/api/config)与静态资产(/ 与 /assets/)。
// 数据接口中 meta/dashboard 只读;config 接口经 ConfigStore 走 configapp
// 的锁内原子写回。本地仅涉及生命周期文件 serve.json/serve.log(由 cli 层
// 维护);无 CORS 头,按同源使用;响应统一禁用缓存(开发期所见即所得)。
package web

import (
	"encoding/json"
	"net/http"

	"github.com/YuLaiZ/token-usage/internal/config"
	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// server 持有 HTTP 服务的依赖:查询器、版本串、每请求配置源与配置读写
// store。configFn 与 configStore 均可缺省(测试/最小装配):无 configFn 时
// provider 别名与自定义视图不生效;无 configStore 时 /api/config 返回 501。
type server struct {
	q           *querier.Querier
	version     string
	configFn    func() *config.Config
	configStore ConfigStore
}

// ServerOption 是 NewServer 的可选参数。
type ServerOption func(*server)

// WithConfigProvider 注入每请求配置源:provider 维度别名与自定义视图
// (query.subqueries)按当前配置即时生效;返回 nil 视为无配置。配置在
// 生产装配方每请求重读磁盘快照(serve_dashboard.go),配置保存与外部
// 编辑都在下一次请求即生效。
func WithConfigProvider(fn func() *config.Config) ServerOption {
	return func(s *server) { s.configFn = fn }
}

// WithConfigStore 注入配置读写 store(/api/config 的 GET/PUT)。
func WithConfigStore(store ConfigStore) ServerOption {
	return func(s *server) { s.configStore = store }
}

// NewServer 构造本地仪表板服务的 HTTP Handler。路由:
//
//	GET /api/meta       服务版本与数据边界
//	GET /api/dashboard  区间汇总 + 四维度行 + 日期×小时热力 + 自定义视图 + Top sessions
//	GET /api/config     配置编辑模型与 revision
//	PUT /api/config     应用配置草稿(revision 冲突 409/校验失败 400)
//	GET /               内嵌仪表板首页
//	GET /assets/        内嵌静态资产
func NewServer(q *querier.Querier, version string, opts ...ServerOption) http.Handler {
	s := &server{q: q, version: version}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /assets/", s.handleAssets)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	return noStore(mux)
}

// currentConfig 返回当前配置与 provider 别名;无配置源时返回 nil(维度
// 聚合核对 nil map 读取安全,provider 维度不做别名合并)。
func (s *server) currentConfig() *config.Config {
	if s.configFn == nil {
		return nil
	}
	return s.configFn()
}

// noStore 中间件:全部响应(含静态资产)携带 Cache-Control: no-store。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// handleIndex 返回内嵌仪表板首页 index.html。
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// handleAssets 返回内嵌静态资产,Content-Type 由扩展名推断;目录请求
// (含 /assets/ 本身)一律 404,不暴露文件清单。
func (s *server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/assets/" {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/assets/", http.FileServer(http.FS(assetFS))).ServeHTTP(w, r)
}

// apiError 是带 HTTP 状态码的双语请求错误:400 参数非法、404 资源不存在。
type apiError struct {
	status int
	en, zh string
}

// writeError 输出统一错误 JSON:{"error":{"message":"English / 中文"}}。
func writeError(w http.ResponseWriter, e apiError) {
	writeJSON(w, e.status, map[string]any{
		"error": map[string]string{"message": ui.Bi(e.en, e.zh)},
	})
}

// writeInternal 输出内部错误(500):双语前缀附查询错误原文,便于排查。
func writeInternal(w http.ResponseWriter, en, zh string, err error) {
	writeError(w, apiError{
		status: http.StatusInternalServerError,
		en:     en + ": " + err.Error(),
		zh:     zh + ": " + err.Error(),
	})
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

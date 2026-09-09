// Package web 提供 serve 命令的本地只读 HTTP 服务:为内嵌的 HTML 仪表板
// 提供 JSON(/api/meta、/api/dashboard)与 SVG(/api/chart/{kind}.svg)接口,
// 以及静态资产(/ 与 /assets/)。全部数据接口只读,服务端不写数据库与配置;
// 本地仅涉及生命周期文件 serve.json/serve.log(由 cli 层维护);无 CORS
// 头,按同源使用;响应统一禁用缓存(开发期所见即所得)。
package web

import (
	"encoding/json"
	"net/http"

	"github.com/YuLaiZ/token-usage/internal/querier"
	"github.com/YuLaiZ/token-usage/internal/ui"
)

// server 持有 HTTP 服务的只读依赖:查询器、版本串与可选的 provider 显示别名。
type server struct {
	q               *querier.Querier
	version         string
	providerAliases map[string]string
}

// ServerOption 是 NewServer 的可选参数。
type ServerOption func(*server)

// WithProviderAliases 注入 [provider_aliases] 配置。仅 provider 维度消费
// (与 cli 侧 dimensionAliases 同构,见 aliasesFor);缺省时 provider 维度
// 不做别名合并,其余维度语义不受影响。
func WithProviderAliases(m map[string]string) ServerOption {
	return func(s *server) { s.providerAliases = m }
}

// NewServer 构造本地仪表板服务的 HTTP Handler。路由:
//
//	GET /api/meta              服务版本与数据边界
//	GET /api/dashboard         区间汇总 + 8 维度行 + 活动热力矩阵 + Top sessions
//	GET /api/chart/{kind}.svg  单维度柱状/饼图与热力矩阵(独立取图接口)
//	GET /                      内嵌仪表板首页
//	GET /assets/               内嵌静态资产
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
	mux.HandleFunc("GET /api/chart/", s.handleChart)
	return noStore(mux)
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

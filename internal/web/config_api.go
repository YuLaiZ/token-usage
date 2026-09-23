package web

// config_api.go 定义网页配置读写的 HTTP 合同:编辑模型(ConfigDraft)、
// 读写接口(ConfigStore)与 handler。handler 只做 HTTP 解码、来源边界、
// 错误映射与响应;真实配置应用由 store 实现层走 configapp.Application.
// ApplyConfig 的 revision 校验、锁内原子写入与副作用处理,不在本文件
// 出现任何直接写 TOML 的逻辑。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/YuLaiZ/token-usage/internal/ui"
)

// DaemonDraft 是 [daemon] 段的编辑模型。
type DaemonDraft struct {
	PollInterval int  `json:"poll_interval"`
	AutoStart    bool `json:"autostart"`
}

// LogDraft 是 [log] 段的编辑模型;Level 为空串表示「默认级别」。
type LogDraft struct {
	Level   string `json:"level"`
	Dir     string `json:"dir"`
	MaxDays int    `json:"max_days"`
}

// ClientDraft 是一个采集客户端的编辑模型:启用开关、路由中间件名与
// 数据来源路径(paths 的键由客户端注册表定义)。
type ClientDraft struct {
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Router  string            `json:"router"`
	Paths   map[string]string `json:"paths"`
}

// RouterDraft 是一个路由中间件的编辑模型。
type RouterDraft struct {
	Name   string `json:"name"`
	DBPath string `json:"db_path"`
}

// AliasDraft 是一条供应商别名映射(原始标签 → 显示名)。
type AliasDraft struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// QueryDraft 是 query 段的编辑模型:default/subqueries/groups 为配置键
// 原语义,OutputColumns 为 query.output.columns 的布局;Diagnostics 在
// 当前配置解析存在问题时由 GET 下发(omitempty,PUT 忽略)。
type QueryDraft struct {
	Default       string            `json:"default"`
	Subqueries    map[string]string `json:"subqueries"`
	Groups        map[string]string `json:"groups"`
	OutputColumns []string          `json:"output_columns"`
	Diagnostics   []string          `json:"diagnostics,omitempty"`
}

// ConfigDraft 是配置页的完整编辑模型。刻意不含 data_dir:数据目录迁移
// 是带确认链路的维护操作,继续由 CLI 承担,网页不展示也不接受该字段。
//
// Query 为指针语义:nil 表示「保持磁盘上的 query 段原样」。GET 恒返回
// 非 nil(解析失败时为回退值+diagnostics);PUT 省略 query 即可在不触碰
// query 段的前提下保存其他区块——磁盘 query 段处于问题态(解析失败)时,
// 回退草稿直接写回会静默清掉用户的既有定义,因此前端只在用户明确编辑
// 过 query 段时才回传。
type ConfigDraft struct {
	Daemon          DaemonDraft   `json:"daemon"`
	Log             LogDraft      `json:"log"`
	Clients         []ClientDraft `json:"clients"`
	Routers         []RouterDraft `json:"routers"`
	ProviderAliases []AliasDraft  `json:"provider_aliases"`
	Query           *QueryDraft   `json:"query,omitempty"`
}

// ConfigView 是 GET /api/config 的载荷:revision 与当前编辑模型。
type ConfigView struct {
	Revision string      `json:"revision"`
	Config   ConfigDraft `json:"config"`
}

// ConfigApplyResult 是 PUT /api/config 的成功载荷:W nil 切片在 handler
// 出口统一归一为空数组,保证前端可安全取 length。
type ConfigApplyResult struct {
	Changed        bool        `json:"changed"`
	Revision       string      `json:"revision"`
	Config         ConfigDraft `json:"config"`
	Warnings       []string    `json:"warnings"`
	SuggestedSteps []string    `json:"suggested_steps"`
}

// ConfigStore 是配置读写 seam:生产实现绑定 configapp.Application
// (见 webconfig_app.go),测试注入 fake。Apply 返回的错误按类型映射
// HTTP 状态:ErrConfigRevisionConflict → 409、*ConfigInvalidError → 400、
// 其余 → 500。
type ConfigStore interface {
	Current(ctx context.Context) (ConfigView, error)
	CurrentDefaults(ctx context.Context) (ConfigDraft, error)
	Apply(ctx context.Context, revision string, draft ConfigDraft) (ConfigApplyResult, error)
}

// ErrConfigRevisionConflict 表示 expected revision 与磁盘当前 revision 不一致
// (配置已被其他进程修改),本次未写入。handler 映射 409 并在响应中附带
// 当前 revision 供前端重载,不自动覆盖他处修改。
var ErrConfigRevisionConflict = errors.New(ui.Bi(
	"config was modified by another process; reload the latest config and retry",
	"配置已被其他进程修改，请重新加载最新配置后再保存",
))

// ConfigInvalidError 表示草稿未通过校验(query 定义、注册表白名单、字段
// 边界等),handler 映射 400;Message 为可展示的双语诊断。
type ConfigInvalidError struct{ Message string }

func (e *ConfigInvalidError) Error() string { return e.Message }

// handleConfigGet 输出当前配置编辑模型与 revision。`?defaults=1` 返回
// 全默认值编辑模型(无 revision——不用于保存),供「恢复全部默认值」形成
// 待保存草稿,默认值定义不复制到前端。
func (s *server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.configStore == nil {
		writeError(w, apiError{status: http.StatusNotImplemented, en: "config API is not configured", zh: "配置接口未启用"})
		return
	}
	if r.URL.Query().Get("defaults") == "1" {
		draft, err := s.configStore.CurrentDefaults(r.Context())
		if err != nil {
			writeInternal(w, "failed to build default config", "构造默认配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": draft})
		return
	}
	view, err := s.configStore.Current(r.Context())
	if err != nil {
		writeInternal(w, "failed to read config", "读取配置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// configPutRequest 是 PUT /api/config 的请求体。
type configPutRequest struct {
	Revision string      `json:"revision"`
	Config   ConfigDraft `json:"config"`
}

// handleConfigPut 应用配置草稿:revision 冲突 409(响应附带当前 revision)、
// 校验失败 400、其余 store 错误 500。成功(含「已保存但副作用部分失败」)
// 一律 200,部分失败以非空 warnings 表达,不伪装为完全成功。
func (s *server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.configStore == nil {
		writeError(w, apiError{status: http.StatusNotImplemented, en: "config API is not configured", zh: "配置接口未启用"})
		return
	}
	var req configPutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apiError{status: http.StatusBadRequest, en: "invalid request body: expected {revision, config}", zh: "请求体非法：应为 {revision, config}"})
		return
	}
	if req.Revision == "" {
		writeError(w, apiError{status: http.StatusBadRequest, en: "missing revision: reload /api/config and retry", zh: "缺少 revision：请重新读取 /api/config 后再保存"})
		return
	}
	// 草稿完整性门槛:clients/routers 必须存在(合法草稿恒含全部注册项,
	// 来自 GET)。缺失意味着请求体不完整,若放行会把客户端配置重建为空表。
	if req.Config.Clients == nil || req.Config.Routers == nil {
		writeError(w, apiError{status: http.StatusBadRequest, en: "incomplete config draft: expected the full draft from GET /api/config", zh: "配置草稿不完整：应回传 GET /api/config 的完整草稿"})
		return
	}
	result, err := s.configStore.Apply(r.Context(), req.Revision, req.Config)
	if err != nil {
		var inv *ConfigInvalidError
		switch {
		case errors.Is(err, ErrConfigRevisionConflict):
			// 冲突响应附带当前磁盘 revision,前端重载后可继续编辑。
			resp := map[string]any{
				"error": map[string]string{"message": err.Error()},
			}
			if view, cerr := s.configStore.Current(r.Context()); cerr == nil {
				resp["revision"] = view.Revision
			}
			writeJSON(w, http.StatusConflict, resp)
		case errors.As(err, &inv):
			writeError(w, apiError{status: http.StatusBadRequest, en: inv.Message, zh: inv.Message})
		default:
			writeInternal(w, "failed to save config", "保存配置失败", err)
		}
		return
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	if result.SuggestedSteps == nil {
		result.SuggestedSteps = []string{}
	}
	writeJSON(w, http.StatusOK, result)
}

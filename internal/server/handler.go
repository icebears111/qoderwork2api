// handler.go — HTTP 路由：多租户 OpenAI 兼容 API + 用户/管理员 API + 前端。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoderwork2api/internal/apikey"
	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/modelstate"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
	"qoderwork2api/internal/usagestat"
	"qoderwork2api/internal/user"
)

//go:embed admin.html
var adminHTML embed.FS

// Config handler 配置。
type Config struct {
	Pool         *pool.Pool // 管理员/全局池
	Upstream     *upstream.Client
	APIKey       string // 管理员 API Key
	MaxRotate    int
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	AuthDir      string
	OnReload     func()
	UserStore    *user.Store
	// KeyStore 多 key 表（看板发放的 sk- 调用凭证）。
	// 与全局 APIKey、UserStore 三者并存；nil 表示该功能未启用。
	KeyStore *apikey.Store
	// ModelState 模型启停表（看板「模型」页禁用/恢复）；nil = 功能未启用
	// （读作「没有任何模型被禁用」，而不是「全部禁用」）。
	ModelState *modelstate.Store
	// UsageStats token 用量 / 缓存命中的按桶统计（看板「缓存命中」）；nil = 不统计。
	UsageStats *usagestat.Store
	PoolMgr    *user.PoolManager
}

// Handler HTTP 路由处理器。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	modelsMu       sync.RWMutex
	dynamicModels  []upstream.DynamicModel
	dynamicMap     map[string]string
	dynamicFetched time.Time
}

const dynamicModelsTTL = time.Hour

func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}

	// OpenAI 兼容 API（多租户）
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))

	// 健康检查
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.status)

	// 用户 API（需要认证）
	userAPI := NewUserHandler(cfg.PoolMgr, cfg.Upstream)
	userAPI.SetAdminPool(cfg.Pool, cfg.AuthDir)
	mux := http.NewServeMux()
	userAPI.Register(mux)
	h.mux.Handle("/api/user/", http.StripPrefix("/api/user", AuthMiddleware(h.cfg.UserStore, h.cfg.APIKey, mux)))

	// 管理员 API（需要认证 + admin 权限）
	adminAPI := NewAdminHandler(cfg.UserStore, cfg.PoolMgr, cfg.Upstream, cfg.Pool)
	adminMux := http.NewServeMux()
	adminAPI.Register(adminMux)
	h.mux.Handle("/api/admin/", http.StripPrefix("/api/admin", AuthMiddleware(h.cfg.UserStore, h.cfg.APIKey, RequireAdminMiddleware(adminMux))))

	// 多 key 管理路由。
	// 守卫用 requireGlobalKey，**只认全局 key** —— 若放开给调用方
	// 自己的 key，任何一把外传的 key 就能给自己签发新 key（无限提权）。
	// 路由必须带方法注册，否则与更宽的 catch-all 冲突（Go 1.22+ 会 panic）。
	if h.cfg.KeyStore != nil {
		guard := func(next http.HandlerFunc) http.HandlerFunc {
			return requireGlobalKey(h.cfg.APIKey, next)
		}
		kh := apikey.NewHandler(h.cfg.KeyStore)
		// OwnerFrom **有意不设**（保持 nil = 管理员作用域）。
		//
		// 本桥没有多用户：账号归属与 SSO 身份头（X-Auth-User）都不存在，
		// 成员在看板上也看不到这一家（前端 visibleGWs 只给 buddy）。
		// 本桥自己有 user.Store（usr_ key + 角色），但那是**另一套**体系、
		// 与 SSO 无关；keys 这里是管理员用的发放接口，守卫只认全局 key。
		//
		// 因此 apikey 包里那套「管理员不能取成员凭证明文」的规则在此
		// **恒不触发**；代码仍与另两座桥一致 —— 将来若接多用户，
		// 补上 OwnerFrom 即可生效。
		h.mux.HandleFunc("GET /admin/api/keys", guard(kh.List))
		h.mux.HandleFunc("POST /admin/api/keys", guard(kh.Create))
		h.mux.HandleFunc("DELETE /admin/api/keys/", guard(kh.Delete))
		h.mux.HandleFunc("GET /admin/api/keys/", guard(kh.Get))
	}

	// 模型启停：与 key 管理同一道守卫（全局 key，调用方 key 不得改模型状态）
	h.mux.HandleFunc("POST /admin/api/models/state", requireGlobalKey(h.cfg.APIKey, h.adminSetModelState))
	// 用量 / 缓存命中统计（看板「报表」页的缓存面板）
	h.mux.HandleFunc("GET /admin/api/usage/stats", requireGlobalKey(h.cfg.APIKey, h.adminUsageStats))

	// 用户登录接口（公开）
	h.mux.HandleFunc("POST /api/login", adminAPI.HandleUserLogin)

	// 前端页面
	h.mux.HandleFunc("GET /admin", h.serveAdmin)
	h.mux.HandleFunc("GET /admin/", h.serveAdmin)
	h.mux.HandleFunc("/", h.serveIndex)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := extractKey(r)
		if key == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}

		// 先匹配管理员 key
		if key == h.cfg.APIKey && h.cfg.APIKey != "" {
			adminUser := &user.User{
				ID:     "admin",
				Name:   "Administrator",
				APIKey: h.cfg.APIKey,
				Role:   user.RoleAdmin,
			}
			ctx := context.WithValue(r.Context(), ctxKeyUser, adminUser)
			next(w, r.WithContext(ctx))
			return
		}

		// 多 key 表（看板发放的调用凭证）。
		// 命中时不注入用户上下文 —— getUserPool 会回落到全局池，
		// 即这些 key 用的是与全局 key 相同的账号池，语义与其他两座桥一致。
		if h.cfg.KeyStore != nil {
			if _, ok := h.cfg.KeyStore.Verify(key); ok {
				next(w, r)
				return
			}
		}

		// 匹配普通用户
		u := h.cfg.UserStore.GetByKey(key)
		if u == nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUser, u)
		next(w, r.WithContext(ctx))
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	// models 用于看板「模型」页：**必须含被禁用的项**并带 enabled 标记 ——
	// 只给已启用的会让被禁用的模型从页面上消失，用户再也找不到开关恢复它。
	mm := h.effectiveModelMap()
	names := make([]string, 0, len(mm))
	for name := range mm {
		names = append(names, name)
	}
	for i := 0; i < len(names)-1; i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	models := make([]map[string]any, 0, len(names))
	for _, name := range names {
		models = append(models, map[string]any{
			"id":      name,
			"enabled": !h.modelDisabled(name),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
		"has_auth": h.cfg.APIKey != "",
		"models":   models,
	})
}

// adminSetModelState POST /admin/api/models/state
// body {"id":"qwen3.7-max","enabled":false} —— 启停一个模型（看板「模型」页）。
func (h *Handler) adminSetModelState(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ModelState == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "not_supported",
			"model state store is not enabled on this gateway")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少模型 id")
		return
	}
	if body.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 enabled")
		return
	}
	h.cfg.ModelState.SetDisabled(body.ID, !*body.Enabled)
	fmt.Printf("model %s enabled=%v (by admin API)\n", body.ID, *body.Enabled)
	// 回最新状态（含被禁用项），前端就地重绘
	mm := h.effectiveModelMap()
	out := make([]map[string]any, 0, len(mm))
	for name := range mm {
		out = append(out, map[string]any{"id": name, "enabled": !h.modelDisabled(name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (h *Handler) serveAdmin(w http.ResponseWriter, r *http.Request) {
	data, err := adminHTML.ReadFile("admin.html")
	if err != nil {
		http.Error(w, "admin page not found", http.StatusNotFound)
		return
	}
	html := strings.Replace(string(data), "/*__API_KEY__*/",
		fmt.Sprintf("window.__API_KEY__ = %q; window.__IS_ADMIN__ = %v;",
			h.cfg.APIKey, h.cfg.APIKey != ""), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// models 对外清单（**已过滤禁用**）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	mm := h.visibleModelMap()
	meta := h.dynamicModelsMeta()
	names := make([]string, 0, len(mm))
	for name := range mm {
		names = append(names, name)
	}
	for i := 0; i < len(names)-1; i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	data := h.modelEntries(names, mm, meta)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// visibleModelMap effectiveModelMap 去掉被看板禁用的项。
//
// 只给 **/v1/models 与管理页**用；聊天请求的模型解析走 effectiveModelMap
// 本身 —— 那里要能查到一个模型「是否存在」，才能对禁用与不存在给出
// 不同的错误（见 chatCompletions）。
func (h *Handler) visibleModelMap() map[string]string {
	full := h.effectiveModelMap()
	if h.cfg.ModelState == nil {
		return full
	}
	out := make(map[string]string, len(full))
	for name, key := range full {
		if !h.modelDisabled(name) {
			out[name] = key
		}
	}
	return out
}

// modelDisabled 该模型是否被看板禁用（大小写不敏感，ModelState 为 nil 时恒 false）。
func (h *Handler) modelDisabled(name string) bool {
	return h.cfg.ModelState != nil && h.cfg.ModelState.IsDisabled(name)
}

// recordUsage 记一次真实请求的 token 用量（含缓存命中）。
//
// prompt 直接取上游给的值：本桥不像 catpaw 那样在缺失时估算，
// 上游不给就按 0 记（那说明这轮没上报用量，不该编一个数 ——
// 编了会让命中率的分母虚高，把命中率算低）。
func (h *Handler) recordUsage(up map[string]any) {
	if h.cfg.UsageStats == nil || up == nil {
		return
	}
	var prompt int64
	switch n := up["prompt_tokens"].(type) {
	case float64:
		prompt = int64(n)
	case int64:
		prompt = n
	case int:
		prompt = int64(n)
	}
	h.cfg.UsageStats.Add(time.Now(), prompt, usagestat.CacheRead(up))
}

// adminUsageStats GET /admin/api/usage/stats
// 看板「缓存命中」用：四窗口命中率 + 近 24 小时趋势。
func (h *Handler) adminUsageStats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.UsageStats == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "note": "本网关未启用用量统计",
			"rates": map[string]any{}, "trend24h": []any{},
		})
		return
	}
	now := time.Now()
	hit, input := h.cfg.UsageStats.TotalSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      true,
		"rates":        h.cfg.UsageStats.Rates(now),
		"trend24h":     h.cfg.UsageStats.Trend24h(now),
		"total":        map[string]any{"hitTokens": hit, "inputTokens": input},
		"generated_at": now.Unix(),
	})
}

// modelEntries 把模型名 + 上游键 + 动态元信息组装成 OpenAI 风格的条目。
// /v1/models 与管理页共用，两处字段因此不会漂移。
func (h *Handler) modelEntries(names []string, mm map[string]string,
	meta map[string]upstream.DynamicModel) []map[string]any {
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry := map[string]any{
			"id":       name,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "qoderwork",
		}
		if m, ok := meta[name]; ok {
			entry["display_name"] = m.DisplayName
			entry["upstream_key"] = m.Key
			if m.IsReasoning {
				entry["reasoning"] = true
			}
			if m.IsVL {
				entry["vision"] = true
			}
			if m.MaxInputTokens > 0 {
				entry["context_length"] = m.MaxInputTokens
			}
			if m.PriceFactor > 0 {
				entry["price_factor"] = m.PriceFactor
			}
		} else {
			entry["upstream_key"] = mm[name]
		}
		data = append(data, entry)
	}
	return data
}

func (h *Handler) dynamicModelsMeta() map[string]upstream.DynamicModel {
	h.modelsMu.RLock()
	defer h.modelsMu.RUnlock()
	out := make(map[string]upstream.DynamicModel, len(h.dynamicModels))
	for _, m := range h.dynamicModels {
		name := m.Key
		if m.DisplayName != "" {
			name = upstream.NormalizeModelName(m.DisplayName)
		}
		out[name] = m
	}
	return out
}

func (h *Handler) effectiveModelMap() map[string]string {
	h.modelsMu.RLock()
	if len(h.dynamicMap) > 0 && time.Since(h.dynamicFetched) < dynamicModelsTTL {
		m := h.dynamicMap
		h.modelsMu.RUnlock()
		return m
	}
	h.modelsMu.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return fallbackModelMap()
	}
	if err := acct.EnsureDT(h.cfg.Upstream.Base); err != nil {
		if cred.IsAuthInvalid(err) {
			h.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
		}
		return fallbackModelMap()
	}
	dyn, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(dyn) == 0 {
		return fallbackModelMap()
	}
	resolved := upstream.ResolveModelMap(dyn)
	h.modelsMu.Lock()
	h.dynamicModels = dyn
	h.dynamicMap = resolved
	h.dynamicFetched = time.Now()
	h.modelsMu.Unlock()
	return resolved
}

func fallbackModelMap() map[string]string {
	return map[string]string{
		"auto":                "auto",
		"qwen3.8-max-preview": "qmodel_preview",
		"qwen3.7-max":         "qmodel_latest",
		"qwen3.7-plus":        "qmodel",
		"qwen3.6-flash":       "q36fmodel",
		"deepseek-v4-pro":     "dmodel",
		"deepseek-v4-flash":   "dfmodel",
		"glm-5.2":             "gm51model",
		"kimi-k2.7-code":      "kmodel",
		"minimax-m2.7":        "mmodel",
	}
}

type chatRequest struct {
	Model      string           `json:"model"`
	Messages   []map[string]any `json:"messages"`
	Stream     bool             `json:"stream"`
	Tools      []any            `json:"tools"`
	ToolChoice any              `json:"tool_choice"`
	// ReasoningEffort 思考档位（low/medium/xhigh）；所有模型均生效（2026-09-19 起统
	// 一走 qwen3.8-flash 的思考链路）。客户端用 OpenAI 风格 reasoning_effort 字段。
	ReasoningEffort string `json:"reasoning_effort"`
}

// getUserPool 获取当前用户对应的账号池。
func (h *Handler) getUserPool(r *http.Request) *pool.Pool {
	u, ok := r.Context().Value(ctxKeyUser).(*user.User)
	if !ok || u == nil {
		return h.cfg.Pool
	}
	if u.Role == user.RoleAdmin && u.ID == "admin" {
		return h.cfg.Pool
	}
	p := h.cfg.PoolMgr.GetPool(u.ID)
	if p == nil {
		return h.cfg.Pool
	}
	return p
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	p := h.getUserPool(r)
	if p.Pick() == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "all accounts unavailable (cooling/disabled)")
		return
	}

	modelKey, ok := h.effectiveModelMap()[strings.ToLower(req.Model)]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("unknown model %q (see /v1/models)", req.Model))
		return
	}
	// 被看板禁用的模型：**给出与「模型不存在」不同的错误**。
	// 两者都 400，但对调用方的意义完全不同 —— 前者是「这个模型存在，
	// 但网关管理员关掉了它」（换个模型或找管理员），后者是「你写错了名字」。
	// 合并成一句话会让用户去检查拼写，而问题其实在开关上。
	if h.modelDisabled(req.Model) {
		writeOpenAIError(w, http.StatusBadRequest, "model_disabled",
			fmt.Sprintf("model %q is disabled on this gateway (see /v1/models)", req.Model))
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := p.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		if err := acct.EnsureDT(h.cfg.Upstream.Base); err != nil {
			lastErr = err
			if cred.IsAuthInvalid(err) {
				p.Disable(acct.UID, "auth invalid (re-login required)")
			} else {
				p.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "dt ensure: "+err.Error())
			}
			continue
		}
		acct.EnsureMachineFingerprint()

		rc, err := h.cfg.Upstream.ChatForward(acct, req.Messages, modelKey, req.Tools,
			upstream.NormalizeReasoningEffort(req.ReasoningEffort))
		if err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) {
				switch ue.Kind {
				case upstream.ErrHardCredit:
					p.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
					lastErr = ue
					continue
				case upstream.ErrSoftRate:
					p.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
					lastErr = ue
					continue
				case upstream.ErrTokenExpired:
					acct.DTExpiresAt = 0
					p.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					lastErr = ue
					continue
				case upstream.ErrSessionDead:
					p.Disable(acct.UID, "session dead")
					lastErr = ue
					continue
				case upstream.ErrNotFound:
					p.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
					lastErr = ue
					continue
				default:
					p.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					lastErr = ue
					continue
				}
			}
			p.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			lastErr = err
			continue
		}
		defer rc.Close()
		p.NoteSuccess(acct.UID)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			fl, _ := w.(http.Flusher)
			flush := func() {
				if fl != nil {
					fl.Flush()
				}
			}
			// 流式路径也要记用量：StreamAsOpenAI 边转发边抄 usage，
			// 通过回调把手上的那份交出来（它不改动要下发的字节）。
			_ = upstream.StreamAsOpenAI(w, rc, req.Model, flush, func(u map[string]any) {
				h.recordUsage(u)
			})
			return
		}
		resp, err := upstream.AggregateNested(rc, req.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if u, ok := resp["usage"].(map[string]any); ok {
			h.recordUsage(u)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

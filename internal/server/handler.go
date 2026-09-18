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

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
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
	PoolMgr      *user.PoolManager
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
		"has_auth": h.cfg.APIKey != "",
	})
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

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	mm := h.effectiveModelMap()
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
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
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
	// ReasoningEffort 思考档位（low/medium/xhigh）；仅 qwen3.8-flash 生效。
	// 客户端也可用 OpenAI 风格 reasoning_effort 字段（同一 JSON key）。
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
			_ = upstream.StreamAsOpenAI(w, rc, req.Model, flush)
			return
		}
		resp, err := upstream.AggregateNested(rc, req.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
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

// auth.go — 多租户认证中间件。
package server

import (
	"context"
	"net/http"
	"strings"

	"qoderwork2api/internal/user"
)

// ctxKey 是 context 中用户信息的键。
type ctxKey string

const ctxKeyUser = ctxKey("current_user")

// AuthMiddleware 解析 API Key，识别用户，注入 context。
func AuthMiddleware(store *user.Store, adminKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := extractKey(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing API key"})
			return
		}

		// 先匹配管理员 key
		if key == adminKey && adminKey != "" {
			adminUser := &user.User{
				ID:     "admin",
				Name:   "Administrator",
				APIKey: adminKey,
				Role:   user.RoleAdmin,
			}
			ctx := context.WithValue(r.Context(), ctxKeyUser, adminUser)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// 匹配普通用户
		u := store.GetByKey(key)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid API key"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUser, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdminMiddleware 包装 http.Handler，要求管理员权限。
func RequireAdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := r.Context().Value(ctxKeyUser).(*user.User)
		if !ok || u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if u.Role != user.RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin access required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CurrentUser 从 context 获取当前用户。
func CurrentUser(r *http.Request) *user.User {
	u, _ := r.Context().Value(ctxKeyUser).(*user.User)
	return u
}

func extractKey(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	return ""
}

// requireGlobalKey 只认全局 key —— 用于**管理接口**（key 的发放与吊销）。
//
// 为什么不复用 AuthMiddleware：它同时接受 UserStore 里的用户 key，而那些
// 是"用池子"的凭证。管理面必须收窄到全局 key（只存在于服务器与运维手里），
// 否则任何一把外传的调用方 key 都能给自己签发新 key。
// 全局 key 为空表示未配置，此时放行（内网自用模式，与其它桥一致）。
func requireGlobalKey(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			next(w, r)
			return
		}
		if extractKey(r) != apiKey {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
				"missing or invalid API key")
			return
		}
		next(w, r)
	}
}

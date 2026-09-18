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

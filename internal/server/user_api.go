// user_api.go — 用户 API：账号管理、积分刷新、OAuth 登录。
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
	"qoderwork2api/internal/user"
)

// UserHandler 用户 API 处理器。
type UserHandler struct {
	poolMgr      *user.PoolManager
	upstream     *upstream.Client
	adminPool    *pool.Pool
	adminAuthDir string
}

func NewUserHandler(pm *user.PoolManager, up *upstream.Client) *UserHandler {
	return &UserHandler{poolMgr: pm, upstream: up}
}

func (h *UserHandler) SetAdminPool(p *pool.Pool, authDir string) {
	h.adminPool = p
	h.adminAuthDir = authDir
}

func (h *UserHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", h.handleStatus)
	mux.HandleFunc("GET /quota", h.handleQuota)
	mux.HandleFunc("POST /quota/refresh", h.handleQuotaRefresh)
	mux.HandleFunc("POST /login/url", h.handleLoginURL)
	mux.HandleFunc("POST /login/poll", h.handleLoginPoll)
	mux.HandleFunc("POST /login/cancel", h.handleLoginCancel)
	mux.HandleFunc("DELETE /accounts/", h.handleDeleteAccount)
}

// userPool 获取用户对应的池（admin 返回全局池）。
func (h *UserHandler) userPool(u *user.User) *pool.Pool {
	if u == nil {
		return nil
	}
	if u.Role == user.RoleAdmin && u.ID == "admin" {
		return h.adminPool
	}
	return h.poolMgr.GetPool(u.ID)
}

func (h *UserHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	p := h.userPool(u)
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}, "count": 0})
		return
	}
	accounts := p.List()
	result := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		item := map[string]any{
			"uid":      acc.UID,
			"nickname": acc.Nickname,
			"credits":  acc.Credits,
			"cooling":  acc.Cooling,
			"disabled": acc.Disabled,
			"reason":   acc.Reason,
		}
		result = append(result, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": result, "count": len(result)})
}

func (h *UserHandler) handleQuota(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	p := h.userPool(u)
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}})
		return
	}
	accounts := p.List()
	result := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		item := map[string]any{
			"uid":      acc.UID,
			"nickname": acc.Nickname,
			"credits":  acc.Credits,
			"disabled": acc.Disabled,
		}
		if !acc.Disabled {
			creds := p.AuthByUID(acc.UID)
			if creds != nil && creds.DT != "" {
				remain, exceeded, err := h.upstream.QuotaUsage(creds.DT)
				if err == nil {
					item["realtime_credits"] = remain
					item["exceeded"] = exceeded
				} else {
					item["error"] = err.Error()
				}
			}
		}
		result = append(result, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": result})
}

func (h *UserHandler) handleQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	p := h.userPool(u)
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"updated": 0})
		return
	}
	accounts := p.List()
	updated := 0
	var errs []string
	for _, acc := range accounts {
		if acc.Disabled {
			continue
		}
		creds := p.AuthByUID(acc.UID)
		if creds == nil || (creds.DT == "" && creds.DRT == "") {
			continue
		}
		remain, exceeded, err := h.upstream.QuotaUsage(creds.DT)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", acc.Nickname, err))
			continue
		}
		if exceeded {
			p.Cooldown(acc.UID, pool.CoolHard, 12*time.Hour, "quota exceeded")
		} else {
			p.ReenableIfCredits(acc.UID, remain)
		}
		updated++
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "errors": errs})
}

// 用户登录 session 存储
var userSessionStore = &userLoginSessions{
	sessions: map[string]*userLoginSession{},
}

type userLoginSessions struct {
	mu       sync.RWMutex
	sessions map[string]*userLoginSession
}

type userLoginSession struct {
	UserID    string
	Verifier  string
	Nonce     string
	MachineID string
	AuthURL   string
	CreatedAt time.Time
}

func (h *UserHandler) handleLoginURL(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	verifier, challenge := makePKCE()
	nonce := uuid4()
	machineID := uuid4()

	authURL := fmt.Sprintf(
		"%s/device/selectAccounts?challenge=%s&challenge_method=S256&nonce=%s&machine_id=%s&client_id=%s&redirect_uri=%s",
		oauthWebsiteCN, challenge, nonce, machineID, oauthClientID, oauthRedirectURI,
	)

	sessionID := uuid4()
	userSessionStore.mu.Lock()
	userSessionStore.sessions[sessionID] = &userLoginSession{
		UserID:    u.ID,
		Verifier:  verifier,
		Nonce:     nonce,
		MachineID: machineID,
		AuthURL:   authURL,
		CreatedAt: time.Now(),
	}
	userSessionStore.mu.Unlock()

	go func() {
		time.AfterFunc(10*time.Minute, func() {
			userSessionStore.mu.Lock()
			delete(userSessionStore.sessions, sessionID)
			userSessionStore.mu.Unlock()
		})
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"auth_url":   authURL,
		"expires_in": 600,
	})
}

func (h *UserHandler) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}

	userSessionStore.mu.RLock()
	sess, ok := userSessionStore.sessions[req.SessionID]
	userSessionStore.mu.RUnlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}

	tok, pending, err := deviceTokenPoll(sess.Nonce, sess.Verifier)
	if err != nil {
		log.Printf("poll error (session=%s, nonce=%s): %v", req.SessionID, sess.Nonce, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if pending {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "message": "授权未完成"})
		return
	}

	userSessionStore.mu.Lock()
	delete(userSessionStore.sessions, req.SessionID)
	userSessionStore.mu.Unlock()

	token, _ := tok["token"].(string)
	if token == "" {
		token, _ = tok["device_token"].(string)
	}
	refresh, _ := tok["refresh_token"].(string)
	uid, _ := tok["user_id"].(string)
	expiresIn, _ := tok["expires_in"].(float64)

	nickname := h.fetchNickname(token)
	if nickname == "" {
		nickname = "user_" + uid[:8]
	}

	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Millisecond).Unix()
	auth := map[string]any{
		"auth": map[string]any{
			"accessToken":  token,
			"refreshToken": refresh,
			"expiresAt":    expiresAt,
			"domain":       "qoder.com.cn",
		},
		"account": map[string]any{
			"uid":      uid,
			"nickname": nickname,
		},
	}

	authDir := h.poolMgr.GetUserAuthDir(sess.UserID)
	if authDir == "" && h.adminAuthDir != "" && sess.UserID == "admin" {
		authDir = h.adminAuthDir
	}
	if authDir == "" {
		log.Printf("poll: no auth dir for user %s", sess.UserID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user pool not found"})
		return
	}

	authFile := filepath.Join(authDir, fmt.Sprintf("qoderwork-%s.json", uid))
	raw, _ := json.MarshalIndent(auth, "", "  ")
	if err := os.WriteFile(authFile, raw, 0o600); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save failed: " + err.Error()})
		return
	}

	// 重新加载用户池
	p := h.poolMgr.GetPool(sess.UserID)
	if p == nil && sess.UserID == "admin" {
		p = h.adminPool
	}
	if p != nil {
		if c, err := cred.LoadFile(authFile); err == nil {
			c.EnsureMachineFingerprint()
			p.Add(c)
			p.SaveState()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "success",
		"uid":      uid,
		"nickname": nickname,
	})
}

func (h *UserHandler) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	userSessionStore.mu.Lock()
	delete(userSessionStore.sessions, req.SessionID)
	userSessionStore.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled"})
}

func (h *UserHandler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	uid := strings.TrimPrefix(r.URL.Path, "/accounts/")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid required"})
		return
	}
	var authDir string
	if u.Role == user.RoleAdmin && u.ID == "admin" {
		// admin 删除全局账号 - 需要从 authDir 获取
		authDir = "" // TODO: admin 的全局 authDir
	} else {
		authDir = h.poolMgr.GetUserAuthDir(u.ID)
	}
	if authDir == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user pool not found"})
		return
	}
	authFile := filepath.Join(authDir, fmt.Sprintf("qoderwork-%s.json", uid))
	if err := os.Remove(authFile); err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "uid": uid})
}

func (h *UserHandler) fetchNickname(token string) string {
	req, err := http.NewRequest("GET", "https://openapi.qoder.com.cn/api/v1/userinfo", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var u struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	}
	json.Unmarshal(raw, &u)
	if u.Name != "" {
		return u.Name
	}
	return u.Username
}

// AdminHandler 管理员 API。
type AdminHandler struct {
	store    *user.Store
	poolMgr  *user.PoolManager
	upstream *upstream.Client
	adminPool *pool.Pool // 管理员自己的账号池
}

func NewAdminHandler(store *user.Store, pm *user.PoolManager, up *upstream.Client, adminPool *pool.Pool) *AdminHandler {
	return &AdminHandler{
		store:     store,
		poolMgr:   pm,
		upstream:  up,
		adminPool: adminPool,
	}
}

func (h *AdminHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /users", h.handleListUsers)
	mux.HandleFunc("POST /users", h.handleCreateUser)
	mux.HandleFunc("DELETE /users/", h.handleDeleteUser)
	mux.HandleFunc("GET /stats", h.handleStats)
}

// RegisterPublic 注册公开接口（无需认证）。
func (h *AdminHandler) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("POST /login", h.handleUserLogin)
}

// HandleUserLogin 公开的用户登录接口。
func (h *AdminHandler) HandleUserLogin(w http.ResponseWriter, r *http.Request) {
	h.handleUserLogin(w, r)
}

// handleUserLogin 用户通过用户名密码登录，返回 api_key。
func (h *AdminHandler) handleUserLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	u := h.store.AuthByPassword(req.Name, req.Password)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid username or password"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      u.ID,
		"name":    u.Name,
		"api_key": u.APIKey,
		"role":    string(u.Role),
	})
}

func (h *AdminHandler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users := h.store.List()
	result := make([]map[string]any, 0, len(users))
	for _, u := range users {
		p := h.poolMgr.GetPool(u.ID)
		accCount := 0
		if p != nil {
			accCount = len(p.List())
		}
		result = append(result, map[string]any{
			"id":         u.ID,
			"name":       u.Name,
			"api_key":    u.APIKey,
			"role":       string(u.Role),
			"created_at": u.CreatedAt,
			"acc_count":  accCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": result})
}

func (h *AdminHandler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if req.Name == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and password required"})
		return
	}
	role := user.RoleUser
	if req.Role == "admin" {
		role = user.RoleAdmin
	}
	u, err := h.store.Create(req.Name, req.Password, role)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	h.poolMgr.RegisterUser(u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      u.ID,
		"name":    u.Name,
		"api_key": u.APIKey,
		"role":    string(u.Role),
	})
}

func (h *AdminHandler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/users/")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	if err := h.store.Delete(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	h.poolMgr.RemoveUser(id)
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (h *AdminHandler) handleStats(w http.ResponseWriter, r *http.Request) {
	users := h.store.List()
	allPools := h.poolMgr.AllPools()

	totalAccounts := 0
	totalCredits := int64(0)
	userStats := make([]map[string]any, 0, len(users))

	for _, u := range users {
		p := allPools[u.ID]
		accCount := 0
		credits := int64(0)
		if p != nil {
			for _, acc := range p.List() {
				accCount++
				credits += acc.Credits
			}
		}
		totalAccounts += accCount
		totalCredits += credits
		userStats = append(userStats, map[string]any{
			"id":        u.ID,
			"name":      u.Name,
			"acc_count": accCount,
			"credits":   credits,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_users":     len(users),
		"total_accounts":  totalAccounts,
		"total_credits":   totalCredits,
		"users":           userStats,
	})
}

// 复用 oauth 常量
const (
	oauthWebsiteCN   = "https://qoder.com.cn"
	oauthOpenapiCN   = "https://openapi.qoder.com.cn"
	oauthClientID    = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	oauthRedirectURI = "qoder-work-cn://"
	clientUA         = "Go-http-client/2.0"
)

func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, 64)
	rand.Read(buf)
	var sb strings.Builder
	sb.Grow(64)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func deviceTokenPoll(nonce, verifier string) (map[string]any, bool, error) {
	url := fmt.Sprintf("%s/api/v1/deviceToken/poll?nonce=%s&verifier=%s&challenge_method=S256",
		oauthOpenapiCN, nonce, verifier)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, true, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, false, fmt.Errorf("parse: %w", err)
	}
	t, _ := tok["token"].(string)
	if t == "" {
		t, _ = tok["device_token"].(string)
	}
	if t == "" {
		return nil, true, nil
	}
	return tok, false, nil
}

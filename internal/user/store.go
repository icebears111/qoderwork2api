// Package user 多用户管理：用户表 + 账号池工厂。
package user

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
)

// Role 用户角色。
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// User 表示一个系统用户。
type User struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	APIKey       string    `json:"api_key"`
	PasswordHash string    `json:"password_hash"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store 用户持久化层。
type Store struct {
	mu      sync.RWMutex
	users   map[string]*User // key = user ID
	byKey   map[string]*User // key = api_key
	filePath string
}

// NewStore 加载用户存储。
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		users:    map[string]*User{},
		byKey:    map[string]*User{},
		filePath: filePath,
	}
	if filePath != "" {
		if err := s.load(); err != nil {
			return nil, fmt.Errorf("load users: %w", err)
		}
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var users []*User
	if err := json.Unmarshal(raw, &users); err != nil {
		return err
	}
	for _, u := range users {
		s.users[u.ID] = u
		s.byKey[u.APIKey] = u
	}
	return nil
}

func (s *Store) save() error {
	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	raw, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.filePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

// Create 创建用户。
func (s *Store) Create(name, password string, role Role) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range s.users {
		if u.Name == name {
			return nil, fmt.Errorf("username %q already exists", name)
		}
	}

	id := generateID()
	apiKey := "usr_" + randomHex(32)
	u := &User{
		ID:           id,
		Name:         name,
		APIKey:       apiKey,
		PasswordHash: hashPassword(password),
		Role:         role,
		CreatedAt:    time.Now(),
	}
	s.users[u.ID] = u
	s.byKey[u.APIKey] = u
	if err := s.save(); err != nil {
		delete(s.users, u.ID)
		delete(s.byKey, u.APIKey)
		return nil, err
	}
	return u, nil
}

// AuthByPassword 验证用户名和密码，成功返回用户。
func (s *Store) AuthByPassword(name, password string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Name == name {
			if verifyPassword(u.PasswordHash, password) {
				return u
			}
			return nil
		}
	}
	return nil
}

// hashPassword 哈希密码：sha256(salt + password)。
func hashPassword(password string) string {
	salt := randomHex(16)
	h := sha256.Sum256([]byte(salt + password))
	return salt + ":" + hex.EncodeToString(h[:])
}

// verifyPassword 验证密码。
func verifyPassword(hash, password string) bool {
	parts := strings.SplitN(hash, ":", 2)
	if len(parts) != 2 {
		return false
	}
	salt, expected := parts[0], parts[1]
	h := sha256.Sum256([]byte(salt + password))
	actual := hex.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

// Delete 删除用户。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return fmt.Errorf("user %q not found", id)
	}
	delete(s.users, id)
	delete(s.byKey, u.APIKey)
	return s.save()
}

// GetByID 按 ID 查找。
func (s *Store) GetByID(id string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

// GetByKey 按 API Key 查找。
func (s *Store) GetByKey(key string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byKey[key]
}

// List 返回所有用户。
func (s *Store) List() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	return users
}

// Count 返回用户数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// PoolManager 为每个用户管理独立的账号池。
type PoolManager struct {
	mu        sync.RWMutex
	pools     map[string]*pool.Pool // key = user ID
	authDirs  map[string]string     // key = user ID -> auth directory
	stateDirs map[string]string     // key = user ID -> state directory
	baseDir   string
	upstream  QuotaQuerier
}

// QuotaQuerier 积分查询接口。
type QuotaQuerier interface {
	QuotaUsage(dt string) (remain int64, exceeded bool, err error)
}

// NewPoolManager 创建池管理器。
func NewPoolManager(baseDir string, upstream QuotaQuerier) *PoolManager {
	return &PoolManager{
		pools:     map[string]*pool.Pool{},
		authDirs:  map[string]string{},
		stateDirs: map[string]string{},
		baseDir:   baseDir,
		upstream:  upstream,
	}
}

// RegisterUser 为用户创建独立池，加载已有凭证。
func (pm *PoolManager) RegisterUser(userID string) *pool.Pool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if p, ok := pm.pools[userID]; ok {
		return p
	}

	authDir := filepath.Join(pm.baseDir, "pools", userID)
	stateFile := filepath.Join(authDir, "state.json")
	_ = os.MkdirAll(authDir, 0o755)

	p := pool.New(stateFile)

	// 加载已有的 OAuth 凭证文件
	creds, err := cred.LoadDir(authDir)
	if err == nil {
		for _, c := range creds {
			c.EnsureMachineFingerprint()
			p.Add(c)
		}
		if len(creds) > 0 {
			p.SaveState()
		}
	}

	pm.pools[userID] = p
	pm.authDirs[userID] = authDir
	pm.stateDirs[userID] = stateFile
	return p
}

// GetPool 获取用户对应的池。
func (pm *PoolManager) GetPool(userID string) *pool.Pool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.pools[userID]
}

// GetUserAuthDir 获取用户的凭证目录。
func (pm *PoolManager) GetUserAuthDir(userID string) string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.authDirs[userID]
}

// RemoveUser 移除用户池。
func (pm *PoolManager) RemoveUser(userID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.pools, userID)
	delete(pm.authDirs, userID)
	delete(pm.stateDirs, userID)
}

// AllPools 返回所有池（用于管理统计）。
func (pm *PoolManager) AllPools() map[string]*pool.Pool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make(map[string]*pool.Pool, len(pm.pools))
	for k, v := range pm.pools {
		out[k] = v
	}
	return out
}

func generateID() string {
	return "u_" + randomHex(12)
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

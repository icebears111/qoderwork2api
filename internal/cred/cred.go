// Package cred 管理 OAuth 设备凭证（dt-/drt-）。
//
// 生命周期：
//   - dt-（device token）有效期 ~30 天，drt-（refresh）~1 年（每次刷新轮换）
//   - dt 临过期（<2h）→ 用 drt 走 POST /api/v1/deviceToken/refresh
//   - drt 也失效 → 报 AuthInvalidError，由 pool 禁用账号（需重新 OAuth 登录）
//   - 无 PAT 任何环节
//
// 字段沿用 DT/DRT 命名（上游 COSY 签名层直接引用），但语义已是 dt/drt：
// DT = 当前 Bearer token（dt-），DRT = refresh token（drt-）。
package cred

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cred 一个账号的凭证 + 运行时状态。
type Cred struct {
	UID      string
	Nickname string

	// DT = dt-（~30d），DRT = drt-（~1y rotating），DTExpiresAt = dt 过期时间
	DT          string
	DRT         string
	DTExpiresAt int64

	// COSY 持久指纹（跨重启保持，落 state.json）
	MachineID    string
	MachineToken string
	MachineType  string

	mu sync.Mutex
}

// Region 当前版本只支持 CN。
func (c *Cred) Region() string { return "cn" }

// AuthInvalidError 凭证已失效（drt refresh 失败）——需要重新 OAuth 登录。
type AuthInvalidError struct{ Msg string }

func (e *AuthInvalidError) Error() string { return "auth_invalid: " + e.Msg }

// IsAuthInvalid 判断错误是否凭证失效。
func IsAuthInvalid(err error) bool {
	var pe *AuthInvalidError
	return errors.As(err, &pe)
}

// LoadFile 从磁盘加载 OAuth auth 文件。
// 期望形态（OAuth device flow 落盘）：
//
//	{"auth":{"accessToken":"dt-...","refreshToken":"drt-...","expiresAt":<unix>,"domain":"qoder.com.cn"},
//	 "account":{"uid":"...","nickname":"..."}}
//
// 也兼容扁平形态：{"token":"dt-...","refresh_token":"drt-...","uid":"..."}
func LoadFile(path string) (*Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var c Cred
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"auth"`
			Account struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		c = Cred{
			DT:          n.Auth.AccessToken,
			DRT:         n.Auth.RefreshToken,
			DTExpiresAt: n.Auth.ExpiresAt,
			UID:         n.Account.UID,
			Nickname:    n.Account.Nickname,
		}
	} else {
		var f struct {
			Token        string `json:"token"`
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at_unix"`
			UID          string `json:"uid"`
			UserID       string `json:"user_id"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		c.DT = f.Token
		if c.DT == "" {
			c.DT = f.AccessToken
		}
		c.DRT = f.RefreshToken
		c.DTExpiresAt = f.ExpiresAt
		c.UID = f.UID
		if c.UID == "" {
			c.UID = f.UserID
		}
		c.Nickname = f.Nickname
	}
	if c.DT == "" && c.DRT == "" {
		return nil, fmt.Errorf("missing dt/drt in %s", path)
	}
	if c.UID == "" {
		// 兜底：从文件名 qoderwork-<uid>.json 提取
		base := filepath.Base(path)
		if strings.HasPrefix(base, "qoderwork-") && strings.HasSuffix(base, ".json") {
			c.UID = strings.TrimSuffix(strings.TrimPrefix(base, "qoderwork-"), ".json")
		}
	}
	if c.UID == "" {
		return nil, fmt.Errorf("missing uid in %s", path)
	}
	return &c, nil
}

// LoadDir 扫描目录下 qoderwork*.json，跳过解析失败的。
func LoadDir(dir string) ([]*Cred, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qoderwork*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Cred
	for _, f := range files {
		c, err := LoadFile(f)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// EnsureMachineFingerprint 生成（首次）或保持（后续）COSY 机器指纹。
// 与桌面端对齐：machineToken 用 RawURLEncoding（无 padding）。
func (c *Cred) EnsureMachineFingerprint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.MachineID == "" {
		c.MachineID = uuid4()
	}
	if c.MachineToken == "" {
		seed := []byte(uuid4() + uuid4())
		if len(seed) > 50 {
			seed = seed[:50]
		}
		c.MachineToken = base64.RawURLEncoding.EncodeToString(seed)
	}
	if c.MachineType == "" {
		c.MachineType = strings.ReplaceAll(uuid4(), "-", "")[:18]
	}
}

// EnsureDT 确保 c.DT(dt) 有效：未过期（>2h 余量）直接用；过期 → deviceToken/refresh；
// refresh 失败 → AuthInvalidError（调用方禁用账号，等重新 OAuth）。
// base 是 openapi 域（如 https://openapi.qoder.com.cn）。
func (c *Cred) EnsureDT(base string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().Unix()
	// dt 有效期 >2h → 直接用
	if c.DT != "" && c.DTExpiresAt-now > 7200 {
		return nil
	}
	// dt 过期（或从未设置过期时间但有 dt——先用一次，由上游 401 触发 refresh 路径）→ drt refresh
	if c.DRT == "" {
		return &AuthInvalidError{Msg: "no drt available"}
	}
	return c.refreshLocked(base)
}

func (c *Cred) refreshLocked(base string) error {
	body, _ := json.Marshal(map[string]string{"refresh_token": c.DRT})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/deviceToken/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &AuthInvalidError{Msg: fmt.Sprintf("deviceToken refresh http %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("deviceToken refresh http %d", resp.StatusCode)
	}
	var out struct {
		Token       string `json:"token"`
		DeviceToken string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt   string `json:"expires_at"`
		ExpiresIn   int64  `json:"expires_in"` // ms
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	c.DT = dt
	c.DRT = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		c.DTExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			c.DTExpiresAt = t.Unix()
		}
	}
	if c.DTExpiresAt == 0 {
		c.DTExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	return nil
}

// uuid4 / hexShort 工具。
func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hexShort(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

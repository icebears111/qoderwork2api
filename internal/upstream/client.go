// client.go QoderWork 业务 API 客户端（dt- Bearer，无签名）。
package upstream

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client 上游业务 API 客户端。
type Client struct {
	HTTP    *http.Client
	Base    string // https://openapi.qoder.com.cn
	Gateway string // https://gateway.qoder.com.cn
}

// New 生产默认。QoderWork gateway 对 HTTP/2 不友好（stream INTERNAL_ERROR），
// 强制 HTTP/1.1。
func New() *Client {
	return NewWithTimeout(180 * time.Second)
}

// NewWithTimeout 指定上游 HTTP 超时（config.timeout_seconds 注入）。
// 配置连接池：MaxIdleConnsPerHost=20（默认 2 太小，高并发时频繁 TLS 握手）。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &Client{
		HTTP:    &http.Client{Timeout: timeout, Transport: tr},
		Base:    "https://openapi.qoder.com.cn",
		Gateway: "https://gateway.qoder.com.cn",
	}
}

// NewWithBase 测试用：覆盖 base/gateway。
func NewWithBase(base, gateway string) *Client {
	c := New()
	c.Base = base
	c.Gateway = gateway
	return c
}

func (c *Client) get(path, dt string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func (c *Client) post(path, dt string, body []byte) (*http.Response, error) {
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func billingHeaders(req *http.Request, dt string) {
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// QuotaUsage 查询积分余额（基础 + 赠送聚合）。
func (c *Client) QuotaUsage(dt string) (remain int64, exceeded bool, err error) {
	resp, err := c.get("/api/v2/quota/usage", dt)
	if err != nil {
		return 0, false, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode >= 400 {
		return 0, false, fmt.Errorf("quota http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var q struct {
		UserQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
		IsQuotaExceeded bool `json:"isQuotaExceeded"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, false, fmt.Errorf("quota parse: %w", err)
	}
	remain = int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining)
	return remain, q.IsQuotaExceeded, nil
}

// DailyCheckin 执行签到。true=本次成功；false=已签/失败。
func (c *Client) DailyCheckin(dt string) (bool, error) {
	resp, err := c.post("/sash/api/v1/me/daily-check-in/claim", dt, nil)
	if err != nil {
		return false, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("checkin http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, fmt.Errorf("checkin parse: %w", err)
	}
	if success, ok := m["success"].(bool); ok && success {
		return true, nil
	}
	errMsg, _ := m["error"].(string)
	if errMsg == "" {
		errMsg = "unknown"
	}
	return false, fmt.Errorf("%s", errMsg)
}

// CheckinStatus 签到状态。
type CheckinStatus struct {
	Status             string `json:"status"` // CLAIMABLE | CLAIMED
	RewardCredits      int64  `json:"rewardCredits"`
	NextClaimAt        int64  `json:"nextClaimAt"`
	CurrentStreakDays  int64  `json:"currentStreakDays"`
	TotalClaimDays     int64  `json:"totalClaimDays"`
	TotalRewardCredits int64  `json:"totalRewardCredits"`
	LastClaimedAt      int64  `json:"lastClaimedAt"`
	RewardExpiresAt    int64  `json:"rewardExpiresAt"`
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(dt string) (*CheckinStatus, error) {
	resp, err := c.get("/sash/api/v1/me/daily-check-in/status", dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkin status http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var st CheckinStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("checkin status parse: %w", err)
	}
	return &st, nil
}

// UserPlan 套餐名。
func (c *Client) UserPlan(dt string) (string, error) {
	resp, err := c.get("/api/v2/user/plan", dt)
	if err != nil {
		return "", err
	}
	raw, err := readBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("plan http %d", resp.StatusCode)
	}
	var p struct {
		UserType     string `json:"user_type"`
		PlanTierName string `json:"plan_tier_name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.PlanTierName != "" {
		return p.PlanTierName, nil
	}
	return p.UserType, nil
}

// UserInfo 用户信息。
type UserInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// UserInfo 查询用户信息。
func (c *Client) UserInfo(dt string) (*UserInfo, error) {
	resp, err := c.get("/api/v1/userinfo", dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo http %d", resp.StatusCode)
	}
	var u UserInfo
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

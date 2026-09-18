// classify.go 错误分类：驱动 pool 冷却状态机。
package upstream

import (
	"errors"
	"fmt"
	"strings"
)

// ErrKind 错误类别。
type ErrKind int

const (
	ErrNone ErrKind = iota
	ErrHardCredit   // 余额不足（402 / 关键词 / isQuotaExceeded）
	ErrSoftRate     // 429 软限流
	ErrTokenExpired // dt- 过期（401 TOKEN_EXPIRE）→ 触发 refresh 重试
	ErrSessionDead  // 凭证失效 → 禁用（需重新 OAuth 登录）
	ErrNotFound     // 404 上游偶发 → 短冷却不累计 errCount（防雪崩）
	ErrServer       // 5xx
	ErrClient       // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrTokenExpired:
		return "token_expired"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 实现 error 接口，让 ErrKind 可作 errors.Is 的 target。
func (k ErrKind) Error() string { return k.String() }

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Is 让 errors.Is(err, ErrKind) 可用。
func (e *Error) Is(target error) bool {
	if k, ok := target.(ErrKind); ok {
		return e.Kind == k
	}
	return false
}

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == 402 {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	// TOKEN_EXPIRE 优先于通用 401
	if status == 401 && strings.Contains(body, "TOKEN_EXPIRE") {
		return ErrTokenExpired
	}
	if status == 429 {
		return ErrSoftRate
	}
	if status == 404 {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// IsKind 工具函数。
func IsKind(err error, kind ErrKind) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Kind == kind
	}
	return false
}

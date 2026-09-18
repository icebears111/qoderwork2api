// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化（含 COSY 机器指纹）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"qoderwork2api/internal/cred"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

// Status 单账号对外状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Credits  int64     `json:"credits"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
}

type entry struct {
	c        *cred.Cred
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

type stateFile struct {
	Accounts map[string]struct {
		Credits      int64     `json:"credits"`
		Disabled     bool      `json:"disabled"`
		Reason       string    `json:"reason,omitempty"`
		Until        time.Time `json:"until,omitempty"`
		MachineID    string    `json:"machine_id,omitempty"`
		MachineToken string    `json:"machine_token,omitempty"`
		MachineType  string    `json:"machine_type,omitempty"`
	} `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建；stateFp 非空时加载。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留冷却/积分/指纹状态，仅更新凭证/Nickname。
func (p *Pool) Add(c *cred.Cred) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[c.UID]; ok {
		// 保留旧 entry 的冷却/积分状态 + 已生成的指纹
		old := e.c
		if old != nil && old.MachineID != "" {
			c.MachineID = old.MachineID
			c.MachineToken = old.MachineToken
			c.MachineType = old.MachineType
		}
		e.c = c
		return
	}
	p.byUID[c.UID] = &entry{c: c}
}

// SyncToDir 对齐目录扫描结果：新账号加入、消失的剔除（状态保留在 state.json）。
func (p *Pool) SyncToDir(creds []*cred.Cred) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range creds {
		seen[c.UID] = true
		if e, ok := p.byUID[c.UID]; ok {
			if e.c != nil && e.c.MachineID != "" {
				c.MachineID = e.c.MachineID
				c.MachineToken = e.c.MachineToken
				c.MachineType = e.c.MachineType
			}
			e.c = c
		} else {
			p.byUID[c.UID] = &entry{c: c}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号。
func (p *Pool) Pick() *cred.Cred { return p.PickExcluding(nil) }

// PickExcluding 同上，跳过 tried。
func (p *Pool) PickExcluding(tried map[string]bool) *cred.Cred {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.c
}

// SetCredits 更新积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（凭证失效，需重新 OAuth 登录）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻（仅当 remain>0 且未禁用）。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteError 记录错误；达阈值自动冷却。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回完整凭证（给 scheduler 用）。
func (p *Pool) AuthByUID(uid string) *cred.Cred {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.c
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	nick := ""
	if e.c != nil {
		nick = e.c.Nickname
	}
	return Status{
		UID:      uid,
		Nickname: nick,
		Credits:  e.credits,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
	}
}

// SaveState 显式保存（机器指纹等字段更新后调）。
func (p *Pool) SaveState() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			c: &cred.Cred{
				UID:          uid,
				MachineID:    s.MachineID,
				MachineToken: s.MachineToken,
				MachineType:  s.MachineType,
			},
			credits:  s.Credits,
			disabled: s.Disabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Credits      int64     `json:"credits"`
		Disabled     bool      `json:"disabled"`
		Reason       string    `json:"reason,omitempty"`
		Until        time.Time `json:"until,omitempty"`
		MachineID    string    `json:"machine_id,omitempty"`
		MachineToken string    `json:"machine_token,omitempty"`
		MachineType  string    `json:"machine_type,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		var mid, mt, mty string
		if e.c != nil {
			mid, mt, mty = e.c.MachineID, e.c.MachineToken, e.c.MachineType
		}
		sf.Accounts[uid] = struct {
			Credits      int64     `json:"credits"`
			Disabled     bool      `json:"disabled"`
			Reason       string    `json:"reason,omitempty"`
			Until        time.Time `json:"until,omitempty"`
			MachineID    string    `json:"machine_id,omitempty"`
			MachineToken string    `json:"machine_token,omitempty"`
			MachineType  string    `json:"machine_type,omitempty"`
		}{
			Credits:      e.credits,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			MachineID:    mid,
			MachineToken: mt,
			MachineType:  mty,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}

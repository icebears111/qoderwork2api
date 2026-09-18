package scheduler

import (
	"context"
	"log"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int
	KeepaliveHours []int
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{0, 12}
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.CheckinHours...), s.cfg.KeepaliveHours...)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.CheckinHours, h) {
				s.RunCheckinNow()
			}
			if contains(s.cfg.KeepaliveHours, h) {
				s.RunKeepaliveNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有非禁用账号签到 + 刷 quota + 解冻。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || (a.DT == "" && a.DRT == "") {
			continue
		}
		if err := a.EnsureDT(s.cfg.Upstream.Base); err != nil {
			log.Printf("checkin %s ensure dt: %v", st.UID, err)
			if cred.IsAuthInvalid(err) {
				s.cfg.Pool.Disable(st.UID, "auth invalid (re-login required)")
			}
			continue
		}
		if _, err := s.cfg.Upstream.DailyCheckin(a.DT); err != nil {
			log.Printf("checkin %s: %v", st.UID, err)
			// 已签到等业务错误也继续走 quota 查询
		}
		remain, exceeded, err := s.cfg.Upstream.QuotaUsage(a.DT)
		if err != nil {
			log.Printf("quota %s: %v", st.UID, err)
			continue
		}
		if exceeded {
			s.cfg.Pool.Cooldown(st.UID, pool.CoolHard, 12*time.Hour, "isQuotaExceeded")
		} else {
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		}
	}
}

// RunKeepaliveNow 立即对所有非禁用账号 EnsureDT（自动 refresh/exchange）。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || (a.DT == "" && a.DRT == "") {
			continue
		}
		if err := a.EnsureDT(s.cfg.Upstream.Base); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			if cred.IsAuthInvalid(err) {
				s.cfg.Pool.Disable(st.UID, "auth invalid (re-login required)")
			}
			continue
		}
		a.EnsureMachineFingerprint()
	}
	s.cfg.Pool.SaveState()
}

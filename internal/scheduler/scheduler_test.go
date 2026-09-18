package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
)

// validCred 测试用有效凭证（dt 10h 内不过期，EnsureDT 不会触发网络）。
func validCred(uid string) *cred.Cred {
	return &cred.Cred{
		UID: uid, DT: "dt-" + uid, DRT: "drt-" + uid,
		DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(),
	}
}

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v", next)
	}
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	var checkinCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/claim"):
			checkinCalls.Add(1)
			w.Write([]byte(`{"success":true,"rewardCredits":100}`))
		case strings.HasSuffix(r.URL.Path, "/quota/usage"):
			w.Write([]byte(`{"userQuota":{"remaining":500},"addOnQuota":{"remaining":0},"isQuotaExceeded":false}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(validCred("u1"))
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	up := upstream.NewWithBase(srv.URL, srv.URL)
	s := New(Config{Pool: p, Upstream: up})
	s.RunCheckinNow()
	if checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("should reenable: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d", st.Credits)
	}
}

func TestRunCheckinQuotaExceededCools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-check-in/claim"):
			w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/quota/usage"):
			w.Write([]byte(`{"userQuota":{"remaining":0},"addOnQuota":{"remaining":0},"isQuotaExceeded":true}`))
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(validCred("u1"))
	up := upstream.NewWithBase(srv.URL, srv.URL)
	s := New(Config{Pool: p, Upstream: up})
	s.RunCheckinNow()
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("isQuotaExceeded should cool: %+v", st)
	}
}

func TestRunKeepaliveAuthInvalidDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"TOKEN_EXPIRE"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-bad", DRT: "drt-bad", DTExpiresAt: 1})
	up := upstream.NewWithBase(srv.URL, srv.URL)
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable: %+v", st)
	}
}

func TestRunKeepaliveGeneratesFingerprint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"dt-1","refresh_token":"drt-1","expires_in":86400000,"refresh_token_expires_in":172800000}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(validCred("u1"))
	up := upstream.NewWithBase(srv.URL, srv.URL)
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	a := p.AuthByUID("u1")
	if a.MachineID == "" || a.MachineToken == "" || a.MachineType == "" {
		t.Errorf("fingerprint not generated: %+v", a)
	}
}

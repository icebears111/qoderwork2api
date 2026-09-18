package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuotaAggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dt-x" {
			t.Errorf("bad bearer: %q", r.Header.Get("Authorization"))
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v2/quota/usage") {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Write([]byte(`{"userQuota":{"total":2000,"used":800,"remaining":1200},"addOnQuota":{"total":1000,"used":300,"remaining":700},"isQuotaExceeded":false}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	remain, exceeded, err := c.QuotaUsage("dt-x")
	if err != nil || remain != 1900 || exceeded {
		t.Errorf("remain=%d exceeded=%v err=%v", remain, exceeded, err)
	}
}

func TestQuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"userQuota":{"remaining":0},"addOnQuota":{"remaining":0},"isQuotaExceeded":true}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	remain, exceeded, _ := c.QuotaUsage("dt-x")
	if remain != 0 || !exceeded {
		t.Errorf("remain=%d exceeded=%v", remain, exceeded)
	}
}

func TestDailyCheckinSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST")
		}
		w.Write([]byte(`{"success":true,"rewardCredits":100}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	ok, err := c.DailyCheckin("dt-x")
	if err != nil || !ok {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestDailyCheckinAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error":"already claimed"}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	ok, err := c.DailyCheckin("dt-x")
	if ok || err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestCheckinStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"CLAIMABLE","rewardCredits":100,"currentStreakDays":3,"totalClaimDays":12,"totalRewardCredits":1200}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	st, err := c.CheckinStatus("dt-x")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "CLAIMABLE" || st.RewardCredits != 100 || st.CurrentStreakDays != 3 {
		t.Errorf("st=%+v", st)
	}
}

func TestUserPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"user_type":"trial","plan_tier_name":"Pro Trial"}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	name, err := c.UserPlan("dt-x")
	if err != nil || name != "Pro Trial" {
		t.Errorf("name=%q err=%v", name, err)
	}
}

func TestUserInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"u1","name":"nick","username":"alt"}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	info, err := c.UserInfo("dt-x")
	if err != nil || info.ID != "u1" || info.Name != "nick" {
		t.Errorf("info=%+v err=%v", info, err)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, srv.URL)
	if _, _, err := c.QuotaUsage("dt-x"); err == nil {
		t.Error("want error for 500")
	}
}

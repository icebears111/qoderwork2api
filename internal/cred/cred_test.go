package cred

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadNestedOAuthFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "qoderwork-u1.json")
	os.WriteFile(fp, []byte(`{"auth":{"accessToken":"dt-x","refreshToken":"drt-x","expiresAt":123},"account":{"uid":"u1","nickname":"n1"}}`), 0o600)
	c, err := LoadFile(fp)
	if err != nil || c.DT != "dt-x" || c.DRT != "drt-x" || c.DTExpiresAt != 123 || c.UID != "u1" || c.Nickname != "n1" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadFlatOAuthFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "qoderwork-u2.json")
	os.WriteFile(fp, []byte(`{"token":"dt-y","refresh_token":"drt-y","uid":"u2","nickname":"n2"}`), 0o600)
	c, err := LoadFile(fp)
	if err != nil || c.DT != "dt-y" || c.DRT != "drt-y" || c.UID != "u2" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadMissingTokens(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "qoderwork-u3.json")
	os.WriteFile(fp, []byte(`{"auth":{},"account":{"uid":"u3"}}`), 0o600)
	if _, err := LoadFile(fp); err == nil {
		t.Fatal("want error for missing dt/drt")
	}
}

func TestLoadUIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "qoderwork-019f8417-aaaa.json")
	os.WriteFile(fp, []byte(`{"token":"dt-z","refresh_token":"drt-z"}`), 0o600)
	c, err := LoadFile(fp)
	if err != nil || c.UID != "019f8417-aaaa" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "qoderwork-a.json"), []byte(`{"token":"dt-1","refresh_token":"drt-1","uid":"a"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "qoderwork-b.json"), []byte(`{"token":"dt-2","refresh_token":"drt-2","uid":"b"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"foo":1}`), 0o600)
	os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"token":"dt-3","uid":"c"}`), 0o600) // 不匹配前缀
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
}

// mockRefreshAPI 起本地 deviceToken/refresh mock。
func mockRefreshAPI(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/deviceToken/refresh" {
			calls.Add(1)
			fn(w, r)
			return
		}
		w.WriteHeader(404)
	}))
	return srv, calls
}

func TestEnsureDTValid(t *testing.T) {
	srv, calls := mockRefreshAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("refresh should not be called for valid dt")
	})
	defer srv.Close()

	c := &Cred{
		UID: "u1", DT: "dt-valid", DRT: "drt-valid",
		DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(),
	}
	if err := c.EnsureDT(srv.URL); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Errorf("unexpected refresh call")
	}
}

func TestEnsureDTRefresh(t *testing.T) {
	srv, calls := mockRefreshAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "drt-1" {
			t.Errorf("refresh body=%v", body)
		}
		w.Write([]byte(`{"token":"dt-2","refresh_token":"drt-2","expires_in":2592000000}`))
	})
	defer srv.Close()

	c := &Cred{
		UID: "u1", DT: "dt-1", DRT: "drt-1",
		DTExpiresAt: time.Now().Add(-time.Hour).Unix(), // 已过期
	}
	if err := c.EnsureDT(srv.URL); err != nil {
		t.Fatal(err)
	}
	if c.DT != "dt-2" || c.DRT != "drt-2" {
		t.Errorf("c=%+v", c)
	}
	if calls.Load() != 1 {
		t.Errorf("calls=%d", calls.Load())
	}
	if c.DTExpiresAt <= time.Now().Unix() {
		t.Errorf("DTExpiresAt not set: %d", c.DTExpiresAt)
	}
}

func TestEnsureDTRefreshDeviceTokenKey(t *testing.T) {
	// refresh 响应用 device_token 字段（而非 token）
	srv, _ := mockRefreshAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"device_token":"dt-new","refresh_token":"drt-new","expires_in":2592000000}`))
	})
	defer srv.Close()

	c := &Cred{UID: "u1", DT: "dt-old", DRT: "drt-old", DTExpiresAt: 1}
	if err := c.EnsureDT(srv.URL); err != nil {
		t.Fatal(err)
	}
	if c.DT != "dt-new" || c.DRT != "drt-new" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnsureDTRefreshInvalid(t *testing.T) {
	srv, _ := mockRefreshAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	})
	defer srv.Close()

	c := &Cred{UID: "u1", DT: "dt-old", DRT: "drt-bad", DTExpiresAt: 1}
	err := c.EnsureDT(srv.URL)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsAuthInvalid(err) {
		t.Errorf("err not AuthInvalid: %v", err)
	}
}

func TestEnsureDTNoDRT(t *testing.T) {
	c := &Cred{UID: "u1", DT: "dt-old", DTExpiresAt: 1}
	err := c.EnsureDT("http://unused")
	if !IsAuthInvalid(err) {
		t.Errorf("want AuthInvalid for missing drt, got %v", err)
	}
}

func TestMachineFingerprintGenerated(t *testing.T) {
	c := &Cred{UID: "u1"}
	c.EnsureMachineFingerprint()
	if c.MachineID == "" || c.MachineToken == "" || c.MachineType == "" {
		t.Error("fingerprint not generated")
	}
	// 再调一次不变
	mid, mt, mty := c.MachineID, c.MachineToken, c.MachineType
	c.EnsureMachineFingerprint()
	if c.MachineID != mid || c.MachineToken != mt || c.MachineType != mty {
		t.Error("fingerprint should be stable")
	}
}

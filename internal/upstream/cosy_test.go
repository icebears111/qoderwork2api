package upstream

import (
	"net/http"
	"strings"
	"testing"

	"qoderwork2api/internal/cred"
)

func TestNewCosySessionFields(t *testing.T) {
	c := &cred.Cred{
		DT: "dt-x", UID: "u1", Nickname: "n1",
		MachineID: "019f8417-mid", MachineToken: "mtoken", MachineType: "mtype",
	}
	s, err := NewCosySession(c, "dt-tok", "drt-tok")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if s.CosyKey == "" {
		t.Error("CosyKey empty")
	}
	if s.Info == "" {
		t.Error("Info empty")
	}
	if len(s.TempKey) != 16 {
		t.Errorf("TempKey len=%d want 16", len(s.TempKey))
	}
}

func TestSignHeaderFormat(t *testing.T) {
	c := &cred.Cred{
		DT: "dt-x", UID: "u1", Nickname: "n1",
		MachineID: "mid", MachineToken: "mt", MachineType: "mty",
	}
	s, err := NewCosySession(c, "dt-tok", "drt-tok")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := s.AuthHeader("test-body", "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=x&AgentId=y&Encode=1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("auth prefix wrong: %q", auth)
	}
	rest := strings.TrimPrefix(auth, "Bearer COSY.")
	dotIdx := strings.LastIndex(rest, ".")
	if dotIdx <= 0 {
		t.Errorf("want payload.sig, got %q", rest)
	}
	sig := rest[dotIdx+1:]
	if len(sig) != 32 {
		t.Errorf("md5 hex sig len=%d want 32", len(sig))
	}
}

func TestApplyHeaders(t *testing.T) {
	c := &cred.Cred{
		DT: "dt-x", UID: "u1", Nickname: "n1",
		MachineID: "mid", MachineToken: "mt", MachineType: "mty",
	}
	s, err := NewCosySession(c, "dt-tok", "drt-tok")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "https://gateway.qoder.com.cn/algo/x", nil)
	err = s.ApplyHeaders(req, "body", "https://gateway.qoder.com.cn/algo/x", "u1", true, "qmodel_preview")
	if err != nil {
		t.Fatal(err)
	}
	required := []string{
		"cosy-data-policy", "content-type", "cosy-machinetype", "cosy-clienttype",
		"cosy-date", "cosy-user", "cosy-key", "accept", "cosy-clientip",
		"authorization", "accept-encoding", "cosy-version", "cosy-machineid",
		"cosy-machinetoken", "login-version",
	}
	for _, h := range required {
		if req.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if req.Header.Get("x-model-key") != "qmodel_preview" {
		t.Errorf("x-model-key=%q", req.Header.Get("x-model-key"))
	}
	if req.Header.Get("x-model-source") != "system" {
		t.Errorf("x-model-source=%q", req.Header.Get("x-model-source"))
	}
	if req.Header.Get("cache-control") != "no-cache" {
		t.Errorf("sse should set cache-control")
	}
	if req.Header.Get("cosy-user") != "u1" {
		t.Errorf("cosy-user=%q", req.Header.Get("cosy-user"))
	}
	if req.Header.Get("authorization") == "" || !strings.HasPrefix(req.Header.Get("authorization"), "Bearer COSY.") {
		t.Errorf("authorization=%q", req.Header.Get("authorization"))
	}
}

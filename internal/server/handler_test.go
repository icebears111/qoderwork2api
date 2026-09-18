package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
)

func newTestHandler(p *pool.Pool, up *upstream.Client) *Handler {
	return NewHandler(Config{
		Pool:     p,
		Upstream: up,
	})
}

func TestModelsEndpoint(t *testing.T) {
	// 无账号 → 回退静态表
	h := newTestHandler(pool.New(""), upstream.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("static fallback failed: %d models", len(data))
	}
}

// fakeModelsGateway mock 上游模型接口 + chat。
func fakeModelsGateway(t *testing.T, modelsJSON string, chatFn func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/model/list") {
			w.Write([]byte(modelsJSON))
			return
		}
		if strings.Contains(r.URL.Path, "/agent_chat_generation") && chatFn != nil {
			chatFn(w, r)
			return
		}
		w.WriteHeader(404)
	}))
}

const fakeModelsJSON = `{"chat":[
  {"key":"dfmodel","display_name":"DeepSeek-V4-Flash","enable":true,"is_reasoning":false,"is_vl":true,"max_input_tokens":180000,"price_factor":0.1},
  {"key":"qmodel_preview","display_name":"Qwen3.8-Max-Preview","enable":true,"is_reasoning":true,"is_vl":true,"max_input_tokens":180000,"price_factor":0.05},
  {"key":"dmodel","display_name":"DeepSeek-V4-Pro","enable":true,"is_reasoning":true,"is_vl":true,"max_input_tokens":180000,"price_factor":0.5}
]}`

func TestModelsDynamicFromUpstream(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, nil)
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-fake", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	up := upstream.NewWithBase(srv.URL, srv.URL)
	h := newTestHandler(p, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 models, got %d: %v", len(data), data)
	}
	ids := map[string]map[string]any{}
	for _, m := range data {
		mm := m.(map[string]any)
		ids[mm["id"].(string)] = mm
	}
	// display_name 规范化作客户端名
	if _, ok := ids["deepseek-v4-flash"]; !ok {
		t.Errorf("deepseek-v4-flash missing: %v", ids)
	}
	if _, ok := ids["qwen3.8-max-preview"]; !ok {
		t.Errorf("qwen3.8-max-preview missing: %v", ids)
	}
	// 元数据透传
	if m := ids["deepseek-v4-flash"]; m["upstream_key"] != "dfmodel" || m["vision"] != true {
		t.Errorf("meta wrong: %+v", m)
	}
	if m := ids["qwen3.8-max-preview"]; m["reasoning"] != true || m["context_length"].(float64) != 180000 {
		t.Errorf("meta wrong: %+v", m)
	}
}

func TestHealthz(t *testing.T) {
	h := newTestHandler(pool.New(""), upstream.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, nil)
	defer srv.Close()
	p := pool.New("")
	h := NewHandler(Config{Pool: p, Upstream: upstream.NewWithBase(srv.URL, srv.URL), APIKey: "secret"})
	// 无 key
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)))
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 对 key（会因无账号而 503，但不是 401）
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code == 401 {
		t.Errorf("right key should pass auth")
	}
}

func TestUnknownModelReturns400(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, nil)
	defer srv.Close()
	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-fake", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	h := newTestHandler(p, upstream.NewWithBase(srv.URL, srv.URL))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"not-exist","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestNoAccountReturns503(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, nil)
	defer srv.Close()
	h := newTestHandler(pool.New(""), upstream.NewWithBase(srv.URL, srv.URL))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)))
	// 无账号 → 先确认健康账号失败 → 503
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1", Nickname: "nick"})
	p.SetCredits("u1", 42)
	h := newTestHandler(p, upstream.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "pt-1") {
		t.Error("token leaked in status")
	}
}

const fakeSSEOK = "data:{\"body\":\"{\\\"id\\\":\\\"chatcmpl-1\\\",\\\"created\\\":1753600000,\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"你好\\\"}}]}\"}\n\n" +
	"data:{\"body\":\"{\\\"id\\\":\\\"chatcmpl-1\\\",\\\"created\\\":1753600000,\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}],\\\"usage\\\":{\\\"prompt_tokens\\\":1,\\\"completion_tokens\\\":1,\\\"total_tokens\\\":2}}\"}\n\n" +
	"data:{\"body\":\"[DONE]\"}\n\nevent:finish\ndata:{\"firstTokenDuration\":100}\n\n"

func TestChatNonStreamAggregates(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fakeSSEOK)
	})
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-fake", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	up := upstream.NewWithBase(srv.URL, srv.URL)
	h := newTestHandler(p, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "deepseek-v4-flash" {
		t.Errorf("model=%v (should be client display_name)", resp["model"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	srv := fakeModelsGateway(t, fakeModelsJSON, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fakeSSEOK)
	})
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-fake", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	up := upstream.NewWithBase(srv.URL, srv.URL)
	h := newTestHandler(p, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	// 转写后是标准 OpenAI SSE（每行 data: 开头）
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("bad line: %q", ln)
		}
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	srv := fakeModelsGateway(t, fakeModelsJSON, func(w http.ResponseWriter, r *http.Request) {
		calls["chat"]++
		if calls["chat"] == 1 {
			w.WriteHeader(402)
			w.Write([]byte(`{"error":"余额不足"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, fakeSSEOK)
	})
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	p.Add(&cred.Cred{UID: "u2", DT: "dt-2", DTExpiresAt: time.Now().Add(10 * time.Hour).Unix(), MachineID: "m", MachineToken: "t", MachineType: "y"})
	p.SetCredits("u1", 2000)
	p.SetCredits("u2", 1000)
	up := upstream.NewWithBase(srv.URL, srv.URL)
	h := newTestHandler(p, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("u1 should be cooling: %+v", st)
	}
}

func TestChatAuthInvalidDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/model/list") {
			w.Write([]byte(fakeModelsJSON))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-bad"})
	up := upstream.NewWithBase(srv.URL, srv.URL)
	h := newTestHandler(p, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)))
	// 凭证无效 → effectiveModelMap 内部 EnsureDT 失败禁用账号 → 但模型回退静态表 →
	// handler 先 Pool.Pick() 健康 → 走 chat 流程 EnsureDT 再失败 → 503
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("u1 should be disabled: %+v", st)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"qoderwork2api/internal/modelstate"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/upstream"
)

// 带模型状态表的 handler。走真实 NewHandler，因此路由也是真的。
func newStatefulHandler(t *testing.T) (*Handler, *modelstate.Store) {
	t.Helper()
	store := modelstate.NewStore(filepath.Join(t.TempDir(), "models.json"))
	h := NewHandler(Config{
		Pool:       pool.New(""),
		Upstream:   upstream.New(),
		APIKey:     testAdminKey,
		ModelState: store,
	})
	return h, store
}

// 测试用管理员 key。/v1/models 与 /status 都在 withAuth 之后，
// 不带 key 会先撞 401 —— 那样测的就不是模型过滤了。
const testAdminKey = "test-admin-key"

// authed 发一个带管理员 key 的请求。
func authed(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	return r
}

func modelIDs(t *testing.T, h *Handler) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed("GET", "/v1/models", ""))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models code=%d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range resp.Data {
		out[m.ID] = true
	}
	return out
}

// 被禁用的模型要从 /v1/models 消失。
func TestQoderDisabledModelHidden(t *testing.T) {
	h, store := newStatefulHandler(t)
	before := modelIDs(t, h)
	if len(before) == 0 {
		t.Fatal("need a non-empty fallback list")
	}

	// 从兜底表里挑一个真实的模型名，避免写死
	var target string
	for id := range before {
		if id != "auto" {
			target = id
			break
		}
	}
	if target == "" {
		t.Skip("no suitable model in fallback list")
	}

	store.SetDisabled(target, true)
	after := modelIDs(t, h)
	if after[target] {
		t.Fatalf("disabled model %q must not be listed", target)
	}
	if len(after) != len(before)-1 {
		t.Fatalf("want %d models, got %d", len(before)-1, len(after))
	}
}

// 关掉开关要能恢复。
func TestQoderReEnableRestores(t *testing.T) {
	h, store := newStatefulHandler(t)
	before := modelIDs(t, h)
	var target string
	for id := range before {
		if id != "auto" {
			target = id
			break
		}
	}
	if target == "" {
		t.Skip("no suitable model")
	}
	store.SetDisabled(target, true)
	store.SetDisabled(target, false)
	if !modelIDs(t, h)[target] {
		t.Fatalf("re-enabled model %q should be listed again", target)
	}
}

// /status 必须带**含禁用项**的清单 —— 看板据此渲染开关，
// 过滤掉的话被禁用的模型在页面上消失，就没法恢复了。
func TestQoderStatusIncludesDisabled(t *testing.T) {
	h, store := newStatefulHandler(t)
	store.SetDisabled("qwen3.7-max", true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed("GET", "/status", ""))
	if rec.Code != 200 {
		t.Fatalf("GET /status code=%d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var seen, enabled bool
	for _, m := range resp.Models {
		if m.ID == "qwen3.7-max" {
			seen, enabled = true, m.Enabled
		}
	}
	if !seen {
		t.Fatal("/status must still include disabled models")
	}
	if enabled {
		t.Fatal("disabled model must be reported enabled=false")
	}
}

// 管理接口：停用一个模型，清单立即变化。
func TestQoderAdminSetModelState(t *testing.T) {
	h, _ := newStatefulHandler(t)
	body := `{"id":"qwen3.7-plus","enabled":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed("POST", "/admin/api/models/state", body))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if modelIDs(t, h)["qwen3.7-plus"] {
		t.Fatal("model should be hidden after admin disable")
	}
}

// 入参校验。
func TestQoderAdminValidation(t *testing.T) {
	h, _ := newStatefulHandler(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{`, 400},
		{"missing id", `{"enabled":true}`, 400},
		{"blank id", `{"id":" ","enabled":true}`, 400},
		{"missing enabled", `{"id":"auto"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authed("POST", "/admin/api/models/state", tc.body))
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d (%s)", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// 未配置状态表时管理接口明确 503。
func TestQoderAdminWithoutStore(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: testAdminKey})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed("POST", "/admin/api/models/state", `{"id":"auto","enabled":false}`))
	if rec.Code != 503 {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

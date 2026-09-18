package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildBodyForcesStreamAndModel(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "你好"}}
	body, err := BuildAgentBody(msgs, "qmodel_preview", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("built body not json: %v", err)
	}
	if m["stream"] != true {
		t.Error("stream not forced")
	}
	if mc, ok := m["model_config"].(map[string]any); !ok || mc["key"] != "qmodel_preview" {
		t.Errorf("model_config=%v", m["model_config"])
	}
	cc := m["chat_context"].(map[string]any)
	extra := cc["extra"].(map[string]any)
	if mc2, ok := extra["modelConfig"].(map[string]any); !ok || mc2["key"] != "qmodel_preview" {
		t.Errorf("extra.modelConfig=%v", extra["modelConfig"])
	}
	if txt, ok := cc["text"].(map[string]any); !ok || txt["text"] != "你好" {
		t.Errorf("chat_context.text=%v", cc["text"])
	}
	if orig, ok := extra["originalContent"].(map[string]any); !ok || orig["text"] != "你好" {
		t.Errorf("originalContent=%v", extra["originalContent"])
	}
	// messages = 仅客户端消息（模板 system 已丢弃，测试模式）
	msgs2, ok := m["messages"].([]any)
	if !ok || len(msgs2) != 1 {
		t.Fatalf("messages len=%d (want 1: user only)", len(msgs2))
	}
	last := msgs2[len(msgs2)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != "你好" {
		t.Errorf("last msg=%v", last)
	}
	// tools 未传时应从 body 删除（测试模式）
	if _, present := m["tools"]; present {
		t.Errorf("tools should be absent when client didn't pass any")
	}
	// request_id 每次不同
	body2, _ := BuildAgentBody(msgs, "qmodel_preview", nil, "")
	var m2 map[string]any
	json.Unmarshal(body2, &m2)
	if m["request_id"] == m2["request_id"] {
		t.Error("request_id should differ per call")
	}
	if m["session_id"] == "" {
		t.Error("session_id missing")
	}
	if m["business"].(map[string]any)["name"] != "你好" {
		t.Errorf("business.name=%v", m["business"])
	}
}

// 思考模式：2026-09-19 起所有模型统一开启，与 qwen3.8-flash 同一链路：
// is_reasoning=true + source="system"（缺 source 时上游不下发 reasoning_content，2026-09-18 实测）。
func TestBuildBodyReasoningWhitelist(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "你好"}}
	check := func(modelKey string, want bool) {
		t.Helper()
		body, err := BuildAgentBody(msgs, modelKey, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("built body not json: %v", err)
		}
		mc := m["model_config"].(map[string]any)
		if mc["is_reasoning"] != want {
			t.Errorf("%s: model_config.is_reasoning=%v (want %v)", modelKey, mc["is_reasoning"], want)
		}
		_, hasSource := mc["source"]
		if want && !hasSource {
			t.Errorf("%s: model_config.source missing (reasoning requires source=system)", modelKey)
		}
		if want && mc["source"] != "system" {
			t.Errorf("%s: model_config.source=%v (want system)", modelKey, mc["source"])
		}
		if !want && hasSource {
			t.Errorf("%s: model_config.source should be absent (got %v)", modelKey, mc["source"])
		}
		extra := m["chat_context"].(map[string]any)["extra"].(map[string]any)
		mc2 := extra["modelConfig"].(map[string]any)
		if mc2["is_reasoning"] != want {
			t.Errorf("%s: extra.modelConfig.is_reasoning=%v (want %v)", modelKey, mc2["is_reasoning"], want)
		}
	}
	// 全部模型统一开思考（2026-09-19 按用户要求放开）
	check("qfmodel", true)         // qwen3.8-flash
	check("gmodel", true)          // glm-5.3
	check("qmodel_preview", true)  // qwen3.8-max-preview
	check("qmodel_latest", true)   // qwen3.7-max
	check("dmodel", true)          // deepseek-v4-pro
	check("gm51model", true)       // glm-5.2
	check("mmodel", true)          // minimax-m2.7
	check("kmodel", true)          // kimi-k2.7-code
	// 空 key 是异常输入，保持 false
	check("", false)
}

// 思考档位透传：所有模型 + 合法档位时注入 parameters；
// 默认（空）/非法值都不注入（走上游默认 medium）。
func TestBuildBodyReasoningEffortPassthrough(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "你好"}}

	// build 每次新建 map（json.Unmarshal 到复用 map 会保留旧键，勿复用）
	build := func(modelKey, effort string) map[string]any {
		t.Helper()
		body, err := BuildAgentBody(msgs, modelKey, nil, effort)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("built body not json: %v", err)
		}
		return m
	}

	// qfmodel + xhigh → parameters 注入
	m := build("qfmodel", "xhigh")
	params, ok := m["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing for qfmodel+xhigh")
	}
	if params["enable_thinking"] != true || params["reasoning_effort"] != "xhigh" {
		t.Errorf("parameters=%v", params)
	}

	// 其它模型 + xhigh → 同样注入（2026-09-19 起统一）
	for _, mk := range []string{"gmodel", "dmodel", "qmodel_latest"} {
		m2 := build(mk, "xhigh")
		p2, ok := m2["parameters"].(map[string]any)
		if !ok {
			t.Errorf("parameters missing for %s+xhigh (all models should support reasoning)", mk)
			continue
		}
		if p2["enable_thinking"] != true || p2["reasoning_effort"] != "xhigh" {
			t.Errorf("%s parameters=%v", mk, p2)
		}
	}

	// qfmodel + 空 → 不注入
	if _, present := build("qfmodel", "")["parameters"]; present {
		t.Errorf("parameters should be absent when effort empty")
	}

	// 空 modelKey + xhigh → 不注入（异常输入不注入思考）
	if _, present := build("", "xhigh")["parameters"]; present {
		t.Errorf("parameters should be absent for empty model key")
	}

	// qfmodel + 非法值（经 Normalize 后为空）→ 不注入
	if _, present := build("qfmodel", NormalizeReasoningEffort("turbo"))["parameters"]; present {
		t.Errorf("parameters should be absent for invalid effort")
	}
	// Normalize 大小写/空白处理
	if NormalizeReasoningEffort("XHIGH") != "xhigh" || NormalizeReasoningEffort(" low ") != "low" ||
		NormalizeReasoningEffort("") != "" || NormalizeReasoningEffort("max") != "" {
		t.Errorf("NormalizeReasoningEffort wrong: %q %q %q %q",
			NormalizeReasoningEffort("XHIGH"), NormalizeReasoningEffort(" low "),
			NormalizeReasoningEffort(""), NormalizeReasoningEffort("max"))
	}
}

func TestBuildBodyLongPromptTruncatesBusinessName(t *testing.T) {
	long := strings.Repeat("a", 100)
	msgs := []map[string]any{{"role": "user", "content": long}}
	body, _ := BuildAgentBody(msgs, "qmodel", nil, "")
	var m map[string]any
	json.Unmarshal(body, &m)
	if n := m["business"].(map[string]any)["name"].(string); len([]rune(n)) > 30 {
		t.Errorf("business.name len=%d", len([]rune(n)))
	}
}

func TestBuildBodyClientToolsOverride(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "北京天气"}}
	clientTools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "获取天气",
				"parameters":  map[string]any{"type": "object"},
			},
		},
	}
	body, err := BuildAgentBody(msgs, "qmodel_preview", clientTools, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	tools := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d (want 1 client tool override)", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name=%v", fn["name"])
	}
}

func TestBuildBodyMultiTurnPreserved(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "Hermes 系统提示"},
		{"role": "user", "content": "第一轮"},
		{"role": "assistant", "content": "回复一"},
		{"role": "user", "content": "第二轮"},
	}
	body, _ := BuildAgentBody(msgs, "qmodel_preview", nil, "")
	var m map[string]any
	json.Unmarshal(body, &m)
	arr := m["messages"].([]any)
	// 仅客户端 4 条（无模板 system）
	if len(arr) != 4 {
		t.Fatalf("messages len=%d (want 4)", len(arr))
	}
	if arr[0].(map[string]any)["content"] != "Hermes 系统提示" {
		t.Errorf("hermes system lost: %v", arr[0])
	}
	if arr[3].(map[string]any)["content"] != "第二轮" {
		t.Errorf("last user lost: %v", arr[3])
	}
	// chat_context.text 取最后一条 user
	cc := m["chat_context"].(map[string]any)
	if cc["text"].(map[string]any)["text"] != "第二轮" {
		t.Errorf("chat_context.text=%v", cc["text"])
	}
}

func TestParseNestedSSE(t *testing.T) {
	raw := "data:{\"headers\":{\"Content-Type\":[\"application/json\"]},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"你好\\\"}}]}\",\"statusCodeValue\":200}\n\n" +
		"data:{\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"世界\\\"}}]}\"}\n\n" +
		"data:{\"body\":\"[DONE]\"}\n\nevent:finish\ndata:{\"firstTokenDuration\":100}\n\n"
	var chunks []string
	err := ParseNestedSSE(strings.NewReader(raw), func(chunk map[string]any) error {
		if ch, ok := chunk["choices"].([]any); ok && len(ch) > 0 {
			if d, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok && c != "" {
					chunks = append(chunks, c)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0] != "你好" || chunks[1] != "世界" {
		t.Errorf("chunks=%v", chunks)
	}
}

func TestParseNestedSSEIgnoresFinishEvent(t *testing.T) {
	raw := "data:{\"body\":\"{\\\"choices\\\":[]}\"}\n\nevent:finish\ndata:{\"firstTokenDuration\":100,\"totalDuration\":200}\n\ndata:{\"body\":\"[DONE]\"}\n\n"
	called := false
	err := ParseNestedSSE(strings.NewReader(raw), func(chunk map[string]any) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("onChunk never called")
	}
}

func TestAggregateNested(t *testing.T) {
	raw := "data:{\"body\":\"{\\\"id\\\":\\\"chatcmpl-1\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"你\\\"}}]}\"}\n\n" +
		"data:{\"body\":\"{\\\"id\\\":\\\"chatcmpl-1\\\",\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"好\\\"}}]}\"}\n\n" +
		"data:{\"body\":\"{\\\"id\\\":\\\"chatcmpl-1\\\",\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}],\\\"usage\\\":{\\\"prompt_tokens\\\":1,\\\"completion_tokens\\\":2,\\\"total_tokens\\\":3}}\"}\n\n" +
		"data:{\"body\":\"[DONE]\"}\n\n"
	resp, err := AggregateNested(strings.NewReader(raw), "qwen3.8-max-preview")
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "qwen3.8-max-preview" {
		t.Errorf("model=%v (should be client name, not upstream 'auto')", resp["model"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 3 {
		t.Errorf("usage=%v", usage)
	}
}

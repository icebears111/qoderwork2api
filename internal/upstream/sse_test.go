package upstream

import (
	"strings"
	"testing"
)

// 上游每个 delta 同时带 content 与 reasoning_content（其一为空串）。
// StreamAsOpenAI 必须删除空串键，否则 Anthropic 风格客户端会把
// 一段正文误拆成多个独立消息块（CCD "逐分块断行" 的根因，2026-09-18）。
func TestStreamAsOpenAIStripsEmptyDeltaKeys(t *testing.T) {
	raw := "data:{\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"\\\",\\\"reasoning_content\\\":\\\"思考A\\\"}}]}\"}\n\n" +
		"data:{\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"你好\\\",\\\"reasoning_content\\\":\\\"\\\"}}]}\"}\n\n" +
		"data:{\"body\":\"[DONE]\"}\n\n"
	var sb strings.Builder
	if err := StreamAsOpenAI(&sb, strings.NewReader(raw), "qwen3.8-flash", nil); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	// 空的键必须被删（否则客户端逐块断行）
	if strings.Contains(out, `"content":""`) {
		t.Errorf("empty content not stripped: %s", out)
	}
	if strings.Contains(out, `"reasoning_content":""`) {
		t.Errorf("empty reasoning_content not stripped: %s", out)
	}
	// 非空内容保持
	if !strings.Contains(out, `"reasoning_content":"思考A"`) {
		t.Errorf("reasoning chunk lost: %s", out)
	}
	if !strings.Contains(out, `"content":"你好"`) {
		t.Errorf("content chunk lost: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

// 非空的两键同时出现时保持原样（不误删）。
func TestNormalizeDeltaKeysKeepsNonEmpty(t *testing.T) {
	chunk := map[string]any{
		"choices": []any{
			map[string]any{"delta": map[string]any{"content": "x", "reasoning_content": "y"}},
		},
	}
	normalizeDeltaKeys(chunk)
	d := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d["content"] != "x" || d["reasoning_content"] != "y" {
		t.Errorf("non-empty keys must be kept: %v", d)
	}
}

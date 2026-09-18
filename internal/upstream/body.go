// body.go 构造 agent_chat_generation 请求体（纯透传模式）。
//
// 纯透传：客户端消息全量转发（含 system/assistant/tool 多轮），
// tools 仅在客户端显式传入时注入。实测模板 system + 模板 tools 均非必需，
// 且 baseline prompt_tokens 从 ~10K 降到 ~60。
package upstream

import (
	"encoding/json"
	"strings"
	"time"
)

// IsReasoningModel 判断上游模型 key 是否开启思考模式（请求体 is_reasoning=true）。
// 2026-09-19 起对所有模型统一开启——与 qwen3.8-flash（qfmodel）走同一条思考链路：
// is_reasoning=true + source="system"。上游对不支持的模型会忽略该字段，
// 实测不会因此报错（曾用 totally-fake-model-xyz 验证过 key 被忽略的行为）。
func IsReasoningModel(modelKey string) bool {
	return modelKey != ""
}

// NormalizeReasoningEffort 校验思考档位（上游 thinking_config 支持
// low/medium/xhigh）。非法值返回空串 = 不注入 parameters，走上游默认（medium）。
// 所有模型共用同一档位集合，与 qfmodel 完全一致。
func NormalizeReasoningEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "xhigh":
		return "xhigh"
	default:
		return ""
	}
}

// BuildAgentBody 构造请求体。
//   - openaiMessages：客户端原始消息列表（可含 system/assistant/tool 多轮）
//   - modelKey：上游模型 key（如 qmodel_preview）
//   - clientTools：客户端传来的 OpenAI tools 数组；为空则不注入 tools 字段
//   - reasoningEffort：客户端可选思考档位（low/medium/xhigh）；空 = 上游默认
func BuildAgentBody(openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) ([]byte, error) {
	// 最后一条 user 消息文本（chat_context.text 上游协议要求必填）
	prompt := ""
	for i := len(openaiMessages) - 1; i >= 0; i-- {
		if openaiMessages[i]["role"] == "user" {
			if c, ok := openaiMessages[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	now := time.Now()
	newUUID := uuid4()

	// 思考模式：所有模型统一开启（2026-09-19 起），与 qwen3.8-flash 同一链路。
	// source="system" 是上游触发思考的真正开关——实测缺它则 reasoning_content 永不下发
	//（探测见 2026-09-18；旧版全部硬编码 false，更早版本仅 qfmodel 开启）。
	isReasoning := IsReasoningModel(modelKey)
	modelConfig := map[string]any{"key": modelKey, "is_reasoning": isReasoning}
	if isReasoning {
		modelConfig["source"] = "system"
	}

	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   uuid4(),
		"session_id":       uuid4(),
		"stream":           true,
		"aliyun_user_type": "personal_professional_trial",
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"image_urls":       nil,
		"session_type":     "qodercli",
		"model_config":     modelConfig,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": isReasoning},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": openaiMessages,
		"business": map[string]any{
			"id":       uuid4(),
			"begin_at": now.UnixMilli(),
			"name":     truncateRunes(prompt, 30),
		},
	}

	// tools：客户端传了才注入
	if len(clientTools) > 0 {
		base["tools"] = clientTools
	}

	// 思考档位：所有模型都支持（与 qfmodel 一致），客户端显式传了合法值时注入 parameters。
	// 不传 = 上游默认（medium）。实测 xhigh 推理量约为默认 2~6 倍。
	if isReasoning && reasoningEffort != "" {
		base["parameters"] = map[string]any{
			"enable_thinking":  true,
			"reasoning_effort": reasoningEffort,
		}
	}

	return json.Marshal(base)
}

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

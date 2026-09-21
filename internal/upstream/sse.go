// sse.go 处理 QoderWork 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ParseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - "event:finish" 行 → 忽略
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
func ParseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			// 外层 envelope
			var env struct {
				Body string `json:"body"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(env.Body), &chunk) == nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
		}
		// "event:finish" / 空行 / 其他 → 忽略
		if err == io.EOF {
			return nil
		}
	}
}

// AggregateNested 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 参数用于覆盖响应中的 model 字段（上游恒为 "auto"，客户端要看到自己的模型名）。
// tool_calls 以流式 delta 形式到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func AggregateNested(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				if r2, ok := delta["role"].(string); ok && r2 != "" {
					role = r2
				}
				if txt, ok := delta["content"].(string); ok {
					content.WriteString(txt)
				}
				if rc, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(rc)
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// StreamAsOpenAI 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端。
// 每个 chunk 重写 model 字段为客户端模型名；末尾补 data: [DONE]。
//
// 关键规范化（2026-09-18）：上游每个 delta 同时带 content 与 reasoning_content
// 两个键，其中一个恒为空串（思考/正文交替阶段切换）。Anthropic 风格客户端
// （CCD 等）看到「键集合变化」会误判内容块类型切换，把一段正文拆成多个独立
// 消息块。这里删除空串键，保证每个 delta 至多只有一个内容键。
// StreamAsOpenAI 把上游 SSE 边解析边转发成 OpenAI 流，并回传 usage。
//
// onUsage 是**可选**回调（可变参数，保持既有调用点不变）：上游把 usage
// 放在最后一个 chunk 里，这里看到就抄一份交出去。抄的动作不改动任何
// 要下发的字节 —— 提取失败也绝不影响转发。
func StreamAsOpenAI(w io.Writer, r io.Reader, model string, flush func(),
	onUsage ...func(map[string]any)) error {
	sawDone := false
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if u, ok := chunk["usage"].(map[string]any); ok && len(onUsage) > 0 && onUsage[0] != nil {
			onUsage[0](u)
		}
		chunk["model"] = model
		normalizeDeltaKeys(chunk)
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawDone {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	}
	return nil
}

// normalizeDeltaKeys 删除空的 content/reasoning_content 键（就地修改 chunk）。
// 上游把两个键都塞进每个 delta，空值键会触发客户端误判块类型切换。
func normalizeDeltaKeys(chunk map[string]any) {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if v, ok := d["content"].(string); ok && v == "" {
			delete(d, "content")
		}
		if v, ok := d["reasoning_content"].(string); ok && v == "" {
			delete(d, "reasoning_content")
		}
	}
}

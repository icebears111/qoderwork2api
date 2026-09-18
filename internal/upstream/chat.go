// chat.go COSY 签名 + QoderEncoding 的对话转发。
package upstream

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"qoderwork2api/internal/cred"
)

// ChatPath 对话端点路径（拼在 Gateway 后）。
const ChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// ChatForwardRaw 与 ChatForward 相同，但请求体由调用方提供（已构造好的 JSON）。
// 供探测工具复用签名/转发链路。
func (c *Client) ChatForwardRaw(cred *cred.Cred, modelKey string, rawJSON []byte) (io.ReadCloser, error) {
	if cred.DT == "" {
		return nil, &Error{Kind: ErrTokenExpired, Status: 0, Msg: "no dt- available"}
	}
	encoded := QoderEncode(rawJSON)
	url := c.Gateway + ChatPath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	sess, err := NewCosySession(cred, cred.DT, cred.DRT)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, cred.UID, true, modelKey); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	return resp.Body, nil
}

// ChatForward 把 OpenAI 消息构造 + 编码 + 签名后发到上游。
// clientTools 为客户端传来的 OpenAI tools 数组（可为 nil）。
// reasoningEffort 为客户端可选思考档位（low/medium/xhigh，空 = 上游默认）。
// 成功返回原始嵌套 SSE body 流（调用方 Close）；
// 失败返回带分类的 *Error（status 取自上游，body 已读取）。
func (c *Client) ChatForward(cred *cred.Cred, openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) (io.ReadCloser, error) {
	if cred.DT == "" {
		return nil, &Error{Kind: ErrTokenExpired, Status: 0, Msg: "no dt- available"}
	}
	// 构造 body
	rawBody, err := BuildAgentBody(openaiMessages, modelKey, clientTools, reasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}
	encoded := QoderEncode(rawBody)
	if IsReasoningModel(modelKey) {
		log.Printf("chat_forward reasoning=on uid=%s model=%s", cred.UID, modelKey)
	}

	url := c.Gateway + ChatPath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	sess, err := NewCosySession(cred, cred.DT, cred.DRT)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, cred.UID, true, modelKey); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("chat_forward uid=%s model=%s: transport error: %v", cred.UID, modelKey, err)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_forward uid=%s model=%s: upstream %d %s body=%s",
			cred.UID, modelKey, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	return resp.Body, nil
}

// package agent：本文件是 LLM 客户端——
// 用标准库 net/http 手写一个【OpenAI 兼容】的 Chat Completions 调用，
// 不引入第三方 SDK。好处：少一个依赖、流程自己完全可控，也能体现手写能力。
//
// OpenAI 兼容接口（OpenAI、DeepSeek、Moonshot、本地 vLLM 等都长这样）：
//   POST {BaseURL}/chat/completions
//   请求头 Authorization: Bearer {APIKey}
//   请求体 {"model": "...", "messages": [{"role":"...","content":"..."}]}
package agent

import (
	// bytes：拼请求体。
	"bytes"
	// context：超时控制。
	"context"
	// encoding/json：编解码。
	"encoding/json"
	// fmt：拼错误。
	"fmt"
	// io：读响应体。
	"io"
	// net/http：发 HTTP。
	"net/http"
	// strings：处理地址末尾斜杠。
	"strings"
	// time：设置超时。
	"time"
)

// toolCall 是模型要求调用某个工具的请求。
type toolCall struct {
	// ID 本次调用的唯一编号（回填工具结果时要用它对应上）。
	ID string `json:"id"`
	// Type 固定为 function。
	Type string `json:"type"`
	// Function 具体要调的函数名和参数（参数是一个 JSON 字符串）。
	Function struct {
		// Name 函数名。
		Name string `json:"name"`
		// Arguments JSON 格式的参数文本。
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatMessage 是发给模型的一条消息。
type chatMessage struct {
	// Role 角色：system / user / assistant / tool。
	Role string `json:"role"`
	// Content 文本内容；assistant 只发工具调用时可能为空，故 omitempty。
	Content string `json:"content,omitempty"`
	// ToolCalls 仅 assistant 消息可能带：模型请求调用的工具列表。
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 role=tool 消息带：对应之前哪一次工具调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// toolSpec 是发给模型的函数描述（名字/用途/参数的 JSON Schema）。
type toolSpec struct {
	// Name 函数名，需和注册工具一致。
	Name string `json:"name"`
	// Description 告诉模型"什么时候该用这个工具"。
	Description string `json:"description"`
	// Parameters 参数的 JSON Schema（直接内嵌原始 JSON）。
	Parameters json.RawMessage `json:"parameters"`
}

// toolDefinition 是请求体 tools 数组的一项：type=function 包一层函数描述。
type toolDefinition struct {
	// Type 固定 function。
	Type string `json:"type"`
	// Function 函数描述。
	Function toolSpec `json:"function"`
}

// chatRequest 是 /chat/completions 的请求体。
type chatRequest struct {
	// Model 模型名。
	Model string `json:"model"`
	// Messages 消息列表。
	Messages []chatMessage `json:"messages"`
	// Temperature 采样温度：审核要稳定，给 0（尽量确定）。
	Temperature float64 `json:"temperature"`
	// Tools 可供模型调用的工具列表；为空则不出现该字段。
	Tools []toolDefinition `json:"tools,omitempty"`
	// ToolChoice 工具选择策略："auto" 让模型自行决定调不调。
	ToolChoice string `json:"tool_choice,omitempty"`
}

// chatResponse 是 /chat/completions 响应里我们关心的部分。
type chatResponse struct {
	// Choices 候选回答列表（一般取第一个）。
	Choices []struct {
		// Message 回答消息（可能含 tool_calls）。
		Message chatMessage `json:"message"`
		// FinishReason 结束原因：stop / tool_calls。
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Error 接口返回错误时的结构（OpenAI 风格会在 body 里带 error）。
	Error *struct {
		// Message 错误信息。
		Message string `json:"message"`
		// Type 错误类型。
		Type string `json:"type"`
	} `json:"error"`
}

// Client 是 LLM 客户端，持有地址、Key、模型名和一个复用的 http.Client。
type Client struct {
	// baseURL 接口基础地址。
	baseURL string
	// apiKey 密钥（只在内存里用，不打印）。
	apiKey string
	// model 模型名。
	model string
	// httpClient 复用连接的 HTTP 客户端。
	httpClient *http.Client
}

// NewClient 构造函数：创建 LLM 客户端。
//
// 参数：baseURL 基础地址、apiKey 密钥、model 模型名、timeout 单次请求超时；
// 返回值 *Client。
func NewClient(baseURL string, apiKey string, model string, timeout time.Duration) *Client {
	// 去掉末尾多余斜杠，方便后面直接拼 /chat/completions。
	baseURL = strings.TrimRight(baseURL, "/")
	// 装配；http.Client 设整体超时。
	return &Client{
		// baseURL。
		baseURL: baseURL,
		// apiKey。
		apiKey: apiKey,
		// model。
		model: model,
		// httpClient：Timeout 覆盖连接+读取全程。
		httpClient: &http.Client{Timeout: timeout},
	}
}

// maxChatAttempts 单次对话的最多尝试次数：网络抖动 / 429 限流 / 5xx 时自动多试一次。
const maxChatAttempts = 2

// chatRetryBackoff 重试前的等待时间（固定值：重试只有一次，无需退避算法）。
const chatRetryBackoff = 500 * time.Millisecond

// Chat 方法：发一次对话请求，返回模型文本和（可能的）工具调用。
//
// 参数：ctx 上下文（可再叠加超时/取消）、messages 消息列表、tools 工具定义（可为空）；
// 返回值：string 文本内容、[]toolCall 工具调用、error 网络/接口/空结果错误。
//
// 约定：如果模型这一轮要调工具，返回的 content 往往为空、toolCalls 非空；
// 如果它直接给最终答案，toolCalls 为空、content 是决策 JSON。
//
// 重试策略：只对"再试一次可能恢复"的瞬时故障重试（网络错误、429、5xx）；
// 鉴权类 4xx、JSON 解析错误重试无意义，直接返回。整体仍受 ctx 的总超时约束。
func (c *Client) Chat(ctx context.Context, messages []chatMessage, tools []toolDefinition) (string, []toolCall, error) {
	// reqBody 组装请求体；温度 0。
	reqBody := chatRequest{
		// Model。
		Model: c.model,
		// Messages。
		Messages: messages,
		// Temperature 0。
		Temperature: 0,
		// Tools 传入工具定义。
		Tools: tools,
	}
	// 有工具时让模型自行选择调用（tool_choice=auto）。
	if len(tools) > 0 {
		// auto。
		reqBody.ToolChoice = "auto"
	}
	// data 编码成 JSON（只编一次，重试时复用）；err 接收错误。
	data, err := json.Marshal(reqBody)
	if err != nil {
		// 编码失败返回。
		return "", nil, fmt.Errorf("编码请求失败: %w", err)
	}

	// lastErr 记录最后一次瞬时错误，耗尽重试时包装返回。
	var lastErr error
	// attempt 尝试轮次。
	for attempt := 0; attempt < maxChatAttempts; attempt++ {
		// 第二次尝试前先等一小段（可被 ctx 取消打断）。
		if attempt > 0 {
			// select 等取消或退避结束。
			select {
			// 总超时/取消。
			case <-ctx.Done():
				// 返回。
				return "", nil, ctx.Err()
			// 退避结束。
			case <-time.After(chatRetryBackoff):
			}
		}

		// chatOnce 真正发一次请求；retryable 表示该错误是否值得重试。
		content, calls, retryable, err := c.chatOnce(ctx, data)
		// 成功：直接返回。
		if err == nil {
			// 返回。
			return content, calls, nil
		}
		// 不可重试的错误：立即返回，不浪费时间。
		if !retryable {
			// 返回。
			return "", nil, err
		}
		// 瞬时错误：记下来，准备下一轮重试。
		lastErr = err
	}

	// 重试后仍失败：包装最后一次错误返回（上层 Agent 会转人工）。
	return "", nil, fmt.Errorf("重试 %d 次后仍失败: %w", maxChatAttempts, lastErr)
}

// chatOnce 方法：执行一次完整的 HTTP 往返（建请求 → 发送 → 读响应 → 基础解析）。
//
// 参数：ctx 上下文、data 已编码的请求体；
// 返回值：string 文本、[]toolCall 工具调用、bool 错误是否可重试、error 错误。
func (c *Client) chatOnce(ctx context.Context, data []byte) (string, []toolCall, bool, error) {
	// url 拼完整接口地址。
	url := c.baseURL + "/chat/completions"
	// http.NewRequestWithContext 建带上下文的 POST 请求；req、err。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		// 建请求失败：重试也没用。
		return "", nil, false, fmt.Errorf("构建请求失败: %w", err)
	}
	// 设置请求头：内容类型。
	req.Header.Set("Content-Type", "application/json")
	// 设置请求头：鉴权（Key 只放在这，不写进日志）。
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	// Do 真正发请求；resp、err。
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 网络错误（超时、连接被掐断）：通常是暂时的，可重试。
		return "", nil, true, fmt.Errorf("调用 LLM 失败: %w", err)
	}
	// defer 关闭响应体，防止连接泄漏。
	defer resp.Body.Close()
	// raw 读全部响应字节。
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// 读取中断：连接级问题，可重试。
		return "", nil, true, fmt.Errorf("读取响应失败: %w", err)
	}

	// 状态码非 2xx：把 body 带回来方便排查（body 里一般没有 Key）。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 429 限流、5xx 服务端故障：歇一会儿可能恢复，标记可重试；其余 4xx（如 Key 错）不可重试。
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		// 返回。
		return "", nil, retryable, fmt.Errorf("LLM 返回状态 %d: %s", resp.StatusCode, string(raw))
	}

	// out 解析响应。
	var out chatResponse
	// 反序列化；err。
	if err := json.Unmarshal(raw, &out); err != nil {
		// 响应体不是合法 JSON：重试无法保证改善，按不可重试处理。
		return "", nil, false, fmt.Errorf("解析响应失败: %w", err)
	}
	// body 里带 error 字段。
	if out.Error != nil {
		// 业务错误（内容被拦截等）：重试无意义。
		return "", nil, false, fmt.Errorf("LLM 接口错误: %s", out.Error.Message)
	}
	// 没有任何候选回答。
	if len(out.Choices) == 0 {
		// 返回空结果错误。
		return "", nil, false, fmt.Errorf("LLM 未返回 choices")
	}
	// msg 取第一条回答。
	msg := out.Choices[0].Message
	// 正常：返回文本内容和工具调用（可能两者之一为空），错误位全空。
	return msg.Content, msg.ToolCalls, false, nil
}

// package agent：本文件是 Agent 的"装配 + 主流程"。
//
// D3 的主流程很简单（还没有工具调用）：
//   组装 Prompt → 调模型 → 解析 JSON → Schema 校验
//   格式不对就纠正后重试一次；硬错误/仍不合法 → 交给调用方转人工。
package agent

import (
	// context：生命周期。
	"context"
	// encoding/json：解析决策。
	"encoding/json"
	// errors：判断/返回错误。
	"errors"
	// fmt：拼工具结果/错误文本。
	"fmt"
	// config：读取 AgentConfig。
	"feedsystem_video_go/internal/config"
	// os：按环境变量名取 API Key。
	"os"
	// strings：抽取 JSON 时用。
	"strings"
	// time：超时。
	"time"
)

// Agent 持有运行配置和 LLM 客户端。
type Agent struct {
	// cfg 运行期配置（模型、轮次、阈值等）。
	cfg *runtimeConfig
	// client LLM 客户端。
	client *Client
	// tools 只读查询工具注册表（可能为空：没有工具时模型只能凭举报信息判断）。
	tools *ToolRegistry
}

// runtimeConfig 是 Agent 真正运行时用的配置（已把 Key 解析好、默认值填好）。
type runtimeConfig struct {
	// model 模型名。
	model string
	// maxRounds 最大轮次（D3 先存，工具循环在后面用）。
	maxRounds int
	// timeout 单次运行超时。
	timeout time.Duration
	// thresholdRemove 删除门槛。
	thresholdRemove float64
	// thresholdWarn 警告门槛。
	thresholdWarn float64
}

// NewAgent 构造函数：根据配置和工具创建 Agent。
//
// 参数：c 配置里的 agent 段、queryTools 可供模型调用的只读工具（可传空）；
// 返回值：*Agent 和 error。约定：
//   - 配置 Enabled=false：返回 (nil, nil)，调用方据此跳过 AI；
//   - Enabled=true 但 Key/地址/模型缺失：返回 (nil, error)，说明配置有误。
func NewAgent(c config.AgentConfig, queryTools ...Tool) (*Agent, error) {
	// 没启用：返回空，不报错（工具也用不上）。
	if !c.Enabled {
		// nil, nil。
		return nil, nil
	}

	// keyEnv 确定去哪个环境变量取 Key，默认 AGENT_API_KEY。
	keyEnv := c.APIKeyEnv
	// 为空给默认。
	if keyEnv == "" {
		// 默认环境变量名。
		keyEnv = "AGENT_API_KEY"
	}
	// apiKey 读环境变量。
	apiKey := os.Getenv(keyEnv)
	// 启用了却没 Key：配置错误。
	if apiKey == "" {
		// 返回错误，明确指出缺哪个变量。
		return nil, errors.New("agent 已启用，但环境变量未设置: " + keyEnv)
	}
	// BaseURL 缺失。
	if strings.TrimSpace(c.BaseURL) == "" {
		// 返回错误。
		return nil, errors.New("agent.base_url 不能为空")
	}
	// Model 缺失。
	if strings.TrimSpace(c.Model) == "" {
		// 返回错误。
		return nil, errors.New("agent.model 不能为空")
	}

	// rounds 兜底。
	rounds := c.MaxRounds
	// 非法给默认 4。
	if rounds <= 0 {
		// 默认。
		rounds = 4
	}
	// timeoutSec 兜底。
	timeoutSec := c.TimeoutSeconds
	// 非法给默认 20 秒。
	if timeoutSec <= 0 {
		// 默认。
		timeoutSec = 20
	}

	// rt 组装运行期配置。
	rt := &runtimeConfig{
		// model。
		model: c.Model,
		// maxRounds。
		maxRounds: rounds,
		// timeout。
		timeout: time.Duration(timeoutSec) * time.Second,
		// thresholdRemove 兜底为 0.85。
		thresholdRemove: c.Thresholds.Remove,
		// thresholdWarn 兜底为 0.7。
		thresholdWarn: c.Thresholds.Warn,
	}
	// 阈值若没配（0）给默认。
	if rt.thresholdRemove <= 0 {
		// 默认。
		rt.thresholdRemove = 0.85
	}
	// 警告阈值默认。
	if rt.thresholdWarn <= 0 {
		// 默认。
		rt.thresholdWarn = 0.7
	}

	// client 建 LLM 客户端，超时取运行硬超时。
	client := NewClient(c.BaseURL, apiKey, c.Model, rt.timeout)
	// registry 把传入的只读工具建成注册表。
	registry := NewToolRegistry(queryTools...)
	// 返回装配好的 Agent。
	return &Agent{cfg: rt, client: client, tools: registry}, nil
}

// ModelName 方法：返回当前使用的模型名（供 Worker 记到工单上）。
//
// 返回值 string：模型名；Agent 为空时返回空串。
func (a *Agent) ModelName() string {
	// 空指针保护。
	if a == nil || a.cfg == nil {
		// 返回空。
		return ""
	}
	// 返回模型名。
	return a.cfg.model
}

// Decide 方法：对一条举报跑完整的 Function Calling 循环，返回合法决策。
//
// 参数：ctx 上下文、in 举报信息；
// 返回值：Decision、error。
//   - 拿到合法决策：Decision、nil；
//   - 超过最大轮次仍无合法决策/网络错误：返回错误，调用方据此转人工。
//
// 每一轮：调模型 → 若要调工具就执行只读工具并把结果喂回、进入下一轮；
// 若模型直接作答，就解析决策 JSON，格式不对就纠错后再试，直到成功或耗尽轮次。
func (a *Agent) Decide(ctx context.Context, in ReportInput) (Decision, error) {
	// 给【整次运行】套一个硬超时：无论模型来回调多少轮，总耗时不能超过 timeout，
	// 超时即返回错误（Worker 会转人工），避免一个任务长期占用 worker。
	ctx, cancel := context.WithTimeout(ctx, a.cfg.timeout)
	// defer 释放。
	defer cancel()

	// messages 对话历史：先系统提示、再本次举报。
	messages := []chatMessage{
		// 系统消息。
		{Role: "system", Content: systemPrompt()},
		// 用户消息。
		{Role: "user", Content: userPrompt(in)},
	}

	// defs 本轮要带给模型的工具定义（注册表为空时得到空切片）。
	defs := a.tools.Definitions()

	// round 从 0 到 maxRounds-1。
	for round := 0; round < a.cfg.maxRounds; round++ {
		// content 文本、calls 工具调用、err 错误。
		content, calls, err := a.client.Chat(ctx, messages, defs)
		// 网络/接口硬错误：直接返回（不在这无限重试，交给 Worker 决定重发/转人工）。
		if err != nil {
			// 返回。
			return Decision{}, err
		}

		// 模型这一轮请求调用工具：
		if len(calls) > 0 {
			// 先把 assistant 这条"带工具调用"的消息原样记进历史（接口要求回传它）。
			messages = append(messages, chatMessage{Role: "assistant", Content: content, ToolCalls: calls})
			// runToolCalls 执行每个只读工具、生成对应的 tool 结果消息。
			messages = append(messages, a.runToolCalls(ctx, calls)...)
			// 进入下一轮，让模型基于证据继续思考/再调工具/给结论。
			continue
		}

		// 没有工具调用：content 应是最终决策。解析它。
		d, parseErr := parseDecision(content)
		// 合法决策：返回。
		if parseErr == nil {
			// 返回。
			return d, nil
		}
		// 格式非法：若还有轮次，追加一条纠错消息让它重答。
		if round+1 < a.cfg.maxRounds {
			// 记下它这次的回答。
			messages = append(messages, chatMessage{Role: "assistant", Content: content})
			// 追加纠错要求。
			messages = append(messages, chatMessage{
				// Role user。
				Role: "user",
				// Content 指出错误并重申只输出 JSON。
				Content: "你上一条输出不符合要求：" + parseErr.Error() + "。请只输出一个合法的 JSON 对象，不要代码块和多余文字。",
			})
			// 继续。
			continue
		}
		// 最后一轮仍非法：返回错误。
		return Decision{}, parseErr
	}

	// 用完所有轮次（比如一直只调工具、迟迟不下结论）：返回错误。
	return Decision{}, errors.New("超过最大轮次仍未得到合法决策")
}

// runToolCalls 方法：执行一批工具调用，返回要追加的 tool 结果消息。
//
// 参数：ctx 上下文、calls 模型请求的工具调用；
// 返回值 []chatMessage：每次调用对应一条 role=tool 的结果消息。
//
// 安全要点：这里即使工具不存在/执行报错，也【不向模型返回 Go 的硬错误】，
// 而是把错误文字作为该工具的结果喂回去，让模型自己换个办法或改判。
func (a *Agent) runToolCalls(ctx context.Context, calls []toolCall) []chatMessage {
	// out 结果消息切片。
	out := make([]chatMessage, 0, len(calls))
	// 逐个执行。
	for _, call := range calls {
		// executeOneTool 执行单次调用（内部把 panic 也兜成错误文本）。
		resultText := a.executeOneTool(ctx, call)
		// 组装一条 tool 消息，ToolCallID 对应上这次调用。
		out = append(out, chatMessage{
			// Role tool。
			Role: "tool",
			// Content 结果文本。
			Content: resultText,
			// ToolCallID 对应。
			ToolCallID: call.ID,
		})
	}
	// 返回结果消息。
	return out
}

// executeOneTool 方法：执行一次工具调用并返回喂给模型的结果文本。
//
// 参数：ctx、call 模型的工具调用请求；
// 返回值 string：结果/错误的 JSON 文本。
//
// 健壮性：用 defer/recover 兜底——仓储代码万一 panic（空指针、越界等），
// 也被转成一条错误信息喂回模型，绝不让 panic 冒泡杀掉整个 Agent 流程。
func (a *Agent) executeOneTool(ctx context.Context, call toolCall) (resultText string) {
	// defer 注册 panic 拦截；r 是 recover() 抓到的 panic 值。
	defer func() {
		// 确实发生 panic。
		if r := recover(); r != nil {
			// b 把异常信息编成 JSON；mErr。
			b, mErr := json.Marshal(map[string]string{"error": fmt.Sprintf("工具内部异常: %v", r)})
			// 编码成功。
			if mErr == nil {
				// 覆盖返回值为错误 JSON。
				resultText = string(b)
			} else {
				// 兜底文本。
				resultText = `{"error":"工具内部异常"}`
			}
		}
	}()

	// tool 按名字找工具；ok 是否找到。
	tool, ok := a.tools.Get(call.Function.Name)
	// 工具不存在。
	if !ok {
		// 告知模型该工具不可用。
		return fmt.Sprintf(`{"error":"工具不存在: %s"}`, call.Function.Name)
	}
	// data 执行工具；err。
	data, err := tool.Execute(ctx, json.RawMessage(call.Function.Arguments))
	// 转成 JSON 文本返回。
	return toolResultText(data, err)
}

// toolResultText 普通函数：把工具执行结果转成喂回模型的文本。
//
// 参数：data 成功结果（可为 nil）、err 执行错误；
// 返回值 string：JSON 文本。
func toolResultText(data any, err error) string {
	// 执行出错：返回错误 JSON。
	if err != nil {
		// 用 marshal 避免错误信息里的特殊字符破坏 JSON。
		b, mErr := json.Marshal(map[string]string{"error": err.Error()})
		// 正常编码。
		if mErr == nil {
			// 返回。
			return string(b)
		}
		// 兜底（理论上不会走到）。
		return `{"error":"工具执行失败"}`
	}
	// 成功但无数据。
	if data == nil {
		// 返回空对象。
		return "{}"
	}
	// b 编码结果数据；err。
	b, err := json.Marshal(data)
	// 编码失败。
	if err != nil {
		// 返回错误 JSON。
		return `{"error":"结果编码失败"}`
	}
	// 返回结果 JSON。
	return string(b)
}

// parseDecision 普通函数：从模型文本里抽出 JSON 并解析、校验。
//
// 参数 content：模型原始回答；
// 返回值：Decision、error。
//
// 为什么需要它：模型有时会手滑包一层 ```json 代码块，或前后加客套话，
// 这里截取第一个 '{' 到最后一个 '}' 之间的内容，尽量稳健地取出 JSON。
func parseDecision(content string) (Decision, error) {
	// start 第一个花括号位置。
	start := strings.Index(content, "{")
	// end 最后一个花括号位置。
	end := strings.LastIndex(content, "}")
	// 两个括号都找到且顺序合理。
	if start < 0 || end < 0 || end < start {
		// 返回找不到 JSON 的错误。
		return Decision{}, errors.New("回答中没有 JSON 对象")
	}
	// jsonText 截取 JSON 片段。
	jsonText := content[start : end+1]

	// d 准备接收。
	var d Decision
	// 反序列化；err。
	if err := json.Unmarshal([]byte(jsonText), &d); err != nil {
		// JSON 语法错误返回。
		return Decision{}, errors.New("JSON 解析失败: " + err.Error())
	}
	// 再做字段级校验；返回其结果。
	return d, d.Validate()
}

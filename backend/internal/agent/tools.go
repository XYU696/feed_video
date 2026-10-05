// package agent：本文件定义"工具"长什么样、以及怎么注册/查找。
//
// 什么是 Function Calling：模型本身不能直接查数据库。它如果觉得需要证据，
// 就输出一个"我想调用 xxx 工具、参数是 yyy"的请求；由我们的 Go 程序真正执行，
// 再把结果喂回给它。工具就是这种"模型可以请我们代劳的动作"。
//
// D4 的工具全部是【只读查询】：只取证，不改动任何东西。
package agent

// import 导入 context、encoding/json：执行工具、解析参数。
import (
	"context"
	"encoding/json"
)

// Tool 是一个可被模型调用的只读工具。
type Tool interface {
	// Name 工具名（snake_case），模型靠它指定要调谁；必须在注册表里唯一。
	Name() string
	// Description 一句话说明用途，模型据此判断"什么时候该用我"。
	Description() string
	// Parameters 参数的 JSON Schema（描述要传哪些字段、什么类型）。
	Parameters() json.RawMessage
	// Execute 真正执行：args 是模型给出的原始 JSON 参数；
	// 返回结果（会被转成 JSON 喂回模型）或 error。
	Execute(ctx context.Context, args json.RawMessage) (any, error)
}

// ToolRegistry 是工具注册表：按名字存放所有可用工具。
type ToolRegistry struct {
	// tools 名字 → 工具。
	tools map[string]Tool
}

// NewToolRegistry 构造函数：用给定工具创建注册表。
//
// 参数 tools：要注册的工具；
// 返回值 *ToolRegistry。
func NewToolRegistry(tools ...Tool) *ToolRegistry {
	// r 初始化 map。
	r := &ToolRegistry{tools: make(map[string]Tool, len(tools))}
	// 逐个登记。
	for _, t := range tools {
		// 跳过空工具。
		if t == nil {
			// continue。
			continue
		}
		// 以名字为 key 存入。
		r.tools[t.Name()] = t
	}
	// 返回。
	return r
}

// Get 方法：按名字取工具。
//
// 返回值：Tool 和 bool（是否找到）。
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	// 空指针保护。
	if r == nil {
		// 没找到。
		return nil, false
	}
	// 从 map 取。
	t, ok := r.tools[name]
	// 返回。
	return t, ok
}

// Definitions 方法：生成请求体里 tools 字段要用的定义列表。
//
// 返回值 []toolDefinition：每个工具转成 OpenAI 要的格式。
func (r *ToolRegistry) Definitions() []toolDefinition {
	// defs 结果切片。
	defs := make([]toolDefinition, 0, len(r.tools))
	// 遍历注册的工具。
	for _, t := range r.tools {
		// 组装一项。
		defs = append(defs, toolDefinition{
			// Type 固定 function。
			Type: "function",
			// Function 函数描述。
			Function: toolSpec{
				// Name。
				Name: t.Name(),
				// Description。
				Description: t.Description(),
				// Parameters 直接内嵌其 JSON Schema。
				Parameters: t.Parameters(),
			},
		})
	}
	// 返回定义列表。
	return defs
}

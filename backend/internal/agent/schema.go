// package agent：内容安全 Agent 核心包。
//
// 这个文件定义"决策"的数据结构（Schema）和校验规则。
// 业务理解：模型看完一条举报后，必须按一个【固定格式】回答，
// 不能让它自由发挥写小作文——固定格式才方便程序读取、检查、执行。
package agent

// import 导入 fmt、strings：拼错误信息、判空。
import (
	// fmt 拼错误。
	"fmt"
	// strings 判空白。
	"strings"
)

// Decision 是模型必须输出的"决策结构"，对应我们规定的 JSON 格式。
type Decision struct {
	// Decision 最终判定：safe/remove/warn/ban/escalate（json 键 decision）。
	Decision string `json:"decision"`
	// Confidence 置信度，0~1 的小数，越高越确定。
	Confidence float64 `json:"confidence"`
	// Reason 判定理由（为什么这么判）。
	Reason string `json:"reason"`
	// PolicyHit 命中的平台政策条款（json 键 policy_hit）。
	PolicyHit string `json:"policy_hit"`
	// EvidenceIDs 用到的证据编号（json 键 evidence_ids）；D3 允许为空切片。
	EvidenceIDs []string `json:"evidence_ids"`
}

// 决策取值常量，和 moderation 包里的含义一致（这里独立定义，避免 agent 反向依赖 moderation）。
const (
	// decisionSafe 安全。
	decisionSafe = "safe"
	// decisionRemove 删除/下架。
	decisionRemove = "remove"
	// decisionWarn 警告。
	decisionWarn = "warn"
	// decisionBan 封禁。
	decisionBan = "ban"
	// decisionEscalate 转人工。
	decisionEscalate = "escalate"
)

// validDecisions 用 map 当"合法取值集合"。
var validDecisions = map[string]bool{
	// 安全。
	decisionSafe: true,
	// 删除。
	decisionRemove: true,
	// 警告。
	decisionWarn: true,
	// 封禁。
	decisionBan: true,
	// 转人工。
	decisionEscalate: true,
}

// Validate 方法：检查这份决策是否"字段齐全、取值合法"。
//
// 返回值 error：第一条不满足规则的错误；全部合法为 nil。
//
// 为什么要在 Go 里再校验一遍？不能信模型一定守规矩：它可能多写字、
// 漏字段、置信度给成 1.5。程序自己把关，坏结果就转人工，绝不放行。
func (d Decision) Validate() error {
	// 取值必须在白名单里。
	if !validDecisions[d.Decision] {
		// 返回错误。
		return fmt.Errorf("decision 取值非法: %q", d.Decision)
	}
	// 置信度必须落在 [0,1]。
	if d.Confidence < 0 || d.Confidence > 1 {
		// 返回错误。
		return fmt.Errorf("confidence 超出 [0,1]: %v", d.Confidence)
	}
	// 理由不能为空（只有空白也算空）。
	if strings.TrimSpace(d.Reason) == "" {
		// 返回错误。
		return fmt.Errorf("reason 不能为空")
	}
	// 除了"安全/转人工"，凡涉及处置的决策都必须写清命中哪条政策。
	if d.Decision != decisionSafe && d.Decision != decisionEscalate {
		// 政策条款为空。
		if strings.TrimSpace(d.PolicyHit) == "" {
			// 返回错误。
			return fmt.Errorf("policy_hit 不能为空")
		}
	}
	// 全部通过。
	return nil
}

// EscalateDecision 普通函数：生成一张"转人工"决策。
//
// 参数 why：转人工的原因（模型报错、格式非法、超时等）；
// 返回值 Decision：置信度 0、动作 escalate 的决策。
//
// 用途：任何拿不到合法自动决策的情况，都统一兜底成转人工，而不是报错或乱放。
func EscalateDecision(why string) Decision {
	// 返回转人工决策。
	return Decision{
		// Decision 转人工。
		Decision: decisionEscalate,
		// Confidence 0：表示系统没把握。
		Confidence: 0,
		// Reason 记录原因。
		Reason: why,
		// PolicyHit 转人工不要求命中条款。
		PolicyHit: "",
		// EvidenceIDs 空。
		EvidenceIDs: []string{},
	}
}

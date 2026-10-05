// package agent：本文件是"处置动作执行器"（D6）。
//
// 关键定位——它【不是模型工具】，绝不会注册进 ToolRegistry、模型碰不到它：
// 模型只负责输出"决策意图"（decision），真正删视频、删评论、发警告私信
// 都由本文件这个 Go 编排层执行。这样即使 Prompt 被注入，攻击者也无法越过
// 置信度门槛、白名单动作、防重复锁直接造成破坏。
//
// 自动处置必须连过三道关：
//  1. 闸门（gate）：对象类型与 decision 匹配、confidence ≥ 动作阈值、
//     policy_hit 非空、证据快照存在——任一不满足，强制转人工；
//  2. Redis 防重复锁 lock:moderation:{type}:{id}：同一时刻只允许一个流程处置同一对象；
//  3. 白名单动作：remove_video / remove_comment / warn 三种，没有别的。
package agent

// import 导入：
import (
	// context：贯穿执行过程。
	"context"
	// errors：人工执行路径返回未抢锁等错误。
	"errors"
	// fmt：拼动作标识、转人工原因、警告文案。
	"fmt"
	// time：锁 TTL。
	"time"

	// config：阈值配置类型。
	"feedsystem_video_go/internal/config"
	// rediscache：防重复锁、视频详情缓存失效。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// message：警告私信仓储。
	"feedsystem_video_go/internal/message"
	// video：视频/评论仓储。
	"feedsystem_video_go/internal/video"
)

// actionLockTTL 是处置锁的存活时间：持锁进程哪怕崩溃，锁也会在这么久后自动释放，
// 不会把对象"锁死"导致永远无法处置。
const actionLockTTL = 10 * time.Second

// systemAccountID 是系统通知的发送者编号：0 表示"平台官方"，不对应任何真实用户。
const systemAccountID uint = 0

// warnMessageTemplate 是警告私信的模板文案（%s/%d 由 fmt.Sprintf 填入）。
// 注意：文案里只放系统字段，不拼接举报人的 Detail，防止把不可信文本带进官方通知。
const warnMessageTemplate = `【平台提醒】您发布的内容（%s #%d）经核实违反社区规则（%s）。请遵守社区公约；再次违规可能导致内容删除或账号封禁。如有异议，可通过官方申诉渠道反馈。`

// ActionRequest 是"请执行处置"所需的全部输入（由 Worker 组装）。
type ActionRequest struct {
	// TargetType 被处置对象类型 video/comment/user。
	TargetType string
	// TargetID 被处置对象编号。
	TargetID uint

	// Decision 模型给出的决策意图。
	Decision string
	// Confidence 模型置信度。
	Confidence float64
	// PolicyHit 命中的政策条款。
	PolicyHit string

	// EvidenceSnapshot 证据快照文本（闸门要求非空）。
	EvidenceSnapshot string

	// WarnUserID warn 动作的接收者（Worker 已从证据里解析好）。
	WarnUserID uint
}

// ActionResult 是处置结果。
type ActionResult struct {
	// AutoDone 是否自动处置完成（含 safe 关单）；为 true 时工单可置 auto_executed。
	AutoDone bool
	// Action 实际动作标识（如 safe / remove:video / warn:user:7），写入 executed_action 留痕。
	Action string
	// Escalate 是否强制转人工。
	Escalate bool
	// Reason 转人工原因（闸门哪条没过/没抢到锁），用于日志。
	Reason string
}

// ActionExecutor 处置执行器，持有动作所需的仓储与缓存。
type ActionExecutor struct {
	// videoRepo 删视频。
	videoRepo *video.VideoRepository
	// commentRepo 查/删评论。
	commentRepo *video.CommentRepository
	// messageRepo 发警告私信。
	messageRepo *message.Repository
	// cache 防重复锁 + 视频详情缓存失效；为 nil 时自动处置一律转人工。
	cache *rediscache.Client

	// thresholdRemove remove 动作的置信度阈值。
	thresholdRemove float64
	// thresholdWarn warn 动作的置信度阈值。
	thresholdWarn float64
}

// NewActionExecutor 是构造函数：创建处置执行器。
//
// 参数：videoRepo/commentRepo/messageRepo 三个仓储、cache Redis 客户端、
// thresholds 阈值配置；返回值 *ActionExecutor。
// 阈值若没配置（≤0）则回落到 0.85 / 0.7，与配置默认值保持一致。
func NewActionExecutor(
	// videoRepo。
	videoRepo *video.VideoRepository,
	// commentRepo。
	commentRepo *video.CommentRepository,
	// messageRepo。
	messageRepo *message.Repository,
	// cache。
	cache *rediscache.Client,
	// thresholds。
	thresholds config.ThresholdConfig,
) *ActionExecutor {
	// remove、warn 取出配置值。
	remove, warn := thresholds.Remove, thresholds.Warn
	// remove 未配置：回落默认。
	if remove <= 0 {
		// 默认 0.85。
		remove = 0.85
	}
	// warn 未配置：回落默认。
	if warn <= 0 {
		// 默认 0.7。
		warn = 0.7
	}
	// 装配返回。
	return &ActionExecutor{
		videoRepo: videoRepo, commentRepo: commentRepo, messageRepo: messageRepo,
		cache: cache, thresholdRemove: remove, thresholdWarn: warn,
	}
}

// Execute 方法：按决策走"闸门 → 抢锁 → 执行"。
//
// 参数：ctx 上下文、req 处置请求；
// 返回值：ActionResult 结果、error。
//
// 两种失败要区分开：
//   - error 只表示【执行期的基础设施故障】（DB/Redis 命令出错），
//     Worker 收到后返回错误、让 MQ 重试；
//   - 闸门不通过、没抢到锁都不是系统故障，结果体里 Escalate=true，转人工即可。
func (x *ActionExecutor) Execute(ctx context.Context, req ActionRequest) (ActionResult, error) {
	// switch 按决策分发到对应流程。
	switch req.Decision {
	// safe：仅关单，不做任何处罚（"不误杀"）。
	case decisionSafe:
		// 关单也要求证据快照存在：什么都没核实过，就不能自动把举报结掉。
		if req.EvidenceSnapshot == "" {
			// 转人工。
			return escalateResult("safe 关单缺少证据快照"), nil
		}
		// 自动完成，动作标识 safe。
		return ActionResult{AutoDone: true, Action: decisionSafe}, nil

	// escalate：模型主动要求转人工，不执行任何动作。
	case decisionEscalate:
		// 转人工。
		return escalateResult("模型决策为 escalate"), nil

	// ban：账号封禁属于二期能力（账号要有 status 字段），现在一律转人工。
	case decisionBan:
		// 转人工。
		return escalateResult("ban 属于二期能力"), nil

	// remove：删视频/评论。
	case decisionRemove:
		// 走 remove 完整流程。
		return x.executeRemove(ctx, req)

	// warn：给作者发警告私信。
	case decisionWarn:
		// 走 warn 完整流程。
		return x.executeWarn(ctx, req)

	// 空串（Agent 未启用时工单没有决策）或其他未知值。
	default:
		// 转人工。
		return escalateResult(fmt.Sprintf("未知决策: %q", req.Decision)), nil
	}
}

// executeRemove 方法：remove 动作的闸门校验 + 抢锁 + 删除。
//
// 参数：ctx、req；
// 返回值：ActionResult、error（执行期故障）。
func (x *ActionExecutor) executeRemove(ctx context.Context, req ActionRequest) (ActionResult, error) {
	// 闸门 1：对象类型必须与 remove 匹配——只支持视频、评论；
	// 用户没有"删除用户"动作（那是 ban，二期）。
	if req.TargetType != TargetTypeVideo && req.TargetType != TargetTypeComment {
		// 不匹配：转人工。
		return escalateResult(fmt.Sprintf("remove 与对象类型不匹配: %s（仅支持 video/comment）", req.TargetType)), nil
	}
	// 闸门 2：置信度 ≥ remove 阈值（默认 0.85）。
	if req.Confidence < x.thresholdRemove {
		// 不够确信：转人工。
		return escalateResult(fmt.Sprintf("remove 置信度 %.2f 低于阈值 %.2f", req.Confidence, x.thresholdRemove)), nil
	}
	// 闸门 3：policy_hit 非空——说不清违反哪条规则，就不能自动删。
	if req.PolicyHit == "" {
		// 转人工。
		return escalateResult("remove 缺少 policy_hit"), nil
	}
	// 闸门 4：证据快照必须存在。
	if req.EvidenceSnapshot == "" {
		// 转人工。
		return escalateResult("remove 缺少证据快照"), nil
	}

	// 抢 Redis 防重复锁；token 锁凭证、ok 是否抢到、err 基础设施错误。
	token, ok, err := x.acquireLock(ctx, req.TargetType, req.TargetID)
	// Redis 命令出错：执行期故障，返回 error 让 MQ 重试。
	if err != nil {
		// 返回。
		return ActionResult{}, err
	}
	// 没抢到（Redis 不可用，或已有别的流程在处置同一对象）：绝不硬删，转人工。
	if !ok {
		// 转人工。
		return escalateResult("未获得处置锁（Redis 不可用，或其他流程正在处置该对象）"), nil
	}
	// defer 注册：本函数返回前用 token 安全释放锁（Lua 校验只删自己的锁）。
	defer x.releaseLock(req.TargetType, req.TargetID, token)

	// 按类型执行白名单删除动作。
	switch req.TargetType {
	// 删视频。
	case TargetTypeVideo:
		// DeleteVideo 按主键 DELETE；对象已不存在时 GORM 也不报错（RowsAffected=0），天然幂等。
		if err := x.videoRepo.DeleteVideo(ctx, req.TargetID); err != nil {
			// DB 故障：返回 error 重试（锁会随 defer 释放）。
			return ActionResult{}, err
		}
		// 删除后失效视频详情缓存，避免"库已删、缓存还在"。
		x.invalidateVideoCache(req.TargetID)

	// 删评论。
	case TargetTypeComment:
		// 删除评论的仓储方法需要整条评论对象，先按 ID 查出来。
		cm, err := x.commentRepo.GetByID(ctx, req.TargetID)
		// 查询出错：执行期故障。
		if err != nil {
			// 返回 error 重试。
			return ActionResult{}, err
		}
		// cm 为 nil：评论已经不存在（可能已被别的举报删掉），按幂等成功处理。
		if cm == nil {
			// 自动完成。
			return ActionResult{AutoDone: true, Action: "remove:comment"}, nil
		}
		// 删除评论；出错返回重试。
		if err := x.commentRepo.DeleteComment(ctx, cm); err != nil {
			// 执行期故障。
			return ActionResult{}, err
		}
	}

	// 成功：自动处置完成，动作标识 remove:video / remove:comment。
	return ActionResult{AutoDone: true, Action: "remove:" + req.TargetType}, nil
}

// executeWarn 方法：warn 动作的闸门校验 + 抢锁 + 发送警告私信。
//
// 参数：ctx、req；
// 返回值：ActionResult、error（执行期故障）。
func (x *ActionExecutor) executeWarn(ctx context.Context, req ActionRequest) (ActionResult, error) {
	// 闸门 1：必须能确定警告发给谁（证据里解析出的作者/被举报用户）。
	if req.WarnUserID == 0 {
		// 接收者未知：转人工。
		return escalateResult("warn 无法确定警告接收者（证据缺失）"), nil
	}
	// 闸门 2：置信度 ≥ warn 阈值（默认 0.7）。
	if req.Confidence < x.thresholdWarn {
		// 不够确信：转人工。
		return escalateResult(fmt.Sprintf("warn 置信度 %.2f 低于阈值 %.2f", req.Confidence, x.thresholdWarn)), nil
	}
	// 闸门 3：policy_hit 非空。
	if req.PolicyHit == "" {
		// 转人工。
		return escalateResult("warn 缺少 policy_hit"), nil
	}
	// 闸门 4：证据快照必须存在。
	if req.EvidenceSnapshot == "" {
		// 转人工。
		return escalateResult("warn 缺少证据快照"), nil
	}

	// 锁仍然加在【被举报对象】上而不是用户上：
	// 这样对同一对象的 remove 和 warn 彼此互斥，不会一边删一边发警告。
	token, ok, err := x.acquireLock(ctx, req.TargetType, req.TargetID)
	// Redis 命令出错：执行期故障。
	if err != nil {
		// 返回重试。
		return ActionResult{}, err
	}
	// 没抢到：转人工。
	if !ok {
		// 转人工。
		return escalateResult("未获得处置锁（Redis 不可用，或其他流程正在处置该对象）"), nil
	}
	// defer 释放锁。
	defer x.releaseLock(req.TargetType, req.TargetID, token)

	// m 组装警告私信：平台官方 → 作者，模板文案填入对象类型/编号/命中条款。
	m := &message.Message{
		// FromID 平台官方（0）。
		FromID: systemAccountID,
		// ToID 被警告的作者。
		ToID: req.WarnUserID,
		// Content 模板文案。
		Content: fmt.Sprintf(warnMessageTemplate, req.TargetType, req.TargetID, req.PolicyHit),
	}
	// Send 写库（内部会 trim、盖服务器时间）；失败为执行期故障，重试。
	if err := x.messageRepo.Send(ctx, m); err != nil {
		// 返回 error。
		return ActionResult{}, err
	}

	// 成功：自动处置完成，动作标识 warn:user:<编号>。
	return ActionResult{AutoDone: true, Action: fmt.Sprintf("warn:user:%d", req.WarnUserID)}, nil
}

// lockKey 方法：拼防重复锁的键名。
//
// 用 cache.Key 拼而不是直接写字符串，是为了带上客户端统一的 key 前缀（如 v1:），
// 最终键形如 v1:lock:moderation:video:42。
func (x *ActionExecutor) lockKey(targetType string, targetID uint) string {
	// Key 按模板生成。
	return x.cache.Key("lock:moderation:%s:%d", targetType, targetID)
}

// acquireLock 方法：尝试加防重复锁。
//
// 参数：ctx、targetType、targetID；
// 返回值：token 锁凭证、ok 是否抢到、err Redis 命令错误。
//
// 约定：cache 为 nil（Redis 没连上）时返回 ok=false 且无错误，
// 调用方据此转人工——没有防重复保障，就不做自动处置。
func (x *ActionExecutor) acquireLock(ctx context.Context, targetType string, targetID uint) (string, bool, error) {
	// Redis 客户端不可用。
	if x.cache == nil {
		// 没抢到、无错误（区别于 Redis 命令故障）。
		return "", false, nil
	}
	// Lock 底层是 SET key token NX PX（原子抢锁 + 自动过期）。
	return x.cache.Lock(ctx, x.lockKey(targetType, targetID), actionLockTTL)
}

// releaseLock 方法：用 token 安全释放锁。
//
// 解锁用 background 上下文：处置已经完成，释放动作不依附任何请求/任务的生命周期。
func (x *ActionExecutor) releaseLock(targetType string, targetID uint, token string) {
	// Redis 客户端不可用：锁本来就没加成，无事可做。
	if x.cache == nil {
		// 返回。
		return
	}
	// Unlock 用 Lua 脚本"先比对 token 再 DEL"，避免误删别人的锁；错误忽略（锁有 TTL 兜底）。
	_ = x.cache.Unlock(context.Background(), x.lockKey(targetType, targetID), token)
}

// invalidateVideoCache 方法：删除视频详情缓存。
//
// 参数 id：视频编号。
// 和用户主动删视频时 VideoService.Delete 里的缓存清理保持同一套键规则。
func (x *ActionExecutor) invalidateVideoCache(id uint) {
	// 缓存不可用：无需清理。
	if x.cache == nil {
		// 返回。
		return
	}
	// cacheKey 详情缓存键 video:detail:id=<id>（Key 会带上统一前缀）。
	cacheKey := x.cache.Key("video:detail:id=%d", id)
	// Del 用 background 上下文；错误忽略——缓存 TTL 只有 5 分钟，最多短暂残留。
	_ = x.cache.Del(context.Background(), cacheKey)
}

// ResolveTargetAuthorID 方法：实时查对象的作者编号（审核 approve/warn 前确定警告接收者）。
//
// 不从证据快照里读，而是重新查库：审核可能发生在取证很久之后，以当前数据为准。
//
// 参数：ctx、targetType、targetID；
// 返回值：作者编号（user 对象返回其本人 ID）、error（对象已删除时返回错误）。
func (x *ActionExecutor) ResolveTargetAuthorID(ctx context.Context, targetType string, targetID uint) (uint, error) {
	// switch 按对象类型。
	switch targetType {
	// 视频。
	case TargetTypeVideo:
		// v 查视频；err。
		v, err := x.videoRepo.GetByID(ctx, targetID)
		// 查询故障。
		if err != nil {
			// 返回。
			return 0, err
		}
		// 视频不存在（可能已被处置）。
		if v == nil {
			// 返回。
			return 0, errors.New("视频已不存在")
		}
		// 返回作者。
		return v.AuthorID, nil
	// 评论。
	case TargetTypeComment:
		// cm 查评论。
		cm, err := x.commentRepo.GetByID(ctx, targetID)
		// 故障。
		if err != nil {
			// 返回。
			return 0, err
		}
		// 评论不存在。
		if cm == nil {
			// 返回。
			return 0, errors.New("评论已不存在")
		}
		// 返回评论作者。
		return cm.AuthorID, nil
	// 用户：作者即其本人。
	case TargetTypeUser:
		// 直接返回对象编号。
		return targetID, nil
	// 非法类型。
	default:
		// 返回。
		return 0, fmt.Errorf("非法对象类型: %s", targetType)
	}
}

// humanPolicyFallback 是人工终审警告时，审核员没填命中条款的兜底文案。
const humanPolicyFallback = "人工复核判定违规"

// HumanRemoveVideo 方法：审核员终审要求删视频时执行。
//
// 与自动路径 executeRemove 的区别：跳过置信度闸门（人就是最高权威），
// 但 Redis 防重复锁、白名单删除、详情缓存失效一律保留。
//
// 参数：ctx、id 视频编号；
// 返回值 error：抢锁/删除期故障。
func (x *ActionExecutor) HumanRemoveVideo(ctx context.Context, id uint) error {
	// 抢锁；token、ok、err。
	token, ok, err := x.acquireLock(ctx, TargetTypeVideo, id)
	// Redis 命令故障。
	if err != nil {
		// 返回。
		return err
	}
	// 没抢到锁：拒绝执行，提示审核员稍后重试（绝不绕过锁强删）。
	if !ok {
		// 返回。
		return errors.New("未获得处置锁，请稍后重试")
	}
	// defer 释放锁。
	defer x.releaseLock(TargetTypeVideo, id, token)
	// 删除；出错返回。
	if err := x.videoRepo.DeleteVideo(ctx, id); err != nil {
		// 返回。
		return err
	}
	// 失效详情缓存。
	x.invalidateVideoCache(id)
	// 成功。
	return nil
}

// HumanRemoveComment 方法：审核员终审要求删评论时执行。
//
// 参数：ctx、id 评论编号；
// 返回值 error：抢锁/查询/删除期故障。评论已不存在按幂等成功处理。
func (x *ActionExecutor) HumanRemoveComment(ctx context.Context, id uint) error {
	// 抢锁。
	token, ok, err := x.acquireLock(ctx, TargetTypeComment, id)
	// 故障返回。
	if err != nil {
		// 返回。
		return err
	}
	// 没抢到。
	if !ok {
		// 返回。
		return errors.New("未获得处置锁，请稍后重试")
	}
	// defer 释放。
	defer x.releaseLock(TargetTypeComment, id, token)
	// cm 查整条评论；err。
	cm, err := x.commentRepo.GetByID(ctx, id)
	// 查询故障。
	if err != nil {
		// 返回。
		return err
	}
	// 已不存在：幂等成功。
	if cm == nil {
		// 返回 nil。
		return nil
	}
	// 删除并返回其错误。
	return x.commentRepo.DeleteComment(ctx, cm)
}

// HumanWarn 方法：审核员终审要求警告作者时执行。
//
// 参数：ctx、userID 被警告用户、targetType/targetID 被举报对象（锁加在对象上）、
// policyHit 命中条款（空则用兜底文案）；
// 返回值 error：抢锁/发送期故障。
func (x *ActionExecutor) HumanWarn(ctx context.Context, userID uint, targetType string, targetID uint, policyHit string) error {
	// 接收者必填。
	if userID == 0 {
		// 返回。
		return errors.New("无法确定警告接收者")
	}
	// policyHit 为空：用兜底文案，保证通知里有"违反什么"的说明。
	if policyHit == "" {
		// 兜底。
		policyHit = humanPolicyFallback
	}
	// 锁加在被举报对象上，与该对象上的其他动作互斥。
	token, ok, err := x.acquireLock(ctx, targetType, targetID)
	// 故障返回。
	if err != nil {
		// 返回。
		return err
	}
	// 没抢到。
	if !ok {
		// 返回。
		return errors.New("未获得处置锁，请稍后重试")
	}
	// defer 释放。
	defer x.releaseLock(targetType, targetID, token)
	// m 组装警告私信：平台官方 → 用户，仍走同一套模板文案。
	m := &message.Message{
		// FromID 平台官方。
		FromID: systemAccountID,
		// ToID 被警告用户。
		ToID: userID,
		// Content 模板文案。
		Content: fmt.Sprintf(warnMessageTemplate, targetType, targetID, policyHit),
	}
	// 发送并返回其错误。
	return x.messageRepo.Send(ctx, m)
}

// NotifyResult 方法：给举报人发一条"举报处理结果"站内信（D10）。
//
// 实现 moderation.ResultNotifier 接口；走现有私信系统，FromID=0 平台官方。
//
// 参数：ctx、reporterID 举报人编号、caseID 工单编号、content 结果文案；
// 返回值 error：私信系统故障。
func (x *ActionExecutor) NotifyResult(ctx context.Context, reporterID, caseID uint, content string) error {
	// 举报人或文案缺失：不发送。
	if reporterID == 0 || content == "" {
		// 返回。
		return errors.New("举报人编号与结果文案不能为空")
	}
	// messageRepo 未配置（如未启用私信模块）：无法通知。
	if x.messageRepo == nil {
		// 返回。
		return errors.New("私信系统未配置")
	}
	// m 组装平台官方私信发送，返回其错误。
	return x.messageRepo.Send(ctx, &message.Message{
		// FromID 平台官方。
		FromID: systemAccountID,
		// ToID 举报人。
		ToID: reporterID,
		// Content 结果文案。
		Content: content,
	})
}

// ResolveWarnUserID 普通函数：从取证结果里解析"警告私信该发给谁"。
//
// 参数 ev：取证结果；
// 返回值 uint：视频/评论的作者编号，或被举报用户本人的编号；解析不出为 0。
func ResolveWarnUserID(ev *Evidence) uint {
	// 证据为空：解析不出。
	if ev == nil {
		// 0。
		return 0
	}
	// switch 按证据里实际取到的对象判断。
	switch {
	// 视频对象：作者 = 视频作者。
	case ev.Video != nil:
		// 返回视频作者。
		return ev.Video.AuthorID
	// 评论对象：作者 = 评论作者。
	case ev.Comment != nil:
		// 返回评论作者。
		return ev.Comment.AuthorID
	// 用户对象：警告其本人。
	case ev.TargetAccount != nil:
		// 返回该用户编号。
		return ev.TargetAccount.ID
	}
	// 对象详情缺失（取证阶段失败）：接收者未知。
	return 0
}

// escalateResult 普通函数：快速构造一个"转人工"结果。
//
// 参数 why：转人工原因；
// 返回值 ActionResult：Escalate=true 的结果。
func escalateResult(why string) ActionResult {
	// 返回。
	return ActionResult{Escalate: true, Reason: why}
}

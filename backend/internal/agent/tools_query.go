// package agent：本文件实现 4 个只读查询工具——
// 它们是对【现有仓储方法】的薄封装，Agent 不自己写 SQL。
//
//  1. get_video         查视频详情
//  2. get_video_comments 查某视频下的评论
//  3. get_account       查用户资料
//  4. count_target_reports 查某对象被举报的次数
package agent

// import 导入：
import (
	// account 用户仓储。
	"feedsystem_video_go/internal/account"
	// moderation 举报仓储。
	"feedsystem_video_go/internal/moderation"
	// video 视频/评论仓储。
	"feedsystem_video_go/internal/video"

	// context、encoding/json、fmt：执行/解析/报错。
	"context"
	"encoding/json"
	"fmt"
)

// jsonObject 是便捷类型：用 map 装任意 JSON 对象（如计数结果）。
type jsonObject = map[string]any

// ------------------------------------------------------------------
// 1. get_video
// ------------------------------------------------------------------

// getVideoTool 工具：查视频详情，内部调 VideoRepository.GetByID。
type getVideoTool struct {
	// repo 视频仓储。
	repo *video.VideoRepository
}

// Name 返回工具名。
func (t *getVideoTool) Name() string { return "get_video" }

// Description 返回用途。
func (t *getVideoTool) Description() string {
	return "根据视频 ID 获取视频详情（标题、描述、作者、点赞数、热度、发布时间）。"
}

// Parameters 返回参数的 JSON Schema。
func (t *getVideoTool) Parameters() json.RawMessage {
	// 直接返回写好的 Schema。
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "video_id": {"type": "integer", "description": "视频 ID"}
  },
  "required": ["video_id"],
  "additionalProperties": false
}`)
}

// getVideoArgs 是该工具的参数结构。
type getVideoArgs struct {
	// VideoID 视频编号。
	VideoID uint `json:"video_id"`
}

// Execute 解析参数、查视频。
func (t *getVideoTool) Execute(ctx context.Context, raw json.RawMessage) (any, error) {
	// args 接收参数。
	var args getVideoArgs
	// 解析；失败返回错误。
	if err := json.Unmarshal(raw, &args); err != nil {
		// 返回。
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	// ID 必须有效。
	if args.VideoID == 0 {
		// 返回。
		return nil, fmt.Errorf("video_id is required")
	}
	// 调仓储；返回视频实体（其 json tag 决定输出字段）。
	return t.repo.GetByID(ctx, args.VideoID)
}

// ------------------------------------------------------------------
// 2. get_video_comments
// ------------------------------------------------------------------

// getCommentsTool 工具：查某视频下的评论，调 CommentRepository.GetAllComments。
type getCommentsTool struct {
	// repo 评论仓储。
	repo *video.CommentRepository
}

// Name 返回工具名。
func (t *getCommentsTool) Name() string { return "get_video_comments" }

// Description 返回用途。
func (t *getCommentsTool) Description() string {
	return "获取指定视频下的评论列表（用于判断评论区是否存在违规或大量负面反馈）。"
}

// Parameters 返回参数 Schema。
func (t *getCommentsTool) Parameters() json.RawMessage {
	// 返回。
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "video_id": {"type": "integer", "description": "视频 ID"}
  },
  "required": ["video_id"],
  "additionalProperties": false
}`)
}

// getCommentsArgs 参数结构。
type getCommentsArgs struct {
	// VideoID 视频编号。
	VideoID uint `json:"video_id"`
}

// Execute 解析参数、查评论。
func (t *getCommentsTool) Execute(ctx context.Context, raw json.RawMessage) (any, error) {
	// args。
	var args getCommentsArgs
	// 解析。
	if err := json.Unmarshal(raw, &args); err != nil {
		// 返回。
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	// ID 校验。
	if args.VideoID == 0 {
		// 返回。
		return nil, fmt.Errorf("video_id is required")
	}
	// 调仓储取评论。
	return t.repo.GetAllComments(ctx, args.VideoID)
}

// ------------------------------------------------------------------
// 3. get_account
// ------------------------------------------------------------------

// getAccountTool 工具：查用户资料，调 AccountRepository.FindByID。
type getAccountTool struct {
	// repo 用户仓储。
	repo *account.AccountRepository
}

// Name 返回工具名。
func (t *getAccountTool) Name() string { return "get_account" }

// Description 返回用途。
func (t *getAccountTool) Description() string {
	return "根据用户 ID 获取账号资料（用户名、头像、简介）；用于了解被举报内容的作者。"
}

// Parameters 返回参数 Schema。
func (t *getAccountTool) Parameters() json.RawMessage {
	// 返回。
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "account_id": {"type": "integer", "description": "用户 ID"}
  },
  "required": ["account_id"],
  "additionalProperties": false
}`)
}

// getAccountArgs 参数结构。
type getAccountArgs struct {
	// AccountID 用户编号。
	AccountID uint `json:"account_id"`
}

// Execute 解析参数、查用户。
func (t *getAccountTool) Execute(ctx context.Context, raw json.RawMessage) (any, error) {
	// args。
	var args getAccountArgs
	// 解析。
	if err := json.Unmarshal(raw, &args); err != nil {
		// 返回。
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	// ID 校验。
	if args.AccountID == 0 {
		// 返回。
		return nil, fmt.Errorf("account_id is required")
	}
	// 调仓储；Account 的密码/Token 字段是 json:"-"，不会被输出，安全。
	return t.repo.FindByID(ctx, args.AccountID)
}

// ------------------------------------------------------------------
// 4. count_target_reports
// ------------------------------------------------------------------

// countReportsTool 工具：查某对象被举报的次数，调 ReportRepository.CountByTarget。
type countReportsTool struct {
	// repo 举报仓储。
	repo *moderation.ReportRepository
}

// Name 返回工具名。
func (t *countReportsTool) Name() string { return "count_target_reports" }

// Description 返回用途。
func (t *countReportsTool) Description() string {
	return "统计某个对象（视频/评论/用户）历史上被举报过多少次；举报频繁说明风险更高。"
}

// Parameters 返回参数 Schema。
func (t *countReportsTool) Parameters() json.RawMessage {
	// 返回。
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "target_type": {"type": "string", "enum": ["video", "comment", "user"]},
    "target_id":   {"type": "integer"}
  },
  "required": ["target_type", "target_id"],
  "additionalProperties": false
}`)
}

// countReportsArgs 参数结构。
type countReportsArgs struct {
	// TargetType 对象类型。
	TargetType string `json:"target_type"`
	// TargetID 对象编号。
	TargetID uint `json:"target_id"`
}

// Execute 解析参数、统计举报次数。
func (t *countReportsTool) Execute(ctx context.Context, raw json.RawMessage) (any, error) {
	// args。
	var args countReportsArgs
	// 解析。
	if err := json.Unmarshal(raw, &args); err != nil {
		// 返回。
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	// 参数校验。
	if args.TargetType == "" || args.TargetID == 0 {
		// 返回。
		return nil, fmt.Errorf("target_type 与 target_id 都是必填")
	}
	// 白名单校验：target_type 只允许三个固定取值，拒绝模型传入任何其他文本。
	if args.TargetType != TargetTypeVideo &&
		args.TargetType != TargetTypeComment &&
		args.TargetType != TargetTypeUser {
		// 返回。
		return nil, fmt.Errorf("target_type 非法，只允许 video/comment/user: %q", args.TargetType)
	}
	// count 调仓储计数；err。
	count, err := t.repo.CountByTarget(ctx, args.TargetType, args.TargetID)
	if err != nil {
		// 返回。
		return nil, err
	}
	// 用对象包装计数返回。
	return jsonObject{"target_type": args.TargetType, "target_id": args.TargetID, "report_count": count}, nil
}

// 下面是 4 个工具的【导出构造函数】：供外部（worker 装配处）创建工具，
// 工具具体类型保持包内私有，外部只面向 Tool 接口编程。

// NewGetVideoTool 创建 get_video 工具。
//
// 参数 repo：视频仓储；返回值 Tool。
// &getVideoTool相当于可以访问这个结构体内部的所有字段和方法，但外部无法直接访问 getVideoTool 的字段。
func NewGetVideoTool(repo *video.VideoRepository) Tool {
	// 返回。
	return &getVideoTool{repo: repo}
}

// NewGetCommentsTool 创建 get_video_comments 工具。
func NewGetCommentsTool(repo *video.CommentRepository) Tool {
	// 返回。
	return &getCommentsTool{repo: repo}
}

// NewGetAccountTool 创建 get_account 工具。
func NewGetAccountTool(repo *account.AccountRepository) Tool {
	// 返回。
	return &getAccountTool{repo: repo}
}

// NewCountReportsTool 创建 count_target_reports 工具。
func NewCountReportsTool(repo *moderation.ReportRepository) Tool {
	// 返回。
	return &countReportsTool{repo: repo}
}

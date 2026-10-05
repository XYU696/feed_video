// package video：视频业务包。本文件是评论服务层，
// 结构和点赞服务高度相似：异步优先、双路降级；
// 额外有两个亮点：删除时的"归属校验"，以及 @某人 的提及通知。
package video

import (
	// context：请求上下文。
	"context"
	// errors：创建/识别错误。
	"errors"
	// apierror：删除越权时返回 ErrUnauthorized。
	"feedsystem_video_go/internal/apierror"
	// rabbitmq：评论 MQ、热度 MQ。
	"feedsystem_video_go/internal/middleware/rabbitmq"
	// rediscache：Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"
	// log：提及通知写库失败时记日志。
	"log"
	// regexp：解析评论里的 @用户名。
	"regexp"
	// strings：去空白。
	"strings"

	// gorm：事务、模型。
	"gorm.io/gorm"
)

// CommentService 结构体是评论服务层。
type CommentService struct {
	// repo 评论仓储。
	repo *CommentRepository
	// VideoRepository 视频仓储（大写可包外访问），校验视频存在。
	VideoRepository *VideoRepository
	// cache Redis 客户端，可能为 nil。
	cache *rediscache.Client
	// commentMQ 评论事件发布器，可能为 nil。
	commentMQ *rabbitmq.CommentMQ
	// popularityMQ 热度事件发布器，可能为 nil。
	popularityMQ *rabbitmq.PopularityMQ
}

// NewCommentService 是构造函数：创建评论服务，注入五个依赖。
//
// 返回值 *CommentService：服务。
func NewCommentService(repo *CommentRepository, videoRepo *VideoRepository, cache *rediscache.Client, commentMQ *rabbitmq.CommentMQ, popularityMQ *rabbitmq.PopularityMQ) *CommentService {
	// 装配并返回指针。
	return &CommentService{repo: repo, VideoRepository: videoRepo, cache: cache, commentMQ: commentMQ, popularityMQ: popularityMQ}
}

// Publish 方法：发表评论的完整业务逻辑。
//
// 流程：参数校验 → 确认视频存在 → 尝试发评论 MQ + 热度 MQ
// → 都成功则处理 @提及 后返回 → 失败侧走同步降级。
//
// 参数 comment：评论对象；
// 返回值 error：错误。
func (s *CommentService) Publish(ctx context.Context, comment *Comment) error {
	// 空对象。
	if comment == nil {
		// 返回错误。
		return errors.New("comment is nil")
	}
	// 清洗用户名、内容的首尾空白。
	comment.Username = strings.TrimSpace(comment.Username)
	// 清洗内容。
	comment.Content = strings.TrimSpace(comment.Content)
	// 视频编号或作者编号缺失。
	if comment.VideoID == 0 || comment.AuthorID == 0 {
		// 返回错误。
		return errors.New("video_id and author_id are required")
	}
	// 内容为空。
	if comment.Content == "" {
		// 返回错误（不允许发空评论刷屏）。
		return errors.New("content is required")
	}

	// exists 校验评论所属视频真实存在；err 接收错误。
	exists, err := s.VideoRepository.IsExist(ctx, comment.VideoID)
	if err != nil {
		// 查询出错：返回。
		return err
	}
	// 视频不存在。
	if !exists {
		// 返回错误。
		return errors.New("video not found")
	}

	// 两个入队标记（同点赞服务）。
	mysqlEnqueued := false
	// redisEnqueued 热度入队标记。
	redisEnqueued := false
	// 评论 MQ 可用时发布评论事件。
	if s.commentMQ != nil {
		// s.commentMQ.Publish(用户名, 视频, 作者, 内容) 发布 comment.publish 事件；成功才标记。
		if err := s.commentMQ.Publish(ctx, comment.Username, comment.VideoID, comment.AuthorID, comment.Content); err == nil {
			// 标记落库事件已入队（CommentWorker 异步写 comments 表）。
			mysqlEnqueued = true
		}
	}
	// 热度 MQ：发评论给热度 +1。
	if s.popularityMQ != nil {
		// Update(视频, 1)；成功才标记。
		if err := s.popularityMQ.Update(ctx, comment.VideoID, 1); err == nil {
			// 标记热度事件已入队。
			redisEnqueued = true
		}
	}
	// 两条 MQ 都成功。
	if mysqlEnqueued && redisEnqueued {
		// s.notifyMentions：处理评论里 @某人 的通知（异步路径也要通知）。
		s.notifyMentions(ctx, comment)
		// 返回 nil。
		return nil
	}

	// Fallback: direct MySQL write when comment MQ publish fails.
	// 评论事件没入队：同步事务直写。
	if !mysqlEnqueued {
		// 开事务；err 接收结果。
		if err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// 第 1 步：事务里确认视频存在。
			if err := tx.Select("id").First(&Video{}, comment.VideoID).Error; err != nil {
				// 视频不存在。
				if errors.Is(err, gorm.ErrRecordNotFound) {
					// 返回错误 → 回滚。
					return errors.New("video not found")
				}
				// 其他错误 → 回滚。
				return err
			}
			// 第 2 步：插入评论。
			if err := tx.Create(comment).Error; err != nil {
				// 失败 → 回滚。
				return err
			}
			// 第 3 步：视频热度 +1（发评论也加热度）；返回错误决定提交/回滚。
			return tx.Model(&Video{}).Where("id = ?", comment.VideoID).
				// popularity + 1。
				UpdateColumn("popularity", gorm.Expr("popularity + 1")).Error
		}); err != nil {
			// 事务失败：返回。
			return err
		}
	}

	// Fallback: direct Redis update when popularity MQ publish fails.
	// 热度事件没入队：直接更新 Redis，增量 +1。
	if !redisEnqueued {
		// UpdatePopularityCache 直写缓存。
		UpdatePopularityCache(ctx, s.cache, comment.VideoID, 1)
	}
	// 同步降级路径同样处理 @提及 通知。
	s.notifyMentions(ctx, comment)
	// 完成：返回 nil。
	return nil
}

// Delete 方法：删除评论的业务逻辑，关键是【归属校验】。
//
// 规则：只有评论作者本人才能删自己的评论（comment.AuthorID == 当前登录账号）。
//
// 参数：commentID 要删的评论编号、accountID 当前登录账号编号；
// 返回值 error：错误（不是作者返回 apierror.ErrUnauthorized）。
func (s *CommentService) Delete(ctx context.Context, commentID uint, accountID uint) error {
	// comment 按 ID 查到评论（查不到时仓储返回 nil, nil）；err 接收错误。
	comment, err := s.repo.GetByID(ctx, commentID)
	if err != nil {
		// 查询出错：返回。
		return err
	}
	// 评论为 nil：不存在。
	if comment == nil {
		// 返回错误。
		return errors.New("comment not found")
	}
	// comment.AuthorID != accountID：当前用户不是这条评论的作者。
	if comment.AuthorID != accountID {
		// 返回"未授权"——水平越权防护：不能删别人的评论。
		return apierror.ErrUnauthorized
	}
	// 优先走 MQ：发评论删除事件，由 CommentWorker 异步删库。
	if s.commentMQ != nil {
		// s.commentMQ.Delete(评论编号) 发布删除事件；err == nil 成功。
		if err := s.commentMQ.Delete(ctx, commentID); err == nil {
			// 已交给 worker：返回 nil。
			return nil
		}
	}
	// MQ 没配或发布失败：同步直接删除评论，返回其错误。
	return s.repo.DeleteComment(ctx, comment)
}

// GetAll 方法：查某视频全部评论，先校验视频存在。
//
// 参数 videoID：视频编号；
// 返回值：[]Comment 评论列表、error 错误。
func (s *CommentService) GetAll(ctx context.Context, videoID uint) ([]Comment, error) {
	// exists 校验视频存在；err 接收错误。
	exists, err := s.VideoRepository.IsExist(ctx, videoID)
	if err != nil {
		// 出错：返回 nil。
		return nil, err
	}
	// 视频不存在。
	if !exists {
		// 返回错误。
		return nil, errors.New("video not found")
	}
	// 返回仓储的评论查询结果。
	return s.repo.GetAllComments(ctx, videoID)
}

// mentionRegex 是包级正则：匹配评论里的 "@用户名"。
//
// regexp.MustCompile 在程序启动时就把正则编译好（编译失败会 panic，适合写死的正确正则）。
// \w+ 匹配字母/数字/下划线组成的用户名；第 1 个捕获组（括号里）就是用户名本身。
var mentionRegex = regexp.MustCompile(`@(\w+)`)

// notifyMentions 方法：扫描评论里的 @用户名，给每个被提及且存在的用户写一条通知。
//
// 参数：comment 刚发表的评论（从内容里找 @、从作者字段知道是谁发的）；无返回值。
func (s *CommentService) notifyMentions(ctx context.Context, comment *Comment) {
	// matches 接收所有匹配结果。
	// FindAllStringSubmatch(内容, -1)：-1 表示找全部；每个元素是一个切片，
	// matches[i][0] 是整段 "@张三"，matches[i][1] 是捕获组 "张三"。
	matches := mentionRegex.FindAllStringSubmatch(comment.Content, -1)
	// len(matches) == 0：没有 @任何人。
	if len(matches) == 0 {
		// 直接返回。
		return
	}
	// seen 创建去重映射：同一条评论里重复 @同一个人只通知一次。
	seen := make(map[string]bool)
	// for range 遍历每个匹配；m 是当前匹配切片。
	// 对每个被 @ 的人：
	// ① 按用户名去 accounts 表查 ID（@了不存在的人就跳过）
	// ② 去重（重复 @同一个人只通知一次；@自己也不通知）
	// ③ 往 notifications 表写一条："小王 在评论中提到了你"
	for _, m := range matches {
		// username 取捕获组里的用户名（m[1]）。
		username := m[1]
		// 已经通知过该用户，或 @的是评论作者自己（不用通知自己）。
		if seen[username] || username == comment.Username {
			// 跳过本次。
			continue
		}
		// 标记已处理该用户名。
		seen[username] = true
		// accID 准备接收被提及用户的编号（零值 0 表示还没查到）。
		var accID uint
		// 直接查 accounts 表：按用户名取 id，扫描进 accID。
		// .Table("accounts") 指定表、.Select("id") 只取主键列；err 接收错误。
		if err := s.repo.db.WithContext(ctx).Table("accounts").Where("username = ?", username).Select("id").Scan(&accID).Error; err != nil || accID == 0 {
			// 查不到这个用户（@了个不存在的名字）或出错：跳过，不影响评论本身。
			continue
		}
		// notif 用一个【匿名结构体】临时组装通知记录（不需要提前定义类型，用完即弃）。
		notif := struct {
			// RecipientID 接收通知的人（被 @ 的用户）。
			RecipientID uint
			// SenderID 发起者（评论作者）。
			SenderID uint
			// Type 通知类型 "mention"。
			Type string
			// TargetID 关联目标（视频编号）。
			TargetID uint
			// Content 通知文案。
			Content string
		}{
			// 填入接收者编号。
			RecipientID: accID,
			// 填入发送者（评论作者）。
			SenderID: comment.AuthorID,
			// 类型标记为提及。
			Type: "mention",
			// 目标视频。
			TargetID: comment.VideoID,
			// 文案："某某 在评论中提到了你"。
			Content: comment.Username + " 在评论中提到了你",
		}
		// 把匿名结构体作为一行写进 notifications 表；err 接收错误。
		if err := s.repo.db.WithContext(ctx).Table("notifications").Create(&notif).Error; err != nil {
			// 写通知失败只记日志，不能让通知问题影响评论成功。
			log.Printf("create mention notification failed: %v", err)
		}
	}
}

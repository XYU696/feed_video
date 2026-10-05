// package agent：本文件负责"并发取证"。
//
// 业务理解：在让模型判定前，我们先替它把相关证据【一次性都取回来】——
// 被举报对象详情、作者资料、该对象下的评论、举报频次、作者近期内容。
// 这些查询彼此独立，若一个个串行查要 1.5s 以上；用 goroutine 并发后，
// 总耗时只取决于【最慢的那一个】，通常几百毫秒。
//
// 降级原则：单个数据源失败【不致命】。证据不全本身就是降置信度/转人工的理由，
// 所以我们把失败记进 Errors、把 Partial 置 true，绝不让一路失败拖垮整个取证。
package agent

// import 导入：
import (
	// account 用户仓储、实体。
	"feedsystem_video_go/internal/account"
	// moderation 举报仓储。
	"feedsystem_video_go/internal/moderation"
	// video 视频/评论仓储、实体。
	"feedsystem_video_go/internal/video"

	// context、fmt、sync、time：超时/拼错误/加锁/计时。
	"context"
	"fmt"
	"sync"
	"time"

	// errgroup：一组 goroutine + 等待它们全部结束。
	"golang.org/x/sync/errgroup"
)

// 对象类型常量（与 moderation 包取值一致；agent 包内自定义，避免反向依赖 moderation）。
const (
	// TargetTypeVideo 对象是视频。
	TargetTypeVideo = "video"
	// TargetTypeComment 对象是评论。
	TargetTypeComment = "comment"
	// TargetTypeUser 对象是用户。
	TargetTypeUser = "user"
)

// sourceTimeout 是【单个数据源】的独立短超时：某一路卡住最多等这么久，不拖慢整体。
const sourceTimeout = 800 * time.Millisecond

// recentVideoLimit 取证时取作者最近多少条视频。
const recentVideoLimit = 10

// Evidence 是汇总后的取证结果（随后会 JSON 序列化成证据快照）。
type Evidence struct {
	// CollectedAt 取证完成时刻。
	CollectedAt time.Time `json:"collected_at"`
	// Partial 是否为"部分证据"：有任意一路失败即为 true。
	Partial bool `json:"partial"`
	// TargetType 被举报对象类型。
	TargetType string `json:"target_type"`
	// TargetID 被举报对象编号。
	TargetID uint `json:"target_id"`

	// Video 当对象是视频时的详情。
	Video *video.Video `json:"video,omitempty"`
	// Comment 当对象是评论时的详情。
	Comment *video.Comment `json:"comment,omitempty"`
	// TargetAccount 当对象是用户时，该用户资料。
	TargetAccount *account.Account `json:"target_account,omitempty"`

	// Author 被举报内容作者的资料（对象是用户时即其本人）。
	Author *account.Account `json:"author,omitempty"`
	// VideoComments 视频对象下的评论（仅视频有）。
	VideoComments []video.Comment `json:"video_comments,omitempty"`
	// ReportCount 该对象历史被举报次数。
	ReportCount int64 `json:"report_count"`
	// AuthorRecentVideos 作者近期发布的视频。
	AuthorRecentVideos []video.Video `json:"author_recent_videos,omitempty"`

	// Errors 各数据源失败原因（有内容时 Partial 必为 true）。
	Errors []string `json:"errors,omitempty"`
}

// EvidenceCollector 取证器，持有取证要用的全部仓储。
type EvidenceCollector struct {
	// videoRepo 视频仓储。
	videoRepo *video.VideoRepository
	// commentRepo 评论仓储。
	commentRepo *video.CommentRepository
	// accountRepo 用户仓储。
	accountRepo *account.AccountRepository
	// reportRepo 举报仓储。
	reportRepo *moderation.ReportRepository
}

// NewEvidenceCollector 构造函数：创建取证器。
func NewEvidenceCollector(
	// videoRepo。
	videoRepo *video.VideoRepository,
	// commentRepo。
	commentRepo *video.CommentRepository,
	// accountRepo。
	accountRepo *account.AccountRepository,
	// reportRepo。
	reportRepo *moderation.ReportRepository,
) *EvidenceCollector {
	// 装配返回。
	return &EvidenceCollector{
		videoRepo: videoRepo, commentRepo: commentRepo, accountRepo: accountRepo, reportRepo: reportRepo,
	}
}

// fail 方法：记录一路取证失败——加锁追加错误、置 Partial=true。
//
// 参数：ev 证据、mu 保护 Errors 的锁、name 数据源名、err 错误。
func fail(ev *Evidence, mu *sync.Mutex, name string, err error) {
	// 加锁：多 goroutine 可能同时失败。
	mu.Lock()
	// 函数结束解锁。
	defer mu.Unlock()
	// 标记证据不完整。
	ev.Partial = true
	// 追加可读的错误原因。
	ev.Errors = append(ev.Errors, fmt.Sprintf("%s: %v", name, err))
}

// Collect 方法：对一条举报执行两阶段并发取证，返回汇总证据。
//
// 参数：ctx 生命周期、in 举报信息；
// 返回值 *Evidence：永不为 nil（即使全部失败也返回带 Errors 的证据）。
//
// 为什么分两阶段：第二阶段的"作者资料"要先知道作者 ID，
// 而作者 ID 藏在第一阶段取回的"对象详情"里，存在依赖，故先取详情、再 fan-out 其余。
func (c *EvidenceCollector) Collect(ctx context.Context, in ReportInput) *Evidence {
	// ev 初始化证据骨架。
	ev := &Evidence{TargetType: in.TargetType, TargetID: in.TargetID}
	// mu 保护 Errors/Partial 的并发写入。
	var mu sync.Mutex

	// —— 阶段 1：并发取"被举报对象详情"和"该对象举报频次"（两者互不依赖）。——
	// g1 一组并发任务；gctx 派生上下文。
	g1, gctx := errgroup.WithContext(ctx)
	// 任务 A：对象详情（按类型分流）。
	g1.Go(func() error {
		// sctx 单源超时；cancel 释放。
		sctx, cancel := context.WithTimeout(gctx, sourceTimeout)
		// defer。
		defer cancel()
		// 按类型取详情，失败记录、不中断。
		c.fetchTargetDetail(sctx, ev, &mu, in)
		// 始终返回 nil：见本函数末"为何不用 errgroup 的失败即取消"。
		return nil
	})
	// 任务 B：举报频次。
	g1.Go(func() error {
		// sctx。
		sctx, cancel := context.WithTimeout(gctx, sourceTimeout)
		// defer。
		defer cancel()
		// count 统计；err。
		count, err := c.reportRepo.CountByTarget(sctx, in.TargetType, in.TargetID)
		// 失败记录。
		if err != nil {
			// 记录。
			fail(ev, &mu, "report_count", err)
		} else {
			// 成功写入（ReportCount 是本任务独占字段，无需加锁）。
			ev.ReportCount = count
		}
		// nil。
		return nil
	})
	// 等阶段 1 全部结束。
	_ = g1.Wait()

	// —— 从对象详情解析作者 ID，并决定阶段 2 要查什么。——
	// authorID 作者编号；fetchComments 是否查视频评论。
	authorID, fetchComments := resolveAuthor(ev)

	// —— 阶段 2：并发取"作者资料 / 作者近期视频 / 视频评论"。——
	g2, gctx := errgroup.WithContext(ctx)
	// 作者资料：对象是用户时作者即本人，已在阶段 1 取到，跳过。
	if ev.Author == nil && authorID != 0 {
		// 闭包捕获本轮 authorID。
		id := authorID
		// 任务。
		g2.Go(func() error {
			// sctx。
			sctx, cancel := context.WithTimeout(gctx, sourceTimeout)
			// defer。
			defer cancel()
			// a 查用户；err。
			a, err := c.accountRepo.FindByID(sctx, id)
			// 失败/查无此人记录。
			if err != nil || a == nil {
				// 区分 nil 与错误。
				if err == nil {
					// 查无此人。
					err = fmt.Errorf("account %d 不存在", id)
				}
				// 记录。
				fail(ev, &mu, "author", err)
			} else {
				// 写入作者（Author 字段本任务独占）。
				ev.Author = a
			}
			// nil。
			return nil
		})
	}
	// 作者近期视频：作者 ID 已知时查询。
	if authorID != 0 {
		// 捕获。
		id := authorID
		// 任务。
		g2.Go(func() error {
			// sctx。
			sctx, cancel := context.WithTimeout(gctx, sourceTimeout)
			// defer。
			defer cancel()
			// vids 查最近视频；err。
			vids, err := c.videoRepo.ListRecentByAuthorID(sctx, id, recentVideoLimit)
			// 失败记录。
			if err != nil {
				// 记录。
				fail(ev, &mu, "author_recent_videos", err)
			} else {
				// 写入（独占字段）。
				ev.AuthorRecentVideos = vids
			}
			// nil。
			return nil
		})
	}
	// 视频评论：仅被举报对象是视频时查询。
	if fetchComments {
		// 任务。
		g2.Go(func() error {
			// sctx。
			sctx, cancel := context.WithTimeout(gctx, sourceTimeout)
			// defer。
			defer cancel()
			// list 查评论；err。
			list, err := c.commentRepo.GetAllComments(sctx, in.TargetID)
			// 失败记录。
			if err != nil {
				// 记录。
				fail(ev, &mu, "video_comments", err)
			} else {
				// 写入（独占字段）。
				ev.VideoComments = list
			}
			// nil。
			return nil
		})
	}
	// 等阶段 2 全部结束。
	_ = g2.Wait()

	// 记录取证完成时刻。
	ev.CollectedAt = time.Now().UTC()
	// 返回证据。
	return ev
}

// fetchTargetDetail 方法：阶段 1 按对象类型取详情，写入证据对应字段。
//
// 参数：ctx 单源超时、ev 证据、mu 锁、in 举报信息。
func (c *EvidenceCollector) fetchTargetDetail(ctx context.Context, ev *Evidence, mu *sync.Mutex, in ReportInput) {
	// switch 按类型。
	switch in.TargetType {
	// 视频。
	case TargetTypeVideo:
		// v 查视频；err。
		v, err := c.videoRepo.GetByID(ctx, in.TargetID)
		// 失败或已删除（nil,nil）。
		if err != nil || v == nil {
			// 区分。
			if err == nil {
				// 视频不存在。
				err = fmt.Errorf("video %d 不存在", in.TargetID)
			}
			// 记录。
			fail(ev, mu, "video", err)
		} else {
			// 写入。
			ev.Video = v
		}
	// 评论。
	case TargetTypeComment:
		// cm 查评论；err。
		cm, err := c.commentRepo.GetByID(ctx, in.TargetID)
		// 失败或不存在。
		if err != nil || cm == nil {
			// 区分。
			if err == nil {
				// 不存在。
				err = fmt.Errorf("comment %d 不存在", in.TargetID)
			}
			// 记录。
			fail(ev, mu, "comment", err)
		} else {
			// 写入。
			ev.Comment = cm
		}
	// 用户。
	case TargetTypeUser:
		// a 查用户；err。
		a, err := c.accountRepo.FindByID(ctx, in.TargetID)
		// 失败或不存在。
		if err != nil || a == nil {
			// 区分。
			if err == nil {
				// 不存在。
				err = fmt.Errorf("account %d 不存在", in.TargetID)
			}
			// 记录。
			fail(ev, mu, "target_account", err)
		} else {
			// 写入用户资料；对象是用户时作者即本人，顺带填好 Author。
			ev.TargetAccount = a
			// 作者本人。
			ev.Author = a
		}
	// 类型非法：取证阶段理论上不会发生（Service 已校验），兜底记录。
	default:
		// 记录。
		fail(ev, mu, "target", fmt.Errorf("非法对象类型: %s", in.TargetType))
	}
}

// resolveAuthor 普通函数：从阶段 1 的对象详情里解析作者 ID，
// 并判断阶段 2 是否需要查视频评论。
//
// 参数 ev：已含对象详情的证据；
// 返回值：authorID 作者编号（解析不出为 0）、fetchComments 是否查评论。
func resolveAuthor(ev *Evidence) (authorID uint, fetchComments bool) {
	// 按已取到的详情判断。
	switch {
	// 视频详情存在。
	case ev.Video != nil:
		// 作者 = 视频作者；需要查评论。
		return ev.Video.AuthorID, true
	// 评论详情存在。
	case ev.Comment != nil:
		// 作者 = 评论作者；评论对象不再查"其下评论"。
		return ev.Comment.AuthorID, false
	// 用户对象。
	case ev.TargetAccount != nil:
		// 作者即该用户本人；阶段 2 不用重复查作者资料（ev.Author 已填）。
		return ev.TargetAccount.ID, false
	}
	// 详情缺失（阶段 1 失败）：作者未知，阶段 2 仅能跳过依赖作者的查询。
	return 0, false
}

// 说明：为什么用了 errgroup，却让每个任务始终返回 nil，不利用它"出错即取消其余"？
// errgroup 默认一个任务返回错误就会取消组内其他任务。但取证的诉求恰恰相反——
// 我们希望【哪怕一路失败，其余几路也照常跑完】，尽量多收集证据。
// 所以失败只记录到证据里、任务返回 nil；errgroup 在这里主要提供
// "开一组 goroutine + Wait 全部结束"的便利，也方便面试时讲清它的取消语义与取舍。

// package video：视频业务包。本文件是【分片上传】处理器，
// 解决"大视频文件上传"的问题。
//
// 先通俗理解为什么要分片：一个 200MB 的视频，如果网络一抖整文件上传失败就得从头再来。
// 分片就是把大文件切成很多 5MB 的小块分别上传：
//   - 哪一块失败只重传那一块；
//   - 中途断网，下次可以查"已传哪些"接着传（断点续传）；
//   - 全部传完后，服务器按顺序把小块合并成完整 mp4。
//
// 上传进度（谁传了哪些块）存在 Redis 里，小块文件先存在临时目录。
package video

import (
	// crypto/md5：计算每个分片的 MD5，校验内容有没有传坏。
	"crypto/md5"
	// encoding/json：会话对象的序列化/反序列化。
	"encoding/json"
	// errors：定义哨兵错误。
	"errors"
	// fmt：拼字符串、格式化哈希。
	"fmt"
	// io：文件流复制（哈希计算、写盘、合并）。
	"io"
	// net/http：状态码。
	"net/http"
	// os：创建目录/文件、打开分片、删除临时文件。
	"os"
	// path/filepath：拼磁盘路径。
	"path/filepath"
	// time：会话有效期、时间戳、按日期分目录。
	"time"

	// jwt：取当前登录账号编号。
	"feedsystem_video_go/internal/middleware/jwt"
	// rediscache：Redis 客户端（分片上传必须依赖它）。
	rediscache "feedsystem_video_go/internal/middleware/redis"

	// gin：Web 框架。
	"github.com/gin-gonic/gin"
)

// sessionTTL 常量：一个上传会话在 Redis 里保留 24 小时。
const sessionTTL = 24 * time.Hour

// errChunkCacheUnavailable 哨兵错误：分片上传必须有 Redis，没有就用这个错误提示。
var errChunkCacheUnavailable = errors.New("chunk upload requires redis")

// ChunkUploadHandler 结构体是分片上传处理器。
type ChunkUploadHandler struct {
	// cache Redis 客户端，会话进度存这里（为 nil 时整个分片功能不可用）。
	cache *rediscache.Client
}

// NewChunkUploadHandler 是构造函数：创建分片上传处理器。
//
// 参数 cache：Redis 客户端；
// 返回值 *ChunkUploadHandler：处理器。
func NewChunkUploadHandler(cache *rediscache.Client) *ChunkUploadHandler {
	// 注入缓存并返回指针。
	return &ChunkUploadHandler{cache: cache}
}

// sessionKey 方法：根据上传编号拼出会话在 Redis 里的键。
//
// 参数 uploadID：本次上传的唯一编号；
// 返回值 string：键 chunk_upload:{uploadID}（自动带统一前缀）。
func (h *ChunkUploadHandler) sessionKey(uploadID string) string {
	// Key 方法格式化并加前缀，返回完整键。
	return h.cache.Key("chunk_upload:%s", uploadID)
}

// hashKey 方法：根据"账号 + 文件整体哈希"拼键，用于秒传/断点续传时找回原上传编号。
//
// 参数：accountID 账号编号、fileHash 整个文件的哈希；
// 返回值 string：键 chunk_upload_hash:{账号}:{文件哈希}。
func (h *ChunkUploadHandler) hashKey(accountID uint, fileHash string) string {
	// 返回完整键。
	return h.cache.Key("chunk_upload_hash:%d:%s", accountID, fileHash)
}

// getSession 方法：从 Redis 读出并还原一个上传会话。
//
// 参数：ctx 这里传的是 *gin.Context（用它的 Request.Context()）、uploadID 上传编号；
// 返回值：*ChunkUploadSession 会话对象、error 错误。
func (h *ChunkUploadHandler) getSession(ctx *gin.Context, uploadID string) (*ChunkUploadSession, error) {
	// 没配 Redis。
	if h.cache == nil {
		// 返回哨兵错误。
		return nil, errChunkCacheUnavailable
	}
	// b 接收会话的 JSON 字节；err 接收错误。GetBytes 读会话键。
	b, err := h.cache.GetBytes(ctx.Request.Context(), h.sessionKey(uploadID))
	if err != nil {
		// 读不到（会话过期或不存在）：返回提示。
		return nil, fmt.Errorf("upload session not found")
	}
	// s 准备接收反序列化结果。
	var s ChunkUploadSession
	// json.Unmarshal 把字节还原成会话结构体；err 接收错误。
	if err := json.Unmarshal(b, &s); err != nil {
		// 数据损坏：返回提示。
		return nil, fmt.Errorf("invalid session data")
	}
	// 返回会话。
	return &s, nil
}

// saveSession 方法：把上传会话序列化后写回 Redis（刷新 24h 有效期）。
//
// 参数：ctx *gin.Context、s 要保存的会话；
// 返回值 error：错误。
func (h *ChunkUploadHandler) saveSession(ctx *gin.Context, s *ChunkUploadSession) error {
	// 没配 Redis。
	if h.cache == nil {
		// 返回哨兵错误。
		return errChunkCacheUnavailable
	}
	// b 接收会话 JSON 字节；err 接收错误。
	b, err := json.Marshal(s)
	if err != nil {
		// 序列化失败：返回。
		return err
	}
	// SetBytes 写入会话键并设置 TTL，返回其错误。
	return h.cache.SetBytes(ctx.Request.Context(), h.sessionKey(s.UploadID), b, sessionTTL)
}

// InitChunkUpload 方法：处理"初始化分片上传"请求，返回上传编号。
//
// 如果同一账号上传过同样哈希的文件（断点续传/秒传场景），直接找回旧会话和已传列表。
//
// 参数 c：Gin 上下文。
func (h *ChunkUploadHandler) InitChunkUpload(c *gin.Context) {
	// 没配 Redis：功能不可用，返回 503。
	if h.cache == nil {
		// http.StatusServiceUnavailable = 503。
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errChunkCacheUnavailable.Error()})
		// 结束。
		return
	}

	// req 声明初始化请求结构体。
	var req InitChunkUploadRequest
	// 绑定并校验 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 出错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// maxSize 整个文件最大 200MB（200 << 20）。
	const maxSize = 200 << 20
	// 文件大小超限。
	if req.FileSize > maxSize {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "file size exceeds 200MB limit"})
		// 结束。
		return
	}

	// Check for existing session (resume)
	// hashKey 用账号+文件哈希拼"找回键"。
	hashKey := h.hashKey(accountID, req.FileHash)
	// existingID 尝试读出之前对应的上传编号；err 接收错误。
	existingID, err := h.cache.GetBytes(c.Request.Context(), hashKey)
	// 读到了且非空：之前传过同一文件。
	if err == nil && len(existingID) > 0 {
		// session 取回旧会话；sessErr 接收错误。
		session, sessErr := h.getSession(c, string(existingID))
		// 旧会话还在。
		if sessErr == nil {
			// Refresh TTL on resume
			// 刷新"找回键"的有效期，续传时不让它过期。
			_ = h.cache.SetBytes(c.Request.Context(), hashKey, existingID, sessionTTL)
			// 刷新会话本身的有效期。
			_ = h.saveSession(c, session)
			// 200 返回原上传编号，以及【已传分片编号列表】——前端据此跳过已传块，实现断点续传。
			c.JSON(http.StatusOK, gin.H{
				// upload_id 原编号，前端后续上传仍用它。
				"upload_id": session.UploadID,
				// uploaded_chunks 已成功上传的块编号列表。
				"uploaded_chunks": session.UploadedChunks(),
			})
			// 结束（不再新建会话）。
			return
		}
	}

	// id 生成 16 字节随机串（错误用 _ 忽略）。
	id, _ := randHex(16)
	// uploadID 在随机串后再拼当前纳秒时间戳，保证编号全局唯一。
	uploadID := id + fmt.Sprintf("%d", time.Now().UnixNano())
	// session 组装全新会话（指针）。
	session := &ChunkUploadSession{
		// UploadID 唯一编号。
		UploadID: uploadID,
		// AccountID 归属账号。
		AccountID: accountID,
		// Filename 原始文件名。
		Filename: req.Filename,
		// FileSize 整个文件大小。
		FileSize: req.FileSize,
		// ChunkSize 每块大小。
		ChunkSize: req.ChunkSize,
		// TotalChunks 总块数。
		TotalChunks: req.TotalChunks,
		// FileHash 整个文件哈希（续传找回用）。
		FileHash: req.FileHash,
		// UploadedBits：位图，make 创建长度=总块数的 bool 切片，初始全 false（一块都没传）。
		UploadedBits: make([]bool, req.TotalChunks),
	}

	// 把会话写进 Redis；err 接收错误。
	if err := h.saveSession(c, session); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		// 结束。
		return
	}

	// 再写"账号+文件哈希 → 上传编号"的找回键；err 接收错误。
	if err := h.cache.SetBytes(c.Request.Context(), hashKey, []byte(uploadID), sessionTTL); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		// 结束。
		return
	}

	// 200 返回新上传编号；已传列表给空切片 []（JSON 输出 [] 而非 null）。
	c.JSON(http.StatusOK, gin.H{
		// upload_id 新编号。
		"upload_id": uploadID,
		// uploaded_chunks 空。
		"uploaded_chunks": []int{},
	})
}

// UploadChunk 方法：处理"上传单个分片"请求，校验通过后把这一块存进临时目录并标记已传。
//
// 参数 c：Gin 上下文。
func (h *ChunkUploadHandler) UploadChunk(c *gin.Context) {
	// 没配 Redis：503。
	if h.cache == nil {
		// 返回错误。
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errChunkCacheUnavailable.Error()})
		// 结束。
		return
	}

	// req 声明分片上传请求（这里用 ShouldBind，因为块编号等可能走表单而不是 JSON）。
	var req UploadChunkRequest
	// c.ShouldBind(&req) 自动按内容类型绑定（表单/JSON 都能解析）；err 接收错误。
	if err := c.ShouldBind(&req); err != nil {
		// 参数错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// session 取回该上传编号对应的会话；err 接收错误。
	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		// 会话不存在/过期：404。
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// accountID 取当前登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 出错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 会话归属账号不是当前用户：防止别人往你的上传里塞分片。
	if session.AccountID != accountID {
		// 403 Forbidden。
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		// 结束。
		return
	}

	// 块编号越界：小于 0，或 >= 总块数。
	if req.ChunkIndex < 0 || req.ChunkIndex >= session.TotalChunks {
		// 400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk_index"})
		// 结束。
		return
	}

	// 位图显示这一块已经传过（重复上传）。
	if session.UploadedBits[req.ChunkIndex] {
		// 幂等：直接回成功，不重复落盘。
		c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
		// 结束。
		return
	}

	// f 取上传的分片文件（字段名 "file"）；err 接收错误。
	f, err := c.FormFile("file")
	if err != nil {
		// 没带文件：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		// 结束。
		return
	}

	// chunkFile 打开上传文件得到可读流；err 接收错误。
	chunkFile, err := f.Open()
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		// 结束。
		return
	}
	// defer 函数退出时关闭分片流。
	defer chunkFile.Close()

	// hash 创建一个 MD5 计算器。
	hash := md5.New()
	// io.Copy(hash, chunkFile)：把分片内容喂给 MD5 计算器（读完后文件流指针在末尾）；_ 忽略字节数；err 接收错误。
	if _, err := io.Copy(hash, chunkFile); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash chunk"})
		// 结束。
		return
	}
	// actualHash：hash.Sum(nil) 算出 MD5 摘要，%x 转成十六进制字符串。
	actualHash := fmt.Sprintf("%x", hash.Sum(nil))

	// 实际哈希和前端上报的不一致：说明这一块传坏了/被篡改。
	if actualHash != req.ChunkHash {
		// 400，并同时回传期望/实际哈希方便排查（要求重传这一块）。
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk hash mismatch", "expected": req.ChunkHash, "actual": actualHash})
		// 结束。
		return
	}

	// tmpDir 拼本次上传的临时目录 .run/uploads/tmp/{uploadID}。
	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	// MkdirAll 递归建临时目录；err 接收错误。
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create temp dir"})
		// 结束。
		return
	}

	// chunkPath 拼这一块的存盘路径，文件名直接用块编号（0、1、2……，合并时按编号排序）。
	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", req.ChunkIndex))
	// chunkFile.Seek(0, io.SeekStart)：刚才算哈希把流读到了末尾，这里把读指针移回开头，等下才能重新读到内容；seekErr 接收错误。
	if _, seekErr := chunkFile.Seek(0, io.SeekStart); seekErr != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read chunk"})
		// 结束。
		return
	}

	// dst 在目标路径创建一个空文件，准备写分片内容；err 接收错误。
	dst, err := os.Create(chunkPath)
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save chunk"})
		// 结束。
		return
	}
	// defer 关闭目标文件。
	defer dst.Close()

	// io.Copy(dst, chunkFile)：把分片内容写到磁盘文件；_ 忽略字节数；err 接收错误。
	if _, err := io.Copy(dst, chunkFile); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save chunk"})
		// 结束。
		return
	}

	// 把位图中该块编号位置标成 true。
	session.UploadedBits[req.ChunkIndex] = true
	// 保存更新后的会话；err 接收错误。
	if err := h.saveSession(c, session); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		// 结束。
		return
	}

	// 200 返回刚上传成功的块编号。
	c.JSON(http.StatusOK, gin.H{"chunk_index": req.ChunkIndex})
}

// ChunkStatus 方法：处理"查询上传进度"请求，返回已传块列表（断点续传靠它）。
//
// 参数 c：Gin 上下文。
func (h *ChunkUploadHandler) ChunkStatus(c *gin.Context) {
	// 没配 Redis：503。
	if h.cache == nil {
		// 返回错误。
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errChunkCacheUnavailable.Error()})
		// 结束。
		return
	}

	// req 声明进度查询请求。
	var req ChunkStatusRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// session 取回会话；err 接收错误。
	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		// 不存在：404。
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 出错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 不是会话归属者：403。
	if session.AccountID != accountID {
		// 禁止。
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		// 结束。
		return
	}

	// 200 返回上传编号、已传块列表、总块数。
	c.JSON(http.StatusOK, gin.H{
		// upload_id 上传编号。
		"upload_id": session.UploadID,
		// uploaded_chunks 已传块编号列表。
		"uploaded_chunks": session.UploadedChunks(),
		// total_chunks 总块数，前端据此显示进度。
		"total_chunks": session.TotalChunks,
	})
}

// CompleteChunkUpload 方法：处理"完成上传"请求——校验块齐全后，按顺序合并成完整 mp4，并清理临时数据。
//
// 参数 c：Gin 上下文。
func (h *ChunkUploadHandler) CompleteChunkUpload(c *gin.Context) {
	// 没配 Redis：503。
	if h.cache == nil {
		// 返回错误。
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errChunkCacheUnavailable.Error()})
		// 结束。
		return
	}

	// req 声明完成请求。
	var req CompleteChunkUploadRequest
	// 绑定 JSON；err 接收错误。
	if err := c.ShouldBindJSON(&req); err != nil {
		// 参数错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// session 取回会话；err 接收错误。
	session, err := h.getSession(c, req.UploadID)
	if err != nil {
		// 不存在：404。
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		// 结束。
		return
	}

	// accountID 取登录编号；err 接收错误。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		// 出错：400。
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		// 结束。
		return
	}
	// 归属校验：403。
	if session.AccountID != accountID {
		// 禁止。
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		// 结束。
		return
	}

	// session.IsComplete() 为 false：还有块没传。
	if !session.IsComplete() {
		// missing 统计缺失块数量（只用于提示）。
		missing := 0
		// 遍历位图；uploaded 是每块的标记。
		for _, uploaded := range session.UploadedBits {
			// 这块没传。
			if !uploaded {
				// 计数 +1。
				missing++
				// 只汇报最多 5 个，避免列表太长。
				if missing > 5 {
					// 封顶为 5。
					missing = 5
					// 跳出循环。
					break
				}
			}
		}
		// 400 告诉前端还没传齐，并给出缺失数、已完成数、总数。
		c.JSON(http.StatusBadRequest, gin.H{
			// error 提示。
			"error": "not all chunks uploaded",
			// missing 缺失块数（最多显示 5）。
			"missing": missing,
			// completed 已完成块数。
			"completed": len(session.UploadedChunks()),
			// total 总块数。
			"total": session.TotalChunks,
		})
		// 结束。
		return
	}

	// date 今天日期，用于分目录。
	date := time.Now().Format("20060102")
	// relDir 相对目录 videos/{账号}/{日期}。
	relDir := filepath.Join("videos", fmt.Sprintf("%d", accountID), date)
	// root 上传根目录。
	root := filepath.Join(".run", "uploads")
	// absDir 磁盘绝对目录。
	absDir := filepath.Join(root, relDir)
	// 建目录；err 接收错误。
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create output dir"})
		// 结束。
		return
	}

	// filename 生成 16 字节随机文件名；err 接收错误。
	filename, err := randHex(16)
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		// 结束。
		return
	}
	// finalPath 拼最终 mp4 的完整路径（随机名 + .mp4）。
	finalPath := filepath.Join(absDir, filename+".mp4")

	// finalFile 创建最终文件（合并的内容都写进它）；err 接收错误。
	finalFile, err := os.Create(finalPath)
	if err != nil {
		// 失败：500。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create final file"})
		// 结束。
		return
	}
	// defer 关闭最终文件。
	defer finalFile.Close()

	// tmpDir 定位存放分片的临时目录。
	tmpDir := filepath.Join(".run", "uploads", "tmp", req.UploadID)
	// 按块编号从 0 到总块数依次合并（顺序绝不能乱，否则视频内容就花了）。
	for i := 0; i < session.TotalChunks; i++ {
		// chunkPath 拼第 i 块的路径。
		chunkPath := filepath.Join(tmpDir, fmt.Sprintf("%d", i))
		// cf 打开第 i 块；err 接收错误。
		cf, err := os.Open(chunkPath)
		if err != nil {
			// 这块文件缺失（位图说有但磁盘没有）：先关掉最终文件。
			finalFile.Close()
			// 删除残缺的最终文件，避免留下坏视频。
			os.Remove(finalPath)
			// 500 报告具体哪块缺失。
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("chunk %d missing", i)})
			// 结束。
			return
		}
		// io.Copy(finalFile, cf)：把第 i 块内容追加进最终文件；err 接收错误（注意这里是 = 复用外层 err）。
		_, err = io.Copy(finalFile, cf)
		// cf.Close() 这块用完立刻关掉。
		cf.Close()
		if err != nil {
			// 合并写入失败：关闭并删除最终文件。
			finalFile.Close()
			// 删除。
			os.Remove(finalPath)
			// 500。
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to merge chunks"})
			// 结束。
			return
		}
	}
	// 全部块写完：显式关闭最终文件（确保内容刷到磁盘），后面要删临时目录。
	finalFile.Close()

	// Clean up temp chunks
	// RemoveAll 删掉整个临时目录（所有分片），合并完就不需要了。
	_ = os.RemoveAll(tmpDir)

	// Clean up Redis session
	// 删除 Redis 里的会话键。
	_ = h.cache.Del(c.Request.Context(), h.sessionKey(req.UploadID))
	// 删除"账号+文件哈希"找回键。
	_ = h.cache.Del(c.Request.Context(), h.hashKey(accountID, session.FileHash))

	// urlPath 拼最终视频的浏览器访问路径。
	urlPath := fmt.Sprintf("/static/videos/%d/%s/%s.mp4", accountID, date, filename)
	// playURL 补全成带主机的完整 URL。
	playURL := buildAbsoluteURL(c, urlPath)

	// 200 返回视频地址（url 和 play_url 同值）。
	c.JSON(http.StatusOK, gin.H{
		// url 完整地址。
		"url": playURL,
		// play_url 供后续"发布视频"接口使用。
		"play_url": playURL,
	})
}

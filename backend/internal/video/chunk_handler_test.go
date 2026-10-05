// package video：分片上传处理器的测试文件，与被测代码同包。
//
// 这是一组【端到端集成测试】：不真开 HTTP 端口，但用 Gin 的测试上下文，
// 完整走 "初始化 → 上传分片 → 查询状态 → 合并完成" 的流程，并真的读写临时文件。
// Redis 用 miniredis 代替。覆盖六个场景：
//  1. 完整流程，且合并后的文件内容和原始分片一致、临时目录被清理；
//  2. 断点续传（用相同 file_hash 重新 init，应返回同一个 upload_id 和已传列表）；
//  3. 分片重复上传的幂等性；
//  4. 分片 MD5 不匹配返回 400，改正后能成功；
//  5. 分片没传齐就合并返回 400 并报告缺几片，补齐后成功；
//  6. 状态查询接口随上传进度变化。
package video

import (
	// bytes：拼分片、比较字节内容。
	"bytes"
	// crypto/md5：算分片 MD5。
	"crypto/md5"
	// crypto/rand：生成随机的测试分片数据。
	"crypto/rand"
	// encoding/hex：MD5 字节转十六进制字符串。
	"encoding/hex"
	// encoding/json：序列化请求体、解析响应。
	"encoding/json"
	// fmt：数字转字符串。
	"fmt"
	// mime/multipart：构造带文件的 multipart 表单请求。
	"mime/multipart"
	// net/http：方法、状态码。
	"net/http"
	// net/http/httptest：假请求、响应记录器。
	"net/http/httptest"
	// os：建临时目录、读写文件、切目录。
	"os"
	// path/filepath：拼跨平台文件路径。
	"path/filepath"
	// testing：标准测试库。
	"testing"

	// rediscache：项目 Redis 客户端。
	rediscache "feedsystem_video_go/internal/middleware/redis"

	// miniredis：假 Redis。
	"github.com/alicebob/miniredis/v2"
	// gin：测试模式、测试上下文。
	"github.com/gin-gonic/gin"
	// goredis：go-redis 客户端，连 miniredis。
	goredis "github.com/redis/go-redis/v9"
)

// ── helpers ──
// 下面是一组测试辅助函数（不以 Test 开头，不会被当成测试执行），用来减少重复代码。

// testAccountID 测试中模拟的当前登录账号编号（常量）。
const testAccountID uint = 1

// setupTestEnv 搭建测试环境：起 miniredis、构造分片处理器、切到临时工作目录；
// 返回处理器和一个 cleanup 清理函数（测试结束务必 defer 调用）。
//
// 参数 t：测试对象；
// 返回值：*ChunkUploadHandler 处理器、func() 清理函数。
func setupTestEnv(t *testing.T) (*ChunkUploadHandler, func()) {
	// t.Helper() 把本函数标记为"辅助函数"：出错时日志报的是【调用方】的行号，方便定位。
	t.Helper()
	// gin.SetMode 切到 TestMode：测试时不输出多余的 Gin 日志。
	gin.SetMode(gin.TestMode)

	// mr 启动 miniredis；err 错误。
	mr, err := miniredis.Run()
	if err != nil {
		// 起不来终止。
		t.Fatalf("start miniredis: %v", err)
	}

	// client 构造项目 Redis 客户端：连 miniredis 地址，第二个参数是 key 前缀（这里留空）。
	client := rediscache.NewClient(
		// 真 go-redis 客户端指向 miniredis。
		goredis.NewClient(&goredis.Options{Addr: mr.Addr()}),
		// key 前缀空。
		"",
	)
	// handler 用该客户端创建被测的分片上传处理器。
	handler := NewChunkUploadHandler(client)

	// origDir 记住当前工作目录（_ 忽略错误），cleanup 时要切回来。
	origDir, _ := os.Getwd()
	// tmpDir 创建一个系统临时目录（* 会被随机串替换）；err 错误。
	tmpDir, err := os.MkdirTemp("", "chunk-upload-test-*")
	if err != nil {
		// 创建失败终止。
		t.Fatalf("create temp dir: %v", err)
	}
	// 切到临时目录：处理器按相对路径写文件，这样测试产物都落在临时目录里、不污染项目。
	if err := os.Chdir(tmpDir); err != nil {
		// 切目录失败终止。
		t.Fatalf("chdir: %v", err)
	}

	// cleanup 组装清理闭包：
	cleanup := func() {
		// 切回原工作目录。
		os.Chdir(origDir)
		// 删掉整个临时目录（含测试写出的文件）。
		os.RemoveAll(tmpDir)
		// 关 Redis 客户端。
		client.Close()
		// 关 miniredis。
		mr.Close()
	}
	// 返回处理器和清理函数。
	return handler, cleanup
}

// newJSONContext 构造一个带 JSON body 的 Gin 测试上下文。
//
// 参数：path 请求路径；body 任意会被序列化成 JSON 的对象；
// 返回值：*gin.Context 测试上下文、*httptest.ResponseRecorder 响应记录器。
func newJSONContext(t *testing.T, path string, body interface{}) (*gin.Context, *httptest.ResponseRecorder) {
	// 标记辅助函数。
	t.Helper()
	// b 把 body 序列化成 JSON；err 错误。
	b, err := json.Marshal(body)
	if err != nil {
		// 序列化失败终止。
		t.Fatalf("marshal: %v", err)
	}
	// req 造一个 POST 请求，body 用 JSON 字节的读取器。
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	// 设置 JSON 内容类型头。
	req.Header.Set("Content-Type", "application/json")
	// 指定 Host（处理器拼绝对 URL 时要用）。
	req.Host = "localhost"

	// rec 响应记录器。
	rec := httptest.NewRecorder()
	// gin.CreateTestContext 造一个绑定到 rec 的测试上下文；c 是上下文、_ 忽略引擎。
	c, _ := gin.CreateTestContext(rec)
	// 把假请求挂到上下文。
	c.Request = req
	// 模拟登录中间件：直接塞进 accountID，处理器就从这里取当前用户。
	c.Set("accountID", testAccountID)
	// 返回。
	return c, rec
}

// newMultipartContext 构造一个带文件上传的 multipart 表单测试上下文。
//
// 参数：path 路径；fields 普通文本字段（upload_id、chunk_index 等）；fileContent 文件内容字节；
// 返回值：测试上下文、响应记录器。
func newMultipartContext(t *testing.T, path string, fields map[string]string, fileContent []byte) (*gin.Context, *httptest.ResponseRecorder) {
	// 标记辅助。
	t.Helper()
	// buf 作为整个 multipart 请求体的缓冲。
	var buf bytes.Buffer
	// writer 往 buf 里写 multipart 格式数据。
	writer := multipart.NewWriter(&buf)

	// 逐个写入普通文本字段；k 字段名、v 字段值。
	for k, v := range fields {
		// WriteField 写一个文本字段，失败终止。
		if err := writer.WriteField(k, v); err != nil {
			// 终止。
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	// part 创建文件字段，字段名固定 "file"、文件名 chunk.bin；err 错误。
	part, err := writer.CreateFormFile("file", "chunk.bin")
	if err != nil {
		// 失败终止。
		t.Fatalf("create form file: %v", err)
	}
	// 把测试分片内容写进文件字段。
	if _, err := part.Write(fileContent); err != nil {
		// 失败终止。
		t.Fatalf("write file: %v", err)
	}
	// writer.Close 写入 multipart 的结束边界，必须调用，请求体才完整。
	writer.Close()

	// req 用拼好的 buf 造 POST 请求。
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	// Content-Type 必须是 multipart 并带上 boundary（FormDataContentType 会生成）。
	req.Header.Set("Content-Type", writer.FormDataContentType())
	// 指定 Host。
	req.Host = "localhost"

	// rec 响应记录器。
	rec := httptest.NewRecorder()
	// c 测试上下文。
	c, _ := gin.CreateTestContext(rec)
	// 挂请求。
	c.Request = req
	// 模拟登录。
	c.Set("accountID", testAccountID)
	// 返回。
	return c, rec
}

// computeMD5 计算一段数据的 MD5 十六进制字符串（测试里当"期望值"用）。
//
// 参数 data：字节数据；
// 返回值 string：32 位十六进制 MD5。
func computeMD5(data []byte) string {
	// md5.Sum 返回固定长度数组 h（不是切片）。
	h := md5.Sum(data)
	// h[:] 把数组转成切片，hex.EncodeToString 转成十六进制字符串返回。
	return hex.EncodeToString(h[:])
}

// parseJSON 把响应记录器里的 body 解析成通用 map。
//
// 参数 rec：响应记录器；
// 返回值 map[string]interface{}：解析结果。
func parseJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	// 标记辅助。
	t.Helper()
	// m 准备接收解析结果。
	var m map[string]interface{}
	// rec.Body.Bytes() 取响应体字节并解析；失败终止（同时把 body 打出来好排查）。
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		// 终止。
		t.Fatalf("parse json: %v, body: %s", err, rec.Body.String())
	}
	// 返回 map。
	return m
}

// makeTestChunks 生成指定数量和大小的随机测试分片。
//
// 参数：totalChunks 分片数量；chunkSize 每片字节数；
// 返回值：[][]byte 各分片内容、[]string 各分片 MD5、string 整个文件的 MD5。
func makeTestChunks(t *testing.T, totalChunks, chunkSize int) ([][]byte, []string, string) {
	// 标记辅助。
	t.Helper()
	// chunks 分配存分片内容的切片（长度=分片数）。
	chunks := make([][]byte, totalChunks)
	// chunkHashes 分配存各分片 MD5 的切片。
	chunkHashes := make([]string, totalChunks)
	// full 用来累积全部内容，最后算整个文件的 MD5。
	var full bytes.Buffer
	// i 逐个生成分片。
	for i := 0; i < totalChunks; i++ {
		// data 分配一片零值字节。
		data := make([]byte, chunkSize)
		// rand.Read 用密码学随机数填满 data，保证每片内容不同；err 错误。
		if _, err := rand.Read(data); err != nil {
			// 失败终止。
			t.Fatalf("rand read: %v", err)
		}
		// 存分片内容。
		chunks[i] = data
		// 存该分片 MD5。
		chunkHashes[i] = computeMD5(data)
		// 累积到完整内容缓冲。
		full.Write(data)
	}
	// 返回分片、各片 MD5、整个文件 MD5。
	return chunks, chunkHashes, computeMD5(full.Bytes())
}

// initUpload 调一次"初始化分片上传"接口，并返回得到的 upload_id。
//
// 参数：h 处理器；filename 文件名；fileSize 整文件大小；chunkSize 分片大小；
// totalChunks 分片数；fileHash 整文件 MD5；
// 返回值 string：upload_id。
func initUpload(t *testing.T, h *ChunkUploadHandler, filename string, fileSize int64, chunkSize int64, totalChunks int, fileHash string) string {
	// 标记辅助。
	t.Helper()
	// c、rec 构造 init 请求上下文。
	c, rec := newJSONContext(t, "/video/chunk/init", InitChunkUploadRequest{
		// Filename 文件名。
		Filename: filename,
		// FileSize 整文件大小。
		FileSize: fileSize,
		// ChunkSize 分片大小。
		ChunkSize: chunkSize,
		// TotalChunks 分片总数。
		TotalChunks: totalChunks,
		// FileHash 整文件 MD5（断点续传靠它识别）。
		FileHash: fileHash,
	})
	// 直接调用处理器方法（不经网络）。
	h.InitChunkUpload(c)
	// 不是 200 就终止并打出 body。
	if rec.Code != http.StatusOK {
		// 终止。
		t.Fatalf("init: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	// resp 解析响应。
	resp := parseJSON(t, rec)
	// upload_id 字段是字符串，类型断言后返回。
	return resp["upload_id"].(string)
}

// uploadChunk 调一次"上传单个分片"接口。
//
// 参数：h 处理器；uploadID 上传会话 ID；chunkIndex 分片下标；
// chunkHash 该分片期望 MD5；chunkData 分片内容；
// 无返回值（失败内部直接终止）。
func uploadChunk(t *testing.T, h *ChunkUploadHandler, uploadID string, chunkIndex int, chunkHash string, chunkData []byte) {
	// 标记辅助。
	t.Helper()
	// c、rec 构造 multipart 请求，文本字段三个。
	c, rec := newMultipartContext(t, "/video/chunk/upload", map[string]string{
		// upload_id 会话 ID。
		"upload_id": uploadID,
		// chunk_index 分片下标（数字转字符串）。
		"chunk_index": fmt.Sprintf("%d", chunkIndex),
		// chunk_hash 该分片 MD5。
		"chunk_hash": chunkHash,
	}, chunkData)
	// 调处理器。
	h.UploadChunk(c)
	// 不是 200 终止并打出 body。
	if rec.Code != http.StatusOK {
		// 终止。
		t.Fatalf("upload chunk %d: expected 200, got %d, body: %s", chunkIndex, rec.Code, rec.Body.String())
	}
}

// completeUpload 调"合并完成"接口，返回响应 map（不在这里断言状态码，由调用方决定）。
//
// 参数：h 处理器；uploadID 会话 ID；
// 返回值 map[string]interface{}：响应。
func completeUpload(t *testing.T, h *ChunkUploadHandler, uploadID string) map[string]interface{} {
	// 标记辅助。
	t.Helper()
	// c、rec 构造 complete 请求。
	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	// 调处理器合并。
	h.CompleteChunkUpload(c)
	// 解析并返回响应（状态码由调用方检查）。
	return parseJSON(t, rec)
}

// readMergedFile 从响应里的 URL 反推出文件落盘路径，并读出合并后的文件内容。
// URL format: http://localhost/static/videos/<accountID>/<date>/<name>.mp4
//
// 参数 resp：complete 接口的响应；
// 返回值 []byte：合并文件的字节内容。
func readMergedFile(t *testing.T, resp map[string]interface{}) []byte {
	// 标记辅助。
	t.Helper()
	// urlStr 取出响应里的 url 字符串。
	urlStr := resp["url"].(string)
	// idx 是固定前缀的长度，用来把 URL 里的"静态资源相对路径"切出来。
	idx := len("http://localhost/static/")
	// fsPath 拼出真实文件路径：上传根目录 .run/uploads + 相对路径。
	fsPath := filepath.Join(".run", "uploads", urlStr[idx:])
	// data 读出合并后的文件；err 错误。
	data, err := os.ReadFile(fsPath)
	if err != nil {
		// 读不到终止。
		t.Fatalf("read merged file %s: %v", fsPath, err)
	}
	// 返回文件内容。
	return data
}

// ── tests ──
// 下面是真正的测试用例。

// TestFullChunkUploadFlow 测试完整的分片上传流程，并校验合并结果正确、临时目录已清理。
func TestFullChunkUploadFlow(t *testing.T) {
	// h 处理器；cleanup 清理函数。
	h, cleanup := setupTestEnv(t)
	// 结束时清理。
	defer cleanup()

	// chunkSize 每片 1024 字节。
	chunkSize := 1024
	// totalChunks 共 3 片。
	totalChunks := 3
	// chunks 分片内容；chunkHashes 各片 MD5；fileHash 整文件 MD5。
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化会话：整文件大小=片数×片长。
	uploadID := initUpload(t, h, "test.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)

	// i 按顺序上传每一片。
	for i := 0; i < totalChunks; i++ {
		// 上传第 i 片。
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}

	// c、rec 调 complete 接口。
	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	// 执行合并。
	h.CompleteChunkUpload(c)
	// 不是 200 终止。
	if rec.Code != http.StatusOK {
		// 终止。
		t.Fatalf("complete: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	// resp 解析响应。
	resp := parseJSON(t, rec)
	// 必须同时返回 url 和 play_url。
	if resp["url"] == nil || resp["play_url"] == nil {
		// 缺字段终止。
		t.Fatal("complete response missing url or play_url")
	}

	// verify merged file content matches original chunks
	// merged 读出合并后的文件。
	merged := readMergedFile(t, resp)
	// expected 手工把所有分片按顺序拼起来，作为"应有内容"。
	var expected bytes.Buffer
	// ch 逐个分片。
	for _, ch := range chunks {
		// 写进期望缓冲。
		expected.Write(ch)
	}
	// bytes.Equal 逐字节比较合并结果和期望：必须完全一致（验证顺序和内容都对）。
	if !bytes.Equal(merged, expected.Bytes()) {
		// 不一致终止并打印双方长度。
		t.Fatalf("merged file content mismatch: got %d bytes, want %d bytes", len(merged), expected.Len())
	}

	// verify temp dir cleaned up
	// tmpDir 拼该会话的临时分片目录路径。
	tmpDir := filepath.Join(".run", "uploads", "tmp", uploadID)
	// os.Stat 检查它是否存在；!os.IsNotExist(err) 表示"它还在"（或出了别的错）。
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		// 临时目录应已被删除，却还在：失败。
		t.Fatalf("temp dir should be removed: %s", tmpDir)
	}
}

// TestBreakpointResume 测试断点续传：传了一半后用同一 file_hash 重新 init，应接着原会话传。
func TestBreakpointResume(t *testing.T) {
	// h、cleanup。
	h, cleanup := setupTestEnv(t)
	// 清理。
	defer cleanup()

	// chunkSize 每片 512。
	chunkSize := 512
	// totalChunks 4 片。
	totalChunks := 4
	// chunks、chunkHashes、fileHash。
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化。
	uploadID := initUpload(t, h, "resume.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)

	// upload first 2 chunks
	// i 先传前 2 片。
	for i := 0; i < 2; i++ {
		// 上传。
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}

	// re-init with same file_hash → should resume existing session
	// c、rec 用完全相同的文件信息（尤其 file_hash）再 init 一次。
	c, rec := newJSONContext(t, "/video/chunk/init", InitChunkUploadRequest{
		// Filename 同名。
		Filename: "resume.mp4",
		// FileSize 同大小。
		FileSize: int64(totalChunks * chunkSize),
		// ChunkSize 同片长。
		ChunkSize: int64(chunkSize),
		// TotalChunks 同片数。
		TotalChunks: totalChunks,
		// FileHash 相同：触发续传而不是新建。
		FileHash: fileHash,
	})
	// 调 init。
	h.InitChunkUpload(c)
	// 不是 200 终止。
	if rec.Code != http.StatusOK {
		// 终止。
		t.Fatalf("re-init: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	// resp 解析。
	resp := parseJSON(t, rec)

	// resumedID 取返回的 upload_id。
	resumedID := resp["upload_id"].(string)
	// 续传必须返回【同一个】upload_id。
	if resumedID != uploadID {
		// 不一样说明新建了会话：失败。
		t.Fatalf("resume should return same upload_id: got %s, want %s", resumedID, uploadID)
	}

	// chunkList 取已传分片列表（JSON 数组解析成 []interface{}）。
	chunkList := resp["uploaded_chunks"].([]interface{})
	// 应只显示之前传过的 2 片。
	if len(chunkList) != 2 {
		// 数量不对失败。
		t.Fatalf("expected 2 uploaded chunks on resume, got %d", len(chunkList))
	}

	// upload remaining chunks and complete
	// i 接着传第 2、3 片。
	for i := 2; i < totalChunks; i++ {
		// 上传。
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}

	// resp 调 complete 合并。
	resp = completeUpload(t, h, uploadID)
	// merged 读出合并文件。
	merged := readMergedFile(t, resp)
	// expected 重拼全部内容。
	var expected bytes.Buffer
	// ch 逐片。
	for _, ch := range chunks {
		// 累积。
		expected.Write(ch)
	}
	// 续传后的合并结果也必须和原始内容一致。
	if !bytes.Equal(merged, expected.Bytes()) {
		// 不一致失败。
		t.Fatalf("merged file mismatch after resume")
	}
}

// TestIdempotentChunkUpload 测试分片重复上传的幂等性：同一片传两次都应成功、不影响合并。
func TestIdempotentChunkUpload(t *testing.T) {
	// h、cleanup。
	h, cleanup := setupTestEnv(t)
	// 清理。
	defer cleanup()

	// chunkSize 每片 256。
	chunkSize := 256
	// totalChunks 2 片。
	totalChunks := 2
	// chunks、chunkHashes、fileHash。
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化。
	uploadID := initUpload(t, h, "idempotent.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)

	// upload chunk 0 twice — both should succeed
	// 第 0 片传第一次。
	uploadChunk(t, h, uploadID, 0, chunkHashes[0], chunks[0])
	// 第 0 片再传一次（重复）：也应返回 200，而不是报错。
	uploadChunk(t, h, uploadID, 0, chunkHashes[0], chunks[0])

	// 传第 1 片。
	uploadChunk(t, h, uploadID, 1, chunkHashes[1], chunks[1])

	// c、rec 合并。
	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	// 执行。
	h.CompleteChunkUpload(c)
	// 必须 200：说明重复上传没破坏会话。
	if rec.Code != http.StatusOK {
		// 否则终止。
		t.Fatalf("complete: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
}

// TestHashMismatch 测试分片 MD5 对不上时返回 400，并带上期望/实际值；用正确 MD5 重试应成功。
func TestHashMismatch(t *testing.T) {
	// h、cleanup。
	h, cleanup := setupTestEnv(t)
	// 清理。
	defer cleanup()

	// chunkSize 256。
	chunkSize := 256
	// totalChunks 1 片。
	totalChunks := 1
	// chunks、_（这里不关心正确 hash 列表）、fileHash。
	chunks, _, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化。
	uploadID := initUpload(t, h, "hashfail.mp4", int64(chunkSize), int64(chunkSize), totalChunks, fileHash)

	// upload with wrong hash
	// c、rec 故意在 chunk_hash 字段填一个错误的 MD5。
	c, rec := newMultipartContext(t, "/video/chunk/upload", map[string]string{
		// upload_id 会话。
		"upload_id": uploadID,
		// chunk_index 第 0 片。
		"chunk_index": "0",
		// chunk_hash 一个假的 32 位十六进制串。
		"chunk_hash": "deadbeef000000000000000000000000",
	}, chunks[0])
	// 调处理器。
	h.UploadChunk(c)
	// 期望返回 400（而不是 200）。
	if rec.Code != http.StatusBadRequest {
		// 不对失败。
		t.Fatalf("expected 400 for hash mismatch, got %d", rec.Code)
	}
	// resp 解析错误响应。
	resp := parseJSON(t, rec)
	// 错误信息应明确是 hash 不匹配。
	if resp["error"] != "chunk hash mismatch" {
		// 不符失败。
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	// 响应应同时给出 expected 和 actual，方便客户端定位问题。
	if resp["expected"] == nil || resp["actual"] == nil {
		// 缺字段失败。
		t.Fatal("response should contain expected and actual hash")
	}
	// actualHash 本地算出这片真实的 MD5。
	actualHash := computeMD5(chunks[0])
	// 响应里的 actual 必须等于真实 MD5。
	if resp["actual"] != actualHash {
		// 不符失败。
		t.Fatalf("actual hash: got %v, want %s", resp["actual"], actualHash)
	}

	// retry with correct hash should succeed
	// 用正确的 MD5 重新上传同一片：这次应成功。
	uploadChunk(t, h, uploadID, 0, actualHash, chunks[0])
}

// TestIncompleteMerge 测试分片没传齐就点完成：返回 400 并报告完成/缺失/总数；补齐后能成功。
func TestIncompleteMerge(t *testing.T) {
	// h、cleanup。
	h, cleanup := setupTestEnv(t)
	// 清理。
	defer cleanup()

	// chunkSize 128。
	chunkSize := 128
	// totalChunks 5 片。
	totalChunks := 5
	// chunks、chunkHashes、fileHash。
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化。
	uploadID := initUpload(t, h, "incomplete.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)

	// upload only 3 out of 5
	// i 只传前 3 片。
	for i := 0; i < 3; i++ {
		// 上传。
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}

	// c、rec 尝试合并。
	c, rec := newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	// 调处理器。
	h.CompleteChunkUpload(c)
	// 没传齐应返回 400。
	if rec.Code != http.StatusBadRequest {
		// 不对失败。
		t.Fatalf("expected 400 for incomplete, got %d", rec.Code)
	}
	// resp 解析。
	resp := parseJSON(t, rec)
	// 错误信息应说明"没传齐"。
	if resp["error"] != "not all chunks uploaded" {
		// 不符失败。
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	// JSON 数字默认解析成 float64：missing 应是缺的 2 片。
	if int(resp["missing"].(float64)) != 2 {
		// 不对失败。
		t.Fatalf("expected missing=2, got %v", resp["missing"])
	}
	// completed 应是已传的 3 片。
	if int(resp["completed"].(float64)) != 3 {
		// 不对失败。
		t.Fatalf("expected completed=3, got %v", resp["completed"])
	}
	// total 应是 5。
	if int(resp["total"].(float64)) != 5 {
		// 不对失败。
		t.Fatalf("expected total=5, got %v", resp["total"])
	}

	// upload remaining and retry
	// i 把剩下第 3、4 片传上。
	for i := 3; i < totalChunks; i++ {
		// 上传。
		uploadChunk(t, h, uploadID, i, chunkHashes[i], chunks[i])
	}
	// c、rec 再次合并。
	c, rec = newJSONContext(t, "/video/chunk/complete", CompleteChunkUploadRequest{UploadID: uploadID})
	// 执行。
	h.CompleteChunkUpload(c)
	// 这次必须 200。
	if rec.Code != http.StatusOK {
		// 否则终止。
		t.Fatalf("complete after fix: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
}

// TestChunkStatus 测试分片状态查询：已传数量应随上传进度从 0 → 1 → 3 变化。
func TestChunkStatus(t *testing.T) {
	// h、cleanup。
	h, cleanup := setupTestEnv(t)
	// 清理。
	defer cleanup()

	// chunkSize 256。
	chunkSize := 256
	// totalChunks 3 片。
	totalChunks := 3
	// chunks、chunkHashes、fileHash。
	chunks, chunkHashes, fileHash := makeTestChunks(t, totalChunks, chunkSize)

	// uploadID 初始化。
	uploadID := initUpload(t, h, "status.mp4", int64(totalChunks*chunkSize), int64(chunkSize), totalChunks, fileHash)

	// assertStatus 定义一个局部断言闭包：查一次状态、校验已传数量和总数。
	assertStatus := func(wantCount int) {
		// 闭包里也标记辅助，失败时指向调用 assertStatus 的位置。
		t.Helper()
		// c、rec 构造 status 请求。
		c, rec := newJSONContext(t, "/video/chunk/status", ChunkStatusRequest{UploadID: uploadID})
		// 调处理器。
		h.ChunkStatus(c)
		// 状态接口应 200。
		if rec.Code != http.StatusOK {
			// 否则终止。
			t.Fatalf("status: expected 200, got %d", rec.Code)
		}
		// resp 解析。
		resp := parseJSON(t, rec)
		// list 取已传分片列表，_ 忽略类型断言的 ok。
		list, _ := resp["uploaded_chunks"].([]interface{})
		// 已传数量应等于期望值。
		if len(list) != wantCount {
			// 不对终止。
			t.Fatalf("expected %d uploaded chunks, got %d", wantCount, len(list))
		}
		// total_chunks 应始终是 3。
		if int(resp["total_chunks"].(float64)) != totalChunks {
			// 不对终止。
			t.Fatalf("expected total_chunks=%d", totalChunks)
		}
	}

	// 还没传：应为 0。
	assertStatus(0)

	// 传第 0 片。
	uploadChunk(t, h, uploadID, 0, chunkHashes[0], chunks[0])
	// 应为 1。
	assertStatus(1)

	// 传第 1、2 片。
	uploadChunk(t, h, uploadID, 1, chunkHashes[1], chunks[1])
	uploadChunk(t, h, uploadID, 2, chunkHashes[2], chunks[2])
	// 应全部到齐为 3。
	assertStatus(3)
}

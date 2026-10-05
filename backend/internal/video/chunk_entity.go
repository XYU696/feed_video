// package video：视频模块（这个文件定义"分片上传"过程中用到的会话结构和请求结构）。
package video

// ChunkSize 是一个常量，规定前端上传视频时每个分片的大小：5 MB。
//
// 技术点：
//  1. const 用来声明常量（值一旦定下不可修改），类似 Python 里全大写的约定常量，但 Go 会在语言层面强制它不能被改。
//  2. 5 << 20 是"位运算"写法：<< 表示把二进制位向左移动，左移 20 位等于乘以 2 的 20 次方。
//     因为 1 MB = 1024 KB = 1024 * 1024 字节 = 2^20 字节，所以 5 << 20 = 5 * 2^20 = 5242880 字节，正好 5 MB。
const ChunkSize = 5 << 20 // 5 MB

// ChunkUploadSession 是"分片上传会话"，记录一次大文件上传进行到哪了。
//
// 业务理解（分片上传+断点续传）：
// 一个 200MB 的视频如果一次性上传，中途网络断了就要从头再来，太痛苦。
// 所以前端把文件切成一个个 5MB 的小分片分别上传。后端用这个结构体记录：
// 总共多少片、哪些片已经传上来了。这个会话会被存进 Redis（24 小时有效），
// 即使上传中断、用户刷新页面，前端也能凭文件指纹查到进度，只补传没传过的分片（这就是断点续传）。
type ChunkUploadSession struct {
	// UploadID 是这次上传任务的唯一编号（一串随机字符串），之后每传一个分片都要带上它，后端才知道是哪个任务。
	UploadID string `json:"upload_id"`

	// AccountID 记录是谁在上传（用户编号）。
	AccountID uint `json:"account_id"`

	// Filename 记录原始文件名。
	Filename string `json:"filename"`

	// FileSize 记录整个文件的总大小（字节数），int64 是 64 位整数（大文件大小可能超过普通 int 的范围）。
	FileSize int64 `json:"file_size"`

	// ChunkSize 记录每个分片的大小（字节）。
	ChunkSize int64 `json:"chunk_size"`

	// TotalChunks 记录这个文件一共被切成多少片。
	TotalChunks int `json:"total_chunks"`

	// FileHash 是整个文件的"指纹"(MD5 值)：文件内容算出来的一串字符，内容不同指纹就不同。
	// 它有两个作用：① 断点续传时靠"用户+文件指纹"找到之前的上传会话；② 秒传——如果服务器已有相同指纹的文件，可以直接复用。
	FileHash string `json:"file_hash"`

	// UploadedBits 是一个 bool 切片（类似 Python 的 list[bool]），用一串 true/false 记录每个分片是否已上传：
	// 例如 [true, true, false, ...] 表示第 0、1 片已上传，第 2 片还没传。
	// 下标 = 分片编号，值 = 是否已传，这种"用位图记录状态"的方式既省空间又直观。
	UploadedBits []bool `json:"uploaded_bits"`
}

// UploadedChunks 是 ChunkUploadSession 的一个"方法"：返回"已经上传完成的分片编号列表"。
//
// 重点技术点——方法(method)：
//
//	func (s *ChunkUploadSession) UploadedChunks() []int
//	   ^^^^^^^^^^^^^^^^^^^^^^^^
//	   函数名前面多了 (s *ChunkUploadSession)，这叫"接收者(receiver)"，
//	   意思是把这个函数【挂到 ChunkUploadSession 类型上】，成为它的方法，
//	   调用时写 session.UploadedChunks()，就像 Python 里 class 的 self 方法：
//	     Python:  def uploaded_chunks(self):
//	     Go:      func (s *ChunkUploadSession) UploadedChunks()
//	   其中 s 相当于 Python 的 self（名字可以随便起，习惯用类型首字母）；
//	   * 号表示"指针接收者"——操作的是原对象本身而不是它的一份拷贝（先记住：要改对象内容、或对象较大时用指针）。
//
// 返回值 []int：已上传分片的编号列表（类似 Python 的 list[int]）。
func (s *ChunkUploadSession) UploadedChunks() []int {
	// indices 变量收集所有"已上传"分片的编号，开始时是空切片 nil。
	var indices []int

	// for range 遍历 UploadedBits 切片：i 是分片编号（下标），uploaded 是该位置的 true/false。
	for i, uploaded := range s.UploadedBits {
		// if uploaded 是 if uploaded == true 的简写：只处理"已经上传"的分片。
		if uploaded {
			// append 把这个已上传分片的编号 i 追加到 indices 列表末尾。
			indices = append(indices, i)
		}
	}

	// return 返回所有已上传分片的编号，断点续传时前端拿它和"全部编号"对比，就知道还差哪些片要补传。
	return indices
}

// IsComplete 是 ChunkUploadSession 的另一个方法：判断"是否所有分片都传完了"，
// 返回 bool：true 表示可以合并成完整文件了，false 表示还有分片没传。
func (s *ChunkUploadSession) IsComplete() bool {
	// for range 遍历所有分片的上传标记：这里不需要编号，用下划线 _ 丢掉，b 接住每一个 true/false。
	for _, b := range s.UploadedBits {
		// if !b：! 是逻辑非，意思是"只要发现有一个分片是 false（没上传）"。
		if !b {
			// return false 立刻下结论"还没传完"并结束函数（Go 的 return 可以直接写返回值，这里 false 就是返回结果）。
			return false
		}
	}

	// 能走到这里说明循环里没发现任何 false，所有分片都是 true，返回 true 表示全部传完。
	return true
}

// InitChunkUploadRequest 是"初始化分片上传"接口（/video/chunk/init）接收的请求体。
// 前端在正式传分片之前，先把文件信息发给后端登记，后端建好会话返回 UploadID。
type InitChunkUploadRequest struct {
	// Filename 接收文件名。
	// binding:"required" 是给 Gin 框架的"校验规则"：这个字段必填，没传或为空就直接返回参数错误，不会进入业务代码。
	Filename string `json:"filename" binding:"required"`

	// FileSize 接收文件总大小。
	// binding:"required,min=1" 表示必填且最小值为 1（文件大小必须是正数，防止传个 0 字节的文件）。
	FileSize int64 `json:"file_size" binding:"required,min=1"`

	// ChunkSize 接收每个分片的大小，必填且至少 1 字节。
	ChunkSize int64 `json:"chunk_size" binding:"required,min=1"`

	// TotalChunks 接收分片总数，必填且至少 1。
	TotalChunks int `json:"total_chunks" binding:"required,min=1"`

	// FileHash 接收前端算好的整个文件的 MD5 指纹，必填（断点续传全靠它识别同一个文件）。
	FileHash string `json:"file_hash" binding:"required"`
}

// UploadChunkRequest 是"上传单个分片"接口（/video/chunk/upload）接收的【表单字段】。
// 注意：这个接口是 multipart 表单提交（文件本身也要一起传），所以标签用的是 form 而不是 json。
type UploadChunkRequest struct {
	// UploadID 接收这次上传任务的编号，告诉后端这个分片属于哪个会话。
	// form:"upload_id" 表示从 multipart 表单的 upload_id 字段取值（不是从 JSON body）。
	UploadID string `form:"upload_id" binding:"required"`

	// ChunkIndex 接收当前传的是第几片（分片编号，从 0 开始）。
	// binding:"min=0" 要求编号不能是负数；没写 required 是因为 0 本身就是合法的第一片。
	ChunkIndex int `form:"chunk_index" binding:"min=0"`

	// ChunkHash 接收这一个分片的 MD5 指纹，必填——
	// 后端会重新算收到的分片的 MD5 跟它比对，不一致说明传输坏了，要求重传该片。
	ChunkHash string `form:"chunk_hash" binding:"required"`
}

// ChunkStatusRequest 是"查询上传进度"接口（/video/chunk/status）接收的请求体。
// 断点续传时前端只凭 UploadID 就能问出"哪些片已经传过了"。
type ChunkStatusRequest struct {
	// UploadID 接收要查询进度的任务编号，必填。
	UploadID string `json:"upload_id" binding:"required"`
}

// CompleteChunkUploadRequest 是"完成分片上传"接口（/video/chunk/complete）接收的请求体。
// 前端确认分片都传完后调用它，后端会把这些分片按顺序合并成一个完整的 mp4 文件。
type CompleteChunkUploadRequest struct {
	// UploadID 接收要合并的任务编号，必填。
	UploadID string `json:"upload_id" binding:"required"`
}

// package video：视频模块（这个文件定义话题标签表，并提供从文字里提取 #话题 的函数）。
package video

// import 导入 regexp 正则表达式包，类似 Python 的 re 模块，用来按规则从文字中找出 #话题。
import "regexp"

// Tag 是"话题标签"在程序里的样子，对应 MySQL 里 tags 表的一行。
//
// 业务理解：比如视频标题写"今天去爬山 #旅行 #日常"，系统会自动识别出"旅行""日常"两个话题，
// 每个不同的话题在这张表里存一行，用户就能点进 #旅行 看所有带这个话题的视频。
type Tag struct {
	// ID 是话题的唯一编号（主键）。
	ID uint `gorm:"primaryKey" json:"id"`

	// Name 是话题的名字（不带 # 号，只存 "旅行"）。
	// gorm:"uniqueIndex;type:varchar(100);not null"：
	//   - uniqueIndex 唯一索引：话题名不能重复（"旅行"这个话题在表里只存一次，多个视频用它时共用这一行）；
	//   - type:varchar(100) 列最长 100 字符；
	//   - not null 不能为空。
	Name string `gorm:"uniqueIndex;type:varchar(100);not null" json:"name"`
}

// VideoTag 是"视频和话题的对应关系"表 video_tags 的一行，也叫"中间表/关联表"。
//
// 技术点（多对多关系）：一个视频可以带多个话题，一个话题也可以属于多个视频，
// 这是典型的"多对多"关系，数据库里不能直接在某一张表里表示，就需要第三张表来记录两两的对应：
//   - 视频 A 带了"旅行""日常" → 这张表里存 (视频A, 旅行) 和 (视频A, 日常) 两行；
//   - 想查 #旅行 下有哪些视频，就在这张表里 WHERE tag_id = 旅行的ID 找出所有 video_id。
type VideoTag struct {
	// ID 是这条对应关系的唯一编号（主键）。这个结构体字段没写 json 标签，
	// 因为它只在数据库内部做关联用，基本不会直接返回给前端。
	ID uint `gorm:"primaryKey"`

	// VideoID 记录是哪个视频（建了普通索引，按视频查它的所有话题时更快），不能为空。
	VideoID uint `gorm:"index;not null"`

	// TagID 记录是哪个话题（同样建索引，按话题查所有相关视频时很快），不能为空。
	TagID uint `gorm:"index;not null"`
}

// tagRegex 是一个"包级变量"，存的是编译好的正则表达式规则。
//
// 技术点逐一说：
//  1. var 关键字用来声明变量（类似 Python 的 变量名 = 值，但 var 写在函数外面时表示整个包共用的变量）；
//  2. regexp.MustCompile(...) 把一段正则表达式"编译"成可以反复使用的规则对象；
//     "Must"开头的函数特点是：规则写错了它会直接 panic（程序崩溃报错），而不是返回 error，
//     适合这种"规则是程序员写死的、不可能错"的场景；
//  3. 正则内容 `#([\p{L}\p{N}_]+)` 的含义（用反引号包起来表示【原始字符串】，里面的反斜杠不用转义，类似 Python 的 r'...'）：
//       #            先匹配一个井号；
//       ( ... )      括号表示"捕获组"——井号后面的内容才是我们要的话题名；
//       \p{L}        匹配任意"字母"（包括中文等 Unicode 字母，所以中文话题也能识别）；
//       \p{N}        匹配任意"数字"；
//       _            匹配下划线；
//       [ ... ]+     方括号表示"里面任意一种字符"，+ 表示"一个或多个"，连起来就是：
//                    # 后面紧跟一串由字母/数字/下划线组成的文字，这一整串就是话题名。
var tagRegex = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

// ExtractTags 函数的作用：给它一段文字（标题+描述），它把里面所有 #话题 的名字提取出来，
// 返回一个字符串切片（可以理解为 Python 的 list[str]），并且自动去重。
//
// 参数 text：传进来的原始文字，类型 string；
// 返回值 []string：提取到的话题名列表（不带 # 号），没有话题时返回 nil（相当于空列表）。
func ExtractTags(text string) []string {
	// matches 变量保存正则在文字里找到的所有结果。
	// tagRegex.FindAllStringSubmatch(text, -1)：
	//   - "FindAll" 表示找出全部匹配（不止第一个）；
	//   - "Submatch" 表示连同括号捕获组的内容一起返回；
	//   - 参数 -1 表示不限制匹配个数。
	// 返回结果是一个二维切片（类似 Python 的 list[list[str]]），每个元素形如：
	//   ["#旅行", "旅行"] —— 第 0 个是整段匹配，第 1 个是括号捕获的话题名。
	matches := tagRegex.FindAllStringSubmatch(text, -1)

	// seen 变量是一个 map（类似 Python 的 dict），key 是话题名，value 是 bool。
	// make(map[string]bool) 用来创建一个空 map（Go 里 map 必须用 make 创建后才能写入）。
	// 它的作用是"记录本话题是否已经见过"，实现去重：同一个 #旅行 在文字里出现两次，结果里只保留一个。
	seen := make(map[string]bool)

	// tags 变量用来收集最终要返回的话题名列表。
	// 这里只声明不赋值，此时它是 nil（空切片），等下面用 append 往里加内容。
	var tags []string

	// for range 循环遍历 matches，类似 Python 的 for m in matches。
	// 第一个返回值本来是"序号(下标)"，这里用下划线 _ 接住表示"我不要这个值"（Go 里声明了变量不用会报错，不想要就用 _ 丢掉）。
	for _, m := range matches {
		// tag 变量取出本次匹配的话题名：m[0] 是 "#旅行"，m[1] 是括号捕获的 "旅行"。
		tag := m[1]

		// if 判断：if !seen[tag] 中的 ! 是"逻辑非"，整句意思是"如果这个话题还没被记录过"。
		// （Go 从 map 取一个不存在的 key 时，bool 类型会返回零值 false，所以没见过的话题 seen[tag] 就是 false）。
		if !seen[tag] {
			// 把这个话题在 seen 里标记为 true，意思是"已经见过了"，下次再遇到就不会重复加入。
			seen[tag] = true

			// append 是 Go 的内置函数，作用是把新元素追加到切片末尾，返回追加后的新切片（必须重新赋值接住）。
			// 这里把新话题名 tag 追加到 tags 列表里。
			tags = append(tags, tag)
		}
	}

	// return 返回收集好的话题名列表，函数结束。
	return tags
}

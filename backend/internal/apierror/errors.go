// package apierror：接口错误处理包。
// 作用：把程序内部各种 error（错误）归类，翻译成前端能看懂的 HTTP 状态码（200/400/401/404/500）。
package apierror

// import 同时导入多个包时用圆括号包起来（一行一个），这是 Go 常见的分组写法。
import (
	// errors 是标准库里处理错误的包（创建错误、判断错误类型）。
	"errors"
	// net/http 提供 HTTP 相关的常量和工具，这里用它里面的状态码常量（比如 http.StatusOK = 200）。
	"net/http"

	// gorm.io/gorm 是 GORM 框架包，这里用它的"查不到记录"错误 gorm.ErrRecordNotFound。
	"gorm.io/gorm"
)

// var ( ... )：用圆括号一次声明多个包级变量（和 import 的分组写法同理）。
var (
	// ErrUnauthorized 是一个预先定义好的错误，含义是"未授权/没登录或 token 无效"。
	// errors.New("...") 创建一个内容为指定文字的错误，类似 Python 里 raise ValueError("unauthorized") 里那个错误对象。
	// 这种预先定义好、放在包级别的错误叫"哨兵错误(sentinel error)"，
	// 好处是全项目可以用 errors.Is(实际错误, ErrUnauthorized) 统一判断"是不是这一类错误"。
	ErrUnauthorized = errors.New("unauthorized")

	// ErrValidation 是"参数校验错误"（前端传的数据不合法，比如必填字段没传）。
	ErrValidation = errors.New("validation error")
)

// ClassifyHTTPStatus 函数的作用：给它一个错误 err，它判断这是什么类型的错误，
// 返回应该回复给前端的 HTTP 状态码（int 整数）。
//
// 参数 err：业务代码产生的错误；
// 返回值 int：对应的 HTTP 状态码。
func ClassifyHTTPStatus(err error) int {
	// switch 是"多分支判断"，类似 Python 的 match/if-elif-else。
	// 技术点：switch 后面【不写表达式】时，每个 case 直接写一个条件判断，
	// 相当于一串 if-else if，哪个条件先成立就走哪个分支（比连续写很多 if-else 更清爽）。
	switch {
	// case err == nil：错误为 nil 表示"没有出错"（Go 用 nil 错误表示成功），返回 200。
	case err == nil:
		// return 返回 http.StatusOK（常量值 200）。
		return http.StatusOK

	// errors.Is(err, ErrUnauthorized)：判断 err 这个错误"是不是" ErrUnauthorized。
	// 技术点：为什么不直接写 err == ErrUnauthorized，而要用 errors.Is？
	// 因为错误可能被 fmt.Errorf("%w", err) 一层层"包装"过（外面套了补充信息，里面才是原始错误），
	// errors.Is 能一层层剥开往里找，只要里面包着 ErrUnauthorized 就能认出来；直接 == 比较则认不出被包装的错误。
	case errors.Is(err, ErrUnauthorized):
		// 未授权类错误 → 返回 401。
		return http.StatusUnauthorized

	// 参数校验类错误 → 返回 400（Bad Request，请求本身有问题）。
	case errors.Is(err, ErrValidation):
		// 返回 http.StatusBadRequest（400）。
		return http.StatusBadRequest

	// gorm.ErrRecordNotFound 是 GORM 在"按条件查不到记录"时返回的错误 → 返回 404。
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 返回 http.StatusNotFound（404，资源不存在）。
		return http.StatusNotFound

	// default 表示"以上都不是"的兜底分支。
	default:
		// 未知错误一律按 500（服务器内部错误）处理，不把内部错误细节暴露给前端。
		return http.StatusInternalServerError
	}
}

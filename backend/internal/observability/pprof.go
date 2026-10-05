// package observability：可观测性包。
// 本文件负责启动一个独立的 pprof（性能分析）HTTP 服务，并支持优雅关闭。
//
// 先通俗理解 pprof：它是 Go 标准库自带的"体检工具"，
// 可以在程序运行时查看 CPU 花在哪、内存占了多少、goroutine 卡在哪等，
// 是排查线上性能问题、死锁问题的利器，面试常问。
package observability

import (
	// context：给关闭操作设置超时。
	"context"
	// errors：判断是不是"服务器正常关闭"错误。
	"errors"
	// fmt：包装错误。
	"fmt"
	// log：打印监听/错误日志。
	"log"
	// net：监听 TCP 端口。
	"net"
	// net/http：HTTP 服务器、路由、错误常量。
	"net/http"
	// net/http/pprof：标准库 pprof 包，导入后它会注册一堆性能分析接口。
	"net/http/pprof"
	// time：关闭超时。
	"time"
)

// PprofServer 结构体包装一个 pprof HTTP 服务器，方便统一启动和关闭。
type PprofServer struct {
	// name 是进程名字（"API" 或 "Worker"），只用于日志里区分。
	name string
	// server 是真正的 HTTP 服务器对象。
	server *http.Server
	// shutdownTimeout 是优雅关闭时最多等待多久。
	shutdownTimeout time.Duration
}

// NewPprofMux 函数的作用：创建并返回一个注册好全部 pprof 接口的路由器。
//
// 技术点：http.ServeMux 是标准库自带的"URL 路由分发器"（类似一个极简 Gin），
// 把不同路径对应到不同处理函数。
//
// 没有参数；返回值 *http.ServeMux：配置好的路由器。
func NewPprofMux() *http.ServeMux {
	// mux 创建一个空路由器。http.NewServeMux() 分配并返回它。
	mux := http.NewServeMux()
	// mux.HandleFunc(路径, 处理函数) 逐个注册 pprof 提供的页面：

	// "/debug/pprof/" 带结尾斜杠表示"前缀匹配"，这是 pprof 总索引页
	// （能点进各类分析：堆内存、goroutine 等）。pprof.Index 是它的处理函数。
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	// "/debug/pprof/cmdline" 返回当前程序的启动命令行。
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	// "/debug/pprof/profile" 采集 CPU 性能分析（访问时可带秒数参数，采一段时间）。
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	// "/debug/pprof/symbol" 用来查程序地址对应的函数符号。
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	// "/debug/pprof/trace" 采集执行轨迹（调度、系统调用等事件流）。
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// 返回配好的路由器。
	return mux
}

// NewPprofServer 函数的作用：在指定地址启动 pprof 服务（在后台 goroutine 里运行）。
//
// 参数：name 进程名（日志用）、enabled 是否开启、addr 监听地址（如 "localhost:6060"）；
// 返回值：*PprofServer 启动好的服务器（未开启时为 nil）、error 监听失败错误。
func NewPprofServer(name string, enabled bool, addr string) (*PprofServer, error) {
	// 未开启，或地址为空。
	if !enabled || addr == "" {
		// 返回 nil, nil：没有服务器、也不是错误（pprof 是可选能力）。
		return nil, nil
	}
	// ln 变量是 TCP 监听器；err 接收错误。
	// net.Listen("tcp", addr)：先把端口占下来（这一步能立刻发现"端口被占用"等问题）。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 监听失败：返回 nil 和包装错误（%w 保留原始错误，日志能看出是哪个进程、哪个地址）。
		return nil, fmt.Errorf("failed to start %s pprof server on %s: %w", name, addr, err)
	}
	// pprofServer 变量创建包装对象。
	pprofServer := &PprofServer{
		// name 记录进程名。
		name: name,
		// shutdownTimeout 关闭时最多等 3 秒。
		shutdownTimeout: 3 * time.Second,
	}
	// pprofServer.server 给包装对象挂上真正的 HTTP 服务器配置。
	pprofServer.server = &http.Server{
		// Addr 监听地址。
		Addr: addr,
		// Handler 使用前面注册好 pprof 接口的路由器。
		Handler: NewPprofMux(),
		// ReadHeaderTimeout：读请求头最多 5 秒，防止慢连接占住资源。
		ReadHeaderTimeout: 5 * time.Second,
	}
	// go func() { ... }()：
	// 重点技术点——goroutine。go 关键字加在函数调用前，就让这个函数【并发地在后台运行】，
	// 当前函数不会等它，直接往下返回。HTTP 服务器必须这样启动，否则 Serve 会阻塞住主流程。
	go func() {
		// 打印日志，提示 pprof 在哪个地址监听。
		log.Printf("%s pprof listening on %s", name, addr)
		// pprofServer.server.Serve(ln)：在监听器上开始接收请求（它会一直运行直到被关闭）；
		// err 接收返回错误；
		// !errors.Is(err, http.ErrServerClosed)：服务器被正常 Shutdown 时也会返回一个错误，
		// errors.Is 判断"如果不是正常关闭那种错误"才记日志（正常关闭不该刷成一条吓人报错）。
		if err := pprofServer.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 真正的服务异常：打印错误。
			log.Printf("%s pprof server error: %v", name, err)
		}
	}()
	// 返回包装对象，错误为 nil。
	return pprofServer, nil
}

// Shutdown 函数的作用：优雅关闭一个 HTTP 服务器。
//
// 技术点——"优雅关闭"：不是直接掐断，而是 srv.Shutdown 会停止接收新连接，
// 并等待正在处理的请求处理完（或等到 ctx 超时），避免请求做到一半被杀掉。
//
// 参数：ctx 控制最多等多久、srv 要关闭的服务器；
// 返回值 error：关闭错误，成功为 nil。
func Shutdown(ctx context.Context, srv *http.Server) error {
	// 服务器为空。
	if srv == nil {
		// 没东西可关，返回 nil。
		return nil
	}
	// srv.Shutdown(ctx) 执行优雅关闭并返回错误。
	return srv.Shutdown(ctx)
}

// Close 是 PprofServer 的方法：关闭 pprof 服务（带超时保护）。
//
// 参数 s：pprof 服务器包装对象；
// 返回值 error：关闭错误。
func (s *PprofServer) Close() error {
	// 允许对空对象调用。
	if s == nil {
		// 返回 nil。
		return nil
	}
	// shutdownCtx 是带超时的上下文；cancel 是它的取消函数。
	// context.WithTimeout(父上下文, 超时时长)：返回一个"到点自动取消"的上下文，
	// 用来限制关闭最多等 shutdownTimeout。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	// defer cancel()：函数退出时调用 cancel 释放上下文相关资源（这是固定写法，即使超时已触发也要调）。
	defer cancel()
	// Shutdown(shutdownCtx, s.server) 优雅关闭真正的 HTTP 服务器；err 接收错误。
	if err := Shutdown(shutdownCtx, s.server); err != nil {
		// 关闭出错：记日志并把错误返回。
		log.Printf("Failed to shutdown %s pprof server: %v", s.name, err)
		// 返回错误。
		return err
	}
	// 关闭成功：返回 nil。
	return nil
}

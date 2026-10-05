// package observability：pprof 模块的测试文件，与被测代码同包。
// 本文件有三个测试：
//  1. pprof 路由能正常响应 200；
//  2. 配置【禁用】pprof 时，构造函数返回 nil（而不是偷偷起服务）；
//  3. 在"已禁用"返回的 nil 服务器上调 Close 也不会 panic/报错。
package observability

import (
	// net/http：请求方法、状态码。
	"net/http"
	// net/http/httptest：不用真开端口就能发请求、录响应的测试工具。
	"net/http/httptest"
	// testing：标准测试库。
	"testing"
)

// TestNewPprofMux 测试 pprof 路由集合的首页返回 200。
func TestNewPprofMux(t *testing.T) {
	// t.Parallel() 声明本测试可以和其他并行测试【同时跑】，缩短整体测试时间。
	t.Parallel()

	// req 用 httptest 造一个 GET /debug/pprof/ 请求（body 为 nil）。
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	// rr 是"响应记录器"：会把处理函数写出的状态码、内容都记下来供断言。
	rr := httptest.NewRecorder()

	// NewPprofMux() 拿到 pprof 路由，直接 ServeHTTP 处理上面的假请求、写进 rr。
	NewPprofMux().ServeHTTP(rr, req)

	// rr.Code 是记录到的状态码，不是 200 就报错。
	if rr.Code != http.StatusOK {
		// t.Errorf 只标记失败但【继续执行】（不像 Fatalf 立即终止；这里后面没代码，区别不大）。
		t.Errorf("Expected status code 200, got %d", rr.Code)
	}
}

// TestNewPprofServerWithDisabled 测试：禁用开关时 NewPprofServer 应返回 nil。
func TestNewPprofServerWithDisabled(t *testing.T) {
	// 允许并行。
	t.Parallel()

	// pprofServer 第二个参数传 false（禁用）；err 错误。
	pprofServer, err := NewPprofServer("api", false, "localhost:6060")
	if err != nil {
		// 禁用不该返回错误：有 err 就终止。
		t.Fatalf("Failed to create pprof server: %v", err)
	}
	// 禁用时应返回 nil 服务器。
	if pprofServer != nil {
		// 却返回了非 nil：失败。
		t.Fatalf("Expected nil pprof server when disabled, got non-nil")
	}
}

// TestPprofServerCloseWithDisabledServer 测试：对"禁用得到的 nil 服务器"调 Close 也安全。
func TestPprofServerCloseWithDisabledServer(t *testing.T) {
	// 允许并行。
	t.Parallel()

	// pprofServer 同样以禁用方式创建；err 错误。
	pprofServer, err := NewPprofServer("api", false, "localhost:6060")
	if err != nil {
		// 有 err 终止。
		t.Fatalf("Failed to create pprof server: %v", err)
	}
	// 直接对它调 Close；这里验证 Close 内部对 nil 接收者做了防护、不会 panic。
	if err := pprofServer.Close(); err != nil {
		// 报错就终止。
		t.Fatalf("Expected no error when closing disabled pprof server, got: %v", err)
	}
}

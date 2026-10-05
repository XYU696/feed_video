// package redis：注意测试文件和被测代码在【同一个包】里（包名也叫 redis）。
// 这样测试可以直接访问包内未导出的东西（如 Client 结构体里的 rdb 字段）。
//
// 本文件测试限流用的 IncrementWithExpire：验证两件事——
//  1. 第一次计数会给 key 设置过期时间（TTL）；
//  2. 之后再次计数只加数字，【不会】把过期时间往后顺延（固定窗口限流的关键）。
//
// 测试用 miniredis：一个纯 Go 实现的"内存版 Redis"，跑单测不用真的装 Redis。
package redis

import (
	// context：测试用上下文。
	"context"
	// testing：Go 标准测试库。
	"testing"
	// time：过期时长、快进时间。
	"time"

	// miniredis：假 Redis 服务器。
	miniredis "github.com/alicebob/miniredis/v2"
	// goredis：真的 go-redis 客户端，让它去连 miniredis。
	goredis "github.com/redis/go-redis/v9"
)

// TestIncrementWithExpireSetsTTLWithoutExtendingWindow 是一个测试函数。
// Go 约定：所有测试函数都以 Test 开头、参数是 t *testing.T，
// 用 `go test` 命令时会被自动发现并执行。
func TestIncrementWithExpireSetsTTLWithoutExtendingWindow(t *testing.T) {
	// miniredis.Run 启动一个假 Redis；mr 是它的实例、err 错误。
	mr, err := miniredis.Run()
	if err != nil {
		// t.Fatalf 打印信息并【立即终止】本测试（后面没法继续）。
		t.Fatalf("start miniredis: %v", err)
	}
	// defer：测试结束关掉假 Redis。
	defer mr.Close()

	// client 手工组装一个 Redis Client：
	client := &Client{
		// rdb 字段放真客户端，地址指向 miniredis 的监听地址。
		rdb: goredis.NewClient(&goredis.Options{Addr: mr.Addr()}),
	}
	// 测试结束关闭连接池。
	defer client.Close()

	// ctx 用空白上下文。
	ctx := context.Background()
	// key 测试用的键名。
	key := "feedsystem:ratelimit:test"
	// expire 过期时间 30 秒。
	expire := 30 * time.Second

	// count 第一次自增（应返回 1）；err 错误。
	count, err := client.IncrementWithExpire(ctx, key, expire)
	if err != nil {
		// 出错就终止。
		t.Fatalf("first increment: %v", err)
	}
	// 计数不等于预期的 1。
	if count != 1 {
		// 终止并打印实际值。
		t.Fatalf("expected count 1, got %d", count)
	}

	// firstTTL 直接问 miniredis 这个 key 当前的 TTL。
	firstTTL := mr.TTL(key)
	// TTL 应在 (0, 30s] 范围内。
	if firstTTL <= 0 || firstTTL > expire {
		// 不对就终止。
		t.Fatalf("expected ttl in (0, %s], got %s", expire, firstTTL)
	}

	// mr.FastForward 让 miniredis 的内部时钟【快进 5 秒】（真实时间不用等），key 的 TTL 会随之减少。
	mr.FastForward(5 * time.Second)
	// ttlBeforeSecond 记录第二次自增前的 TTL（应该约剩 25 秒）。
	ttlBeforeSecond := mr.TTL(key)

	// count 第二次自增（应返回 2）；err 错误。
	count, err = client.IncrementWithExpire(ctx, key, expire)
	if err != nil {
		// 出错终止。
		t.Fatalf("second increment: %v", err)
	}
	// 计数应为 2。
	if count != 2 {
		// 不对终止。
		t.Fatalf("expected count 2, got %d", count)
	}

	// ttlAfterSecond 第二次自增后的 TTL。
	ttlAfterSecond := mr.TTL(key)
	// 关键断言：TTL 必须和自增前【完全一样】——
	// 说明第二次操作没有把窗口重置成 30 秒，否则固定窗口限流就失效了。
	if ttlAfterSecond != ttlBeforeSecond {
		// 不一致就终止。
		t.Fatalf("expected ttl to stay at %s, got %s", ttlBeforeSecond, ttlAfterSecond)
	}
}

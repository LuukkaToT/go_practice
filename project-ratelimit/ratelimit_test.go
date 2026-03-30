// Package ratelimit 集成测试：4 组覆盖固定窗口边界突刺和滑动窗口精确限流。
//
// 运行方式（仅需 Redis 在线）：
//
//	go test ./project-ratelimit/... -v -timeout 30s
//
// 前置条件：docker-compose up -d（infrastructure/docker-compose.yml）
package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infra "gopractice/infrastructure"
	"gopractice/project-ratelimit/application"
	appinfra "gopractice/project-ratelimit/infra"
	"gopractice/project-ratelimit/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// 测试辅助
// ─────────────────────────────────────────────────────────────────────────────

// cleanKeys 测试结束后清理 Redis 限流 key（避免测试间干扰）。
func cleanKeys(clientID string) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	// 用 SCAN 清理所有 ratelimit:* 前缀的 key（测试专用）
	iter := rdb.Scan(ctx, 0, fmt.Sprintf("ratelimit:*%s*", clientID), 0).Iterator()
	for iter.Next(ctx) {
		rdb.Del(ctx, iter.Val())
	}
}

// sendBatch 并发发送 n 个请求，返回（通过数, 被拒数）。
// handler 是单个请求的处理函数，返回 true 代表通过。
func sendBatch(n int, handler func() bool) (passed, rejected int32) {
	var wg sync.WaitGroup
	var passedCnt, rejectedCnt atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if handler() {
				passedCnt.Add(1)
			} else {
				rejectedCnt.Add(1)
			}
		}()
	}
	wg.Wait()
	return passedCnt.Load(), rejectedCnt.Load()
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 1：BadCase — 固定窗口边界突刺
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCase_FixedWindow_BoundaryBurst 演示固定窗口在时间边界处的双倍流量问题。
//
// 【测试设计】
// 窗口大小 200ms，阈值 5。
// 第1批：在当前窗口末尾（~190ms 后）发 5 个请求 → 全部通过（W1 计数=5）
// sleep 等窗口切换
// 第2批：在新窗口开始（~10ms 后）发 5 个请求 → 全部通过（W2 计数=5，W2 重置）
// → 在约 200ms 时间内，实际放行 10 个请求（是阈值的 2 倍！）
func TestBadCase_FixedWindow_BoundaryBurst(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	const limit = 5
	const windowMS = int64(200)
	clientID := fmt.Sprintf("test-fixed-burst-%d", time.Now().UnixNano())
	defer cleanKeys(clientID)

	cfg, _ := domain.NewRateLimit(time.Duration(windowMS)*time.Millisecond, limit, "api:/order")
	limiter := appinfra.NewRedisFixedWindowLimiter(rdb)
	svc := application.NewBadCaseSvc(limiter)

	fmt.Println("\n========== [BadCase 测试1] 固定窗口边界突刺 ==========")
	fmt.Printf("配置：窗口=%dms，阈值=%d\n", windowMS, limit)

	// 等到窗口末尾（窗口剩余时间约 10ms 时开始）
	// nowMs / windowMS 得到 windowIndex，乘回去得到窗口起始时间
	nowMs := time.Now().UnixMilli()
	windowIndex := nowMs / windowMS
	windowEnd := (windowIndex + 1) * windowMS
	// 等到窗口结束前 10ms
	waitUntil := windowEnd - 10
	sleepDuration := time.Duration(waitUntil-nowMs) * time.Millisecond
	if sleepDuration > 0 {
		time.Sleep(sleepDuration)
	}

	fmt.Printf("[第1批] 在窗口 W%d 末尾发 %d 个请求...\n", windowIndex, limit)
	passed1, rejected1 := sendBatch(limit, func() bool {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		return ok
	})
	fmt.Printf("[第1批结果] 通过=%d 拒绝=%d（W%d 计数=%d）\n", passed1, rejected1, windowIndex, passed1)

	// 等窗口切换（等到新窗口的前 10ms 内）
	time.Sleep(20 * time.Millisecond)

	nowMs2 := time.Now().UnixMilli()
	windowIndex2 := nowMs2 / windowMS
	fmt.Printf("[第2批] 在窗口 W%d 开始发 %d 个请求...\n", windowIndex2, limit)
	passed2, rejected2 := sendBatch(limit, func() bool {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		return ok
	})
	fmt.Printf("[第2批结果] 通过=%d 拒绝=%d（W%d 计数=%d）\n", passed2, rejected2, windowIndex2, passed2)

	total := passed1 + passed2
	fmt.Printf("\n[统计] 两批合计通过=%d，阈值=%d\n", total, limit)

	if total > limit {
		fmt.Printf("☠️  [BadCase确认] 边界突刺！在窗口切换时刻通过了 %d 个请求（阈值 %d 的 %.1f 倍）\n",
			total, limit, float64(total)/float64(limit))
	} else {
		fmt.Printf("⚠️  [偶发] 本次测试未复现边界突刺（时间精度问题，可重试）\n")
	}

	fmt.Println("\n[结论] 固定窗口的根本缺陷：窗口切换时新窗口计数重置，上一窗口的请求不参与新窗口计算")
	fmt.Println("       → 使用 GoodCase 滑动窗口可彻底消除此问题")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 2：BadCase — 固定窗口正常场景（阈值内拒绝）
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCase_FixedWindow_Normal 验证固定窗口在正常场景下的限流有效性。
func TestBadCase_FixedWindow_Normal(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	const limit = 5
	clientID := fmt.Sprintf("test-fixed-normal-%d", time.Now().UnixNano())
	defer cleanKeys(clientID)

	cfg, _ := domain.NewRateLimit(1*time.Second, limit, "api:/order")
	limiter := appinfra.NewRedisFixedWindowLimiter(rdb)
	svc := application.NewBadCaseSvc(limiter)

	fmt.Println("\n========== [BadCase 测试2] 固定窗口正常限流 ==========")
	fmt.Printf("配置：窗口=1s，阈值=%d，发送 %d 个请求\n", limit, limit+1)

	// 同一窗口内发 limit+1 个请求（串行发送，避免并发竞争影响断言）
	passed, rejected := 0, 0
	for i := 0; i < limit+1; i++ {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		if ok {
			passed++
		} else {
			rejected++
			fmt.Printf("[请求%d] 🚫 被拒绝（正确！计数已达阈值）\n", i+1)
		}
	}

	fmt.Printf("[结果] 通过=%d 拒绝=%d\n", passed, rejected)
	if rejected != 1 {
		t.Errorf("期望拒绝 1 个，实际拒绝 %d 个", rejected)
	}
	fmt.Println("[断言] 第 limit+1 个请求被正确拒绝 ✅")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 3：GoodCase — 滑动窗口边界场景（无突刺）
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_SlidingWindow_BoundaryBurst 验证滑动窗口在边界时刻无突刺。
//
// 同样的场景：窗口 200ms，阈值 5，在窗口切换时发两批请求。
// 预期：两批合计通过数 ≤ limit（而不是 BadCase 的 2×limit）。
func TestGoodCase_SlidingWindow_BoundaryBurst(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	const limit = 5
	const windowMS = int64(300) // 稍长一些，给 sleep 留余量
	clientID := fmt.Sprintf("test-sliding-burst-%d", time.Now().UnixNano())
	defer cleanKeys(clientID)

	cfg, _ := domain.NewRateLimit(time.Duration(windowMS)*time.Millisecond, limit, "api:/order")
	limiter := appinfra.NewRedisSlidingWindowLimiter(rdb)
	svc := application.NewGoodCaseSvc(limiter)

	fmt.Println("\n========== [GoodCase 测试3] 滑动窗口边界无突刺 ==========")
	fmt.Printf("配置：窗口=%dms，阈值=%d\n", windowMS, limit)
	fmt.Println("场景：在窗口末尾发第1批，等窗口切换后发第2批")

	// 第1批：立即发 limit 个
	fmt.Printf("[第1批] 发 %d 个请求...\n", limit)
	passed1, rejected1 := sendBatch(limit, func() bool {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		return ok
	})
	fmt.Printf("[第1批结果] 通过=%d 拒绝=%d\n", passed1, rejected1)

	// sleep 短暂时间（不超过窗口大小，确保两批都在同一滑动窗口内）
	sleepMs := windowMS / 3
	fmt.Printf("[等待] sleep %dms（在窗口内，第1批记录仍在滑动窗口中）...\n", sleepMs)
	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	// 第2批：再发 limit 个（此时滑动窗口内已有第1批的记录）
	fmt.Printf("[第2批] 发 %d 个请求...\n", limit)
	passed2, rejected2 := sendBatch(limit, func() bool {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		return ok
	})
	fmt.Printf("[第2批结果] 通过=%d 拒绝=%d\n", passed2, rejected2)

	total := passed1 + passed2
	fmt.Printf("\n[统计] 两批合计通过=%d，阈值=%d\n", total, limit)

	if int64(total) <= int64(limit) {
		fmt.Printf("✅ [GoodCase确认] 滑动窗口严格限流！两批合计 %d ≤ 阈值 %d，无边界突刺\n", total, limit)
	} else {
		t.Errorf("滑动窗口漏洞！两批合计 %d > 阈值 %d", total, limit)
	}

	fmt.Println("\n[结论] 滑动窗口以当前时刻为右边界动态计算，第1批的记录在窗口内始终参与计数，杜绝突刺")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 4：GoodCase — 100 并发严格限流
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_SlidingWindow_Concurrent 验证高并发下滑动窗口精确限流。
//
// 100 个 goroutine 同时发请求，limit=10，验证通过数严格等于 10。
// 核心验证点：Lua 脚本原子执行，无竞态 → 不会放行超过 limit 的请求。
func TestGoodCase_SlidingWindow_Concurrent(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	const limit = 10
	const concurrency = 100
	clientID := fmt.Sprintf("test-sliding-concurrent-%d", time.Now().UnixNano())
	defer cleanKeys(clientID)

	cfg, _ := domain.NewRateLimit(5*time.Second, limit, "api:/pay")
	limiter := appinfra.NewRedisSlidingWindowLimiter(rdb)
	svc := application.NewGoodCaseSvc(limiter)

	fmt.Println("\n========== [GoodCase 测试4] 100并发滑动窗口精确限流 ==========")
	fmt.Printf("配置：窗口=5s，阈值=%d，并发=%d\n", limit, concurrency)

	passed, rejected := sendBatch(concurrency, func() bool {
		ok, _ := svc.HandleRequest(ctx, cfg, clientID)
		return ok
	})

	fmt.Printf("[结果] 通过=%d 拒绝=%d（合计=%d）\n", passed, rejected, passed+rejected)

	if int64(passed) != int64(limit) {
		t.Errorf("期望通过数严格=%d，实际=%d（Lua 原子性可能有问题）", limit, passed)
	} else {
		fmt.Printf("✅ [GoodCase确认] 通过数精确=%d，Lua 原子执行无竞态 ✅\n", passed)
	}

	fmt.Printf("[被拒绝] %d 个请求被正确拒绝\n", rejected)
	fmt.Println("\n[结论] Redis Lua 脚本保证原子性，100并发下通过数严格等于阈值，无超卖/超量问题")
	fmt.Println("       对比 BadCase（INCR+EXPIRE 非原子）：高并发时可能出现计数偏差")
}

// TestMain 所有测试前后打印分隔符。
func TestMain(m *testing.M) {
	fmt.Printf("\n%s\n  Demo 4: 微服务接口级限流 集成测试开始\n%s\n",
		strings.Repeat("─", 60), strings.Repeat("─", 60))
	m.Run()
	fmt.Printf("\n%s\n  集成测试结束\n%s\n",
		strings.Repeat("─", 60), strings.Repeat("─", 60))
}

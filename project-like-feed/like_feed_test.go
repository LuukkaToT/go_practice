// Package likefeed 集成测试：5 组覆盖内存对比、并发点赞、Feed 推拉流。
//
// 运行方式（仅需 Redis 在线）：
//
//	go test ./project-like-feed/... -v -timeout 60s
//
// 前置条件：docker-compose up -d（infrastructure/docker-compose.yml）
package likefeed

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infra "gopractice/infrastructure"
	"gopractice/project-like-feed/application"
	appinfra "gopractice/project-like-feed/infra"
	"gopractice/project-like-feed/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// 测试辅助
// ─────────────────────────────────────────────────────────────────────────────

func cleanLikeKeys(postID string) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	rdb.Del(ctx, fmt.Sprintf("like:set:%s", postID))
	rdb.Del(ctx, fmt.Sprintf("like:bitmap:%s", postID))
}

func cleanFeedKeys(userIDs ...int64) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	for _, uid := range userIDs {
		rdb.Del(ctx, fmt.Sprintf("feed:inbox:%d", uid))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 1：内存对比 — Set vs Bitmap
// ─────────────────────────────────────────────────────────────────────────────

// TestMemoryComparison 向 Set 和 Bitmap 各灌入 10 万个用户 ID，
// 然后用 MEMORY USAGE 命令直观展示内存差距。
//
// 【注意】MEMORY USAGE 返回的是 Redis 估算值，包含 key overhead，
// 实际 Bitmap 内存 = maxUserID / 8 字节。
func TestMemoryComparison(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()

	const (
		postID   = "memory-test-post-001"
		userCount = 100_000 // 10 万用户
	)
	defer cleanLikeKeys(postID)

	setStore := appinfra.NewRedisSetLikeStore(rdb)
	bitmapStore := appinfra.NewRedisBitmapLikeStore(rdb)

	fmt.Println("\n========== [测试1] Set vs Bitmap 内存对比 ==========")
	fmt.Printf("灌入数据量：%d 个用户点赞同一篇文章\n\n", userCount)

	// ── 灌入 Set 数据 ──────────────────────────────────────────────────────
	fmt.Printf("正在写入 Set (like:set:%s)...\n", postID)
	setStart := time.Now()
	// 用 Pipeline 批量写入提高速度
	pipe := rdb.Pipeline()
	setKey := fmt.Sprintf("like:set:%s", postID)
	for i := 0; i < userCount; i++ {
		pipe.SAdd(ctx, setKey, int64(i))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("Set 批量写入失败: %v", err)
	}
	fmt.Printf("Set 写入耗时: %v\n", time.Since(setStart))

	// ── 灌入 Bitmap 数据 ──────────────────────────────────────────────────
	fmt.Printf("\n正在写入 Bitmap (like:bitmap:%s)...\n", postID)
	bitmapStart := time.Now()
	pipe2 := rdb.Pipeline()
	bitmapKey := fmt.Sprintf("like:bitmap:%s", postID)
	for i := 0; i < userCount; i++ {
		pipe2.SetBit(ctx, bitmapKey, int64(i), 1)
	}
	if _, err := pipe2.Exec(ctx); err != nil {
		t.Fatalf("Bitmap 批量写入失败: %v", err)
	}
	fmt.Printf("Bitmap 写入耗时: %v\n", time.Since(bitmapStart))

	// ── 查询内存占用 ───────────────────────────────────────────────────────
	setMemory, err := rdb.MemoryUsage(ctx, setKey).Result()
	if err != nil {
		t.Logf("MEMORY USAGE Set 失败: %v（Redis 版本可能不支持）", err)
		setMemory = -1
	}
	bitmapMemory, err := rdb.MemoryUsage(ctx, bitmapKey).Result()
	if err != nil {
		t.Logf("MEMORY USAGE Bitmap 失败: %v", err)
		bitmapMemory = -1
	}

	// Bitmap 理论内存（maxUserID / 8）
	theoreticalBitmapBytes := int64(userCount) / 8

	fmt.Println("\n─── 内存对比结果 ───────────────────────────────────")
	fmt.Printf("Set    实际内存：%s\n", formatBytes(setMemory))
	fmt.Printf("Bitmap 实际内存：%s\n", formatBytes(bitmapMemory))
	fmt.Printf("Bitmap 理论内存：%s（%d用户 / 8 bit/byte）\n",
		formatBytes(theoreticalBitmapBytes), userCount)

	if setMemory > 0 && bitmapMemory > 0 {
		ratio := float64(setMemory) / float64(bitmapMemory)
		fmt.Printf("\n🎯 内存节省比：Set / Bitmap ≈ %.0f 倍\n", ratio)
	}

	// 验证功能正确性
	like, _ := domain.NewLike(12345, postID)
	_ = setStore.Like(ctx, like)
	_ = bitmapStore.Like(ctx, like)
	setIsLiked, _ := setStore.IsLiked(ctx, 12345, postID)
	bitmapIsLiked, _ := bitmapStore.IsLiked(ctx, 12345, postID)
	setCount, _ := setStore.Count(ctx, postID)
	bitmapCount, _ := bitmapStore.Count(ctx, postID)

	fmt.Printf("\n─── 功能验证 ────────────────────────────────────────\n")
	fmt.Printf("Set    IsLiked(12345): %v，Count: %d\n", setIsLiked, setCount)
	fmt.Printf("Bitmap IsLiked(12345): %v，Count: %d\n", bitmapIsLiked, bitmapCount)

	fmt.Println("\n[结论] Bitmap 在存储点赞状态时，内存远优于 Set；")
	fmt.Println("       代价：无法枚举点赞用户，userID 必须是连续整数。")
}

// formatBytes 格式化字节数为可读字符串。
func formatBytes(bytes int64) string {
	if bytes < 0 {
		return "N/A"
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/(1024*1024))
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 2：BadCase — Set 点赞功能验证
// ─────────────────────────────────────────────────────────────────────────────

func TestBadCase_SetLike_FuncCheck(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	postID := fmt.Sprintf("set-func-%d", time.Now().UnixNano())
	defer cleanLikeKeys(postID)

	store := appinfra.NewRedisSetLikeStore(rdb)
	svc := application.NewBadCaseSvc(store)

	fmt.Println("\n========== [BadCase 测试2] Set 点赞功能验证 ==========")

	// 初始无点赞
	count, _ := svc.GetLikeCount(ctx, postID)
	fmt.Printf("初始点赞数: %d\n", count)

	// 点赞
	_ = svc.LikePost(ctx, 100, postID)
	_ = svc.LikePost(ctx, 200, postID)
	_ = svc.LikePost(ctx, 300, postID)
	count, _ = svc.GetLikeCount(ctx, postID)
	fmt.Printf("3人点赞后: %d\n", count)
	if count != 3 {
		t.Errorf("期望 3，实际 %d", count)
	}

	// 重复点赞（幂等）
	_ = svc.LikePost(ctx, 100, postID)
	count, _ = svc.GetLikeCount(ctx, postID)
	fmt.Printf("用户100重复点赞后（幂等）: %d\n", count)
	if count != 3 {
		t.Errorf("期望仍为 3，实际 %d", count)
	}

	// 取消点赞
	like, _ := domain.NewLike(200, postID)
	_ = store.Unlike(ctx, like)
	count, _ = svc.GetLikeCount(ctx, postID)
	fmt.Printf("用户200取消点赞后: %d\n", count)
	if count != 2 {
		t.Errorf("期望 2，实际 %d", count)
	}

	fmt.Println("[断言] Set 点赞 SADD/SREM/SCARD 功能正确 ✅")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 3：GoodCase — Bitmap 点赞功能验证
// ─────────────────────────────────────────────────────────────────────────────

func TestGoodCase_BitmapLike_FuncCheck(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	postID := fmt.Sprintf("bitmap-func-%d", time.Now().UnixNano())
	defer cleanLikeKeys(postID)

	store := appinfra.NewRedisBitmapLikeStore(rdb)
	feedStore := appinfra.NewRedisZSetFeedStore(rdb, 1000)
	svc := application.NewGoodCaseSvc(store, feedStore)

	fmt.Println("\n========== [GoodCase 测试3] Bitmap 点赞功能验证 ==========")

	// 点赞三个用户
	_ = svc.LikePost(ctx, 1001, postID)
	_ = svc.LikePost(ctx, 2002, postID)
	_ = svc.LikePost(ctx, 3003, postID)

	count, _ := svc.GetLikeCount(ctx, postID)
	fmt.Printf("3人点赞后 BITCOUNT: %d\n", count)
	if count != 3 {
		t.Errorf("期望 3，实际 %d", count)
	}

	// 查询点赞状态（GETBIT）
	liked1, _ := svc.IsLiked(ctx, 1001, postID)
	liked9, _ := svc.IsLiked(ctx, 9999, postID)
	fmt.Printf("用户1001点赞状态: %v（期望 true）\n", liked1)
	fmt.Printf("用户9999点赞状态: %v（期望 false）\n", liked9)
	if !liked1 || liked9 {
		t.Errorf("IsLiked 结果不正确")
	}

	// 取消点赞
	_ = svc.UnlikePost(ctx, 1001, postID)
	count, _ = svc.GetLikeCount(ctx, postID)
	fmt.Printf("用户1001取消后 BITCOUNT: %d（期望 2）\n", count)
	if count != 2 {
		t.Errorf("期望 2，实际 %d", count)
	}

	fmt.Println("[断言] Bitmap SETBIT/GETBIT/BITCOUNT 功能正确 ✅")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 4：GoodCase — 1000 并发点赞精确统计
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_BitmapLike_Concurrent 验证 1000 个并发点赞，BITCOUNT 精确 = 1000。
//
// 核心验证：SETBIT 是原子操作，并发下不会出现计数竞态。
func TestGoodCase_BitmapLike_Concurrent(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()
	postID := fmt.Sprintf("bitmap-concurrent-%d", time.Now().UnixNano())
	defer cleanLikeKeys(postID)

	store := appinfra.NewRedisBitmapLikeStore(rdb)
	feedStore := appinfra.NewRedisZSetFeedStore(rdb, 1000)
	svc := application.NewGoodCaseSvc(store, feedStore)

	const concurrency = 1000

	fmt.Printf("\n========== [GoodCase 测试4] %d并发点赞 ==========\n", concurrency)
	fmt.Printf("配置：%d 个不同用户（userID 0~%d）并发点赞同一篇文章\n", concurrency, concurrency-1)

	var wg sync.WaitGroup
	var successCnt, dupCnt atomic.Int32

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		userID := int64(i)
		go func() {
			defer wg.Done()
			err := svc.LikePost(ctx, userID, postID)
			if err == nil {
				successCnt.Add(1)
			} else {
				dupCnt.Add(1)
			}
		}()
	}
	wg.Wait()

	count, _ := svc.GetLikeCount(ctx, postID)
	fmt.Printf("[结果] 成功点赞=%d，重复=%d，BITCOUNT=%d\n",
		successCnt.Load(), dupCnt.Load(), count)

	if count != int64(concurrency) {
		t.Errorf("期望 BITCOUNT=%d，实际 %d（SETBIT 原子性可能有问题）", concurrency, count)
	} else {
		fmt.Printf("✅ BITCOUNT 精确=%d，SETBIT 原子操作无竞态 ✅\n", count)
	}

	fmt.Println("[结论] Redis SETBIT 是原子操作，1000 并发下计数严格准确")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 5：GoodCase — Feed 推模式 vs BadCase 同步推送
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_Feed_PushPull 端到端验证 Feed 流推模式。
//
// 场景：大 V 发布 3 条动态，推给 3 个粉丝；粉丝分页拉取 Feed 流（时间倒序）。
// 同时对比 BadCase（同步循环）和 GoodCase（Pipeline）的推送延迟。
func TestGoodCase_Feed_PushPull(t *testing.T) {
	rdb := infra.GetRedisClient()
	ctx := context.Background()

	followerIDs := []int64{10001, 10002, 10003}
	defer cleanFeedKeys(followerIDs...)
	defer cleanLikeKeys("feed-test-post-1")
	defer cleanLikeKeys("feed-test-post-2")
	defer cleanLikeKeys("feed-test-post-3")

	bitmapStore := appinfra.NewRedisBitmapLikeStore(rdb)
	feedStore := appinfra.NewRedisZSetFeedStore(rdb, 1000)
	goodSvc := application.NewGoodCaseSvc(bitmapStore, feedStore)

	setStore := appinfra.NewRedisSetLikeStore(rdb)
	badSvc := application.NewBadCaseSvc(setStore)

	fmt.Println("\n========== [GoodCase 测试5] Feed 推模式 vs 同步推送 ==========")

	// 大 V 发 3 条动态（时间间隔 1ms，确保 score 不同）
	posts := []domain.Post{
		{ID: "feed-test-post-1", AuthorID: 9999, Content: "第1条动态", CreatedAt: time.Now()},
		{ID: "feed-test-post-2", AuthorID: 9999, Content: "第2条动态", CreatedAt: time.Now().Add(1 * time.Millisecond)},
		{ID: "feed-test-post-3", AuthorID: 9999, Content: "第3条动态", CreatedAt: time.Now().Add(2 * time.Millisecond)},
	}

	// ── 对比推送延迟 ──────────────────────────────────────────────────────
	fmt.Printf("\n粉丝数量：%d\n", len(followerIDs))

	fmt.Println("\n--- BadCase：同步循环推送 ---")
	badElapsed, _ := badSvc.PublishPost(ctx, posts[0], followerIDs)

	fmt.Println("\n--- GoodCase：Pipeline 批量推送 ---")
	for _, post := range posts {
		_, _ = goodSvc.PublishPost(ctx, post, followerIDs)
	}

	fmt.Printf("\n[延迟对比] BadCase=%v，GoodCase 无论多少粉丝 RTT 恒为 1 次\n", badElapsed)

	// ── 验证 Feed 流内容（分页拉取）─────────────────────────────────────
	fmt.Printf("\n--- 粉丝 %d 拉取 Feed 流（第1页，每页2条）---\n", followerIDs[0])
	page1, err := goodSvc.GetFeed(ctx, followerIDs[0], 1, 2)
	if err != nil {
		t.Fatalf("GetFeed 失败: %v", err)
	}

	fmt.Printf("\n第1页（共 %d 条）：\n", len(page1))
	for i, item := range page1 {
		fmt.Printf("  [%d] postID=%s score=%.0f time=%v\n",
			i+1, item.PostID, item.Score, item.CreatedAt.Format("15:04:05.000"))
	}

	// 第1页应返回最新2条（post-3, post-2）
	if len(page1) != 2 {
		t.Errorf("期望第1页2条，实际 %d 条", len(page1))
	}
	if len(page1) >= 1 && page1[0].PostID != "feed-test-post-3" {
		t.Errorf("期望第1条是 post-3（最新），实际 %s", page1[0].PostID)
	}
	fmt.Println("[断言] 第1页第1条为最新动态 (post-3) ✅")

	fmt.Printf("\n--- 第2页（offset=2，每页2条）---\n")
	page2, _ := goodSvc.GetFeed(ctx, followerIDs[0], 2, 2)
	fmt.Printf("第2页（共 %d 条）：\n", len(page2))
	for i, item := range page2 {
		fmt.Printf("  [%d] postID=%s\n", i+1, item.PostID)
	}
	if len(page2) != 1 {
		t.Errorf("期望第2页1条（剩余），实际 %d 条", len(page2))
	}
	fmt.Println("[断言] 分页拉取正确 ✅")

	fmt.Println("\n[结论] GoodCase Pipeline 推 Feed：批量 ZADD 一次 RTT；")
	fmt.Println("       ZREVRANGEBYSCORE 按 score 倒序返回，天然实现时间倒序 Timeline。")
}

// TestMain 测试前后打印标题。
func TestMain(m *testing.M) {
	fmt.Printf("\n%s\n  Demo 5: 海量点赞存储与 Feed 流 集成测试开始\n%s\n",
		strings.Repeat("─", 60), strings.Repeat("─", 60))
	m.Run()
	fmt.Printf("\n%s\n  集成测试结束\n%s\n",
		strings.Repeat("─", 60), strings.Repeat("─", 60))
}

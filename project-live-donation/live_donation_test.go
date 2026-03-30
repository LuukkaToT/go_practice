//go:build integration

// 直播打赏高并发集成测试
//
// 前置条件：
//   cd infrastructure && docker compose up -d（MySQL:3307, Redis:6380 均已起）
//
// 运行：
//
//	go test -tags=integration -v -count=1 -timeout=120s ./project-live-donation/...
//
// 预期控制台输出：
//   TestBadCase_ConcurrentGift  → 耗时较长（行锁排队），打印 DB 查询排行榜
//   TestGoodCase_ConcurrentGift → 耗时极短（Redis Lua），打印 ZSET 实时排行榜
//   TestGoodCase_AsyncSettle    → 验证 Consumer 异步落库正确性
package livedonation_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraPkg "gopractice/infrastructure"
	"gopractice/project-live-donation/application"
	"gopractice/project-live-donation/domain"
	"gopractice/project-live-donation/infra"
	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// 测试常量
// ─────────────────────────────────────────────────────────────────────────────

const (
	testRoomID    = "room-888"    // 测试直播间 ID
	testStreamerID = int64(1)     // 主播 ID
	giftAmount    = int64(50)     // 每次打赏 50 分
	initBalance   = int64(5000)  // 每人初始余额 5000 分
	userCount     = 10           // 测试用户数量
	giftsPerUser  = 100          // 每人打赏次数（50*100=5000，恰好花光）
	totalGifts    = userCount * giftsPerUser // 1000 次并发打赏
)

var testUserIDs = func() []int64 {
	ids := make([]int64, userCount)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}()

// ─────────────────────────────────────────────────────────────────────────────
// 测试夹具（fixture）
// ─────────────────────────────────────────────────────────────────────────────

type fixture struct {
	wallet       *infra.RedisWalletStore
	rank         *infra.RedisRankStore
	userRepo     *infra.MySQLUserRepo
	streamerRepo *infra.MySQLStreamerRepo
	donationRepo *infra.MySQLDonationRepo
	uow          *infra.MySQLUnitOfWork
}

func setupFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	db := infraPkg.InitMySQLDB("db_live_donation")
	rdb := infraPkg.GetRedisClient()

	// 幂等建表
	if err := infra.InitTable(db); err != nil {
		t.Fatalf("InitTable: %v", err)
	}

	// 确保测试用户和主播存在
	if err := infra.EnsureUsers(ctx, db, testUserIDs, initBalance); err != nil {
		t.Fatalf("EnsureUsers: %v", err)
	}
	if err := infra.EnsureStreamer(ctx, db, testStreamerID); err != nil {
		t.Fatalf("EnsureStreamer: %v", err)
	}

	wallet := infra.NewRedisWalletStore(rdb)
	rank := infra.NewRedisRankStore(rdb)
	userRepo := infra.NewMySQLUserRepo(db, nil)
	streamerRepo := infra.NewMySQLStreamerRepo(db, nil)
	donationRepo := infra.NewMySQLDonationRepo(db, nil)
	uow := infra.NewMySQLUnitOfWork(db)

	return &fixture{
		wallet:       wallet,
		rank:         rank,
		userRepo:     userRepo,
		streamerRepo: streamerRepo,
		donationRepo: donationRepo,
		uow:          uow,
	}
}

// resetState 每次测试前重置 DB 数据和 Redis 状态。
func resetState(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()

	db := infraPkg.InitMySQLDB("db_live_donation")
	rdb := infraPkg.GetRedisClient()

	// 重置 DB：用户余额归位，清空打赏流水，主播收益清零
	if err := infra.ResetTestData(ctx, db, testUserIDs, testStreamerID, initBalance); err != nil {
		t.Fatalf("ResetTestData: %v", err)
	}

	// 重置 Redis：清空钱包 Key 和排行榜 Key
	infra.DeleteWalletKeys(ctx, rdb, testUserIDs)
	infra.DeleteRankKey(ctx, rdb, testRoomID)

	// 预热 Redis 钱包（将 DB 余额加载到 Redis）
	for _, uid := range testUserIDs {
		if err := f.wallet.LoadBalance(ctx, uid, initBalance); err != nil {
			t.Fatalf("LoadBalance user=%d: %v", uid, err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 辅助：打印排行榜
// ─────────────────────────────────────────────────────────────────────────────

func printRank(label string, items []ports.RankItem) {
	fmt.Printf("\n  %-30s\n", "━━━ "+label+" ━━━")
	fmt.Printf("  %-6s %-12s %-12s\n", "排名", "用户ID", "累计打赏(分)")
	fmt.Printf("  %-6s %-12s %-12s\n", "──────", "────────────", "────────────")
	for _, item := range items {
		fmt.Printf("  %-6d %-12d %-12d\n", item.Rank, item.UserID, item.Score)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestBadCase_ConcurrentGift：MySQL 事务并发打赏（行锁热点演示）
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCase_ConcurrentGift 演示 BadCase：1000 并发打赏全走 MySQL 事务。
//
// 预期现象：
//   - 耗时明显（秒级）：所有事务排队等待 ld_users / ld_streamers 行锁
//   - 成功数 = 1000（每人 100 次 × 10 人，恰好花光余额，无余额不足错误）
//   - 排行榜由 DB GROUP BY 计算，每人贡献 5000 分
func TestBadCase_ConcurrentGift(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	// BadCase 不需要 Redis 钱包预热，但需要重置 DB 状态
	db := infraPkg.InitMySQLDB("db_live_donation")
	rdb := infraPkg.GetRedisClient()
	if err := infra.ResetTestData(ctx, db, testUserIDs, testStreamerID, initBalance); err != nil {
		t.Fatalf("ResetTestData: %v", err)
	}
	infra.DeleteRankKey(ctx, rdb, testRoomID)

	badSvc := application.NewBadCaseSvc(f.uow, f.donationRepo)

	var (
		successCount atomic.Int64
		failCount    atomic.Int64
		wg           sync.WaitGroup
	)

	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("【BadCase】%d 并发打赏 → MySQL 事务（行锁热点）\n", totalGifts)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	start := time.Now()
	for i := 0; i < totalGifts; i++ {
		wg.Add(1)
		userID := testUserIDs[i%userCount]
		go func(uid int64) {
			defer wg.Done()
			d, _ := domain.NewDonation(uid, testStreamerID, testRoomID, giftAmount)
			if err := badSvc.SendGift(ctx, d); err != nil {
				failCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(userID)
	}
	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("  耗时：%v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  成功：%d / %d\n", successCount.Load(), totalGifts)
	fmt.Printf("  失败：%d（余额不足/锁超时）\n", failCount.Load())

	// 获取 DB 排行榜（BadCase）
	rankItems, err := badSvc.GetRank(ctx, testRoomID, 10)
	if err != nil {
		t.Fatalf("BadCase GetRank: %v", err)
	}
	printRank("BadCase 排行榜（MySQL GROUP BY SUM）", rankItems)

	// 验证：成功数 + 失败数 = 总请求数
	if successCount.Load()+failCount.Load() != int64(totalGifts) {
		t.Errorf("请求总数不符：success=%d fail=%d total=%d",
			successCount.Load(), failCount.Load(), totalGifts)
	}
	// 验证：无超扣（所有成功的打赏合计不超过总余额）
	totalDeducted := successCount.Load() * giftAmount
	maxPossible := int64(userCount) * initBalance
	if totalDeducted > maxPossible {
		t.Errorf("⚠️  超扣！总扣款 %d > 最大余额 %d", totalDeducted, maxPossible)
	} else {
		fmt.Printf("  ✅ 无超扣（总扣款 %d 分 ≤ 最大余额 %d 分）\n", totalDeducted, maxPossible)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestGoodCase_ConcurrentGift：Redis Lua 并发打赏（高性能演示）
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_ConcurrentGift 演示 GoodCase：1000 并发打赏走 Redis Lua + ZSET + MQ。
//
// 预期现象：
//   - 耗时极短（毫秒级）：Lua 原子扣款无行锁
//   - 成功数 = 1000（余额恰好够，全部成功）
//   - ZSET 排行榜实时准确，每人 5000 分
//   - Redis 余额全部精确归零（无超扣/漏扣）
func TestGoodCase_ConcurrentGift(t *testing.T) {
	f := setupFixture(t)
	resetState(t, f)
	ctx := context.Background()

	// 使用 ChanPublisher 模拟 MQ（无需真实 RabbitMQ）
	chanPub := infra.NewChanPublisher(totalGifts + 100)
	goodSvc := application.NewGoodCaseSvc(f.wallet, f.rank, chanPub)

	var (
		successCount atomic.Int64
		failCount    atomic.Int64
		wg           sync.WaitGroup
	)

	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("【GoodCase】%d 并发打赏 → Redis Lua 扣款 + ZSET 排行榜\n", totalGifts)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	start := time.Now()
	for i := 0; i < totalGifts; i++ {
		wg.Add(1)
		userID := testUserIDs[i%userCount]
		go func(uid int64) {
			defer wg.Done()
			d, _ := domain.NewDonation(uid, testStreamerID, testRoomID, giftAmount)
			if err := goodSvc.SendGift(ctx, d); err != nil {
				failCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(userID)
	}
	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("  耗时：%v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  成功：%d / %d\n", successCount.Load(), totalGifts)
	fmt.Printf("  失败：%d（余额不足/钱包未加载）\n", failCount.Load())

	// 获取 ZSET 排行榜（GoodCase）
	rankItems, err := goodSvc.GetRank(ctx, testRoomID, 10)
	if err != nil {
		t.Fatalf("GoodCase GetRank: %v", err)
	}
	printRank("GoodCase 排行榜（Redis ZSET 实时）", rankItems)

	// 验证 1：无超扣
	if successCount.Load()+failCount.Load() != int64(totalGifts) {
		t.Errorf("请求总数不符：success=%d fail=%d", successCount.Load(), failCount.Load())
	}
	if failCount.Load() > 0 {
		t.Errorf("⚠️  有 %d 次失败（余额应该恰好够，预期全部成功）", failCount.Load())
	}

	// 验证 2：所有用户的 Redis 余额精确归零（50分 × 100次 = 5000分，恰好花光）
	fmt.Printf("\n  ── Redis 余额验证 ──\n")
	allZero := true
	for _, uid := range testUserIDs {
		bal, err := f.wallet.GetBalance(ctx, uid)
		if err != nil {
			t.Errorf("GetBalance user=%d: %v", uid, err)
			continue
		}
		status := "✅"
		if bal != 0 {
			status = "❌"
			allZero = false
		}
		fmt.Printf("  用户 %2d 余额：%d 分 %s\n", uid, bal, status)
	}
	if allZero {
		fmt.Printf("  ✅ 所有用户余额精确归零（Lua 原子扣款无超扣/漏扣）\n")
	} else {
		t.Error("❌ 部分用户余额未归零，存在漏扣！")
	}

	// 验证 3：ZSET 排行榜中每人 Score = 5000 分
	fmt.Printf("\n  ── ZSET Score 验证 ──\n")
	for _, item := range rankItems {
		expected := int64(giftsPerUser) * giftAmount
		status := "✅"
		if item.Score != expected {
			status = "❌"
			t.Errorf("用户 %d ZSET Score=%d，期望=%d", item.UserID, item.Score, expected)
		}
		fmt.Printf("  用户 %2d：Score=%d %s\n", item.UserID, item.Score, status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestGoodCase_AsyncSettle：验证 Consumer 异步落库正确性
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_AsyncSettle 验证 ChanPublisher + Consumer 的异步落库链路。
//
// 场景：
//   100 次打赏 → GoodCase 发布到 channel → Consumer 消费落库
//   验证：ld_donations 中的记录数 = 100；主播收益 = 100 × 50 = 5000 分
func TestGoodCase_AsyncSettle(t *testing.T) {
	f := setupFixture(t)
	resetState(t, f)
	ctx := context.Background()

	const giftNum = 100

	chanPub := infra.NewChanPublisher(giftNum + 10)
	goodSvc := application.NewGoodCaseSvc(f.wallet, f.rank, chanPub)

	// 启动 Consumer goroutine（异步消费 chanPub 中的消息）
	consumer := infra.NewDonationConsumer(f.streamerRepo, f.donationRepo)
	stopCh := make(chan struct{})
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		// 使用内存 channel 直接消费（不经过真实 MQ）
		for {
			select {
			case <-stopCh:
				// 排空剩余消息
				for {
					select {
					case msg := <-chanPub.Chan():
						consumer.Settle(ctx, msg) //nolint:errcheck
					default:
						return
					}
				}
			case msg := <-chanPub.Chan():
				consumer.Settle(ctx, msg) //nolint:errcheck
			}
		}
	}()

	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("【GoodCase 异步落库】%d 次打赏 → Consumer 落库验证\n", giftNum)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	// 发送打赏（全走 user_1）
	for i := 0; i < giftNum; i++ {
		d, _ := domain.NewDonation(testUserIDs[0], testStreamerID, testRoomID, giftAmount)
		if err := goodSvc.SendGift(ctx, d); err != nil {
			t.Fatalf("SendGift[%d]: %v", i, err)
		}
	}
	fmt.Printf("  ✅ %d 次打赏已发布到 ChanPublisher\n", giftNum)

	// 等待 Consumer 消费完所有消息（最多等 5s）
	deadline := time.Now().Add(5 * time.Second)
	db := infraPkg.InitMySQLDB("db_live_donation")
	for time.Now().Before(deadline) {
		count, _ := infra.CountDonations(ctx, db, testRoomID)
		if count >= giftNum {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 停止 Consumer
	close(stopCh)
	consumerWg.Wait()

	// 验证 1：ld_donations 中的记录数
	count, err := infra.CountDonations(ctx, db, testRoomID)
	if err != nil {
		t.Fatalf("CountDonations: %v", err)
	}
	fmt.Printf("  ld_donations 记录数：%d（期望 %d）", count, giftNum)
	if count == int64(giftNum) {
		fmt.Printf(" ✅\n")
	} else {
		fmt.Printf(" ❌\n")
		t.Errorf("ld_donations 记录数=%d，期望=%d", count, giftNum)
	}

	// 验证 2：主播累计收益
	earnings, err := infra.GetStreamerEarnings(ctx, db, testStreamerID)
	if err != nil {
		t.Fatalf("GetStreamerEarnings: %v", err)
	}
	expectedEarnings := int64(giftNum) * giftAmount
	fmt.Printf("  主播收益：%d 分（期望 %d 分）", earnings, expectedEarnings)
	if earnings == expectedEarnings {
		fmt.Printf(" ✅\n")
	} else {
		fmt.Printf(" ❌\n")
		t.Errorf("主播收益=%d，期望=%d", earnings, expectedEarnings)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestBadCaseVsGoodCase_LatencyComparison：耗时直观对比
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCaseVsGoodCase_LatencyComparison 在同一个测试中对比 BadCase 和 GoodCase 的耗时。
//
// 此测试是面试场景的最直观展示：
//   BadCase  (MySQL): ??? ms   ← 行锁排队
//   GoodCase (Redis): ??? ms   ← 无行锁，极速
func TestBadCaseVsGoodCase_LatencyComparison(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()
	db := infraPkg.InitMySQLDB("db_live_donation")
	rdb := infraPkg.GetRedisClient()

	// ─── BadCase ───
	if err := infra.ResetTestData(ctx, db, testUserIDs, testStreamerID, initBalance); err != nil {
		t.Fatalf("reset: %v", err)
	}
	infra.DeleteRankKey(ctx, rdb, testRoomID)

	badSvc := application.NewBadCaseSvc(f.uow, f.donationRepo)
	var (
		badSuccess atomic.Int64
		badFail    atomic.Int64
		wg         sync.WaitGroup
	)
	badStart := time.Now()
	for i := 0; i < totalGifts; i++ {
		wg.Add(1)
		uid := testUserIDs[i%userCount]
		go func(id int64) {
			defer wg.Done()
			d, _ := domain.NewDonation(id, testStreamerID, testRoomID, giftAmount)
			if err := badSvc.SendGift(ctx, d); err != nil {
				badFail.Add(1)
			} else {
				badSuccess.Add(1)
			}
		}(uid)
	}
	wg.Wait()
	badElapsed := time.Since(badStart)

	// ─── GoodCase ───
	if err := infra.ResetTestData(ctx, db, testUserIDs, testStreamerID, initBalance); err != nil {
		t.Fatalf("reset: %v", err)
	}
	infra.DeleteWalletKeys(ctx, rdb, testUserIDs)
	infra.DeleteRankKey(ctx, rdb, testRoomID)
	for _, uid := range testUserIDs {
		f.wallet.LoadBalance(ctx, uid, initBalance)
	}

	chanPub := infra.NewChanPublisher(totalGifts + 100)
	goodSvc := application.NewGoodCaseSvc(f.wallet, f.rank, chanPub)
	var (
		goodSuccess atomic.Int64
		goodFail    atomic.Int64
	)
	goodStart := time.Now()
	for i := 0; i < totalGifts; i++ {
		wg.Add(1)
		uid := testUserIDs[i%userCount]
		go func(id int64) {
			defer wg.Done()
			d, _ := domain.NewDonation(id, testStreamerID, testRoomID, giftAmount)
			if err := goodSvc.SendGift(ctx, d); err != nil {
				goodFail.Add(1)
			} else {
				goodSuccess.Add(1)
			}
		}(uid)
	}
	wg.Wait()
	goodElapsed := time.Since(goodStart)

	// ─── 打印对比结果 ───
	fmt.Printf("\n")
	fmt.Printf("┌────────────────────────────────────────────────────────┐\n")
	fmt.Printf("│          1000 并发打赏：BadCase vs GoodCase 对比         │\n")
	fmt.Printf("├──────────────┬─────────────────┬────────────────────────┤\n")
	fmt.Printf("│ %-12s │ %-15s │ %-22s │\n", "方案", "耗时", "成功/失败")
	fmt.Printf("├──────────────┼─────────────────┼────────────────────────┤\n")
	fmt.Printf("│ %-12s │ %-15v │ %d 成功 / %d 失败    │\n",
		"BadCase(MySQL)", badElapsed.Round(time.Millisecond), badSuccess.Load(), badFail.Load())
	fmt.Printf("│ %-12s │ %-15v │ %d 成功 / %d 失败    │\n",
		"GoodCase(Redis)", goodElapsed.Round(time.Millisecond), goodSuccess.Load(), goodFail.Load())
	fmt.Printf("└──────────────┴─────────────────┴────────────────────────┘\n")

	if badElapsed > 0 && goodElapsed > 0 {
		ratio := float64(badElapsed) / float64(goodElapsed)
		fmt.Printf("  🚀 GoodCase 比 BadCase 快 %.1fx\n", ratio)
	}

	// 打印 ZSET 排行榜
	rankItems, _ := goodSvc.GetRank(ctx, testRoomID, 10)
	printRank("GoodCase ZSET 实时排行榜", rankItems)
}

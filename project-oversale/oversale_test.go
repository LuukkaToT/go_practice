//go:build integration

// 秒杀超卖集成测试
//
// 前置条件：
//  1. cd infrastructure && docker compose up -d（MySQL:3307, Redis:6380 均已起）
//  2. mysql -uroot -p123456 -P3307 db_oversale < project-oversale/sql/init.sql
//
// 运行命令：
//
//	go test -tags=integration -v -count=1 -timeout=60s ./project-oversale/...
//
// 预期效果：
//   - TestBadCase_WillOversell   → 库存可能变负，控制台打印"⚠️  超卖发生"
//   - TestGoodCase_Lua           → 库存精确归零，无超卖
//   - TestGoodCase_Lock          → 库存精确归零，无超卖
//   - TestGoodCase_Optimistic    → 库存精确归零，无超卖（成功次数可能 < 10，因重试耗尽放弃）

package oversale_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	infraPkg "gopractice/infrastructure"
	"gopractice/project-oversale/application"
	"gopractice/project-oversale/infra"
)

// ============================================================
// 测试常量
// ============================================================

const (
	testProductID  = int64(1)
	initialStock   = int64(10)  // 初始库存
	concurrency    = 100        // 并发 goroutine 数
)

// ============================================================
// 测试 fixtures：setup / teardown
// ============================================================

type fixture struct {
	repo  *infra.MySQLStockRepo
	cache *infra.RedisStockCache
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := infraPkg.InitMySQLDB("db_oversale")
	rdb := infraPkg.GetRedisClient()

	// 确保表存在（使用原生 SQL 创建，幂等）
	sqls := []string{
		`CREATE TABLE IF NOT EXISTS flash_products (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(100) NOT NULL DEFAULT '',
			stock INT NOT NULL DEFAULT 0,
			version INT NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`INSERT IGNORE INTO flash_products (id, name, stock, version)
		 VALUES (1, '限量秒杀商品', 10, 0)`,
	}
	for _, s := range sqls {
		if err := db.Exec(s).Error; err != nil {
			t.Logf("init sql warning: %v", err)
		}
	}

	return &fixture{
		repo:  &infra.MySQLStockRepo{DB: db},
		cache: &infra.RedisStockCache{RDB: rdb},
	}
}

// setup 每个测试用例前重置库存，保证用例独立。
func (f *fixture) setup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.repo.ResetStock(ctx, testProductID, initialStock); err != nil {
		t.Fatalf("ResetStock mysql: %v", err)
	}
	if err := f.cache.InitStock(ctx, testProductID, initialStock); err != nil {
		t.Fatalf("InitStock redis: %v", err)
	}
}

// ============================================================
// 并发测试辅助：runConcurrent 启动 n 个 goroutine 执行 fn，收集成功/失败数
// ============================================================

type runResult struct {
	success int64
	fail    int64
}

func runConcurrent(n int, fn func() error) runResult {
	var (
		wg      sync.WaitGroup
		success atomic.Int64
		fail    atomic.Int64
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				fail.Add(1)
			} else {
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	return runResult{success: success.Load(), fail: fail.Load()}
}

// ============================================================
// ❌ BadCase：读-判断-写（非原子），100 并发下必超卖
// ============================================================

func TestBadCase_WillOversell(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()

	svc := &application.BadCaseSvc{Repo: f.repo}
	res := runConcurrent(concurrency, func() error {
		return svc.Purchase(ctx, testProductID)
	})

	finalStock, _ := f.repo.CurrentStock(ctx, testProductID)

	fmt.Println()
	fmt.Println("========== ❌ BadCase：非原子读-判断-写 ==========")
	fmt.Printf("  初始库存     : %d\n", initialStock)
	fmt.Printf("  并发请求数   : %d\n", concurrency)
	fmt.Printf("  成功购买次数 : %d（正常应 ≤ %d）\n", res.success, initialStock)
	fmt.Printf("  失败次数     : %d\n", res.fail)
	fmt.Printf("  最终DB库存   : %d\n", finalStock)
	if finalStock < 0 {
		fmt.Println("  ⚠️  超卖发生！库存变为负数，说明多个 goroutine 同时通过了「库存>0」判断")
	} else if res.success > initialStock {
		fmt.Println("  ⚠️  超卖发生！成功购买次数超过库存上限")
	} else {
		fmt.Println("  ✅ 本次未观察到超卖（并发窗口较窄，可增大 concurrency 或 time.Sleep 复现）")
	}
	fmt.Println("=================================================")
	fmt.Println()
	// BadCase 测试不 Fatal：目的是演示问题，而非断言失败阻断 CI
}

// ============================================================
// ✅ GoodCase 1：Redis Lua 原子扣减
// ============================================================

func TestGoodCase_Lua_NoOversell(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()

	svc := &application.GoodCaseLuaSvc{
		Cache: f.cache,
		Repo:  f.repo,
	}
	res := runConcurrent(concurrency, func() error {
		return svc.Purchase(ctx, testProductID)
	})

	finalStock, _ := f.repo.CurrentStock(ctx, testProductID)
	redisStock, _ := f.cache.GetStock(ctx, testProductID)

	fmt.Println()
	fmt.Println("========== ✅ GoodCase 1：Redis Lua 原子扣减 ==========")
	fmt.Printf("  初始库存      : %d\n", initialStock)
	fmt.Printf("  并发请求数    : %d\n", concurrency)
	fmt.Printf("  成功购买次数  : %d（应 = %d）\n", res.success, initialStock)
	fmt.Printf("  失败次数      : %d（应 = %d）\n", res.fail, concurrency-int(initialStock))
	fmt.Printf("  最终DB库存    : %d（应 = 0）\n", finalStock)
	fmt.Printf("  最终Redis库存 : %d（应 = 0）\n", redisStock)
	fmt.Println("=======================================================")
	fmt.Println()

	if res.success != initialStock {
		t.Errorf("成功次数 %d ≠ 初始库存 %d", res.success, initialStock)
	}
	if finalStock != 0 {
		t.Errorf("最终DB库存 %d ≠ 0，超卖或少卖", finalStock)
	}
	if redisStock != 0 {
		t.Errorf("最终Redis库存 %d ≠ 0", redisStock)
	}
}

// ============================================================
// ✅ GoodCase 2：Redis SETNX 分布式锁
// ============================================================

func TestGoodCase_Lock_NoOversell(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()

	svc := &application.GoodCaseLockSvc{
		Cache: f.cache,
		Repo:  f.repo,
	}
	res := runConcurrent(concurrency, func() error {
		return svc.Purchase(ctx, testProductID)
	})

	finalStock, _ := f.repo.CurrentStock(ctx, testProductID)

	fmt.Println()
	fmt.Println("========== ✅ GoodCase 2：SETNX 分布式锁 ==========")
	fmt.Printf("  初始库存      : %d\n", initialStock)
	fmt.Printf("  并发请求数    : %d\n", concurrency)
	fmt.Printf("  成功购买次数  : %d（应 = %d）\n", res.success, initialStock)
	fmt.Printf("  失败次数      : %d\n", res.fail)
	fmt.Printf("  最终DB库存    : %d（应 = 0）\n", finalStock)
	fmt.Println("====================================================")
	fmt.Println()

	if res.success != initialStock {
		t.Errorf("成功次数 %d ≠ 初始库存 %d（部分 goroutine 可能锁重试耗尽）", res.success, initialStock)
	}
	if finalStock != 0 {
		t.Errorf("最终DB库存 %d ≠ 0", finalStock)
	}
}

// ============================================================
// ✅ GoodCase 3：MySQL 乐观锁（version CAS）
// ============================================================

func TestGoodCase_Optimistic_NoOversell(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()

	svc := &application.GoodCaseOptimisticSvc{
		Repo:     f.repo,
		MaxRetry: 10, // 提高重试上限，让更多 goroutine 最终成功
	}
	res := runConcurrent(concurrency, func() error {
		return svc.Purchase(ctx, testProductID)
	})

	finalStock, _ := f.repo.CurrentStock(ctx, testProductID)

	fmt.Println()
	fmt.Println("========== ✅ GoodCase 3：MySQL 乐观锁 ==========")
	fmt.Printf("  初始库存      : %d\n", initialStock)
	fmt.Printf("  并发请求数    : %d\n", concurrency)
	fmt.Printf("  成功购买次数  : %d（应 = %d，可能因重试耗尽略少）\n", res.success, initialStock)
	fmt.Printf("  失败次数      : %d\n", res.fail)
	fmt.Printf("  最终DB库存    : %d（应 = 0 或 > 0 若有重试耗尽）\n", finalStock)
	fmt.Println("==================================================")
	fmt.Println()

	// 乐观锁高并发时 CAS 失败概率高，部分 goroutine 可能重试耗尽；
	// 核心断言：成功次数 ≤ 初始库存（绝不超卖），且库存 ≥ 0。
	if res.success > initialStock {
		t.Errorf("超卖！成功次数 %d > 初始库存 %d", res.success, initialStock)
	}
	if finalStock < 0 {
		t.Errorf("库存为负 %d，超卖！", finalStock)
	}
	// 如果成功次数恰好等于库存，给出额外提示
	if res.success == initialStock && finalStock == 0 {
		t.Logf("完美：所有库存被精确消耗，无超卖无剩余")
	}
}

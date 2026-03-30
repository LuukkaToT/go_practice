//go:build integration

// 缓存三大问题综合治理集成测试
//
// 前置条件：
//   cd infrastructure && docker compose up -d
//
// 运行命令：
//   go test -tags=integration -v -count=1 -timeout=60s ./project-cache-guard/...
//
// 预期控制台输出对比：
//   TestBadCase_Breakdown       → DB 被查询 ~100 次（击穿！）
//   TestBadCase_Penetration     → DB 被查询 100 次（穿透！）
//   TestGoodCase_Singleflight   → DB 被查询 1 次（Singleflight合并）
//   TestGoodCase_BloomFilter    → DB 被查询 0 次（布隆过滤器拦截）
//   TestGoodCase_RandomTTL      → 展示 TTL 分布散布（无雪崩风险）

package cache_guard_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraPkg "gopractice/infrastructure"
	"gopractice/project-cache-guard/application"
	"gopractice/project-cache-guard/domain"
	"gopractice/project-cache-guard/infra"
	"gopractice/project-cache-guard/ports"
)

// ============================================================
// 计数器装饰器：观察 DB 被实际查询了多少次
// ============================================================

// countingRepo 包装 ProductRepository，统计 GetByID 被调用次数。
// 【设计模式 | Decorator】不修改原实现，在外层包一个计数逻辑。
type countingRepo struct {
	inner ports.ProductRepository
	hits  atomic.Int64
}

func (c *countingRepo) GetByID(ctx context.Context, id int64) (*domain.Product, error) {
	c.hits.Add(1) // 每次真正查 DB 时计数 +1
	return c.inner.GetByID(ctx, id)
}

// ============================================================
// Test Fixture
// ============================================================

type fixture struct {
	repo  *infra.MySQLProductRepo
	cache *infra.RedisProductCache
	bloom *infra.RedisBloomFilter
}

const bloomKey = "cache:product:bloom:test"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := infraPkg.InitMySQLDB("db_cache_guard")
	rdb := infraPkg.GetRedisClient()

	repo := &infra.MySQLProductRepo{DB: db}
	if err := repo.InitTable(context.Background()); err != nil {
		t.Logf("init table warning: %v", err)
	}
	return &fixture{
		repo:  repo,
		cache: &infra.RedisProductCache{RDB: rdb},
		bloom: &infra.RedisBloomFilter{RDB: rdb, BitKey: bloomKey},
	}
}

// setupProduct 在 DB 中写入商品并预热布隆过滤器；可选是否同时写缓存。
func (f *fixture) setupProduct(t *testing.T, p domain.Product, cacheIt bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.repo.UpsertProduct(ctx, p); err != nil {
		t.Fatalf("upsert product: %v", err)
	}
	if err := f.bloom.Add(ctx, p.ID); err != nil {
		t.Fatalf("bloom add: %v", err)
	}
	if cacheIt {
		_ = f.cache.Set(ctx, &p, 5*time.Minute)
	} else {
		_ = f.cache.Delete(ctx, p.ID) // 确保缓存中没有
	}
}

// resetBloom 重置布隆过滤器（每个测试隔离）。
func (f *fixture) resetBloom(t *testing.T) {
	t.Helper()
	_ = f.bloom.Reset(context.Background())
}

// ============================================================
// 并发执行辅助
// ============================================================

func runN(n int, fn func()) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	wg.Wait()
}

// ============================================================
// ❌ BadCase 1：缓存击穿（Cache Breakdown）
// ============================================================

func TestBadCase_Breakdown(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 商品在 DB 中，但缓存中没有（模拟热点key过期）
	p := domain.Product{ID: 1, Name: "旗舰手机", Price: 599900, Stock: 1000}
	f.setupProduct(t, p, false /* 不写缓存 */)

	counting := &countingRepo{inner: f.repo}
	svc := &application.BadCacheSvc{Repo: counting, Cache: f.cache}

	runN(100, func() { _ = svc.GetProduct(ctx, 1) })

	dbHits := counting.hits.Load()
	fmt.Println()
	fmt.Println("========== ❌ BadCase: 缓存击穿 (Cache Breakdown) ==========")
	fmt.Println("  场景：热点商品缓存过期，100个请求同时来")
	fmt.Printf("  DB 查询次数: %d（应 = 1，实际 ≈ 100，击穿！每个请求都打了DB）\n", dbHits)
	fmt.Println("  根因：无并发合并机制，所有 goroutine 各自去查 DB")
	fmt.Println("============================================================")
	fmt.Println()
}

// ============================================================
// ❌ BadCase 2：缓存穿透（Cache Penetration）
// ============================================================

func TestBadCase_Penetration(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 不存在的商品 ID = 9999，DB 和缓存中都没有
	_ = f.cache.Delete(ctx, 9999)

	counting := &countingRepo{inner: f.repo}
	svc := &application.BadCacheSvc{Repo: counting, Cache: f.cache}

	runN(100, func() { _ = svc.GetProduct(ctx, 9999) })

	dbHits := counting.hits.Load()
	fmt.Println()
	fmt.Println("========== ❌ BadCase: 缓存穿透 (Cache Penetration) ==========")
	fmt.Println("  场景：100个请求查询根本不存在的商品ID(9999)")
	fmt.Printf("  DB 查询次数: %d（应 = 0，实际 = 100，穿透！ID不存在仍全部打DB）\n", dbHits)
	fmt.Println("  根因：无布隆过滤器，无空值缓存，每次都穿透到 DB")
	fmt.Println("  危害：攻击者可用大量不存在的ID打垮DB")
	fmt.Println("=============================================================")
	fmt.Println()
}

// ============================================================
// ✅ GoodCase 1：Singleflight 防击穿
// ============================================================

func TestGoodCase_Singleflight_Prevents_Breakdown(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 商品在 DB 中，缓存中没有（模拟过期场景）
	p := domain.Product{ID: 2, Name: "无线耳机", Price: 99900, Stock: 500}
	f.setupProduct(t, p, false /* 不写缓存 */)

	counting := &countingRepo{inner: f.repo}
	svc := application.NewGoodCacheSvc(counting, f.cache, f.bloom)

	runN(100, func() { _ = svc.GetProduct(ctx, 2) })

	dbHits := counting.hits.Load()
	fmt.Println()
	fmt.Println("========== ✅ GoodCase: Singleflight 防缓存击穿 ==========")
	fmt.Println("  场景：热点商品缓存过期，100个请求同时来")
	fmt.Printf("  DB 查询次数: %d（应 = 1，100个并发请求被合并为1次DB查询）\n", dbHits)
	fmt.Println("  原理：singleflight.Group.Do() 对同一 key 只让一个 goroutine 执行")
	fmt.Println("        其余 99 个等待，共享同一个结果，DB 压力降低 100 倍")
	fmt.Println("===========================================================")
	fmt.Println()

	if dbHits > 3 { // 允许少量并发漏网（singleflight 基于 goroutine 调度）
		t.Errorf("DB查询次数 %d 超过预期，Singleflight 未生效", dbHits)
	}
}

// ============================================================
// ✅ GoodCase 2：布隆过滤器防穿透
// ============================================================

func TestGoodCase_BloomFilter_Prevents_Penetration(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 布隆过滤器中只添加 id=1~10，id=9999 不添加
	for i := int64(1); i <= 10; i++ {
		if err := f.bloom.Add(ctx, i); err != nil {
			t.Fatalf("bloom add: %v", err)
		}
	}
	// 确保 9999 的缓存和 DB 都不存在
	_ = f.cache.Delete(ctx, 9999)

	counting := &countingRepo{inner: f.repo}
	svc := application.NewGoodCacheSvc(counting, f.cache, f.bloom)

	runN(100, func() { _ = svc.GetProduct(ctx, 9999) })

	dbHits := counting.hits.Load()
	fmt.Println()
	fmt.Println("========== ✅ GoodCase: 布隆过滤器防缓存穿透 ==========")
	fmt.Println("  场景：100个请求查询不存在的商品ID(9999)")
	fmt.Printf("  DB 查询次数: %d（应 = 0，布隆过滤器拦截所有请求，DB零查询）\n", dbHits)
	fmt.Println("  原理：ID 9999 未被 Add 进布隆过滤器")
	fmt.Println("        MayExist=false → 直接返回 ErrNotFound，不查缓存也不查DB")
	fmt.Println("========================================================")
	fmt.Println()

	if dbHits != 0 {
		t.Errorf("DB查询次数 %d ≠ 0，布隆过滤器未生效", dbHits)
	}
}

// ============================================================
// ✅ GoodCase 3：空值缓存 + 防穿透（布隆过滤器假阳性场景）
// ============================================================

func TestGoodCase_NullCache_Prevents_Penetration(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 模拟布隆过滤器假阳性：将 9998 添加进布隆过滤器，但 DB 中不存在
	// （真实场景：hash 碰撞导致误判）
	if err := f.bloom.Add(ctx, 9998); err != nil {
		t.Fatal(err)
	}
	_ = f.cache.Delete(ctx, 9998)

	counting := &countingRepo{inner: f.repo}
	svc := application.NewGoodCacheSvc(counting, f.cache, f.bloom)

	// 第一次查询：布隆过滤器误判通过 → 打一次DB → 写空值缓存
	_ = svc.GetProduct(ctx, 9998)
	firstHits := counting.hits.Load()

	// 后续 99 次查询：命中空值缓存 → DB 零查询
	runN(99, func() { _ = svc.GetProduct(ctx, 9998) })

	totalHits := counting.hits.Load()
	fmt.Println()
	fmt.Println("========== ✅ GoodCase: 空值缓存（布隆假阳性兜底）==========")
	fmt.Println("  场景：布隆过滤器误判（假阳性），9998 不在DB中但通过了布隆")
	fmt.Printf("  第1次查询 DB: %d 次（布隆误判，DB确认不存在，写入空值缓存）\n", firstHits)
	fmt.Printf("  后续99次DB : %d 次（命中空值缓存，不再查DB）\n", totalHits-firstHits)
	fmt.Println("  结论：布隆过滤器+空值缓存形成双重防护，假阳性代价极低")
	fmt.Println("=============================================================")
	fmt.Println()

	if totalHits > firstHits {
		t.Errorf("空值缓存未生效，后续查询仍有 %d 次DB访问", totalHits-firstHits)
	}
}

// ============================================================
// ✅ GoodCase 4：随机 TTL 防雪崩
// ============================================================

func TestGoodCase_RandomTTL_Prevents_Avalanche(t *testing.T) {
	f := newFixture(t)
	f.resetBloom(t)
	ctx := context.Background()

	// 初始化 10 个商品，GoodCase 写缓存时会用随机 TTL
	for i := int64(1); i <= 10; i++ {
		p := domain.Product{ID: i, Name: fmt.Sprintf("商品%d", i), Price: 9900 * i, Stock: 100}
		f.setupProduct(t, p, false)
		_ = f.bloom.Add(ctx, i)
	}

	svc := application.NewGoodCacheSvc(f.repo, f.cache, f.bloom)

	// 触发每个商品被查询一次（写入随机TTL缓存）
	for i := int64(1); i <= 10; i++ {
		_ = svc.GetProduct(ctx, i)
	}

	// 观察 TTL 分布
	fmt.Println()
	fmt.Println("========== ✅ GoodCase: 随机TTL防缓存雪崩 ==========")
	fmt.Println("  BadCase 写法：所有商品固定 5min TTL → 同时过期 → 雪崩")
	fmt.Println("  GoodCase写法：TTL = 5min + rand(0,60s) → 过期时间散布：")
	for i := int64(1); i <= 10; i++ {
		ttl, _ := f.cache.TTLOf(ctx, i)
		fmt.Printf("    商品%2d: TTL = %v\n", i, ttl.Round(time.Second))
	}
	fmt.Println("  结论：TTL 各不相同，过期时间散布在 [5min, 6min]，DB 压力平滑")
	fmt.Println("======================================================")
	fmt.Println()
}

package application

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"gopractice/project-cache-guard/domain"
	"gopractice/project-cache-guard/ports"
)

// ======================================================================
// GoodCase：布隆过滤器 + Singleflight + 随机TTL + 空值缓存
// 三种缓存问题综合防护
// ======================================================================

// GoodCacheSvc 综合缓存防护服务。
//
// 防护层次（每次 GetProduct 调用的执行路径）：
//
//  1. 布隆过滤器（防穿透第一道）：O(k) 位运算，极低开销
//     └─ MayExist=false → 直接返回 ErrProductNotFound，DB 零查询
//
//  2. 查缓存
//     └─ 命中空值占位 → 直接返回 ErrProductNotFound（防穿透第二道）
//     └─ 命中商品数据 → 返回，流程结束
//
//  3. Singleflight（防击穿）：同 key 并发 miss 时只有一个 goroutine 打 DB
//     └─ Do() 返回前，其他并发调用者阻塞等待同一结果
//
//  4. 查 DB + 写缓存
//     └─ 不存在 → SetNull（空值缓存，短 TTL）
//     └─ 存在   → Set（随机 TTL，防雪崩）
//
// 【可能问】「Singleflight 和分布式锁防击穿有何区别？」
// 【答】Singleflight 是进程内合并（同一进程的并发请求），适合单机/单副本场景；
// 分布式锁跨进程合并（多实例场景），开销更大但保护更全面；
// 生产中两者常叠加使用。
type GoodCacheSvc struct {
	Repo  ports.ProductRepository
	Cache ports.ProductCache
	Bloom ports.BloomFilter
	sf    singleflight.Group
}

// NewGoodCacheSvc 构造函数（singleflight.Group 零值即可用，无需显式初始化）。
func NewGoodCacheSvc(repo ports.ProductRepository, cache ports.ProductCache, bloom ports.BloomFilter) *GoodCacheSvc {
	return &GoodCacheSvc{Repo: repo, Cache: cache, Bloom: bloom}
}

const (
	baseTTL   = 5 * time.Minute
	jitterSec = 60 // 随机抖动最大秒数，防雪崩
	nullTTL   = 1 * time.Minute // 空值缓存短TTL
)

// GetProduct 综合防护查询。
func (s *GoodCacheSvc) GetProduct(ctx context.Context, id int64) error {
	// ── 防护层 1：布隆过滤器快速过滤不存在的 ID ──────────────────
	// 【复杂度】O(k)，k = hash函数个数，通常5~7，极快
	// 【可能问】「布隆过滤器加在哪一层？DB 写入时和缓存查询前都要吗？」
	// 【答】Add 在数据写入/预热时调用；MayExist 在每次查询最前面调用。
	exists, err := s.Bloom.MayExist(ctx, id)
	if err == nil && !exists {
		// 一定不存在，直接拒绝，DB 和缓存都不查
		return domain.ErrProductNotFound
	}

	// ── 防护层 2：查缓存 ──────────────────────────────────────────
	product, found, err := s.Cache.Get(ctx, id)
	if err == nil && found {
		if product == nil {
			return domain.ErrProductNotFound // 命中空值占位（防穿透第二道）
		}
		return nil // 缓存命中，正常返回
	}

	// ── 防护层 3：Singleflight 合并并发 DB 请求（防击穿）────────────
	// Do 保证：同一 key 的并发调用只有一个执行 fn，其余阻塞等待共享结果。
	// 【可能问】「DoChan 和 Do 有何区别？」
	// 【答】Do 阻塞调用者 goroutine；DoChan 返回 channel，调用者可 select + context。
	// 生产中如需支持超时取消，用 DoChan 更安全。
	sfKey := strconv.FormatInt(id, 10)
	_, sfErr, _ := s.sf.Do(sfKey, func() (interface{}, error) {
		// ── 防护层 4a：查 DB ─────────────────────────────────────
		p, dbErr := s.Repo.GetByID(ctx, id)
		if dbErr != nil {
			if errors.Is(dbErr, domain.ErrProductNotFound) {
				// ── 防护层 4b：空值缓存（防穿透第二道，覆盖 Bloom 假阳性）─
				_ = s.Cache.SetNull(ctx, id, nullTTL)
			}
			return nil, dbErr
		}
		// ── 防护层 4c：随机 TTL 防雪崩 ──────────────────────────
		// 加随机抖动后，批量商品过期时间散布在 [baseTTL, baseTTL+jitter] 之间，
		// 避免同一批次缓存同时失效引发 DB 压力峰值。
		ttl := baseTTL + time.Duration(rand.Intn(jitterSec))*time.Second
		_ = s.Cache.Set(ctx, p, ttl)
		return p, nil
	})
	return sfErr
}

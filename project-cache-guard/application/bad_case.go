// Package application 缓存治理应用服务层：编排查询逻辑，不含 SQL/Redis 细节。
package application

import (
	"context"
	"time"

	"gopractice/project-cache-guard/ports"
)

// ======================================================================
// BadCase：朴素"查缓存 → 查库 → 写缓存"
// 三大缓存问题根因演示
// ======================================================================

// BadCacheSvc 朴素缓存查询：无任何防护措施。
//
// 【缓存击穿 Cache Breakdown】
// 热点 key 过期瞬间，大量并发请求同时 cache miss，全部打到 DB。
// 本 Service 中：100 个 goroutine 同时查同一个过期 key → 100 次 DB 查询。
//
// 【缓存穿透 Cache Penetration】
// 查询根本不存在的 ID，缓存永远 miss，每次都打 DB。
// 攻击者可利用此点构造大量不存在的 key 打垮 DB。
//
// 【缓存雪崩 Cache Avalanche】
// 大批 key 使用相同 TTL，同时过期，瞬间大量请求打 DB。
// 本 Service 中：Set 时固定 TTL，批量商品同时过期时会有雪崩风险。
type BadCacheSvc struct {
	Repo  ports.ProductRepository
	Cache ports.ProductCache
}

// GetProduct 朴素查询：缓存 → DB → 回填（无任何并发保护）。
func (s *BadCacheSvc) GetProduct(ctx context.Context, id int64) error {
	// Step 1: 查缓存
	if _, found, err := s.Cache.Get(ctx, id); err == nil && found {
		return nil // 命中缓存
	}

	// 【击穿窗口】此处同时 miss 的所有 goroutine 全部进入 DB 查询。
	// 【穿透窗口】不存在的 id 每次都到这里，无拦截。

	// Step 2: 查 DB（并发下 N 个请求同时执行这一步）
	p, err := s.Repo.GetByID(ctx, id)
	if err != nil {
		return err // 不存在时也直接返回，不缓存空值 → 穿透持续
	}

	// Step 3: 写回缓存（固定 TTL = 雪崩隐患）
	// 若批量初始化时所有商品 TTL 相同，会在同一秒全部过期 → 雪崩。
	_ = s.Cache.Set(ctx, p, 5*time.Minute)
	return nil
}

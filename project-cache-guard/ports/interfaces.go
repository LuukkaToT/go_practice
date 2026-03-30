// Package ports 缓存治理场景端口定义（依赖倒置）。
//
// 【知识点 | 依赖倒置】Application 层只依赖这里的 interface；
// 具体实现（MySQL/Redis）在 infra 包，测试时可注入 fake/stub。
package ports

import (
	"context"
	"time"

	"gopractice/project-cache-guard/domain"
)

// ProductRepository 商品持久化仓库接口。
//
// 【面试点 | 接口粒度】只定义业务需要的方法，不照搬 CRUD；
// 让 infra 实现去关心 GORM/SQL 细节，Application 层只关心"给我商品"。
type ProductRepository interface {
	GetByID(ctx context.Context, id int64) (*domain.Product, error)
}

// ProductCache 商品缓存接口。
//
// 【Get 返回值语义说明】
//   - (product, true, nil)  → 缓存命中，有数据
//   - (nil, true, nil)      → 缓存命中，空值占位（商品不存在，防穿透）
//   - (nil, false, nil)     → 缓存未命中（cache miss）
//   - (nil, false, err)     → Redis 故障
//
// 【可能问】「为何空值也要缓存？」
// 【答】不存在的 ID 不缓存时，每次请求都会穿透到 DB。
// 缓存一个短 TTL 的空标记，DB 只被查询一次；代价是该 ID 真正被创建后需要主动删除缓存。
type ProductCache interface {
	Get(ctx context.Context, id int64) (product *domain.Product, found bool, err error)
	Set(ctx context.Context, p *domain.Product, ttl time.Duration) error
	// SetNull 写入空值占位（防穿透第二道）。
	SetNull(ctx context.Context, id int64, ttl time.Duration) error
	// Delete 删除缓存（测试setup / 主动失效）。
	Delete(ctx context.Context, id int64) error
}

// BloomFilter 布隆过滤器接口（防穿透第一道）。
//
// 【知识点 | 布隆过滤器特性】
//   - MayExist=false → 100% 不存在（无假阴性）→ 直接拒绝，DB 零查询
//   - MayExist=true  → 可能存在（有假阳性，约 1%）→ 继续走缓存/DB 流程
//
// 【可能问】「布隆过滤器的误判（假阳性）会有什么问题？」
// 【答】约 1% 的不存在 ID 会被误判为可能存在，仍会走缓存查询；
// 后续空值缓存会拦截这部分，整体 DB 请求量可接受。
// 关键是不会有假阴性：真实存在的 ID 不会被错误拒绝。
type BloomFilter interface {
	// Add 将 ID 加入布隆过滤器（数据写入时/预热时调用）。
	Add(ctx context.Context, id int64) error
	// MayExist 判断 ID 是否可能存在（false = 一定不存在）。
	MayExist(ctx context.Context, id int64) (bool, error)
}

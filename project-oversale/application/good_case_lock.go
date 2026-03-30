package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gopractice/project-oversale/domain"
	"gopractice/project-oversale/ports"
)

// ======================================================================
// GoodCase 2：Redis SETNX 分布式锁
// ======================================================================

// GoodCaseLockSvc 用 Redis SETNX 分布式锁串行化库存扣减。
//
// 【分布式锁工作流程】
//  1. SETNX lock_key unique_token EX ttl_seconds → 成功则持锁
//  2. 执行业务：查库存 → 扣库存
//  3. Lua 脚本原子释放：GET == token → DEL（防误删）
//
// 【可能问】「SETNX 和 SET NX EX 有何区别？」
// 【答】老版本 SETNX 不支持超时，需要再调 EXPIRE，两步非原子（宕机可能死锁）。
// 现代写法用 SET key value NX PX ttl 一条命令，原子设置值+超时。
//
// 【可能问】「锁超时了但业务还没跑完怎么办？」
// 【答】锁续期（Watchdog）：后台 goroutine 每隔 ttl/3 检测业务是否仍在运行，
// 是则 EXPIRE 续期。Redisson（Java）自动实现；Go 需手动实现或用 redsync 库。
// 本 Demo 简化：ttl 设够大 + 业务超时保护，生产建议加 Watchdog。
//
// 【可能问】「主从切换时分布式锁会失效吗？」
// 【答】会（主写锁，同步到从前主挂，新主没有这把锁）。
// RedLock 算法（多数派写入 N 个独立 Redis 节点）可降低概率但有争议（Martin Kleppmann）。
// 工业实践：单节点锁 + 业务幂等兜底通常足够。
type GoodCaseLockSvc struct {
	Cache ports.StockCache
	Repo  ports.StockRepository
}

const (
	lockTTL     = 3 * time.Second
	maxRetry    = 3
	retryDelay  = 50 * time.Millisecond
)

// Purchase 分布式锁版本：同一商品同一时刻只有一个 goroutine 执行扣减。
func (s *GoodCaseLockSvc) Purchase(ctx context.Context, productID int64) error {
	lockKey := fmt.Sprintf("oversale:lock:%d", productID)
	token := newToken()

	// 尝试获取锁（有重试，模拟真实业务等待）。
	var locked bool
	for i := 0; i < maxRetry; i++ {
		ok, err := s.Cache.TryLock(ctx, lockKey, token, lockTTL)
		if err != nil {
			return err
		}
		if ok {
			locked = true
			break
		}
		time.Sleep(retryDelay)
	}
	if !locked {
		return domain.ErrConflict
	}
	// 无论成功与否都要释放锁，Lua 原子校验 token 防误删。
	defer func() { _ = s.Cache.ReleaseLock(ctx, lockKey, token) }()

	// 临界区：持锁后串行执行，无并发竞争。
	stock, err := s.Repo.GetStock(ctx, productID)
	if err != nil {
		return err
	}
	if stock.Available <= 0 {
		return domain.ErrOutOfStock
	}
	return s.Repo.DeductDirect(ctx, productID, 1)
}

// newToken 生成本次加锁的唯一标识，防止锁被其他持有者误释放。
func newToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

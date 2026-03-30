package application

import (
	"context"

	"gopractice/project-oversale/domain"
	"gopractice/project-oversale/ports"
)

// ======================================================================
// GoodCase 1：Redis Lua 脚本原子扣减
// ======================================================================

// GoodCaseLuaSvc 使用 Redis Lua 脚本在单次 EVAL 中原子完成「读-判断-扣减」。
//
// 【为什么 Lua 能解决超卖？】
// Redis 是单线程执行命令的（即使 6.0+ 有 IO 多线程，命令执行仍是单线程）。
// EVAL 执行 Lua 脚本期间，其他任何客户端命令都无法插入，
// 因此「GET → 判断 → DECRBY」三步在 Redis 侧是真正原子的。
//
// 【可能问】「Lua 脚本执行期间 Redis 会阻塞吗？」
// 【答】会。Lua 是原子执行的，过长的脚本会导致 Redis 卡顿；
// 秒杀 Lua 脚本极短（3 行），实测微秒级，生产安全。
//
// 【可能问】「Redis 扣成功后服务崩溃，MySQL 还没扣，如何处理？」
// 【答】这是 Redis+DB 双写一致性问题。工业实践：
//   a) 接受短暂不一致，定时对账任务修复；
//   b) 用 Outbox 模式：Redis 扣成功后发 MQ，Consumer 落库（本 repo 已有实现）；
//   c) 先扣 DB（事务），成功后更新 Redis；Redis 失败则降级走 DB。
type GoodCaseLuaSvc struct {
	Cache ports.StockCache
	Repo  ports.StockRepository // 可选：Lua 成功后异步/同步落库
}

// Purchase Lua 原子扣减：Redis 侧一步完成，成功后同步落 MySQL。
func (s *GoodCaseLuaSvc) Purchase(ctx context.Context, productID int64) error {
	// 一次 EVALSHA：原子读-判断-DECRBY，无并发窗口。
	ok, err := s.Cache.DeductWithLua(ctx, productID, 1)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrOutOfStock
	}

	// Redis 扣成功 → 同步落 MySQL（演示用简单写法；生产推荐 MQ 异步落库解耦）。
	// 注意：若此处失败，Redis 已扣但 DB 未扣，需对账修复。
	// 【面试加分】可说明这里可以改为发 MQ + Consumer 幂等落库（见 project-live-donation）。
	if err := s.Repo.DeductDirect(ctx, productID, 1); err != nil {
		return err
	}
	return nil
}

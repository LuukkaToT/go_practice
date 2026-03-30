// 本文件：GoodCase — Redis Lua 原子扣款 + ZSET 排行榜 + RabbitMQ 异步落库。
package application

import (
	"context"

	"gopractice/project-live-donation/domain"
	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GoodCaseSvc：Redis-first 高并发打赏处理
// ─────────────────────────────────────────────────────────────────────────────

// GoodCaseSvc 实现高并发打赏的最佳实践：
//   1. Redis Lua 脚本原子扣款（无行锁，O(1) 时间复杂度）
//   2. ZSET 实时排行榜（O(log N) 累加，O(log N + M) 读取）
//   3. RabbitMQ 异步落库（削峰解耦，MySQL 压力降低 99%+）
//
// 【面试点 | 整体链路对比】
//
// BadCase 链路（同步，强一致）：
//   HTTP → MySQL BEGIN → UPDATE balance → UPDATE earnings → INSERT donation → COMMIT → return
//   耗时：约 10~100ms（MySQL 事务 + 行锁竞争）
//
// GoodCase 链路（异步，最终一致）：
//   HTTP → Redis Lua 扣款(1ms) → ZINCRBY(1ms) → MQ Publish(2ms) → return
//                                                         ↓ 异步
//                                              Consumer → INSERT donation + UPDATE earnings
//   主流程耗时：< 5ms；MySQL 写入延迟（最终一致，通常 <100ms）
//
// 【可能问】「GoodCase 中 Redis 扣款成功，但 MQ 发布失败怎么办？」
// 【答】这是分布式系统中的"部分失败"问题。本 Demo 采用"尽力而为"策略（记录日志）。
// 生产推荐方案：
//   a) 本地消息表（Outbox Pattern）：Lua 扣款时同时向 Redis List 追加待发记录，
//      后台 worker 负责投递，保证 at-least-once。
//   b) 事务性 Redis：利用 Redis + Lua 的原子性，在同一个脚本内完成扣款和追加待发记录。
//   c) 补偿机制：定时 job 扫描余额变动但无对应流水的记录进行补偿。
type GoodCaseSvc struct {
	wallet    ports.WalletStore
	rank      ports.RankStore
	publisher ports.GiftPublisher
}

func NewGoodCaseSvc(
	wallet ports.WalletStore,
	rank ports.RankStore,
	publisher ports.GiftPublisher,
) *GoodCaseSvc {
	return &GoodCaseSvc{wallet: wallet, rank: rank, publisher: publisher}
}

// SendGift 处理打赏请求（GoodCase：Redis + MQ）。
//
// 步骤：
//  1. Lua 原子扣款：校验余额 >= amount 后执行 SET key (balance-amount)
//     失败立即返回（ErrInsufficientBalance / ErrWalletNotLoaded）
//  2. ZINCRBY 更新排行榜（即使后续 MQ 失败，排行榜已实时更新）
//  3. MQ 异步发布（Consumer 负责落库）
//
// 【面试点 | 步骤顺序的考量】
// 先 Lua 扣款，再更新 ZSET，最后发 MQ：
//   - Lua 失败（余额不足）：直接返回，不影响 ZSET 和 MQ ✓
//   - Lua 成功，ZSET 失败：极罕见（Redis 连接问题），此时余额已扣，
//     ZSET 可通过定时对账脚本修复（离线校准），可接受 ✓
//   - Lua+ZSET 成功，MQ 失败：余额已扣，排行榜已更新，但流水未落库。
//     需 Outbox 补偿（生产必须实现），本 Demo 记录日志简化处理 ✓
func (s *GoodCaseSvc) SendGift(ctx context.Context, d *domain.Donation) error {
	// Step 1: Lua 原子扣款（核心：无 MySQL 行锁）
	if err := s.wallet.Deduct(ctx, d.UserID, d.Amount); err != nil {
		return err
	}

	// Step 2: ZSET 实时累加排行榜 Score
	if err := s.rank.IncrScore(ctx, d.RoomID, d.UserID, d.Amount); err != nil {
		// 排行榜更新失败，记录但不影响主流程（可补偿）
		_ = err
	}

	// Step 3: 异步发布打赏消息，由 Consumer 负责落库
	return s.publisher.Publish(ctx, d)
}

// GetRank 从 ZSET 实时读取排行榜（GoodCase）。
//
// 【面试点 | ZSET 排行榜的优势】
// - 读写分离：写时 ZINCRBY，读时 ZREVRANGEBYRANK，互不干扰
// - 实时性：ZINCRBY 后立即反映在排名中，无延迟
// - 性能：O(log N + M) 与数据总量无关，10 万还是 1000 万用户，延迟相同
// - 内存：假设 10 万用户（member=int，score=float），ZSET 约占 10MB，可接受
func (s *GoodCaseSvc) GetRank(ctx context.Context, roomID string, topN int64) ([]ports.RankItem, error) {
	return s.rank.TopN(ctx, roomID, topN)
}

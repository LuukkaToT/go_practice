// Package application 是应用层，编排领域对象和基础设施协作，完成业务用例。
// 本文件：BadCase — 朴素 MySQL 全量事务实现。
package application

import (
	"context"

	"gopractice/project-live-donation/domain"
	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// BadCaseSvc：直接 MySQL 事务（高并发下的瓶颈演示）
// ─────────────────────────────────────────────────────────────────────────────

// BadCaseSvc 使用纯 MySQL 事务处理打赏请求。
//
// 【BadCase | 核心问题：行锁热点（Hot Row Contention）】
// 高并发打赏场景下（1000 个 goroutine 同时给同一个主播送礼），所有事务都要：
//   1. 对 ld_users.balance 加行级写锁（通过 WHERE id=? AND balance>=? 的原子更新）
//   2. 对 ld_streamers.earnings 加行级写锁
// 这两把行锁的竞争导致：
//   - 大量事务在 MySQL 内部排队等待锁释放
//   - 平均响应时间从毫秒级飙升到秒级
//   - 数据库连接池耗尽，导致新请求直接失败
//
// 【BadCase | 排行榜问题：重量级聚合查询】
// SELECT user_id, SUM(amount) FROM ld_donations WHERE room_id=? GROUP BY user_id ORDER BY SUM DESC
// 此查询：
//   - 随打赏量增长（百万/千万行），执行时间线性增加
//   - 高并发下多个排行榜查询同时扫描同一张表，影响写入性能
//   - 无增量更新能力，每次都全量重算
type BadCaseSvc struct {
	uow          ports.UnitOfWork
	donationRepo ports.DonationRepo // 用于排行榜查询（无事务）
}

func NewBadCaseSvc(uow ports.UnitOfWork, donationRepo ports.DonationRepo) *BadCaseSvc {
	return &BadCaseSvc{uow: uow, donationRepo: donationRepo}
}

// SendGift 处理打赏请求（BadCase：单一 MySQL 事务）。
//
// 事务执行顺序：
//  1. UPDATE ld_users SET balance=balance-amount WHERE id=? AND balance>=amount
//     → RowsAffected=0 则余额不足，回滚
//  2. UPDATE ld_streamers SET earnings=earnings+amount WHERE id=?
//  3. INSERT INTO ld_donations (...)
//  4. COMMIT
//
// 【面试点 | 为什么这里不用 SELECT...FOR UPDATE + 再 UPDATE？】
// 【答】两次 SQL（SELECT FOR UPDATE + UPDATE）需要两次 RTT，效率低。
// 直接用"条件 UPDATE"（WHERE balance>=amount）是等价的原子判断+更新，
// 且只需一次 RTT，是生产中常见的"乐观减法"写法。
// RowsAffected=0 说明 WHERE 条件不满足（余额不足），安全回滚。
func (s *BadCaseSvc) SendGift(ctx context.Context, d *domain.Donation) error {
	return s.uow.ExecTx(ctx, func(
		userRepo ports.UserBalanceRepo,
		streamerRepo ports.StreamerRepo,
		donationRepo ports.DonationRepo,
	) error {
		// Step 1: 扣减用户余额（条件更新，原子，无超扣）
		if err := userRepo.DeductBalance(ctx, d.UserID, d.Amount); err != nil {
			return err // 包含 domain.ErrInsufficientBalance
		}

		// Step 2: 增加主播收益
		if err := streamerRepo.AddEarnings(ctx, d.StreamerID, d.Amount); err != nil {
			return err
		}

		// Step 3: 插入打赏流水
		return donationRepo.Save(ctx, d)
	})
}

// GetRank 从 MySQL 实时计算直播间贡献排行榜（BadCase）。
//
// 【BadCase | 面试对话】
// 面试官：「排行榜的 QPS 能到多少？」
// 答：数据库 GROUP BY 聚合，10 万条流水大约 10~50ms；100 万条可能 100~500ms；
// 加上并发查询的锁竞争，P99 可能超过 1s。完全无法支撑直播间的实时榜单需求。
func (s *BadCaseSvc) GetRank(ctx context.Context, roomID string, topN int) ([]ports.RankItem, error) {
	return s.donationRepo.SumByUser(ctx, roomID, topN)
}

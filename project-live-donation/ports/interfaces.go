// Package ports 定义应用层所需的所有外部依赖接口（端口）。
//
// 【面试点 | 依赖倒置原则（DIP）】
// 应用层和领域层依赖「抽象接口」，而非具体实现（Redis/MySQL/RabbitMQ）。
// 这样可以：
//   1. 在单元测试中用 mock 替换真实基础设施；
//   2. 未来替换底层存储（如从 Redis 换成 Memcached）时只改 infra，不改业务逻辑。
package ports

import (
	"context"

	"gopractice/project-live-donation/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RankItem：排行榜条目（ZSET 成员）
// ─────────────────────────────────────────────────────────────────────────────

// RankItem 表示排行榜中的一个条目。
type RankItem struct {
	UserID int64
	Score  int64 // 累计打赏金额（分）
	Rank   int   // 名次（1-based）
}

// ─────────────────────────────────────────────────────────────────────────────
// WalletStore：用户钱包（余额管理）
// ─────────────────────────────────────────────────────────────────────────────

// WalletStore 管理用户虚拟币余额。
//
// 【面试点 | 为什么要把余额放到 Redis？】
// 高并发打赏场景下，如果每次都走 MySQL FOR UPDATE，行锁竞争会导致：
//   - 大量事务排队等待锁
//   - 响应时间线性增长（P99 可能达秒级）
//   - 极端情况下产生死锁
// 将"热点余额"放入 Redis，用 Lua 脚本原子操作，彻底规避行锁。
type WalletStore interface {
	// LoadBalance 从 MySQL 读取用户余额并写入 Redis（预热/冷启动）。
	// Key: "ld:wallet:user:{userID}"，Value: 余额（分），无过期时间（生产可设 TTL）。
	LoadBalance(ctx context.Context, userID int64, balance int64) error

	// Deduct 原子地校验并扣减用户余额（Lua 脚本保证原子性）。
	// 成功返回 nil；余额不足返回 domain.ErrInsufficientBalance；
	// Key 不存在返回 domain.ErrWalletNotLoaded。
	Deduct(ctx context.Context, userID int64, amount int64) error

	// GetBalance 获取 Redis 中用户当前余额，用于测试验证。
	GetBalance(ctx context.Context, userID int64) (int64, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// RankStore：直播间贡献排行榜（ZSET）
// ─────────────────────────────────────────────────────────────────────────────

// RankStore 管理直播间实时贡献榜。
//
// 【面试点 | 为什么用 ZSET 做排行榜？】
// Redis ZSET 底层是跳跃表（skiplist）+ 压缩列表（ziplist）：
//   - ZINCRBY：O(log N)，原子地累加 Score
//   - ZREVRANGEBYSCORE / ZREVRANGEBYRANK：O(log N + M)，直接返回有序结果
// 对比 MySQL ORDER BY SUM(amount) GROUP BY user_id：
//   - 需要全表扫或索引扫，随打赏量增长线性变慢
//   - 高并发写入时，聚合查询会产生大量锁等待
//
// 【可能问】「ZSET 的 Score 是 float64，会不会精度丢失？」
// 【答】Redis ZSET Score 实际存为 IEEE 754 double（64位浮点）。
// 对于整数金额（分），在 2^53 范围内（约 900 万亿分）精度完全安全。
// 超过此范围可将 Score 设为时间戳+序号，member 携带金额信息。
type RankStore interface {
	// IncrScore 将 userID 在 roomID 排行榜中的 Score 增加 amount。
	// 底层调用：ZINCRBY ld:room:{roomID}:rank {amount} {userID}
	IncrScore(ctx context.Context, roomID string, userID int64, amount int64) error

	// TopN 返回排行榜前 N 名（按 Score 降序）。
	// 底层调用：ZREVRANGEBYSCORE ld:room:{roomID}:rank +inf -inf WITHSCORES LIMIT 0 N
	TopN(ctx context.Context, roomID string, n int64) ([]RankItem, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// GiftPublisher：打赏消息发布（MQ）
// ─────────────────────────────────────────────────────────────────────────────

// GiftPublisher 将打赏事件发布到消息队列。
//
// 【面试点 | 为什么要用 MQ 异步落库？】
// 打赏高峰时（如明星开播，单直播间 10 万人同时送礼）：
//   - 如果每次打赏都同步写 MySQL，DB 连接池会瞬间打满
//   - MQ 作为"削峰填谷"层，将瞬间流量平滑到消费者可处理的速率
//   - 打赏响应时间从"MySQL 事务耗时（10~100ms）"降为"MQ publish 耗时（<1ms）"
//
// 【可能问】「Redis 扣款成功但 MQ 发送失败怎么办？」
// 【答】本 Demo GoodCase 采用"尽力而为"发布：
//   - Redis 已扣款但 MQ 失败时，记录失败日志，异步 Outbox 补偿（生产推荐方案）；
//   - 或结合 Redis 事务性补偿：Lua 扣款时同时写入一个"待发送队列"（LPUSH），
//     后台 worker 轮询处理，保证至少一次投递。
// 本 Demo 为保持简洁，测试用 chanPublisher（内存 channel 模拟），E2E 测试用真实 RabbitMQ。
type GiftPublisher interface {
	Publish(ctx context.Context, d *domain.Donation) error
}

// ─────────────────────────────────────────────────────────────────────────────
// DonationRepo：打赏流水持久化
// ─────────────────────────────────────────────────────────────────────────────

// DonationRepo 负责打赏流水的数据库操作。
type DonationRepo interface {
	// Save 将打赏流水插入 ld_donations 表（由 MQ Consumer 异步调用）。
	Save(ctx context.Context, d *domain.Donation) error

	// SumByUser 查询直播间各用户的累计打赏金额（BadCase 排行榜）。
	//
	// 【BadCase | DB 排行榜的问题】
	// SELECT user_id, SUM(amount) FROM ld_donations WHERE room_id=?
	// GROUP BY user_id ORDER BY SUM(amount) DESC LIMIT ?
	// 随着流水记录增多，此查询越来越慢；高并发下该聚合查询本身也会竞争锁资源。
	SumByUser(ctx context.Context, roomID string, limit int) ([]RankItem, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// StreamerRepo：主播收益管理
// ─────────────────────────────────────────────────────────────────────────────

// StreamerRepo 负责主播收益的持久化。
type StreamerRepo interface {
	// AddEarnings 原子地增加主播收益（带 row lock 保证不漏账）。
	// UPDATE ld_streamers SET earnings=earnings+amount WHERE id=?
	AddEarnings(ctx context.Context, streamerID int64, amount int64) error
}

// ─────────────────────────────────────────────────────────────────────────────
// UserBalanceRepo：用户余额（MySQL 侧，供 BadCase 和钱包预加载使用）
// ─────────────────────────────────────────────────────────────────────────────

// UserBalanceRepo 负责 MySQL 侧的用户余额操作。
type UserBalanceRepo interface {
	// GetBalance 查询用户余额（不加锁，仅用于预加载 Redis）。
	GetBalance(ctx context.Context, userID int64) (int64, error)

	// DeductBalance 在事务内原子扣减余额：
	// UPDATE ld_users SET balance=balance-amount WHERE id=? AND balance>=amount
	// RowsAffected=0 → 余额不足。
	DeductBalance(ctx context.Context, userID int64, amount int64) error
}

// ─────────────────────────────────────────────────────────────────────────────
// UnitOfWork：BadCase 事务边界
// ─────────────────────────────────────────────────────────────────────────────

// UnitOfWork 封装一个跨多 Repo 的数据库事务。
//
// 【面试点 | UoW 模式】
// BadCase 中需要在同一事务内完成：扣用户余额 + 加主播收益 + 插打赏流水。
// UoW 模式确保这三步要么全部成功，要么全部回滚，满足 ACID 中的 A（原子性）。
type UnitOfWork interface {
	ExecTx(ctx context.Context, fn func(userRepo UserBalanceRepo, streamerRepo StreamerRepo, donationRepo DonationRepo) error) error
}

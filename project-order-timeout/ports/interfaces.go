// Package ports 依赖倒置层：定义应用层所需的所有接口（面向接口编程）。
//
// 【知识点 | 依赖倒置原则 DIP】高层（application）不依赖低层（infra）细节；
// 双方都依赖这里的抽象接口，这样可以：
//   1. 测试时用 Mock/Stub 替换 infra（无需真实数据库）
//   2. 基础设施可随意替换（MySQL→PostgreSQL，RabbitMQ→Kafka），应用层零改动
//
// 【可能问】「Port 和 Adapter 分别是什么？」
// 【答】Port = 接口定义（这个文件）；Adapter = 具体实现（infra/ 目录）。
// 这是六边形架构（Hexagonal Architecture）的核心概念。
package ports

import (
	"context"
	"time"

	"gopractice/project-order-timeout/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// 订单仓储
// ─────────────────────────────────────────────────────────────────────────────

// OrderRepository 订单持久化接口。
//
// 【可能问】「仓储层为什么不直接暴露 DB 事务？」
// 【答】事务是基础设施细节；通过 UnitOfWork 统一管理，
// 领域/应用层只调用业务语义方法（Save/Update），不感知事务边界。
type OrderRepository interface {
	// Save 保存新订单，自动填充 ID。
	Save(ctx context.Context, o *domain.Order) error
	// GetByID 查询订单，不存在时返回 domain.ErrOrderNotFound。
	GetByID(ctx context.Context, id int64) (*domain.Order, error)
	// CancelIfPending 原子 UPDATE orders SET status='cancelled' WHERE id=? AND status='pending'
	// 返回 (true, nil) 代表成功取消；(false, nil) 代表订单不在 pending 状态（幂等安全）。
	//
	// 【知识点 | 乐观取消】WHERE status='pending' 保证并发两条超时消息同时消费时只有一条成功。
	// 【可能问】「为什么不 SELECT FOR UPDATE 再 UPDATE？」
	// 【答】SELECT FOR UPDATE 持锁时间长，在超时消费场景非常高频；
	// 直接 WHERE 条件 UPDATE 是原子操作，affected rows == 0 即表示已处理，避免锁争用。
	CancelIfPending(ctx context.Context, id int64) (cancelled bool, err error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Outbox 事件存储
// ─────────────────────────────────────────────────────────────────────────────

// OutboxMessage 待投递的领域事件记录。
type OutboxMessage struct {
	ID          int64
	OrderID     int64
	EventType   string // "order.created" / "order.timeout"
	Payload     []byte
	PublishedAt *time.Time // nil 表示未投递
	CreatedAt   time.Time
}

// OutboxStore Outbox 表的读写接口（仅在事务上下文中使用）。
//
// 【知识点 | Transactional Outbox Pattern】
// 将"写业务表 + 写事件表"放在同一个数据库事务中，
// 确保两者原子成功或原子回滚；事件表由 OutboxDispatcher 异步投递到 MQ。
// 这解决了"先写DB再发MQ，DB成功MQ崩了"的双写一致性问题。
type OutboxStore interface {
	// Enqueue 在同一事务中插入一条 outbox 记录（由 UnitOfWork 事务内调用）。
	Enqueue(ctx context.Context, msg *OutboxMessage) error
	// FetchPending 查询未投递的 outbox 消息（limit 条），供 OutboxDispatcher 轮询。
	FetchPending(ctx context.Context, limit int) ([]*OutboxMessage, error)
	// MarkPublished 标记消息已成功投递（Confirm 确认后调用）。
	MarkPublished(ctx context.Context, id int64) error
}

// ─────────────────────────────────────────────────────────────────────────────
// 消息发布
// ─────────────────────────────────────────────────────────────────────────────

// PublishOption 发布选项，支持为消息设置 TTL（DLX 延迟消息使用）。
type PublishOption struct {
	// ExpirationMS 消息 TTL（毫秒字符串），非空时消息过期后死信路由到 DLX。
	// RabbitMQ 的 Expiration 字段必须是字符串，例如 "5000" 表示 5 秒。
	ExpirationMS string
	// RoutingKey 覆盖默认 routing key（发 parking queue 时使用）。
	RoutingKey string
}

// Publisher 消息发布接口。
//
// 【可能问】「Publisher Confirm 是什么？为什么需要它？」
// 【答】Channel.Confirm(noWait=false) 后，每次 Publish 都会收到 Broker 确认 ACK；
// 若没有 Confirm 直接 Publish，即使网络抖动消息已丢失，Go 侧也毫不知情。
// Confirm 模式下吞吐约降低 20-30%，但消息可靠性大幅提升；生产多用异步 Confirm 批量确认。
type Publisher interface {
	Publish(ctx context.Context, msg *OutboxMessage, opts ...PublishOption) error
}

// ─────────────────────────────────────────────────────────────────────────────
// 幂等检查
// ─────────────────────────────────────────────────────────────────────────────

// IdempotencyChecker 消费端幂等检查接口。
//
// 【知识点 | 幂等消费三层保护】
//   第1层（最快）：Redis SETNX — O(1) 内存操作，99% 的重复在此拦截
//   第2层（可靠）：MySQL 唯一索引 — Redis 重启/脑裂时的兜底
//   第3层（业务）：WHERE status='pending' — 即使前两层失效，DB 仍原子安全
//
// 【可能问】「为什么 Redis 还不够，还要 MySQL 唯一索引？」
// 【答】Redis 可能宕机重启导致数据丢失；网络分区时 Redis Cluster 可能出现脑裂；
// MySQL 是强一致 Source of Truth，唯一索引是终极安全网。
type IdempotencyChecker interface {
	// TryAcquire 尝试标记 msgID 已处理（Redis SETNX）。
	// 返回 true 代表首次处理（可继续业务逻辑）；返回 false 代表已处理（直接 Ack 跳过）。
	TryAcquire(ctx context.Context, msgID string) (acquired bool, err error)
	// Release 仅在业务逻辑失败时释放标记（需要重试时回滚幂等 Key）。
	Release(ctx context.Context, msgID string) error
}

// ─────────────────────────────────────────────────────────────────────────────
// 工作单元（Unit of Work）
// ─────────────────────────────────────────────────────────────────────────────

// OrderTx 单次事务内可调用的操作集合（事务 context 已绑定在 ctx 中）。
//
// 【知识点 | UoW 模式】将多个仓储操作捆绑在同一个数据库事务内，
// 应用层只需调用 WithinTransaction，无需关心 Begin/Commit/Rollback 细节。
type OrderTx interface {
	SaveOrder(ctx context.Context, o *domain.Order) error
	EnqueueOutbox(ctx context.Context, msg *OutboxMessage) error
}

// UnitOfWork 事务抽象，应用层的事务入口。
type UnitOfWork interface {
	// WithinTransaction 开启事务并执行 fn；fn 返回错误则自动 Rollback，否则 Commit。
	WithinTransaction(ctx context.Context, fn func(ctx context.Context, tx OrderTx) error) error
}

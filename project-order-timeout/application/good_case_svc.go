package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopractice/project-order-timeout/domain"
	"gopractice/project-order-timeout/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GoodCaseSvc：事务写 Order + Outbox（Transactional Outbox Pattern）
// ─────────────────────────────────────────────────────────────────────────────

// GoodCaseSvc 演示正确的双写一致性方案。
//
// 【知识点 | Transactional Outbox Pattern 核心思路】
// 把"写业务表"和"写事件表"放进同一个数据库事务：
//   1. 两者原子成功/回滚 → 业务数据和待发事件永远一致
//   2. OutboxDispatcher 异步轮询 order_outbox 表，找到未投递记录 → 发 MQ
//   3. Publish + Confirm 成功后，更新 order_outbox.published_at → 标记完成
//   4. 若进程在 Publish 后崩溃未写 published_at → 重启后再次发送（At-Least-Once）
//      → 消费端幂等兜底（Redis SETNX + DB WHERE status=pending）
//
// 【可能问】「Outbox 会不会重复发送？」
// 【答】会（At-Least-Once），这是设计上的权衡；
// 相比「消息丢失」，「重复投递」可由幂等消费者安全处理，因此 Outbox 是正确选择。
//
// 【可能问】「Outbox 模式的缺点？」
// 【答】1. 增加了 DB 存储压力（每条业务操作多一条 outbox 记录）；
//       2. 消息非实时，有轮询延迟（通常 100ms~几秒，可接受）；
//       3. Outbox 表需要定期清理（published 记录保留 7 天够用）。
type GoodCaseSvc struct {
	uow ports.UnitOfWork
}

// NewGoodCaseSvc 构造 GoodCaseSvc。
func NewGoodCaseSvc(uow ports.UnitOfWork) *GoodCaseSvc {
	return &GoodCaseSvc{uow: uow}
}

// TimeoutDelayMS 超时检测延迟（Demo 用 5 秒，生产用 900000 = 15 分钟）。
const TimeoutDelayMS = "5000"

// CreateOrder GoodCase：事务内同时写 orders 和两条 outbox 消息。
//
// 写入 outbox 的两条消息：
//   1. "order.created"（立即投递）：通知下游积分/库存系统
//   2. "order.timeout"（走 DLX parking queue，TTL=5s）：延迟触发超时检查
//
// 【知识点 | DLX 延迟消息原理】
// RabbitMQ 本身不支持精确延迟（需插件），但可用 DLX 模拟：
//   1. 发一条带 Expiration="5000" 的消息到 order.delay.parking 队列
//   2. parking 队列无消费者，消息 TTL 到期后变成"死信"
//   3. 死信被路由到 order.timeout.dlx Exchange → order.timeout.check 队列
//   4. GoodCaseConsumer 从 order.timeout.check 消费并执行超时取消
func (s *GoodCaseSvc) CreateOrder(ctx context.Context, userID, amount int64) (*domain.Order, error) {
	o := &domain.Order{
		UserID:    userID,
		Amount:    amount,
		Status:    domain.OrderStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	err := s.uow.WithinTransaction(ctx, func(ctx context.Context, tx ports.OrderTx) error {
		// 第 1 步：写订单（事务内）
		if err := tx.SaveOrder(ctx, o); err != nil {
			return fmt.Errorf("save order: %w", err)
		}

		payload, _ := json.Marshal(OrderCreatedEvent{OrderID: o.ID, UserID: userID, Amount: amount})

		// 第 2 步：写 order.created outbox（事务内）—— 立即投递到 order.direct exchange
		if err := tx.EnqueueOutbox(ctx, &ports.OutboxMessage{
			OrderID:   o.ID,
			EventType: "order.created",
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("enqueue order.created: %w", err)
		}

		// 第 3 步：写 order.timeout outbox（事务内）—— 投递到 parking queue（带 TTL）
		// OutboxDispatcher 发送时会根据 EventType == "order.timeout" 自动加 ExpirationMS
		if err := tx.EnqueueOutbox(ctx, &ports.OutboxMessage{
			OrderID:   o.ID,
			EventType: "order.timeout",
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("enqueue order.timeout: %w", err)
		}

		return nil
		// 三步全部成功 → Commit；任意一步失败 → Rollback（DB 无脏数据，outbox 无孤儿记录）
	})
	if err != nil {
		return nil, fmt.Errorf("good-case: create order: %w", err)
	}

	fmt.Printf("[GoodCase] 订单+Outbox 原子写入成功 orderID=%d，等待 OutboxDispatcher 投递\n", o.ID)
	return o, nil
}

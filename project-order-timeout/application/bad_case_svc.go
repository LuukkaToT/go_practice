// Package application 应用服务层：编排领域对象和基础设施，实现业务用例。
//
// 本文件展示 BadCase：创建订单后直接 Publish MQ（无 Outbox），
// 若 MQ 不可用或进程崩溃，消息永久丢失，订单数据和下游系统产生不一致。
package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopractice/project-order-timeout/domain"
	"gopractice/project-order-timeout/ports"
)

// OrderCreatedEvent 下游感兴趣的业务事件（BadCase 和 GoodCase 共用）。
type OrderCreatedEvent struct {
	OrderID int64  `json:"order_id"`
	UserID  int64  `json:"user_id"`
	Amount  int64  `json:"amount"`
}

// ─────────────────────────────────────────────────────────────────────────────
// BadCaseSvc：直接写 DB + 直接 Publish（无事务保护）
// ─────────────────────────────────────────────────────────────────────────────

// BadCaseSvc 演示双写问题。
//
// 【知识点 | 双写一致性问题根源】
// 写 DB 和 Publish MQ 是两个独立操作，任何一步失败都会造成数据与消息不一致：
//   问题1（MQ 先崩）：DB 写成功，Publish 失败 → 消息丢失，下游永不知晓订单创建
//   问题2（进程崩溃）：DB 成功，Publish 时进程被 kill → 同上
//   问题3（MQ 无 Confirm）：Publish 调用返回成功，但 Broker 未实际入队 → 无声丢失
//
// 【可能问】「不用 MQ 能解决吗？直接在事务里调 HTTP 通知下游？」
// 【答】不能，HTTP 调用是网络操作，无法纳入 DB 事务；
// 且 HTTP 调用失败也面临同样的双写问题，还引入了下游的同步耦合。
type BadCaseSvc struct {
	repo      ports.OrderRepository
	publisher ports.Publisher
}

// NewBadCaseSvc 构造 BadCaseSvc。
func NewBadCaseSvc(repo ports.OrderRepository, publisher ports.Publisher) *BadCaseSvc {
	return &BadCaseSvc{repo: repo, publisher: publisher}
}

// CreateOrder BadCase：先写 DB，再直接 Publish。
//
// 【隐患】第 2 步 Publish 失败 → DB 有订单，MQ 无消息 → 数据断层。
func (s *BadCaseSvc) CreateOrder(ctx context.Context, userID, amount int64) (*domain.Order, error) {
	o := &domain.Order{
		UserID:    userID,
		Amount:    amount,
		Status:    domain.OrderStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	// 第 1 步：写 orders 表（成功）
	if err := s.repo.Save(ctx, o); err != nil {
		return nil, fmt.Errorf("bad-case: save order: %w", err)
	}
	fmt.Printf("[BadCase] 订单已落库 orderID=%d\n", o.ID)

	// 第 2 步：直接 Publish（若此处崩溃，消息永久丢失！）
	// 【隐患注释】这里没有 Publisher Confirm，也没有重试，更没有 Outbox。
	// 如果 MQ 当时不可用，Publish 报错，但 DB 事务已经提交无法回滚。
	payload, _ := json.Marshal(OrderCreatedEvent{OrderID: o.ID, UserID: userID, Amount: amount})
	msg := &ports.OutboxMessage{
		OrderID:   o.ID,
		EventType: "order.created",
		Payload:   payload,
	}
	if err := s.publisher.Publish(ctx, msg); err != nil {
		// ☠️ 此处只能打印日志，无法补救！订单数据和 MQ 消息已经不一致。
		fmt.Printf("[BadCase] ❌ MQ Publish 失败 orderID=%d err=%v → 消息永久丢失！\n", o.ID, err)
		return o, nil // 返回 nil error：对用户假装成功（实则下游数据断层）
	}

	fmt.Printf("[BadCase] MQ Publish 成功 orderID=%d（但没有 Confirm，可能只是假象）\n", o.ID)
	return o, nil
}

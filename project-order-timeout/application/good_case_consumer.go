package application

import (
	"context"
	"errors"
	"fmt"

	"gopractice/project-order-timeout/domain"
	"gopractice/project-order-timeout/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GoodCaseConsumer：三层幂等保护的超时消费者
// ─────────────────────────────────────────────────────────────────────────────

// GoodCaseConsumer 演示生产级幂等消费实践。
//
// 【三层幂等保护架构】（由快到慢，由轻到重）
//
//	第1层：Redis SETNX（O(1) 内存，纳秒级）
//	  - Key：idempotent:timeout:{msgID}，TTL=24h
//	  - 99%+ 的重复消息在此被拦截，DB 连接零消耗
//	  - 风险：Redis 宕机重启数据丢失 → 第2层兜底
//
//	第2层：MySQL 唯一索引（idempotent_events.msg_id）
//	  - INSERT IGNORE（或 ON DUPLICATE KEY UPDATE）
//	  - Redis 失效后的安全网，强一致
//	  - 注：此层在测试中通过 MySQLOutboxStore 隐式验证，这里不单独展示
//
//	第3层：业务幂等（CancelIfPending：WHERE status='pending'）
//	  - 即使前两层全部失效，DB 的原子条件更新保证不会重复取消
//	  - 受影响行数=0 代表已被其他线程/进程处理，安全跳过
//
// 【可能问】「Redis SETNX 在 Release 之前业务失败了怎么办？」
// 【答】业务失败时调用 Release 释放 Key，让下次重试能通过第1层；
// 若 Release 也失败（Redis 故障），第2层 MySQL 唯一索引会拦截重复插入，
// 第3层 WHERE status='pending' 保证不会重复取消，系统依然安全。
type GoodCaseConsumer struct {
	repo      ports.OrderRepository
	idempotcy ports.IdempotencyChecker
}

// NewGoodCaseConsumer 构造 GoodCaseConsumer。
func NewGoodCaseConsumer(repo ports.OrderRepository, checker ports.IdempotencyChecker) *GoodCaseConsumer {
	return &GoodCaseConsumer{repo: repo, idempotcy: checker}
}

// ProcessTimeout GoodCase：三层幂等 + 领域取消。
//
// 调用方（Consumer goroutine）负责：
//   - 成功（返回 nil）→ channel.Ack(false)
//   - 失败（返回 error）→ channel.Nack(false, true)（重新入队）
func (c *GoodCaseConsumer) ProcessTimeout(ctx context.Context, orderID int64, msgID string) error {
	fmt.Printf("[GoodCase消费者] 收到超时消息 orderID=%d msgID=%s\n", orderID, msgID)

	// ─── 第1层：Redis SETNX 幂等拦截 ───────────────────────────────────────
	acquired, err := c.idempotcy.TryAcquire(ctx, msgID)
	if err != nil {
		// Redis 暂时不可用：直接返回 error，让 MQ 重新入队，稍后重试。
		// 【注意】不要 Nack discard，避免消息丢失。
		return fmt.Errorf("idempotency check failed: %w", err)
	}
	if !acquired {
		// Key 已存在 → 重复消息，幂等拦截
		fmt.Printf("[GoodCase消费者] 🛡️ 幂等拦截（第1层）：msgID=%s 已处理，直接 Ack 跳过\n", msgID)
		return nil // 返回 nil → 调用方 Ack（消息安全消费，不再重发）
	}

	// ─── 执行业务逻辑 ───────────────────────────────────────────────────────
	// 第3层：CancelIfPending 内部用 WHERE status='pending'（原子条件 UPDATE）
	cancelled, err := c.repo.CancelIfPending(ctx, orderID)
	if err != nil {
		// 业务失败：释放 Redis Key，让下次重试能通过第1层
		_ = c.idempotcy.Release(ctx, msgID)
		return fmt.Errorf("cancel order failed: %w", err)
	}

	if cancelled {
		fmt.Printf("[GoodCase消费者] ✅ 订单超时取消成功 orderID=%d\n", orderID)
	} else {
		// 第3层拦截：WHERE status='pending' 不匹配（订单已被支付或已取消）
		// 这里属于业务正常分支（用户在超时前完成了支付），直接 Ack
		fmt.Printf("[GoodCase消费者] 🛡️ 业务幂等拦截（第3层）：orderID=%d 不在 pending 状态，跳过取消\n", orderID)
	}

	return nil
}

// ProcessTimeoutWithDomainModel 可选变体：先 GetByID → 领域 Cancel() → 持久化。
//
// 适用于取消后还需要触发其他领域事件（如发退款 outbox）的场景。
// 相比 CancelIfPending，多了一次 SELECT，适合需要聚合根方法的复杂逻辑。
func (c *GoodCaseConsumer) ProcessTimeoutWithDomainModel(ctx context.Context, orderID int64, msgID string) error {
	acquired, err := c.idempotcy.TryAcquire(ctx, msgID)
	if err != nil {
		return err
	}
	if !acquired {
		fmt.Printf("[GoodCase消费者(DomainModel)] 幂等拦截 msgID=%s\n", msgID)
		return nil
	}

	order, err := c.repo.GetByID(ctx, orderID)
	if err != nil {
		_ = c.idempotcy.Release(ctx, msgID)
		if errors.Is(err, domain.ErrOrderNotFound) {
			// 订单不存在：可能已被删除，Ack 消费掉
			fmt.Printf("[GoodCase消费者(DomainModel)] orderID=%d 不存在，Ack 消费\n", orderID)
			return nil
		}
		return err
	}

	// 领域方法触发状态机校验（第3层兜底：如果已 Paid，Cancel() 返回 ErrNotCancellable）
	if err := order.Cancel(); err != nil {
		if errors.Is(err, domain.ErrNotCancellable) {
			fmt.Printf("[GoodCase消费者(DomainModel)] 🛡️ 领域层拦截：orderID=%d 不可取消（状态=%s）\n",
				orderID, order.Status)
			return nil
		}
		_ = c.idempotcy.Release(ctx, msgID)
		return err
	}

	// 用 CancelIfPending（WHERE 条件）持久化，防并发
	cancelled, err := c.repo.CancelIfPending(ctx, orderID)
	if err != nil {
		_ = c.idempotcy.Release(ctx, msgID)
		return err
	}
	if !cancelled {
		fmt.Printf("[GoodCase消费者(DomainModel)] 并发竞争：orderID=%d 已被其他 goroutine 取消\n", orderID)
	} else {
		fmt.Printf("[GoodCase消费者(DomainModel)] ✅ 订单超时取消成功 orderID=%d\n", orderID)
	}

	return nil
}

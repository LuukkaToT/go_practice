package application

import (
	"context"
	"fmt"

	"gopractice/project-order-timeout/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// BadCaseConsumer：无幂等直接取消
// ─────────────────────────────────────────────────────────────────────────────

// BadCaseConsumer 演示无幂等消费的危险。
//
// 【知识点 | At-Least-Once 语义】
// RabbitMQ（和大多数 MQ）保证"至少一次投递"（At-Least-Once）：
//   - Consumer 处理后 Ack 前崩溃 → Broker 重新投递同一条消息
//   - 网络超时 → Broker 判定消费失败，重新入队
// 因此消费者必须实现幂等，否则同一消息被消费两次就会执行两次业务逻辑。
//
// 【可能问】「RabbitMQ 能保证 Exactly-Once 吗？」
// 【答】不能，这是分布式系统的 CAP 约束决定的；
// 工程实践是 At-Least-Once + 幂等消费者 ≈ Exactly-Once 效果。
type BadCaseConsumer struct {
	repo ports.OrderRepository
}

// NewBadCaseConsumer 构造 BadCaseConsumer。
func NewBadCaseConsumer(repo ports.OrderRepository) *BadCaseConsumer {
	return &BadCaseConsumer{repo: repo}
}

// ProcessTimeout BadCase：直接取消，无任何幂等保护。
//
// 【隐患】同一条超时消息被投递两次（At-Least-Once）时：
//   第1次：CancelIfPending 返回 true，订单正常取消
//   第2次：CancelIfPending 返回 false（已是 cancelled），但这里无处理，静默忽略
//   更危险的场景：如果是并发 2 个 goroutine 同时消费同一条消息……
//   → 两次都能读到 status='pending'，都会尝试取消，DB 层 WHERE status='pending' 保住了原子性
//   → 但如果没有 WHERE 条件保护（更 bad 的写法），就会重复触发取消后的业务（如退款 2 次）
//
// 【对比 GoodCase】GoodCase 在 DB 操作之前先做 Redis SETNX 幂等检查，
// 99% 重复消息在内存层就被拦截，DB 连接都不会消耗。
func (c *BadCaseConsumer) ProcessTimeout(ctx context.Context, orderID int64, msgID string) {
	fmt.Printf("[BadCase消费者] 收到超时消息 orderID=%d msgID=%s（无幂等保护）\n", orderID, msgID)

	// 没有幂等检查，直接执行业务
	cancelled, err := c.repo.CancelIfPending(ctx, orderID)
	if err != nil {
		fmt.Printf("[BadCase消费者] ❌ 取消失败 orderID=%d err=%v\n", orderID, err)
		return
	}
	if cancelled {
		fmt.Printf("[BadCase消费者] ✅ 订单已取消 orderID=%d\n", orderID)
	} else {
		// 【风险】如果这里还有退款、通知等后续操作，就会被重复执行！
		fmt.Printf("[BadCase消费者] ⚠️  orderID=%d 已不在 pending 状态（可能被重复消费！）\n", orderID)
	}
}

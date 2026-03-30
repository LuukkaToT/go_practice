package application

import (
	"context"
	"fmt"

	"gopractice/project-order-timeout/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// OutboxDispatcher：轮询 outbox 表并补偿投递
// ─────────────────────────────────────────────────────────────────────────────

// OutboxDispatcher 从 order_outbox 表轮询未投递消息，
// 调用 Publisher Confirm 发送后标记 published_at。
//
// 【知识点 | 轮询 vs CDC（Change Data Capture）】
// 轮询（Polling）：简单易懂，无额外组件，适合中小规模（<10w qps）。
// CDC（如 Debezium）：读 MySQL binlog，近实时，无轮询延迟，但需要额外组件维护。
// 字节跳动等大厂内部通常用 CDC，中小团队用轮询足够。
//
// 【可能问】「RunOnce 为什么不直接用 Run（死循环）？」
// 【答】RunOnce 单次执行便于测试（测试注入 flakyPublisher，精确控制重试次数）；
// 生产环境在外层加 ticker 循环调用 RunOnce 即可，关注点分离。
//
// 【可能问】「Outbox 发送失败如何处理？」
// 【答】直接 continue 跳过，下次 RunOnce 再重试；可以加最大重试次数 + 死信表（dead_outbox）
// 避免毒丸消息（poison message）无限阻塞后续消息。
type OutboxDispatcher struct {
	store     ports.OutboxStore
	publisher ports.Publisher
	batchSize int
}

// NewOutboxDispatcher 构造 OutboxDispatcher。
//
// batchSize：每次轮询取多少条待发消息（生产建议 50~200，平衡吞吐和 DB 压力）。
func NewOutboxDispatcher(store ports.OutboxStore, publisher ports.Publisher, batchSize int) *OutboxDispatcher {
	if batchSize <= 0 {
		batchSize = 50
	}
	return &OutboxDispatcher{store: store, publisher: publisher, batchSize: batchSize}
}

// RunOnce 执行一次轮询：查 → 发 → 标记。
// 返回本次成功投递的消息数量（便于测试断言）。
//
// 【关键设计：先标记，还是先发？】
// 正确顺序：Publish → (Confirm) → MarkPublished
// 若顺序反过来（先 Mark 再 Publish），Publish 崩溃后消息永久丢失。
// 现在的顺序：即使 MarkPublished 失败，消息会被重复发送，消费端幂等保底。
func (d *OutboxDispatcher) RunOnce(ctx context.Context) (published int) {
	msgs, err := d.store.FetchPending(ctx, d.batchSize)
	if err != nil {
		fmt.Printf("[OutboxDispatcher] FetchPending 失败: %v\n", err)
		return 0
	}

	for _, msg := range msgs {
		// 根据事件类型决定是否发送到 parking queue（DLX 延迟消息）
		var opts []ports.PublishOption
		if msg.EventType == "order.timeout" {
			opts = append(opts, ports.PublishOption{
				ExpirationMS: TimeoutDelayMS, // Demo: 5 秒；生产: 900000 (15分钟)
				RoutingKey:   "timeout.delay",
			})
		}

		// Publish + Confirm（由具体 Publisher 实现保证）
		if err := d.publisher.Publish(ctx, msg, opts...); err != nil {
			// 【设计取舍】失败时不 panic，记录日志后继续处理下一条。
			// 失败的消息下次 RunOnce 会再次 FetchPending 到（published_at 仍为 nil）。
			fmt.Printf("[OutboxDispatcher] Publish 失败 msgID=%d eventType=%s err=%v（将在下次重试）\n",
				msg.ID, msg.EventType, err)
			continue
		}

		// Publish + Confirm 成功后，标记 published_at
		if err := d.store.MarkPublished(ctx, msg.ID); err != nil {
			// MarkPublished 失败不是致命错误：下次会重发（消费端幂等保底）
			fmt.Printf("[OutboxDispatcher] MarkPublished 失败 msgID=%d err=%v（可能重复投递，幂等消费者保底）\n",
				msg.ID, err)
			continue
		}

		fmt.Printf("[OutboxDispatcher] ✅ 投递成功 msgID=%d orderID=%d eventType=%s\n",
			msg.ID, msg.OrderID, msg.EventType)
		published++
	}

	return published
}

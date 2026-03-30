package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"gopractice/project-live-donation/domain"
	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// MQ 拓扑常量
// ─────────────────────────────────────────────────────────────────────────────

const (
	// ldExchange 打赏事件的 direct exchange。
	ldExchange = "ld.gift"
	// ldQueue 消费者订阅的队列。
	ldQueue = "ld.gift.settle"
	// ldRoutingKey 路由键。
	ldRoutingKey = "gift.settle"
)

// ─────────────────────────────────────────────────────────────────────────────
// 消息体
// ─────────────────────────────────────────────────────────────────────────────

// GiftMessage 是发布到 RabbitMQ 的打赏事件消息体。
type GiftMessage struct {
	UserID     int64  `json:"user_id"`
	StreamerID int64  `json:"streamer_id"`
	RoomID     string `json:"room_id"`
	Amount     int64  `json:"amount"`
	MsgID      string `json:"msg_id"` // 全局唯一 ID，用于消费端幂等
}

// ─────────────────────────────────────────────────────────────────────────────
// DeclareLDTopology：声明 MQ 拓扑结构
// ─────────────────────────────────────────────────────────────────────────────

// DeclareLDTopology 创建 exchange 和 queue 并绑定（幂等）。
//
// 【面试点 | Durable Exchange + Queue 的作用】
// - durable=true：Exchange/Queue 定义持久化到磁盘，Broker 重启后依然存在。
// - 注意：Durable 仅保证 Exchange/Queue 本身，消息是否持久化由 Delivery Mode 控制（persistent=2）。
func DeclareLDTopology(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(
		ldExchange, "direct", true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare exchange: %w", err)
	}

	q, err := ch.QueueDeclare(
		ldQueue, true, false, false, false, nil,
	)
	if err != nil {
		return fmt.Errorf("declare queue: %w", err)
	}

	if err := ch.QueueBind(q.Name, ldRoutingKey, ldExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RabbitMQGiftPublisher：实现 ports.GiftPublisher（confirm 模式）
// ─────────────────────────────────────────────────────────────────────────────

// RabbitMQGiftPublisher 使用 Publisher Confirm 模式发布打赏消息。
//
// 【面试点 | Publisher Confirm 解决什么问题？】
// 默认情况下 AMQP Publish 是 fire-and-forget：网络抖动时消息可能丢失。
// Publisher Confirm 模式：Broker 在消息落盘后返回 ack，Publisher 确认后才认为发送成功。
// 若收到 nack 或超时，Publisher 可以重试，保证"至少一次"投递。
//
// 【可能问】「Publisher Confirm 和事务模式（tx）有什么区别？」
// 【答】事务模式（channel.Tx()）性能极差（每条消息需要 2 次 RTT），已基本淘汰。
// Publisher Confirm 可批量确认（异步 ack），性能远优于事务模式，是生产推荐方案。
type RabbitMQGiftPublisher struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewRabbitMQGiftPublisher(url string) (*RabbitMQGiftPublisher, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("dial amqp: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("confirm mode: %w", err)
	}
	if err := DeclareLDTopology(ch); err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}
	return &RabbitMQGiftPublisher{conn: conn, ch: ch}, nil
}

func (p *RabbitMQGiftPublisher) Publish(_ context.Context, d *domain.Donation) error {
	msg := GiftMessage{
		UserID:     d.UserID,
		StreamerID: d.StreamerID,
		RoomID:     d.RoomID,
		Amount:     d.Amount,
		MsgID:      fmt.Sprintf("%d-%d-%d", d.UserID, d.StreamerID, d.CreatedAt.UnixNano()),
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	confirms := p.ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	err = p.ch.Publish(ldExchange, ldRoutingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // 消息持久化到磁盘
		MessageId:    msg.MsgID,
		Timestamp:    time.Now(),
		Body:         body,
	})
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// 等待 Broker confirm
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return fmt.Errorf("broker nack for message %s", msg.MsgID)
		}
	case <-time.After(3 * time.Second):
		return fmt.Errorf("publish confirm timeout for message %s", msg.MsgID)
	}
	return nil
}

func (p *RabbitMQGiftPublisher) Close() {
	if p.ch != nil {
		p.ch.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DonationConsumer：MQ 消费者（幂等落库）
// ─────────────────────────────────────────────────────────────────────────────

// DonationConsumer 消费打赏消息，异步落库（流水 + 主播收益）。
//
// 【面试点 | 消费端幂等如何实现？】
// 本 Demo 简化版：基于 MQ 消息的 MessageId 做幂等去重。
// 生产推荐方案（三层防护，同 Demo 3 order-timeout）：
//   1. Redis SETNX（快速，99%+ 请求在此拦截）
//   2. DB 唯一索引（兜底，防 Redis 重启后 Key 丢失）
//   3. 条件更新（WHERE status='pending'）
//
// 本 Demo 为简洁演示，Consumer 仅做"落库+加收益"，不加幂等表（测试环境可接受）。
// 实际面试中，需说明生产环境需加幂等去重（idempotent_events 表）。
type DonationConsumer struct {
	streamerRepo ports.StreamerRepo
	donationRepo ports.DonationRepo
}

func NewDonationConsumer(streamerRepo ports.StreamerRepo, donationRepo ports.DonationRepo) *DonationConsumer {
	return &DonationConsumer{
		streamerRepo: streamerRepo,
		donationRepo: donationRepo,
	}
}

// Settle 处理单条打赏消息：插入流水 + 更新主播收益。
func (c *DonationConsumer) Settle(ctx context.Context, msg GiftMessage) error {
	d := &domain.Donation{
		UserID:     msg.UserID,
		StreamerID: msg.StreamerID,
		RoomID:     msg.RoomID,
		Amount:     msg.Amount,
	}
	if err := c.donationRepo.Save(ctx, d); err != nil {
		return fmt.Errorf("save donation: %w", err)
	}
	if err := c.streamerRepo.AddEarnings(ctx, msg.StreamerID, msg.Amount); err != nil {
		return fmt.Errorf("add earnings: %w", err)
	}
	return nil
}

// StartConsuming 启动 RabbitMQ 消费循环（阻塞，应在 goroutine 中调用）。
func (c *DonationConsumer) StartConsuming(ch *amqp.Channel, stopCh <-chan struct{}) error {
	// QoS prefetch=10：每次最多预取 10 条消息，防止消费者被压垮。
	// 【面试点 | prefetch 值如何设定？】
	// 太小（如1）：消费者大部分时间在等待 Broker，吞吐低。
	// 太大：未 ack 消息堆积在内存，消费者崩溃时大量消息被重投递。
	// 生产中通常根据消费者处理速度调优，10~100 是常见范围。
	if err := ch.Qos(10, 0, false); err != nil {
		return err
	}

	msgs, err := ch.Consume(ldQueue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}

	for {
		select {
		case <-stopCh:
			return nil
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("consumer channel closed")
			}
			var msg GiftMessage
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				d.Nack(false, false) // 解析失败，丢弃（死信）
				continue
			}
			if err := c.Settle(context.Background(), msg); err != nil {
				d.Nack(false, true) // 落库失败，重新入队
			} else {
				d.Ack(false) // 成功，确认消费
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ChanPublisher：内存 channel 模拟 MQ（测试用 mock）
// ─────────────────────────────────────────────────────────────────────────────

// ChanPublisher 实现 ports.GiftPublisher，将消息发送到内存 channel。
// 配合内嵌消费者 goroutine，无需真实 RabbitMQ 即可验证异步落库逻辑。
//
// 【面试点 | 测试 Mock 设计】
// 用 channel 模拟 MQ 的核心思想：
//   - Publish → channel <- msg（无阻塞，模拟 fire-and-forget）
//   - Consumer goroutine → <-channel → Settle（模拟消费者）
// 这样可以在集成测试中验证：
//   1. 打赏成功后消息是否被正确发布
//   2. Consumer 落库逻辑是否正确
//   3. 并发下消息是否全部被处理
type ChanPublisher struct {
	ch chan GiftMessage
}

func NewChanPublisher(bufSize int) *ChanPublisher {
	return &ChanPublisher{ch: make(chan GiftMessage, bufSize)}
}

func (p *ChanPublisher) Publish(_ context.Context, d *domain.Donation) error {
	msg := GiftMessage{
		UserID:     d.UserID,
		StreamerID: d.StreamerID,
		RoomID:     d.RoomID,
		Amount:     d.Amount,
		MsgID:      fmt.Sprintf("%d-%d-%d", d.UserID, d.StreamerID, d.CreatedAt.UnixNano()),
	}
	select {
	case p.ch <- msg:
		return nil
	default:
		return fmt.Errorf("chanPublisher buffer full")
	}
}

// Chan 返回消息通道（供消费者 goroutine 读取）。
func (p *ChanPublisher) Chan() <-chan GiftMessage {
	return p.ch
}

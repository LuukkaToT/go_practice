package infra

import (
	"context"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"gopractice/project-order-timeout/ports"
)

// RabbitMQ 连接地址（复用 infrastructure/docker-compose.yml 定义的实例）。
const rabbitMQURL = "amqp://practice:practice@127.0.0.1:5673/"

// ─────────────────────────────────────────────────────────────────────────────
// Exchange / Queue 名称常量
// ─────────────────────────────────────────────────────────────────────────────

const (
	// order.created 立即投递用的 Direct Exchange
	ExchangeOrderDirect  = "order.direct"
	RoutingKeyCreated    = "order.created"
	QueueOrderCreated    = "order.created"

	// DLX 延迟超时拓扑
	ExchangeTimeoutDLX   = "order.timeout.dlx"   // Dead Letter Exchange
	RoutingKeyTimeout    = "timeout"               // DLX routing key
	QueueTimeoutCheck    = "order.timeout.check"  // 消费者在此队列处理超时

	// Parking Queue（无消费者，只用于消息 TTL 到期后死信路由）
	QueueTimeoutParking  = "order.delay.parking"
	RoutingKeyDelay      = "timeout.delay"
)

// ─────────────────────────────────────────────────────────────────────────────
// DeclareDLXTopology：声明完整的 DLX 拓扑
// ─────────────────────────────────────────────────────────────────────────────

// DeclareDLXTopology 声明 Demo 所需的全部 Exchange 和 Queue。
//
// 【知识点 | DLX（Dead Letter Exchange）延迟消息原理】
// RabbitMQ 无需插件即可实现近似延迟：
//   1. 声明 order.delay.parking 队列，附带 x-dead-letter-exchange 属性
//   2. 往 parking 队列发带 Expiration="5000"（毫秒）的消息
//   3. 消息 TTL 到期 → 变成死信 → 路由到 order.timeout.dlx Exchange
//   4. DLX 按 routing-key 路由到 order.timeout.check → 消费者处理超时
//
// 【可能问】「DLX 方案的局限性？」
// 【答】1. 精度不够：只有队列头部的消息 TTL 到期才会被死信，若头部消息未过期，
//          后续消息即使过期也不会先死信（FIFO 约束）。
//       2. 解决方案：每种延迟时间用独立 parking 队列（生产常见做法），
//          或使用 rabbitmq-delayed-message-exchange 插件（支持任意延迟）。
//       3. Demo 场景订单超时时间固定（15分钟），FIFO 无问题。
func DeclareDLXTopology(ch *amqp.Channel) error {
	// 1. 声明 order.direct Exchange（立即消息用）
	if err := ch.ExchangeDeclare(
		ExchangeOrderDirect, "direct", true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare order.direct: %w", err)
	}

	// 2. 声明 order.created 队列并绑定
	if _, err := ch.QueueDeclare(
		QueueOrderCreated, true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare order.created queue: %w", err)
	}
	if err := ch.QueueBind(QueueOrderCreated, RoutingKeyCreated, ExchangeOrderDirect, false, nil); err != nil {
		return fmt.Errorf("bind order.created: %w", err)
	}

	// 3. 声明 DLX Exchange（接收 parking queue 死信）
	if err := ch.ExchangeDeclare(
		ExchangeTimeoutDLX, "direct", true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare DLX exchange: %w", err)
	}

	// 4. 声明 order.timeout.check 队列（消费者队列）并绑定到 DLX
	if _, err := ch.QueueDeclare(
		QueueTimeoutCheck, true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare timeout.check queue: %w", err)
	}
	if err := ch.QueueBind(QueueTimeoutCheck, RoutingKeyTimeout, ExchangeTimeoutDLX, false, nil); err != nil {
		return fmt.Errorf("bind timeout.check: %w", err)
	}

	// 5. 声明 parking queue（无消费者，配置死信路由到 DLX）
	// x-dead-letter-exchange：TTL 到期后消息路由到哪个 Exchange
	// x-dead-letter-routing-key：死信使用的 routing key（路由到 QueueTimeoutCheck）
	parkingArgs := amqp.Table{
		"x-dead-letter-exchange":    ExchangeTimeoutDLX,
		"x-dead-letter-routing-key": RoutingKeyTimeout,
	}
	if _, err := ch.QueueDeclare(
		QueueTimeoutParking, true, false, false, false, parkingArgs,
	); err != nil {
		return fmt.Errorf("declare parking queue: %w", err)
	}
	// parking queue 不绑定 exchange，由 publisher 直接用默认 exchange + queue name 发送
	// 或声明一个专用 exchange 绑定（这里简化：用 default exchange，routing key = queue name）

	fmt.Println("[Infra] DLX 拓扑声明成功：order.direct → order.created | parking → DLX → order.timeout.check")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RabbitMQPublisher：实现 ports.Publisher（Confirm 模式）
// ─────────────────────────────────────────────────────────────────────────────

// RabbitMQPublisher 带 Publisher Confirm 的消息发布器。
//
// 【知识点 | Publisher Confirm 工作原理】
// 1. ch.Confirm(false) 开启确认模式
// 2. 每次 Publish 后，Broker 会发回一个 ack/nack
// 3. 同步等待 confirm（ch.NotifyPublishAsync 或 ch.NotifyPublish）
// 4. 收到 ack → 认为消息已持久化到 Broker 磁盘，可以 MarkPublished
//
// 【可能问】「Confirm 是同步的吗？会影响吞吐吗？」
// 【答】Demo 中用同步 Confirm（等待每条消息的 ack），简单但吞吐低（~3000-5000 msg/s）；
// 生产用异步批量 Confirm（批量发送，异步收 ack），吞吐可达 ~20000+ msg/s。
type RabbitMQPublisher struct {
	conn *amqp.Connection
}

// NewRabbitMQPublisher 构造 RabbitMQPublisher（建立连接）。
func NewRabbitMQPublisher() (*RabbitMQPublisher, error) {
	conn, err := amqp.Dial(rabbitMQURL)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}
	return &RabbitMQPublisher{conn: conn}, nil
}

// Publish 发布一条消息，同步等待 Broker Confirm。
func (p *RabbitMQPublisher) Publish(ctx context.Context, msg *ports.OutboxMessage, opts ...ports.PublishOption) error {
	ch, err := p.conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	// 开启 Publisher Confirm 模式
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("confirm mode: %w", err)
	}

	// 解析发布选项
	var opt ports.PublishOption
	if len(opts) > 0 {
		opt = opts[0]
	}

	// 确定 Exchange 和 RoutingKey
	exchange := ExchangeOrderDirect
	routingKey := msg.EventType // "order.created" or "order.timeout"

	publishing := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // 持久化消息，Broker 重启不丢失
		Body:         msg.Payload,
		MessageId:    fmt.Sprintf("%d", msg.ID), // 消息 ID，供消费端幂等使用
		Timestamp:    time.Now(),
	}

	if opt.ExpirationMS != "" {
		// DLX 延迟消息：发到 parking queue（用默认 Exchange，routing key = queue name）
		exchange = ""                       // 使用默认 Exchange（""）
		routingKey = QueueTimeoutParking    // 直接路由到 parking queue
		publishing.Expiration = opt.ExpirationMS
	}

	// 注册 Confirm 监听器
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	if err := ch.PublishWithContext(ctx, exchange, routingKey, false, false, publishing); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// 同步等待 Broker Confirm
	select {
	case confirm := <-confirms:
		if !confirm.Ack {
			return fmt.Errorf("broker nack msgID=%d", msg.ID)
		}
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("confirm timeout msgID=%d", msg.ID)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 关闭 RabbitMQ 连接（程序退出时调用）。
func (p *RabbitMQPublisher) Close() error {
	return p.conn.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// ConsumeTimeoutQueue：辅助函数，订阅 order.timeout.check 队列
// ─────────────────────────────────────────────────────────────────────────────

// TimeoutMessage 超时消息结构（从 MQ 解析）。
type TimeoutMessage struct {
	DeliveryTag uint64
	MsgID       string
	Body        []byte
}

// ConsumeTimeoutQueue 订阅 order.timeout.check 队列，返回消息 channel。
//
// 调用方从 channel 中读取 TimeoutMessage，处理后调用 Ack/Nack。
// 返回 amqp.Channel 供调用方手动 Ack/Nack（控制权给调用方，便于测试）。
func ConsumeTimeoutQueue(conn *amqp.Connection) (<-chan TimeoutMessage, *amqp.Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, fmt.Errorf("open channel: %w", err)
	}

	// Prefetch = 1：一次只处理 1 条消息，处理完再拉下一条，避免消息积压在客户端
	// 【知识点 | QoS Prefetch】
	// prefetch=0（默认）：Broker 把所有消息一次推给 consumer，consumer OOM 风险
	// prefetch=1：每次只拉 1 条，吞吐略低但更稳定，适合超时取消这类低频场景
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("qos: %w", err)
	}

	deliveries, err := ch.Consume(
		QueueTimeoutCheck,
		"",    // consumer tag（自动生成）
		false, // autoAck=false：手动 Ack，确保业务处理成功后才确认
		false, false, false, nil,
	)
	if err != nil {
		ch.Close()
		return nil, nil, fmt.Errorf("consume: %w", err)
	}

	out := make(chan TimeoutMessage, 16)
	go func() {
		defer close(out)
		for d := range deliveries {
			out <- TimeoutMessage{
				DeliveryTag: d.DeliveryTag,
				MsgID:       d.MessageId,
				Body:        d.Body,
			}
		}
	}()

	return out, ch, nil
}

// DialRabbitMQ 建立 RabbitMQ 连接（测试和初始化用）。
func DialRabbitMQ() (*amqp.Connection, error) {
	conn, err := amqp.Dial(rabbitMQURL)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}
	return conn, nil
}

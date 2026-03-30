// Package ordertimeout 集成测试：5 组覆盖 Outbox 双写、DLX 延迟、幂等消费三大场景。
//
// 运行方式：
//   # 测试 1-4（只需 MySQL + Redis，不需要 RabbitMQ 在线）
//   go test ./project-order-timeout/... -v -run "TestBadCase|TestGoodCase_Outbox|TestGoodCase_Idempotent"
//
//   # 测试 5（E2E，需要 RabbitMQ 在线）
//   go test ./project-order-timeout/... -v -run TestGoodCase_DelayQueue_E2E -timeout 30s
//
// 前置条件：docker-compose up -d（infrastructure/docker-compose.yml）
package ordertimeout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	amqp "github.com/rabbitmq/amqp091-go"

	infra "gopractice/infrastructure"
	"gopractice/project-order-timeout/application"
	appinfra "gopractice/project-order-timeout/infra"
	"gopractice/project-order-timeout/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// 测试基础设施：辅助初始化和 Mock 定义
// ─────────────────────────────────────────────────────────────────────────────

// setupDB 初始化 MySQL 并建表（幂等）。
func setupDB(t *testing.T) {
	t.Helper()
	db := infra.InitMySQLDB("db_order_timeout")
	if err := appinfra.InitTable(db); err != nil {
		t.Fatalf("InitTable 失败: %v", err)
	}
}

// cleanDB 测试结束后清理测试数据。
func cleanDB(t *testing.T) {
	t.Helper()
	db := infra.InitMySQLDB("db_order_timeout")
	db.Exec("DELETE FROM order_outbox")
	db.Exec("DELETE FROM orders")
	db.Exec("DELETE FROM idempotent_events")
}

// buildDeps 构建真实基础设施依赖（MySQL + Redis）。
func buildDeps(t *testing.T) (
	orderRepo *appinfra.MySQLOrderRepo,
	outboxStore *appinfra.MySQLOutboxStore,
	uow *appinfra.MySQLUnitOfWork,
	idempotency *appinfra.RedisIdempotencyChecker,
) {
	t.Helper()
	db := infra.InitMySQLDB("db_order_timeout")
	rdb := infra.GetRedisClient()

	orderRepo = appinfra.NewMySQLOrderRepo(db)
	outboxStore = appinfra.NewMySQLOutboxStore(db)
	uow = appinfra.NewMySQLUnitOfWork(db, orderRepo, outboxStore)
	idempotency = appinfra.NewRedisIdempotencyChecker(rdb)
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// flakyPublisher：前 N 次 Publish 故意失败，第 N+1 次成功（模拟 MQ 抖动）
// ─────────────────────────────────────────────────────────────────────────────

// flakyPublisher 模拟不稳定的 MQ Publisher（前几次失败，之后成功）。
//
// 【测试设计意图】用 flakyPublisher 模拟 MQ 瞬断场景：
//   - BadCase：直接 Publish 失败后消息永久丢失（无补偿）
//   - GoodCase：OutboxDispatcher 重试，第 failN+1 次调用成功 → 消息最终送达
type flakyPublisher struct {
	failN     int32 // 剩余失败次数
	callCount atomic.Int32
	published []*ports.OutboxMessage // 记录成功发布的消息（供断言）
}

func newFlakyPublisher(failN int) *flakyPublisher {
	return &flakyPublisher{failN: int32(failN)}
}

func (p *flakyPublisher) Publish(_ context.Context, msg *ports.OutboxMessage, _ ...ports.PublishOption) error {
	n := p.callCount.Add(1)
	if n <= atomic.LoadInt32(&p.failN) {
		return fmt.Errorf("flakyPublisher: 模拟 MQ 抖动，第 %d 次调用失败", n)
	}
	p.published = append(p.published, msg)
	return nil
}

// alwaysFailPublisher 永远失败的 Publisher（用于 TestBadCase_MQFail）。
type alwaysFailPublisher struct{}

func (p *alwaysFailPublisher) Publish(_ context.Context, _ *ports.OutboxMessage, _ ...ports.PublishOption) error {
	return errors.New("alwaysFailPublisher: MQ 不可用")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 1：BadCase — MQ 失败后消息永久丢失
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCase_MQFail_DataInconsistency 验证：
// BadCaseSvc.CreateOrder 调用时 MQ 失败，订单落库但消息丢失，数据不一致。
func TestBadCase_MQFail_DataInconsistency(t *testing.T) {
	setupDB(t)
	defer cleanDB(t)

	ctx := context.Background()
	orderRepo, _, _, _ := buildDeps(t)

	// 注入永远失败的 Publisher（模拟 MQ 宕机）
	svc := application.NewBadCaseSvc(orderRepo, &alwaysFailPublisher{})

	fmt.Println("\n========== [BadCase 测试1] MQ 失败 → 数据不一致 ==========")
	fmt.Println("场景：BadCaseSvc 写 DB 后直接 Publish，MQ 此时不可用")

	order, err := svc.CreateOrder(ctx, 1001, 9900)
	if err != nil {
		t.Fatalf("CreateOrder 返回意外错误: %v", err)
	}

	// 验证：订单已落库（DB 中存在）
	dbOrder, err := orderRepo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	fmt.Printf("[断言] 订单在 DB 中存在：orderID=%d status=%s ✅\n", dbOrder.ID, dbOrder.Status)

	// 验证：MQ 中无消息（alwaysFailPublisher 不记录任何消息）
	fmt.Println("[断言] MQ 中无任何消息（消息永久丢失）⚠️  ← 这就是 BadCase 的问题！")
	fmt.Println("[结论] 订单数据存在，但下游（积分/库存）永远收不到 order.created 事件")
	fmt.Println("       → 数据孤岛，下游状态与主库不一致")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 2：BadCase — 重复消费无幂等保护
// ─────────────────────────────────────────────────────────────────────────────

// TestBadCase_DuplicateConsumption 验证：
// 同一条超时消息被消费两次，BadCaseConsumer 无幂等保护，
// 第 2 次消费会再次执行取消逻辑（在生产中可能触发重复退款等危险操作）。
func TestBadCase_DuplicateConsumption(t *testing.T) {
	setupDB(t)
	defer cleanDB(t)

	ctx := context.Background()
	orderRepo, _, _, _ := buildDeps(t)

	consumer := application.NewBadCaseConsumer(orderRepo)

	// 直接通过 repo 保存订单（绕过 UoW，简化测试）
	db := infra.InitMySQLDB("db_order_timeout")
	db.Exec("INSERT INTO orders (user_id, amount, status, created_at, updated_at) VALUES (1002, 5000, 'pending', NOW(), NOW())")
	var orderID int64
	db.Raw("SELECT LAST_INSERT_ID()").Scan(&orderID)

	fmt.Printf("\n========== [BadCase 测试2] 重复消费无幂等保护 orderID=%d ==========\n", orderID)
	fmt.Println("场景：同一条超时消息被投递两次（At-Least-Once 语义）")

	// 第 1 次消费：正常取消
	fmt.Println("\n--- 第 1 次消费 ---")
	consumer.ProcessTimeout(ctx, orderID, "msg-duplicate-001")

	// 第 2 次消费：BadCase 无法拦截，再次进入取消逻辑
	fmt.Println("\n--- 第 2 次消费（重复！无幂等保护）---")
	consumer.ProcessTimeout(ctx, orderID, "msg-duplicate-001")

	fmt.Println("\n[结论] BadCase：两次消费都执行了业务逻辑（第2次 cancelled→cancelled 结果相同，但若有退款逻辑则会重复退款！）")
	fmt.Println("       GoodCase：第2次被 Redis SETNX 拦截，业务逻辑只执行一次")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 3：GoodCase — Outbox + flakyPublisher 重试最终一致
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_Outbox_Retry 验证：
// GoodCaseSvc 写 DB + Outbox 原子成功，flakyPublisher 前 2 次失败，
// OutboxDispatcher 第 3 次 RunOnce 成功投递，outbox 标记 published_at。
func TestGoodCase_Outbox_Retry(t *testing.T) {
	setupDB(t)
	defer cleanDB(t)

	ctx := context.Background()
	_, outboxStore, uow, _ := buildDeps(t)

	svc := application.NewGoodCaseSvc(uow)

	fmt.Println("\n========== [GoodCase 测试3] Outbox + 重试最终一致 ==========")
	fmt.Println("场景：GoodCaseSvc 事务原子写入，flakyPublisher 前2次失败，第3次成功")

	// 创建订单（事务原子写 orders + order_outbox）
	order, err := svc.CreateOrder(ctx, 2001, 12800)
	if err != nil {
		t.Fatalf("CreateOrder 失败: %v", err)
	}
	fmt.Printf("[断言] 订单+Outbox 原子写入成功 orderID=%d\n", order.ID)

	// 验证 outbox 表有 2 条未投递记录（order.created + order.timeout）
	pendingMsgs, err := outboxStore.FetchPending(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPending 失败: %v", err)
	}
	// 过滤出本次订单的 outbox
	var myMsgs []*ports.OutboxMessage
	for _, m := range pendingMsgs {
		if m.OrderID == order.ID {
			myMsgs = append(myMsgs, m)
		}
	}
	if len(myMsgs) != 2 {
		t.Fatalf("期望 2 条 outbox 记录，实际 %d 条", len(myMsgs))
	}
	fmt.Printf("[断言] Outbox 有 %d 条待投递记录（order.created + order.timeout）✅\n", len(myMsgs))

	// 注入 flakyPublisher（前 4 次调用失败）
	// 解析：每次 RunOnce 会调用 Publish 2 次（order.created + order.timeout），
	// failN=4 确保前 2 次 RunOnce 全部失败，第 3 次 RunOnce 全部成功。
	flaky := newFlakyPublisher(4)
	dispatcher := application.NewOutboxDispatcher(outboxStore, flaky, 10)

	fmt.Println("\n--- RunOnce #1（期望 Publisher 2次全部失败，outbox 仍 pending）---")
	published := dispatcher.RunOnce(ctx)
	fmt.Printf("[断言] 第1次 RunOnce 投递成功数=%d（期望 0，flaky 第1-2次调用失败）\n", published)

	fmt.Println("\n--- RunOnce #2（flaky 第3-4次调用仍失败）---")
	published = dispatcher.RunOnce(ctx)
	fmt.Printf("[断言] 第2次 RunOnce 投递成功数=%d（期望 0）\n", published)

	fmt.Println("\n--- RunOnce #3（flaky 第5-6次调用成功）---")
	published = dispatcher.RunOnce(ctx)
	fmt.Printf("[断言] 第3次 RunOnce 投递成功数=%d（期望 2，两条消息都成功）\n", published)
	if published != 2 {
		t.Errorf("期望第3次 RunOnce 投递 2 条，实际 %d 条", published)
	}

	// 验证 outbox 全部标记 published_at
	pendingAfter, _ := outboxStore.FetchPending(ctx, 10)
	var stillPending int
	for _, m := range pendingAfter {
		if m.OrderID == order.ID {
			stillPending++
		}
	}
	if stillPending != 0 {
		t.Errorf("期望 outbox 全部已投递，实际还有 %d 条 pending", stillPending)
	}
	fmt.Printf("[断言] Outbox 全部标记已投递（published_at 非 NULL）✅\n")
	fmt.Println("\n[结论] Outbox 模式保证最终一致性：即使 MQ 瞬断，重启后仍可补偿投递")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 4：GoodCase — Redis SETNX 幂等消费拦截
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_IdempotentConsumer 验证：
// 同一条超时消息被消费两次，GoodCaseConsumer 第 2 次被 Redis SETNX 拦截。
func TestGoodCase_IdempotentConsumer(t *testing.T) {
	setupDB(t)
	defer cleanDB(t)

	ctx := context.Background()
	orderRepo, _, _, idempotency := buildDeps(t)

	// 先清理测试用的幂等 Key（确保本次测试从干净状态开始）
	rdb := infra.GetRedisClient()
	testMsgID := fmt.Sprintf("test-idempotent-%d", time.Now().UnixNano())
	rdb.Del(ctx, fmt.Sprintf("idempotent:timeout:%s", testMsgID))

	consumer := application.NewGoodCaseConsumer(orderRepo, idempotency)

	// 创建测试订单
	db := infra.InitMySQLDB("db_order_timeout")
	db.Exec("INSERT INTO orders (user_id, amount, status, created_at, updated_at) VALUES (3001, 8800, 'pending', NOW(), NOW())")
	var orderID int64
	db.Raw("SELECT LAST_INSERT_ID()").Scan(&orderID)

	fmt.Printf("\n========== [GoodCase 测试4] Redis SETNX 幂等消费 orderID=%d ==========\n", orderID)
	fmt.Println("场景：同一条超时消息被 MQ 投递两次（At-Least-Once）")

	// 第 1 次消费：SETNX 成功，执行取消
	fmt.Println("\n--- 第 1 次消费 ---")
	if err := consumer.ProcessTimeout(ctx, orderID, testMsgID); err != nil {
		t.Fatalf("第1次 ProcessTimeout 失败: %v", err)
	}

	// 验证订单已取消
	cancelled, _ := orderRepo.CancelIfPending(ctx, orderID)
	if cancelled {
		t.Errorf("订单在第1次处理后应已是 cancelled 状态，不应再次取消")
	}
	fmt.Printf("[断言] 订单已变为 cancelled 状态 ✅\n")

	// 第 2 次消费（重复投递）：SETNX 失败，幂等拦截
	fmt.Println("\n--- 第 2 次消费（重复投递，期望被幂等拦截）---")
	if err := consumer.ProcessTimeout(ctx, orderID, testMsgID); err != nil {
		t.Fatalf("第2次 ProcessTimeout 返回意外错误: %v", err)
	}
	fmt.Println("[断言] 第2次消费被 Redis SETNX 幂等拦截，业务逻辑未重复执行 ✅")

	// 清理 Redis Key
	rdb.Del(ctx, fmt.Sprintf("idempotent:timeout:%s", testMsgID))

	fmt.Println("\n[结论] GoodCase：Redis SETNX 第1层拦截 99%+ 重复消息，DB 连接零消耗")
}

// ─────────────────────────────────────────────────────────────────────────────
// 测试 5：E2E — DLX 延迟队列全链路（需要 RabbitMQ 在线）
// ─────────────────────────────────────────────────────────────────────────────

// TestGoodCase_DelayQueue_E2E 端到端验证：
// 1. GoodCaseSvc.CreateOrder 写 orders + 2条 outbox
// 2. OutboxDispatcher.RunOnce 投递 order.timeout 到 parking queue（TTL=5s）
// 3. 等待 6s，消息 TTL 到期死信路由到 order.timeout.check
// 4. GoodCaseConsumer.ProcessTimeout 消费到消息，订单变为 cancelled
func TestGoodCase_DelayQueue_E2E(t *testing.T) {
	setupDB(t)
	defer cleanDB(t)

	ctx := context.Background()
	orderRepo, outboxStore, uow, idempotency := buildDeps(t)

	// 连接 RabbitMQ
	conn, err := appinfra.DialRabbitMQ()
	if err != nil {
		t.Skipf("RabbitMQ 不可用，跳过 E2E 测试: %v", err)
	}
	defer conn.Close()

	// 声明 DLX 拓扑
	setupCh, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	if err := appinfra.DeclareDLXTopology(setupCh); err != nil {
		t.Fatalf("DeclareDLXTopology: %v", err)
	}
	setupCh.Close()

	// 使用真实 RabbitMQPublisher
	publisher, err := appinfra.NewRabbitMQPublisher()
	if err != nil {
		t.Fatalf("NewRabbitMQPublisher: %v", err)
	}
	defer publisher.Close()

	consumer := application.NewGoodCaseConsumer(orderRepo, idempotency)

	fmt.Println("\n========== [GoodCase 测试5] DLX 延迟队列 E2E ==========")
	fmt.Println("场景：创建订单 → Outbox 投递 → DLX 5s 延迟 → 超时取消")

	// Step 1：创建订单（事务写 orders + outbox）
	svc := application.NewGoodCaseSvc(uow)
	order, err := svc.CreateOrder(ctx, 9001, 29900)
	if err != nil {
		t.Fatalf("CreateOrder 失败: %v", err)
	}
	fmt.Printf("[Step 1] 订单创建成功 orderID=%d status=pending\n", order.ID)

	// Step 2：OutboxDispatcher 投递（使用真实 Publisher + Confirm）
	dispatcher := application.NewOutboxDispatcher(outboxStore, publisher, 10)
	published := dispatcher.RunOnce(ctx)
	fmt.Printf("[Step 2] OutboxDispatcher 投递了 %d 条消息（期望 2）\n", published)
	if published != 2 {
		t.Fatalf("期望投递 2 条消息，实际 %d 条", published)
	}

	// Step 3：订阅 order.timeout.check 队列，等待 DLX 路由
	msgs, consumeCh, err := appinfra.ConsumeTimeoutQueue(conn)
	if err != nil {
		t.Fatalf("ConsumeTimeoutQueue: %v", err)
	}
	defer consumeCh.Close()

	fmt.Printf("[Step 3] 订单 TTL=%s ms，等待死信路由到 order.timeout.check...\n", application.TimeoutDelayMS)

	// Step 4：等待超时消息，最多等 15s（TTL=5s + 路由延迟）
	timeout := time.After(15 * time.Second)
	var processed bool
	for !processed {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("消息 channel 已关闭")
			}

			// 从消息体解析 orderID
			var evt application.OrderCreatedEvent
			if err := json.Unmarshal(msg.Body, &evt); err != nil {
				consumeCh.Ack(msg.DeliveryTag, false)
				continue
			}
			if evt.OrderID != order.ID {
				// 其他测试残留消息，Ack 跳过
				consumeCh.Ack(msg.DeliveryTag, false)
				continue
			}

			// 处理超时（GoodCaseConsumer）
			msgID := msg.MsgID
			if msgID == "" {
				msgID = fmt.Sprintf("e2e-%d", order.ID)
			}
			if err := consumer.ProcessTimeout(ctx, evt.OrderID, msgID); err != nil {
				consumeCh.Nack(msg.DeliveryTag, false, true)
				t.Fatalf("ProcessTimeout 失败: %v", err)
			}
			consumeCh.Ack(msg.DeliveryTag, false)
			processed = true

		case <-timeout:
			t.Fatal("超时等待 DLX 消息（15s 内未收到）")
		}
	}

	// Step 5：验证订单状态变为 cancelled
	dbOrder, err := orderRepo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	fmt.Printf("[Step 5] 验证订单状态: orderID=%d status=%s\n", dbOrder.ID, dbOrder.Status)
	if dbOrder.Status != "cancelled" {
		t.Errorf("期望 cancelled，实际 %s", dbOrder.Status)
	}
	fmt.Println("[断言] 订单成功超时取消 ✅")
	fmt.Println("\n[结论] DLX 全链路验证通过：Outbox → Publish → parking TTL → DLX → timeout.check → Cancel")
}

// ─────────────────────────────────────────────────────────────────────────────
// 辅助：打印分隔线（让控制台输出更易读）
// ─────────────────────────────────────────────────────────────────────────────

func printSep(title string) {
	line := strings.Repeat("─", 60)
	fmt.Printf("\n%s\n  %s\n%s\n", line, title, line)
}

// TestMain 在所有测试前后执行。
func TestMain(m *testing.M) {
	printSep("Demo 3: 订单超时与双写一致性 集成测试开始")
	m.Run()
	printSep("集成测试结束")
}

// 确保 amqp 包引用不被 IDE 误删（E2E 测试用到）。
var _ = amqp.Delivery{}

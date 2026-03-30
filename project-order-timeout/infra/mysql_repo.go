// Package infra 基础设施层（适配器）：将接口（ports）适配到具体技术实现（GORM/RabbitMQ/Redis）。
//
// 【知识点 | 六边形架构 Adapter】
// infra/ 中的每个文件都是一个"Adapter"，实现 ports/ 中的接口。
// 应用层（application/）只依赖 ports/ 接口，通过依赖注入获得 infra/ 的实例。
// 这样可以在测试中用 Mock 替换 infra/ 实现，实现真正的单元测试隔离。
package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopractice/project-order-timeout/domain"
	"gopractice/project-order-timeout/ports"
	"gorm.io/gorm"
)

// ─────────────────────────────────────────────────────────────────────────────
// GORM 数据模型
// ─────────────────────────────────────────────────────────────────────────────

// orderModel orders 表的 GORM 映射结构（与领域模型分离）。
//
// 【知识点 | ORM 模型 vs 领域模型分离】
// 不让 GORM tag 污染领域对象；infra 层负责映射转换。
// 好处：领域对象不依赖 ORM，可以随时换数据库（甚至换 NoSQL）。
type orderModel struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	UserID    int64  `gorm:"not null;index"`
	Amount    int64  `gorm:"not null"`
	Status    string `gorm:"type:enum('pending','paid','cancelled');not null;default:'pending';index"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (orderModel) TableName() string { return "orders" }

// outboxModel order_outbox 表的 GORM 映射。
type outboxModel struct {
	ID          int64      `gorm:"primaryKey;autoIncrement"`
	OrderID     int64      `gorm:"not null;index"`
	EventType   string     `gorm:"size:64;not null"`
	Payload     []byte     `gorm:"type:json"`
	PublishedAt *time.Time `gorm:"default:null"`
	CreatedAt   time.Time
}

func (outboxModel) TableName() string { return "order_outbox" }

// ─────────────────────────────────────────────────────────────────────────────
// 事务 Key（用于在 Context 中传递事务 DB）
// ─────────────────────────────────────────────────────────────────────────────

type txKeyType struct{}

var txKey = txKeyType{}

// dbFromCtx 优先使用 context 中的事务 DB，否则用全局 DB。
func dbFromCtx(ctx context.Context, db *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txKey).(*gorm.DB); ok && tx != nil {
		return tx
	}
	return db
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLOrderRepo：实现 ports.OrderRepository
// ─────────────────────────────────────────────────────────────────────────────

// MySQLOrderRepo 订单仓储 MySQL 实现。
type MySQLOrderRepo struct {
	db *gorm.DB
}

// NewMySQLOrderRepo 构造 MySQLOrderRepo。
func NewMySQLOrderRepo(db *gorm.DB) *MySQLOrderRepo {
	return &MySQLOrderRepo{db: db}
}

// Save 插入新订单并回填 ID。
func (r *MySQLOrderRepo) Save(ctx context.Context, o *domain.Order) error {
	m := &orderModel{
		UserID:    o.UserID,
		Amount:    o.Amount,
		Status:    string(o.Status),
		CreatedAt: o.CreatedAt,
		UpdatedAt: o.UpdatedAt,
	}
	if err := dbFromCtx(ctx, r.db).WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("MySQLOrderRepo.Save: %w", err)
	}
	o.ID = m.ID // 回填自增 ID
	return nil
}

// GetByID 查询订单，不存在返回 domain.ErrOrderNotFound。
func (r *MySQLOrderRepo) GetByID(ctx context.Context, id int64) (*domain.Order, error) {
	var m orderModel
	err := dbFromCtx(ctx, r.db).WithContext(ctx).First(&m, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrOrderNotFound
		}
		return nil, fmt.Errorf("MySQLOrderRepo.GetByID: %w", err)
	}
	return modelToDomain(&m), nil
}

// CancelIfPending 原子条件更新：WHERE id=? AND status='pending'。
//
// 【知识点 | 乐观取消 vs SELECT FOR UPDATE】
// 此处用单条 UPDATE + WHERE 条件，数据库原子执行：
//   - affected=1：成功取消
//   - affected=0：订单不在 pending 状态（已支付或已取消），幂等安全
// 比 SELECT FOR UPDATE 省去了共享锁，并发更高。
func (r *MySQLOrderRepo) CancelIfPending(ctx context.Context, id int64) (bool, error) {
	result := dbFromCtx(ctx, r.db).WithContext(ctx).
		Model(&orderModel{}).
		Where("id = ? AND status = ?", id, string(domain.OrderStatusPending)).
		Updates(map[string]interface{}{
			"status":     string(domain.OrderStatusCancelled),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return false, fmt.Errorf("MySQLOrderRepo.CancelIfPending: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// modelToDomain orderModel → domain.Order 映射。
func modelToDomain(m *orderModel) *domain.Order {
	return &domain.Order{
		ID:        m.ID,
		UserID:    m.UserID,
		Amount:    m.Amount,
		Status:    domain.OrderStatus(m.Status),
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLOutboxStore：实现 ports.OutboxStore
// ─────────────────────────────────────────────────────────────────────────────

// MySQLOutboxStore Outbox 存储 MySQL 实现。
type MySQLOutboxStore struct {
	db *gorm.DB
}

// NewMySQLOutboxStore 构造 MySQLOutboxStore。
func NewMySQLOutboxStore(db *gorm.DB) *MySQLOutboxStore {
	return &MySQLOutboxStore{db: db}
}

// Enqueue 在事务内插入 outbox 记录。
func (s *MySQLOutboxStore) Enqueue(ctx context.Context, msg *ports.OutboxMessage) error {
	m := &outboxModel{
		OrderID:   msg.OrderID,
		EventType: msg.EventType,
		Payload:   msg.Payload,
		CreatedAt: time.Now().UTC(),
	}
	if err := dbFromCtx(ctx, s.db).WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("MySQLOutboxStore.Enqueue: %w", err)
	}
	msg.ID = m.ID
	return nil
}

// FetchPending 查询未投递的 outbox 消息（published_at IS NULL）。
func (s *MySQLOutboxStore) FetchPending(ctx context.Context, limit int) ([]*ports.OutboxMessage, error) {
	var models []outboxModel
	err := s.db.WithContext(ctx).
		Where("published_at IS NULL").
		Order("created_at ASC"). // 按创建时间升序，保证先进先出
		Limit(limit).
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("MySQLOutboxStore.FetchPending: %w", err)
	}

	msgs := make([]*ports.OutboxMessage, len(models))
	for i, m := range models {
		msgs[i] = &ports.OutboxMessage{
			ID:          m.ID,
			OrderID:     m.OrderID,
			EventType:   m.EventType,
			Payload:     m.Payload,
			PublishedAt: m.PublishedAt,
			CreatedAt:   m.CreatedAt,
		}
	}
	return msgs, nil
}

// MarkPublished 将指定 outbox 记录标记为已投递。
func (s *MySQLOutboxStore) MarkPublished(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).
		Model(&outboxModel{}).
		Where("id = ?", id).
		Update("published_at", now).Error
	if err != nil {
		return fmt.Errorf("MySQLOutboxStore.MarkPublished: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLUnitOfWork：实现 ports.UnitOfWork
// ─────────────────────────────────────────────────────────────────────────────

// MySQLUnitOfWork 事务工作单元 MySQL 实现。
//
// 【知识点 | 通过 Context 传递事务】
// 用 context.WithValue 把 *gorm.DB（已 Begin）注入 Context，
// 事务内所有仓储操作通过 dbFromCtx 自动获取同一个事务 DB，
// 无需在方法签名里显式传 tx 参数，保持接口整洁。
type MySQLUnitOfWork struct {
	db        *gorm.DB
	orderRepo *MySQLOrderRepo
	outbox    *MySQLOutboxStore
}

// NewMySQLUnitOfWork 构造 MySQLUnitOfWork。
func NewMySQLUnitOfWork(db *gorm.DB, orderRepo *MySQLOrderRepo, outbox *MySQLOutboxStore) *MySQLUnitOfWork {
	return &MySQLUnitOfWork{db: db, orderRepo: orderRepo, outbox: outbox}
}

// WithinTransaction 开启事务，执行 fn，成功则 Commit，失败则 Rollback。
func (u *MySQLUnitOfWork) WithinTransaction(ctx context.Context, fn func(context.Context, ports.OrderTx) error) error {
	return u.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 将事务 DB 注入 Context，让 orderRepo 和 outbox 的后续调用都使用同一个 tx
		txCtx := context.WithValue(ctx, txKey, tx)
		return fn(txCtx, &orderTxAdapter{orderRepo: u.orderRepo, outbox: u.outbox})
	})
}

// orderTxAdapter 将 orderRepo 和 outbox 包装为 ports.OrderTx。
type orderTxAdapter struct {
	orderRepo *MySQLOrderRepo
	outbox    *MySQLOutboxStore
}

func (a *orderTxAdapter) SaveOrder(ctx context.Context, o *domain.Order) error {
	return a.orderRepo.Save(ctx, o)
}

func (a *orderTxAdapter) EnqueueOutbox(ctx context.Context, msg *ports.OutboxMessage) error {
	return a.outbox.Enqueue(ctx, msg)
}

// ─────────────────────────────────────────────────────────────────────────────
// InitTable：建表（测试和 Demo 用）
// ─────────────────────────────────────────────────────────────────────────────

// InitTable 读取 SQL 文件内的建表语句并执行（幂等：CREATE TABLE IF NOT EXISTS）。
// 测试中直接调用，避免手动维护 SQL 文件路径依赖。
func InitTable(db *gorm.DB) error {
	sqls := []string{
		`CREATE TABLE IF NOT EXISTS orders (
			id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id    BIGINT       NOT NULL,
			amount     BIGINT       NOT NULL,
			status     ENUM('pending','paid','cancelled') NOT NULL DEFAULT 'pending',
			created_at DATETIME(3)  NOT NULL,
			updated_at DATETIME(3)  NOT NULL,
			INDEX idx_user_id (user_id),
			INDEX idx_status (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS order_outbox (
			id           BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
			order_id     BIGINT      NOT NULL,
			event_type   VARCHAR(64) NOT NULL,
			payload      JSON        NOT NULL,
			published_at DATETIME(3) NULL,
			created_at   DATETIME(3) NOT NULL,
			UNIQUE KEY uq_order_event (order_id, event_type),
			INDEX idx_pending (published_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS idempotent_events (
			id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
			msg_id       VARCHAR(128) NOT NULL,
			processed_at DATETIME(3)  NOT NULL,
			UNIQUE KEY uq_msg_id (msg_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}

	for _, sql := range sqls {
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("InitTable: %w", err)
		}
	}
	return nil
}

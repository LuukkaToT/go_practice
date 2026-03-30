// Package infra 秒杀场景基础设施层：MySQL + Redis 的具体实现。
//
// 【DDD分层】infra 实现 ports 接口，Application/Domain 不直接依赖本包。
// 【可能问】「GORM 和原生 SQL 怎么选？」
// 【答】GORM 便于快速开发和迁移；高并发核心路径（CAS UPDATE）直接用 Exec 写原生 SQL
// 更透明可控，避免 GORM 生成非预期 SQL（如漏 WHERE 导致全表更新）。
package infra

import (
	"context"
	"time"

	"gopractice/project-oversale/domain"

	"gorm.io/gorm"
)

// flashProductModel GORM 持久化模型（基础设施层，Domain 不引用）。
//
// 【知识点 | version 字段】
// 乐观锁的 version 是领域概念（「这一版本的库存」），但作为 DB 列放在持久化模型里；
// 领域层的 domain.Stock.Version 与此字段 1:1 映射，infra 层负责转换。
type flashProductModel struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	Name      string    `gorm:"column:name;size:100"`
	Stock     int64     `gorm:"column:stock"`
	Version   int64     `gorm:"column:version"` // 乐观锁版本号
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (flashProductModel) TableName() string { return "flash_products" }

// MySQLStockRepo 实现 ports.StockRepository。
type MySQLStockRepo struct {
	DB *gorm.DB
}

// GetStock 查询库存（SELECT id, stock, version）。
func (r *MySQLStockRepo) GetStock(ctx context.Context, productID int64) (*domain.Stock, error) {
	var m flashProductModel
	if err := r.DB.WithContext(ctx).
		Select("id", "stock", "version").
		First(&m, productID).Error; err != nil {
		return nil, err
	}
	return &domain.Stock{
		ProductID: m.ID,
		Available: m.Stock,
		Version:   m.Version,
	}, nil
}

// DeductDirect 直接 UPDATE stock -= qty，无 version 校验。
//
// 【BadCase专用】虽然 MySQL UPDATE 本身原子，但调用方在 SELECT 和 UPDATE 之间
// 没有任何锁保护，并发时读到的 stock > 0 判断可能已过时，导致超卖。
//
// 【面试点】这里的 SQL 本身是正确的；超卖发生在应用层的「读-判断」和「写」之间，
// 不是 SQL 的问题，而是调用流程设计的问题。
func (r *MySQLStockRepo) DeductDirect(ctx context.Context, productID, qty int64) error {
	result := r.DB.WithContext(ctx).Exec(
		"UPDATE flash_products SET stock = stock - ? WHERE id = ?",
		qty, productID,
	)
	return result.Error
}

// DeductWithVersion CAS 乐观锁 UPDATE。
//
// SQL：UPDATE flash_products
//
//	SET stock = stock - ?, version = version + 1
//	WHERE id = ? AND stock >= ? AND version = ?
//
// affected == 0 表示：version 已被其他事务更新（冲突），上层重试；
// 或 stock < qty（库存不足），通过 CurrentStock 判断具体原因。
//
// 【可能问】「WHERE stock >= qty 能防超卖吗？」
// 【答】能，这是 DB 侧的兜底保护：即使应用层误判，stock 不会变负。
// 但它不能替代乐观锁：没有 WHERE version=? 时，并发 UPDATE 都能成功，
// 导致一笔完整扣减被执行多次（丢失更新问题）。
func (r *MySQLStockRepo) DeductWithVersion(ctx context.Context, productID, qty, version int64) (bool, error) {
	result := r.DB.WithContext(ctx).Exec(
		`UPDATE flash_products
		 SET stock = stock - ?, version = version + 1
		 WHERE id = ? AND stock >= ? AND version = ?`,
		qty, productID, qty, version,
	)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ResetStock 重置库存和 version（测试 setup 专用）。
func (r *MySQLStockRepo) ResetStock(ctx context.Context, productID, stock int64) error {
	return r.DB.WithContext(ctx).Exec(
		"UPDATE flash_products SET stock = ?, version = 0 WHERE id = ?",
		stock, productID,
	).Error
}

// CurrentStock 查询最新库存数值（测试断言专用）。
func (r *MySQLStockRepo) CurrentStock(ctx context.Context, productID int64) (int64, error) {
	var s int64
	err := r.DB.WithContext(ctx).
		Model(&flashProductModel{}).
		Select("stock").
		Where("id = ?", productID).
		Scan(&s).Error
	return s, err
}

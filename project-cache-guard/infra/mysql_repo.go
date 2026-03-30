// Package infra 缓存治理场景基础设施层。
package infra

import (
	"context"
	"errors"
	"time"

	"gopractice/project-cache-guard/domain"

	"gorm.io/gorm"
)

// hotProductModel GORM 持久化模型。
type hotProductModel struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	Name      string    `gorm:"column:name;size:100"`
	Price     int64     `gorm:"column:price"`
	Stock     int64     `gorm:"column:stock"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (hotProductModel) TableName() string { return "hot_products" }

// MySQLProductRepo 实现 ports.ProductRepository。
type MySQLProductRepo struct {
	DB *gorm.DB
}

// GetByID 查询商品（不存在时返回 domain.ErrProductNotFound）。
//
// 【面试点 | 统一错误映射】infra 层将 gorm.ErrRecordNotFound 映射为领域错误，
// 上层不依赖 GORM 的具体错误类型，满足依赖倒置。
func (r *MySQLProductRepo) GetByID(ctx context.Context, id int64) (*domain.Product, error) {
	var m hotProductModel
	err := r.DB.WithContext(ctx).First(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrProductNotFound
	}
	if err != nil {
		return nil, err
	}
	return &domain.Product{
		ID:    m.ID,
		Name:  m.Name,
		Price: m.Price,
		Stock: m.Stock,
	}, nil
}

// SeedProducts 批量插入测试商品（测试 setup 专用）。
func (r *MySQLProductRepo) SeedProducts(ctx context.Context, products []domain.Product) error {
	models := make([]hotProductModel, len(products))
	for i, p := range products {
		models[i] = hotProductModel{
			ID:    p.ID,
			Name:  p.Name,
			Price: p.Price,
			Stock: p.Stock,
		}
	}
	return r.DB.WithContext(ctx).
		Clauses().
		Exec(`DELETE FROM hot_products`).Error // 清空旧数据
}

// InitTable 建表（测试 setup 专用，幂等）。
func (r *MySQLProductRepo) InitTable(ctx context.Context) error {
	return r.DB.Exec(`CREATE TABLE IF NOT EXISTS hot_products (
		id         BIGINT    NOT NULL AUTO_INCREMENT PRIMARY KEY,
		name       VARCHAR(100) NOT NULL DEFAULT '',
		price      BIGINT    NOT NULL DEFAULT 0,
		stock      BIGINT    NOT NULL DEFAULT 0,
		created_at DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`).Error
}

// UpsertProduct 插入或更新商品（测试专用）。
func (r *MySQLProductRepo) UpsertProduct(ctx context.Context, p domain.Product) error {
	return r.DB.WithContext(ctx).Exec(
		`INSERT INTO hot_products (id, name, price, stock)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE name=VALUES(name), price=VALUES(price), stock=VALUES(stock)`,
		p.ID, p.Name, p.Price, p.Stock,
	).Error
}

package application

import (
	"context"
	"time"

	"gopractice/project-oversale/domain"
	"gopractice/project-oversale/ports"
)

// ======================================================================
// GoodCase 3：MySQL 乐观锁（Version + CAS UPDATE）
// ======================================================================

// GoodCaseOptimisticSvc 通过 CAS（Compare-And-Swap）UPDATE 实现无锁并发控制。
//
// 【乐观锁原理】
// SQL：UPDATE flash_products SET stock=stock-1, version=version+1
//      WHERE id=? AND stock>=1 AND version=?
//
// 若并发 goroutine 读到相同 version=V：
//   - 第一个 UPDATE 成功，version 变为 V+1，affected=1
//   - 其余 UPDATE WHERE version=V 命中零行，affected=0 → CAS 失败 → 重试
//
// 【可能问】「乐观锁重试多少次合适？」
// 【答】视业务：秒杀流量峰值冲突率高，重试无限会打满 DB，通常限 3~5 次；
// 超限后返回「系统繁忙」引导客户端重试（客户端侧退避）。
//
// 【可能问】「乐观锁 vs 悲观锁（SELECT FOR UPDATE）怎么选？」
// 【答】
//   - 乐观锁：无等待，冲突少时高效；冲突多时大量重试，吞吐反而降。
//   - 悲观锁：有等待，串行化强，但连接池会被锁等待耗尽，高并发慎用。
//   - 秒杀：冲突率极高，两者都不适合做第一道防线；Redis Lua 是首选，DB 做二次兜底。
//
// 【可能问】「version 字段能否用 updated_at 时间戳代替？」
// 【答】可以，但时间精度不够时（同一毫秒）会出现版本号相同的假阳性；
// 自增整数 version 更可靠。
type GoodCaseOptimisticSvc struct {
	Repo     ports.StockRepository
	MaxRetry int
}

const defaultMaxRetry = 5

// Purchase 乐观锁版本：CAS 失败则重试，超限返回冲突错误。
func (s *GoodCaseOptimisticSvc) Purchase(ctx context.Context, productID int64) error {
	maxRetry := s.MaxRetry
	if maxRetry <= 0 {
		maxRetry = defaultMaxRetry
	}

	for attempt := 0; attempt < maxRetry; attempt++ {
		// Step 1：读取当前库存和 version（每次重试必须重新读，拿最新 version）。
		stock, err := s.Repo.GetStock(ctx, productID)
		if err != nil {
			return err
		}
		// Step 2：领域层校验不变量（此处仍是乐观判断，非最终保证）。
		if err := stock.Deduct(1); err != nil {
			return err // ErrOutOfStock 直接返回，无需重试
		}

		// Step 3：CAS UPDATE，WHERE version = 读到的旧版本。
		// 若其他 goroutine 已修改 version，此 UPDATE 影响行数=0，返回 false。
		updated, err := s.Repo.DeductWithVersion(ctx, productID, 1, stock.Version)
		if err != nil {
			return err
		}
		if updated {
			return nil // CAS 成功，扣减完成
		}

		// CAS 失败：version 已被其他 goroutine 更新，短暂退避后重试。
		// 【面试点】退避策略：固定间隔 vs 指数退避；秒杀通常用短固定间隔。
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}

	return domain.ErrConflict // 超过最大重试次数
}

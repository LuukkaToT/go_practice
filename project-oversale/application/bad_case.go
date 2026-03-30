// Package application 秒杀应用服务层：编排领域逻辑 + 基础设施端口，不含 SQL/Redis 细节。
package application

import (
	"context"
	"time"

	"gopractice/project-oversale/domain"
	"gopractice/project-oversale/ports"
)

// ======================================================================
// BadCase：读 → 判断 → 写（三步非原子，高并发必超卖）
// ======================================================================

// BadCaseSvc 演示超卖根因。
//
// 【面试必背根因】
// 以下 Purchase 方法包含三个独立操作：
//   1. GetStock（SELECT）
//   2. 应用层 if 判断
//   3. DeductDirect（UPDATE stock = stock - 1）
//
// 在并发场景中：
//   - Goroutine A 和 B 同时执行 Step1，都读到 stock=1
//   - A 和 B 都通过 Step2 的判断（1 > 0）
//   - A 先执行 Step3，stock 变为 0
//   - B 再执行 Step3，stock 变为 -1 ← 超卖！
//
// 【可能问】「如果 MySQL 的 UPDATE 不会出现脏写，为什么还会超卖？」
// 【答】UPDATE 本身不会脏写（InnoDB 行锁），但问题在「读」和「写」之间没有锁。
// 两个事务读到相同的旧值，都认为库存充足，都执行了 UPDATE，导致总扣减量 > 实际库存。
type BadCaseSvc struct {
	Repo ports.StockRepository
}

// Purchase 超卖版本：非原子的读-判断-写。
func (s *BadCaseSvc) Purchase(ctx context.Context, productID int64) error {
	// Step 1: 读库存（SELECT）
	stock, err := s.Repo.GetStock(ctx, productID)
	if err != nil {
		return err
	}

	// 【并发窗口】此处多个 goroutine 同时持有相同的旧 stock 值，
	// 下面的判断对它们全部成立，都会继续执行 Step 3。
	// 加一个人为延迟可以让测试中超卖更容易复现（模拟业务逻辑耗时）。
	time.Sleep(time.Millisecond)

	// Step 2: 应用层判断（非原子！）
	if stock.Available <= 0 {
		return domain.ErrOutOfStock
	}

	// Step 3: 扣减（UPDATE stock = stock - 1，无版本保护）
	// 此时 stock 已可能被其他 goroutine 扣完甚至为负。
	return s.Repo.DeductDirect(ctx, productID, 1)
}

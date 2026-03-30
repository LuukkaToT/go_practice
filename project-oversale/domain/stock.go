// Package domain 秒杀超卖场景领域层：只表达业务规则，不依赖任何基础设施。
//
// 【DDD精简原则】领域层 = 聚合根 + 不变量 + 领域错误；零第三方import，可纯单测。
// 【可能问】「为什么库存校验放 Domain 而不放 Service？」
// 【答】库存 > 0 才能扣减是业务不变量，放 Domain 可在任意切入点（Service/测试/CLI）复用且不可绕过。
package domain

import "errors"

// 【领域错误】面试考点：为何用哨兵 error 而不是 string？
// 【答】上层可用 errors.Is 精确判断类型，便于 HTTP 层映射状态码，不依赖字符串比较。
var (
	ErrOutOfStock = errors.New("oversale: out of stock")
	// ErrConflict 表示并发写冲突（乐观锁/Lua扣减失败），上层按需重试。
	ErrConflict = errors.New("oversale: concurrent conflict, retry")
)

// Stock 库存聚合根。
//
// 【知识点 | 聚合根】库存的所有变更必须经由 Deduct/Restore 方法，
// 外部不能直接改 Available，保证「库存 >= 0」不变量在领域层内闭合。
//
// 【可能问】「Version 字段为何在领域层而不在 infra？」
// 【答】Version 是乐观锁的业务语义（「这一版本的库存」），属于领域概念；
// 具体 SQL 的 WHERE version=? 是基础设施实现细节，分层清晰。
type Stock struct {
	ProductID int64
	Available int64
	Version   int64 // 乐观锁版本号；每次 CAS 更新时 +1
}

// Deduct 在本地做不变量校验并扣减（用于乐观锁场景：先校验再持久化）。
//
// 【注意】此方法仅修改内存中的 Stock；实际落库由 Repository 负责。
// 【可能问】「领域方法 Deduct 和 Repository.DeductDirect 有何区别？」
// 【答】Deduct = 纯领域规则；DeductDirect = 直接执行 SQL，无乐观锁保护。
// 高并发时 DeductDirect 非原子，会超卖；Deduct+乐观锁才是安全路径。
func (s *Stock) Deduct(qty int64) error {
	if qty <= 0 {
		return errors.New("oversale: qty must be positive")
	}
	if s.Available < qty {
		return ErrOutOfStock
	}
	s.Available -= qty
	return nil
}

// Restore 恢复库存（退款/补偿场景）。
func (s *Stock) Restore(qty int64) {
	s.Available += qty
}

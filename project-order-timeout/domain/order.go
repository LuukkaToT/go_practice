// Package domain 订单超时场景领域层：只表达业务规则，无基础设施依赖。
//
// 【DDD精简原则】领域层 = 聚合根 + 状态机 + 领域错误；零第三方import，可纯单测。
// 【可能问】「订单状态机为什么放 Domain 而不是 Service？」
// 【答】状态流转是纯业务规则（Pending→Cancelled 合法，Paid→Cancelled 非法）；
// 放 Domain 后任何调用入口（HTTP/MQ消费者/定时任务）都无法绕过，且可纯测。
package domain

import (
	"errors"
	"time"
)

// OrderStatus 订单状态枚举（字符串类型便于 DB 存储和日志可读性）。
//
// 【面试点 | 状态机设计】用 string 而非 int：
//   - 优点：可读性强，DB 中可直接查看语义，不怕枚举值顺序变化
//   - 缺点：比较略慢（但订单场景完全可接受）
type OrderStatus string

const (
	OrderStatusPending   OrderStatus = "pending"   // 待支付
	OrderStatusPaid      OrderStatus = "paid"       // 已支付
	OrderStatusCancelled OrderStatus = "cancelled"  // 已取消（超时或手动）
)

// 领域错误（应用层可 errors.Is 精确判断）。
//
// 【可能问】「为什么用哨兵 error 变量而不是 error string？」
// 【答】errors.Is 可以跨调用栈精确匹配类型，不依赖字符串；
// 上层可按错误类型映射 HTTP 状态码（NotCancellable→409，NotFound→404）。
var (
	ErrOrderNotFound      = errors.New("order-timeout: order not found")
	ErrNotCancellable     = errors.New("order-timeout: order cannot be cancelled in current status")
	ErrAlreadyPaid        = errors.New("order-timeout: order already paid")
)

// Order 订单聚合根。
//
// 【知识点 | 聚合根】外部只能通过领域方法（Cancel/Pay）修改状态；
// 不允许直接赋值 Status 字段，保证状态机不变量在领域层内闭合。
//
// 【面试点】「为什么不直接在 Handler 里写 order.Status = 'cancelled'？」
// 【答】直接赋值绕过了状态机校验，可能产生「支付后又被取消」等非法状态。
// 领域方法是唯一合法的状态变更入口。
type Order struct {
	ID        int64
	UserID    int64
	Amount    int64       // 单位：分
	Status    OrderStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Cancel 取消订单（超时或手动）。
//
// 【状态机规则】只有 Pending 状态的订单可以取消。
// 已支付订单取消需走退款流程（领域边界不同，此处返回错误由上层处理）。
// 已取消的订单重复取消返回 ErrNotCancellable（幂等保护第三层，领域层兜底）。
func (o *Order) Cancel() error {
	if o.Status != OrderStatusPending {
		return ErrNotCancellable
	}
	o.Status = OrderStatusCancelled
	o.UpdatedAt = time.Now().UTC()
	return nil
}

// Pay 标记支付成功。
func (o *Order) Pay() error {
	if o.Status != OrderStatusPending {
		if o.Status == OrderStatusCancelled {
			return ErrNotCancellable // 已取消订单无法支付
		}
		return ErrAlreadyPaid
	}
	o.Status = OrderStatusPaid
	o.UpdatedAt = time.Now().UTC()
	return nil
}

// IsCancellable 判断是否可取消（供上层提前判断，避免无效 DB 写）。
func (o *Order) IsCancellable() bool {
	return o.Status == OrderStatusPending
}

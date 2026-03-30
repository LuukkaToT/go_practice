// Package domain 定义直播打赏的核心业务概念，不依赖任何外部框架或基础设施。
//
// 【面试点 | DDD 领域层原则】
// 领域层只包含：聚合根、值对象、领域错误、工厂方法。
// 禁止 import gorm / redis / amqp 等基础设施包；领域规则的测试应该在不启动任何服务的情况下运行。
package domain

import (
	"errors"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// 领域错误
// ─────────────────────────────────────────────────────────────────────────────

var (
	// ErrInvalidDonation 表示打赏参数不合法（金额<=0 / 缺少房间/用户/主播 ID）。
	ErrInvalidDonation = errors.New("invalid donation: amount must be > 0 and IDs must be set")

	// ErrInsufficientBalance 表示用户余额不足以完成本次打赏。
	//
	// 【可能问】「余额不足应该在哪一层处理？」
	// 【答】判断逻辑在 Redis Lua 脚本（原子性），但错误语义属于领域层定义。
	// 应用层捕获 Lua 返回码后映射到此错误，供上层（HTTP handler）转换为 4xx 响应。
	ErrInsufficientBalance = errors.New("insufficient balance")

	// ErrWalletNotLoaded 表示用户钱包尚未预加载到 Redis。
	//
	// 【可能问】「Redis 里没有余额 Key 怎么处理？」
	// 【答】生产中有两种策略：
	//   1. Lua 返回"未加载"错误码，应用层触发 LoadBalance 将 DB 余额写入 Redis，再重试。
	//   2. 启动时/用户登录时预热所有活跃用户钱包，避免冷启动打赏失败。
	//   本 Demo 采用策略 1（按需加载），在测试 setup 阶段预热所有测试用户。
	ErrWalletNotLoaded = errors.New("wallet not loaded in redis, please preload first")
)

// ─────────────────────────────────────────────────────────────────────────────
// Donation 聚合根
// ─────────────────────────────────────────────────────────────────────────────

// Donation 代表一次直播打赏行为（送礼物）。
//
// 【面试点 | 聚合根职责】
// 聚合根是事务一致性的边界。Donation 封装打赏的核心属性，并通过 NewDonation 工厂
// 保证所有创建路径都经过不变式校验，外部不能绕过校验直接构造非法对象。
type Donation struct {
	ID         int64     // 数据库自增 ID（0 表示尚未持久化）
	UserID     int64     // 送礼用户 ID
	StreamerID int64     // 主播 ID
	RoomID     string    // 直播间 ID（字符串便于扩展为 UUID）
	Amount     int64     // 打赏金额，单位：分（虚拟币最小单位）
	CreatedAt  time.Time // 打赏时间
}

// NewDonation 工厂方法：校验参数后创建 Donation 聚合根。
//
// 【面试点 | 工厂方法 vs 直接 struct 赋值】
// 工厂方法是 DDD 中保证聚合不变式（Invariant）的惯用手段。
// 任何违反业务规则的输入都应在此处被拒绝，不让非法状态进入系统。
func NewDonation(userID, streamerID int64, roomID string, amount int64) (*Donation, error) {
	if userID <= 0 || streamerID <= 0 || roomID == "" || amount <= 0 {
		return nil, ErrInvalidDonation
	}
	return &Donation{
		UserID:     userID,
		StreamerID: streamerID,
		RoomID:     roomID,
		Amount:     amount,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

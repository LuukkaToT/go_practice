// Package ports 定义秒杀场景的端口（依赖倒置接口）。
//
// 【知识点 | 依赖倒置】Application 层只依赖这里的 interface；
// 具体实现（MySQL/Redis）在 infra 包，切换存储无需改 Application/Domain。
// 【可能问】「端口接口放哪个包最合理？」
// 【答】业界分歧：可放 domain、application 或独立 ports 包；
// 关键是 Application 层不能 import infra；本项目独立 ports 包最清晰。
package ports

import (
	"context"
	"time"

	"gopractice/project-oversale/domain"
)

// StockRepository MySQL 侧库存仓库接口。
//
// 【知识点 | 三种扣减对比】
//   - DeductDirect：UPDATE stock = stock - qty（非原子读-判断-写的 infra 版本，BadCase 演示用）
//   - DeductWithVersion：UPDATE ... WHERE version = ?（CAS 乐观锁，affected=0 表示冲突）
//
// 【可能问】「悲观锁（SELECT FOR UPDATE）vs 乐观锁怎么选？」
// 【答】悲观锁：锁等待，写多冲突多时吞吐高；乐观锁：无锁等待，冲突少时高效但需重试逻辑。
// 秒杀流量峰值冲突极高，两者都有局限；Redis Lua 是更常见的工业解。
type StockRepository interface {
	// GetStock 查询当前库存（含 Version）。
	GetStock(ctx context.Context, productID int64) (*domain.Stock, error)

	// DeductDirect 直接 UPDATE stock -= qty，无 version 校验。
	// 【BadCase专用】非原子语义，高并发必超卖，仅用于演示根因。
	DeductDirect(ctx context.Context, productID, qty int64) error

	// DeductWithVersion CAS 乐观锁更新：WHERE stock >= qty AND version = ?
	// 返回 true 表示更新成功；false 表示并发冲突（version 已变），需上层重试。
	DeductWithVersion(ctx context.Context, productID, qty, version int64) (bool, error)

	// ResetStock 重置库存（测试用）。
	ResetStock(ctx context.Context, productID, stock int64) error

	// CurrentStock 查询当前库存数值（测试断言用）。
	CurrentStock(ctx context.Context, productID int64) (int64, error)
}

// StockCache Redis 侧库存缓存接口。
//
// 【知识点 | 为何 Redis 能解决超卖？】
// Redis 单线程执行命令，Lua 脚本在一次 EVALSHA 中原子完成「读-判断-扣」，
// 不存在并发竞争窗口，是高并发秒杀的工业标准解法。
type StockCache interface {
	// InitStock 初始化 Redis 库存 key（测试前 setup / 系统预热时调用）。
	InitStock(ctx context.Context, productID, qty int64) error

	// DeductWithLua 原子扣减：Lua 脚本内完成读-判断-DECRBY。
	// 返回 true=成功；false=库存不足；error=Redis 异常或 key 未初始化。
	DeductWithLua(ctx context.Context, productID, qty int64) (bool, error)

	// TryLock 尝试获取分布式锁（SETNX）。
	// value 为调用方唯一 token，用于释放时校验所有权，防误删。
	// 【可能问】「为何 value 要唯一？」
	// 【答】若进程 A 持锁超时，锁被 Redis 自动删除，进程 B 拿到锁；
	// 此时若 A 恢复并尝试释放，若 value 不唯一会误删 B 的锁，导致锁失效。
	TryLock(ctx context.Context, key, value string, ttl time.Duration) (bool, error)

	// ReleaseLock 释放分布式锁（Lua 原子校验 value 再 DEL）。
	ReleaseLock(ctx context.Context, key, value string) error

	// GetStock 查询 Redis 中的当前库存（测试断言用）。
	GetStock(ctx context.Context, productID int64) (int64, error)
}

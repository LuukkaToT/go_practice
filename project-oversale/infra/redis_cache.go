package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// key 前缀规范：oversale:stock:{productID}
// 【可能问】「Redis key 命名规范？」
// 【答】业务:实体:ID，冒号分隔；便于 SCAN 模式匹配和 key 归类监控。
func stockKey(productID int64) string {
	return fmt.Sprintf("oversale:stock:%d", productID)
}

// ======================================================================
// Lua 脚本常量
// ======================================================================

// luaDeduct Redis Lua 原子扣减脚本。
//
// 【逐行解释（面试口述用）】
//
//	KEYS[1]  = stockKey（oversale:stock:{id}）
//	ARGV[1]  = 扣减数量（qty）
//
//	1. GET 库存值（string 转 number）
//	2. key 不存在（nil）→ 返回 -1，表示未初始化
//	3. 库存 < qty → 返回 0，表示不足
//	4. DECRBY 原子扣减 → 返回 1，表示成功
//
// 【为何不用 DECR + 回滚？】
// DECR 先扣再判断，扣成功后如果库存变负需要 INCR 回滚，两步仍非原子；
// Lua 把判断和扣减合在一步，才是真正原子。
const luaDeduct = `
local stock = tonumber(redis.call('GET', KEYS[1]))
if not stock then return -1 end
if stock < tonumber(ARGV[1]) then return 0 end
redis.call('DECRBY', KEYS[1], ARGV[1])
return 1
`

// luaReleaseLock 释放分布式锁 Lua 脚本。
//
// 【为什么释放锁也要用 Lua？】
// GET + DEL 两步非原子：GET 返回 token 匹配后、DEL 之前若锁刚好超时被自动删除，
// 并被新持有者 SETNX，此时 DEL 会误删新锁。
// Lua 将 GET + DEL 合并为原子操作，彻底消除这个窗口。
const luaReleaseLock = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('DEL', KEYS[1])
else
    return 0
end
`

// ======================================================================
// RedisStockCache 实现 ports.StockCache
// ======================================================================

// RedisStockCache 封装 Redis 库存操作和分布式锁。
type RedisStockCache struct {
	RDB *redis.Client
}

// InitStock 初始化（或重置）Redis 库存 key。
// 【测试 setup 和系统预热时调用】
func (c *RedisStockCache) InitStock(ctx context.Context, productID, qty int64) error {
	return c.RDB.Set(ctx, stockKey(productID), qty, 0).Err()
}

// DeductWithLua 原子扣减库存。
//
// 返回值：
//   - true, nil  → 扣减成功
//   - false, nil → 库存不足
//   - false, err → Redis 异常或 key 未初始化（返回 -1 时作为 error 处理）
func (c *RedisStockCache) DeductWithLua(ctx context.Context, productID, qty int64) (bool, error) {
	result, err := c.RDB.Eval(ctx, luaDeduct, []string{stockKey(productID)}, qty).Int64()
	if err != nil {
		return false, fmt.Errorf("redis lua deduct: %w", err)
	}
	switch result {
	case 1:
		return true, nil
	case 0:
		return false, nil // 库存不足
	default:
		// result == -1：key 不存在，未初始化
		return false, errors.New("oversale: redis stock key not initialized")
	}
}

// TryLock 尝试获取分布式锁（SET NX PX）。
//
// 【实现细节】
// SET key value NX PX ttl_ms：一条命令原子设置值 + 过期时间。
// 旧版 SETNX + EXPIRE 两步非原子，宕机可能导致锁永不过期（死锁）。
func (c *RedisStockCache) TryLock(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	ok, err := c.RDB.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx: %w", err)
	}
	return ok, nil
}

// ReleaseLock 原子释放分布式锁（Lua：先校验 value 再 DEL）。
func (c *RedisStockCache) ReleaseLock(ctx context.Context, key, value string) error {
	result, err := c.RDB.Eval(ctx, luaReleaseLock, []string{key}, value).Int64()
	if err != nil {
		return fmt.Errorf("redis release lock: %w", err)
	}
	if result == 0 {
		// token 不匹配：锁已超时被他人持有，或已被释放。
		// 通常记录日志即可，不视为严重错误。
		return nil
	}
	return nil
}

// GetStock 查询 Redis 中的当前库存（测试断言用）。
func (c *RedisStockCache) GetStock(ctx context.Context, productID int64) (int64, error) {
	val, err := c.RDB.Get(ctx, stockKey(productID)).Int64()
	if err != nil {
		return 0, err
	}
	return val, nil
}

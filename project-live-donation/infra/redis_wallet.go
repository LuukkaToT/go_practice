package infra

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"gopractice/project-live-donation/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// Redis Key 命名规范
// ─────────────────────────────────────────────────────────────────────────────

// walletKey 生成用户钱包的 Redis Key。
// 格式：ld:wallet:user:{userID}
//
// 【面试点 | Redis Key 设计原则】
// 1. 业务前缀（ld:）：便于 SCAN 按模块查找，也便于设置不同的淘汰策略
// 2. 层级分隔符（:）：便于 Redis Commander 等可视化工具展示树形结构
// 3. 避免过长 Key（节省内存）：用 ID 而非 nickname
func walletKey(userID int64) string {
	return fmt.Sprintf("ld:wallet:user:%d", userID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Lua 脚本：原子校验并扣减余额
// ─────────────────────────────────────────────────────────────────────────────

// deductLua 是核心 Lua 脚本，保证"余额校验 + 扣减"的原子性。
//
// 【面试点 | 为什么用 Lua 脚本而不是 Redis 事务（MULTI/EXEC）？】
// MULTI/EXEC 是乐观锁（WATCH + MULTI/EXEC），步骤：
//   1. WATCH ld:wallet:user:1
//   2. GET ld:wallet:user:1（读余额）
//   3. if balance >= amount { MULTI; DECRBY; EXEC }
// 问题：步骤 2→3 是两次网络往返，并发高时 WATCH 频繁冲突导致大量重试。
//
// Lua 脚本在 Redis 单线程中原子执行，等价于：
//   if GET(key) >= amount { SET(key, balance-amount) }
// 一次网络往返，无并发冲突，适合高频打赏场景。
//
// 【面试点 | Redis 单线程与 Lua 原子性的关系】
// Redis 使用单线程（6.0 后 I/O 多线程但命令执行仍单线程）处理命令。
// Lua 脚本在执行期间不会被其他命令打断（类似 Serializable 隔离级别），
// 因此脚本内的 GET + SET 是原子的，不存在 TOCTOU（检查时间/使用时间）竞争。
//
// 返回值：[]int64{code, newBalance}
//   code=0  → 成功，newBalance 为扣减后的余额
//   code=-1 → Key 不存在（钱包未预加载）
//   code=-2 → 余额不足，newBalance 为当前余额
var deductLua = redis.NewScript(`
local key = KEYS[1]
local amount = tonumber(ARGV[1])

local bal = tonumber(redis.call('GET', key))
if not bal then
    return {-1, 0}
end

if bal < amount then
    return {-2, bal}
end

local newBal = bal - amount
redis.call('SET', key, newBal)
return {0, newBal}
`)

// ─────────────────────────────────────────────────────────────────────────────
// RedisWalletStore：实现 ports.WalletStore
// ─────────────────────────────────────────────────────────────────────────────

// RedisWalletStore 使用 Redis String 存储用户余额，通过 Lua 脚本原子扣减。
type RedisWalletStore struct {
	rdb *redis.Client
}

func NewRedisWalletStore(rdb *redis.Client) *RedisWalletStore {
	return &RedisWalletStore{rdb: rdb}
}

// LoadBalance 将用户余额预加载到 Redis（从 DB 读取后调用此方法）。
//
// 【面试点 | 预热策略】
// 生产场景下有两种预热时机：
//   1. 用户登录时：登录即预热，保证活跃用户钱包始终在 Redis 中。
//   2. 按需加载：首次打赏时触发，Lua 返回 -1 后应用层回源 DB 然后重试。
// 本 Demo 采用测试 setup 阶段批量预热，模拟策略 1。
func (s *RedisWalletStore) LoadBalance(_ context.Context, userID int64, balance int64) error {
	return s.rdb.Set(context.Background(), walletKey(userID), balance, 0).Err()
}

// Deduct 原子扣减用户余额。
//
// 【面试点 | 扣款失败的处理链路】
// code=-1 → ErrWalletNotLoaded → 应用层触发 LoadBalance 后重试（本 Demo 测试前预热）
// code=-2 → ErrInsufficientBalance → 直接返回 4xx，告知用户余额不足
func (s *RedisWalletStore) Deduct(ctx context.Context, userID int64, amount int64) error {
	result, err := deductLua.Run(ctx, s.rdb,
		[]string{walletKey(userID)},
		amount,
	).Int64Slice()
	if err != nil {
		return fmt.Errorf("wallet deduct lua error: %w", err)
	}

	code := result[0]
	switch code {
	case 0:
		return nil
	case -1:
		return domain.ErrWalletNotLoaded
	case -2:
		return domain.ErrInsufficientBalance
	default:
		return fmt.Errorf("wallet deduct unexpected code: %d", code)
	}
}

// GetBalance 获取 Redis 中的当前余额（测试验证用）。
func (s *RedisWalletStore) GetBalance(ctx context.Context, userID int64) (int64, error) {
	val, err := s.rdb.Get(ctx, walletKey(userID)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, domain.ErrWalletNotLoaded
		}
		return 0, err
	}
	return val, nil
}

// DeleteWalletKeys 删除测试用的钱包 Key（测试 teardown 使用）。
func DeleteWalletKeys(ctx context.Context, rdb *redis.Client, userIDs []int64) error {
	keys := make([]string, len(userIDs))
	for i, uid := range userIDs {
		keys[i] = walletKey(uid)
	}
	return rdb.Del(ctx, keys...).Err()
}

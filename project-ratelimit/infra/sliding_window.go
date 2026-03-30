package infra

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"

	"gopractice/project-ratelimit/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisSlidingWindowLimiter：滑动窗口限流（GoodCase 实现）
// ─────────────────────────────────────────────────────────────────────────────

// slidingWindowLua 滑动窗口 Lua 脚本（原子执行，无竞态问题）。
//
// 【Lua 脚本执行流程】
//  1. 清理窗口外的旧记录（ZREMRANGEBYSCORE ... -inf windowStart）
//  2. 统计当前窗口内的请求数（ZCARD）
//  3. 超阈值则拒绝（return 0）
//  4. 允许则记录本次请求（ZADD score=now member=唯一ID）
//  5. 重置 key TTL 避免内存泄漏（PEXPIRE）
//
// KEYS[1] = Redis Key
// ARGV[1] = 窗口大小（ms）
// ARGV[2] = 阈值（最大请求数）
// ARGV[3] = 当前时间戳（ms）
// ARGV[4] = 唯一成员 ID（"timestamp-random"，解决同毫秒多请求的 ZADD 覆盖问题）
//
// 【可能问】「ZADD 的 member 为什么要唯一？」
// 【答】ZSET 中 member 必须唯一；若两个请求使用相同 member，第二次 ZADD 只会更新
// score 而不是新增记录，导致计数器偏低，限流失效。
// 用 "timestamp_ms-random" 确保每个请求有独立 member。
//
// 【可能问】「为什么用 ZSET 而不是 List？」
// 【答】ZSET 可以用 ZREMRANGEBYSCORE 按 score 范围批量删除（O(log N + M)），
// 高效清理过期记录；List 只能线性遍历（O(N)），窗口越大越慢。
var slidingWindowLua = redis.NewScript(`
local key        = KEYS[1]
local windowMS   = tonumber(ARGV[1])
local limit      = tonumber(ARGV[2])
local nowMS      = tonumber(ARGV[3])
local memberID   = ARGV[4]

-- Step 1: 清理窗口起点之前的所有记录（过期数据）
-- windowStart 是当前滑动窗口的左边界（不含）
local windowStart = nowMS - windowMS
redis.call('ZREMRANGEBYSCORE', key, '-inf', windowStart)

-- Step 2: 统计当前窗口内的请求总数
local count = tonumber(redis.call('ZCARD', key))

-- Step 3: 超过阈值，拒绝
if count >= limit then
    return 0
end

-- Step 4: 记录本次请求（score=时间戳，member=唯一ID）
redis.call('ZADD', key, nowMS, memberID)

-- Step 5: 设置 key TTL（窗口大小），避免无请求时 key 永驻内存
-- 每次请求都刷新 TTL，最后一次请求后窗口大小时间内自动过期
redis.call('PEXPIRE', key, windowMS)

return 1
`)

// RedisSlidingWindowLimiter 使用 Redis ZSET + Lua 脚本实现滑动窗口限流。
//
// 【滑动窗口 vs 固定窗口核心区别】
// 固定窗口：窗口按固定时间点切割（0s-1s, 1s-2s...），存在边界突刺
// 滑动窗口：以"当前时刻"为终点，向前看 window 时间（如过去 1s），无切割点
//
// 代价：每次请求需要 ZREMRANGEBYSCORE + ZCARD + ZADD，比 INCR 多 2 步；
// 但都是 O(log N)，N 为窗口内请求数（通常很小），性能影响可忽略。
//
// 【可能问】「滑动窗口有什么缺点？」
// 【答】内存占用：每个请求在 ZSET 中占一条记录（member=~20B + score=8B）；
// 若 limit=10000/s，每个 key 最多 10000 条记录，约 280KB，可接受。
// 极高并发（百万 QPS）时建议用令牌桶（Token Bucket）+ 本地缓存实现。
type RedisSlidingWindowLimiter struct {
	rdb *redis.Client
}

// NewRedisSlidingWindowLimiter 构造滑动窗口限流器。
func NewRedisSlidingWindowLimiter(rdb *redis.Client) *RedisSlidingWindowLimiter {
	return &RedisSlidingWindowLimiter{rdb: rdb}
}

// Allow 滑动窗口限流判断（原子执行，无边界突刺）。
func (l *RedisSlidingWindowLimiter) Allow(ctx context.Context, cfg domain.RateLimit, clientID string) (bool, error) {
	key := cfg.RedisKey(clientID)
	nowMs := nowMillis()
	// 唯一成员 ID：时间戳(ms) + 随机数，确保同毫秒多请求各自独立记录
	memberID := fmt.Sprintf("%d-%d", nowMs, rand.Int63())

	result, err := slidingWindowLua.Run(ctx, l.rdb,
		[]string{key},
		cfg.WindowMS,      // ARGV[1]
		cfg.Limit,         // ARGV[2]
		nowMs,             // ARGV[3]
		memberID,          // ARGV[4]
	).Int64()
	if err != nil {
		return false, fmt.Errorf("sliding-window: lua script failed: %w", err)
	}

	allowed := result == 1
	if !allowed {
		fmt.Printf("[SlidingWindow] 🚫 限流拒绝 key=%s windowMS=%d limit=%d\n",
			key, cfg.WindowMS, cfg.Limit)
	}
	return allowed, nil
}

// nowMillis 返回当前时间戳（毫秒），测试时可通过注入替换（此处为简化版）。
func nowMillis() int64 {
	return time.Now().UnixMilli()
}

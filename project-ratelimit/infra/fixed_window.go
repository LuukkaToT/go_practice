// Package infra 基础设施层（适配器）：实现 ports.Limiter 接口。
package infra

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"gopractice/project-ratelimit/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisFixedWindowLimiter：固定窗口限流（BadCase 实现）
// ─────────────────────────────────────────────────────────────────────────────

// RedisFixedWindowLimiter 使用 Redis INCR + EXPIRE 实现固定窗口限流。
//
// 【知识点 | 固定窗口算法】
// 每个时间窗口（如 1s）内维护一个计数器，超过阈值则拒绝。
// 实现简单，Redis 中 key 格式：ratelimit:{prefix}:{clientID}:{window_index}
// window_index = now_ms / window_ms（当前时间戳整除窗口大小）
//
// 【核心缺陷 — 边界突刺（Boundary Burst）】
//
//	时间轴:  0s ──────── 1s ──────── 2s
//	          |    W1    |    W2    |
//	                  ↑          ↑
//	        t=0.99s: 发 limit 个请求（全部通过，W1 计数=limit）
//	        t=1.01s: 发 limit 个请求（全部通过，W2 计数=limit，key 已过期重置）
//	→ 在 [0.99s, 1.01s] 这 20ms 内，实际通过了 2×limit 个请求！
//
// 【第二个缺陷 — INCR+EXPIRE 非原子】
// 若 INCR 成功但进程在 EXPIRE 前崩溃，key 永不过期 → 永久限流（雪崩）。
// 修复方案：用 SET key 1 EX window NX（原子操作，但只能初始化不能递增）
// 或用 Lua 脚本（见 GoodCase）。
type RedisFixedWindowLimiter struct {
	rdb *redis.Client
}

// NewRedisFixedWindowLimiter 构造固定窗口限流器。
func NewRedisFixedWindowLimiter(rdb *redis.Client) *RedisFixedWindowLimiter {
	return &RedisFixedWindowLimiter{rdb: rdb}
}

// Allow 固定窗口限流判断。
//
// Key 格式：ratelimit:{prefix}:{clientID}:{window_index}
// window_index 每个窗口周期不同，自动隔离不同窗口的计数。
func (l *RedisFixedWindowLimiter) Allow(ctx context.Context, cfg domain.RateLimit, clientID string) (bool, error) {
	// 计算当前窗口索引（整除取整）
	// 同一窗口内所有请求的 windowIndex 相同 → 命中同一 key
	nowMs := nowMillis()
	windowIndex := nowMs / cfg.WindowMS
	key := fmt.Sprintf("%s:%d", cfg.RedisKey(clientID), windowIndex)

	// ☠️ 缺陷1：INCR 和 EXPIRE 是两个独立命令，非原子操作
	// 场景：高并发时多个 goroutine 同时执行 INCR，都得到 count=1，
	//        然后都执行 EXPIRE，问题不大；但若 INCR 后进程崩溃，EXPIRE 未执行，
	//        key 永久存在 → 该 clientID 被永久限流。
	count, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("fixed-window: INCR failed: %w", err)
	}

	if count == 1 {
		// 首次创建 key，设置过期时间（窗口大小 + 1s 缓冲，避免边界误差）
		// ☠️ 这里有竞态：INCR 成功 → 进程 kill → EXPIRE 未执行
		if err := l.rdb.PExpire(ctx, key, cfg.Window()).Err(); err != nil {
			// 仅记录错误，不影响当次判断（但 key 将永不过期）
			fmt.Printf("[FixedWindow] ⚠️ PEXPIRE 失败 key=%s err=%v（key 将永不过期！）\n", key, err)
		}
	}

	allowed := count <= cfg.Limit
	if !allowed {
		fmt.Printf("[FixedWindow] 🚫 限流拒绝 key=%s count=%d limit=%d\n", key, count, cfg.Limit)
	}
	return allowed, nil
}

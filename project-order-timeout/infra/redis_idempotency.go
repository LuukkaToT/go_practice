package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisIdempotencyChecker：实现 ports.IdempotencyChecker
// ─────────────────────────────────────────────────────────────────────────────

// idempotencyTTL 幂等 Key 过期时间。
//
// 【选择 24h 的理由】
// RabbitMQ 消息重复投递的时间窗口通常在分钟到小时级别（网络抖动、Consumer 重启）；
// 24h 覆盖了绝大多数重复场景，过期后 Key 自动释放不占内存。
// 若业务有更长的重复窗口（如跨天重试），可适当延长。
const idempotencyTTL = 24 * time.Hour

// RedisIdempotencyChecker 使用 Redis SETNX 实现幂等检查。
//
// 【知识点 | SETNX + TTL 原子性】
// 旧版：SET key value NX → 再 EXPIRE key ttl（两步非原子，有竞态窗口）
// 新版：SET key value NX EX seconds（单命令原子）→ go-redis 的 SetNX 实现此语义
// 必须使用 SetNX（SET ... NX PX）而不是先 GET 再 SET，否则并发时两个 goroutine
// 可能都 GET 到不存在，然后都 SET 成功，幂等失效。
//
// 【可能问】「Redis 集群环境下 SETNX 还能保证幂等吗？」
// 【答】单节点 Redis / Redis Cluster 中，单个 Key 只在一个主节点，SETNX 原子安全；
// Redis Sentinel 主从切换时有极短窗口可能出现双主（脑裂），此时 MySQL 唯一索引是兜底。
type RedisIdempotencyChecker struct {
	rdb *redis.Client
}

// NewRedisIdempotencyChecker 构造 RedisIdempotencyChecker。
func NewRedisIdempotencyChecker(rdb *redis.Client) *RedisIdempotencyChecker {
	return &RedisIdempotencyChecker{rdb: rdb}
}

// TryAcquire 尝试标记 msgID 为已处理（SETNX）。
// 返回 true：首次处理，可继续业务逻辑；返回 false：已处理，直接 Ack 跳过。
func (c *RedisIdempotencyChecker) TryAcquire(ctx context.Context, msgID string) (bool, error) {
	key := idempotencyKey(msgID)
	// SET key "1" NX EX 86400（原子：仅 Key 不存在时才设置）
	ok, err := c.rdb.SetNX(ctx, key, "1", idempotencyTTL).Result()
	if err != nil {
		return false, fmt.Errorf("RedisIdempotencyChecker.TryAcquire: %w", err)
	}
	// ok=true → SETNX 成功（首次）；ok=false → Key 已存在（重复）
	return ok, nil
}

// Release 释放幂等 Key（业务失败时调用，允许下次重试通过第1层）。
//
// 【注意】只在"业务逻辑失败且需要重试"时调用 Release；
// 不要在消费成功后调用（会导致第2次消费重新通过幂等检查）。
func (c *RedisIdempotencyChecker) Release(ctx context.Context, msgID string) error {
	key := idempotencyKey(msgID)
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("RedisIdempotencyChecker.Release: %w", err)
	}
	return nil
}

// idempotencyKey 生成幂等 Key（命名规范：前缀:业务类型:业务ID）。
//
// 【知识点 | Redis Key 命名规范】
// 推荐：业务前缀:实体:ID，冒号分隔（RedisInsight 可以树状展示）
// 避免过长 Key（>100B）和过短 Key（无语义）
func idempotencyKey(msgID string) string {
	return fmt.Sprintf("idempotent:timeout:%s", msgID)
}

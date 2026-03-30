package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gopractice/project-cache-guard/domain"

	"github.com/redis/go-redis/v9"
)

// nullSentinel 空值占位标记（存入 Redis 表示"此 ID 不存在"）。
//
// 【可能问】「空值缓存的 TTL 为何要比正常数据短？」
// 【答】如果商品后来真的被创建了，短 TTL 能更快让缓存自然失效（或主动删除）；
// 太长的空值 TTL 会导致新商品长时间不可见。通常设 1~5 分钟。
const nullSentinel = "__null__"

func productCacheKey(id int64) string {
	// 【面试点 | key 命名规范】业务:实体:ID，冒号分隔，便于 SCAN 模式匹配。
	return fmt.Sprintf("cache:product:%d", id)
}

// RedisProductCache 实现 ports.ProductCache。
type RedisProductCache struct {
	RDB *redis.Client
}

// Get 查询缓存。返回值语义见 ports.ProductCache 注释。
func (c *RedisProductCache) Get(ctx context.Context, id int64) (*domain.Product, bool, error) {
	val, err := c.RDB.Get(ctx, productCacheKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil // cache miss
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis get product %d: %w", id, err)
	}
	if val == nullSentinel {
		return nil, true, nil // 空值占位：商品不存在
	}
	var p domain.Product
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return nil, false, fmt.Errorf("unmarshal product %d: %w", id, err)
	}
	return &p, true, nil
}

// Set 写入商品缓存（TTL 由调用方控制，GoodCase 会传随机TTL防雪崩）。
func (c *RedisProductCache) Set(ctx context.Context, p *domain.Product, ttl time.Duration) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.RDB.Set(ctx, productCacheKey(p.ID), data, ttl).Err()
}

// SetNull 写入空值占位（防穿透）。
func (c *RedisProductCache) SetNull(ctx context.Context, id int64, ttl time.Duration) error {
	return c.RDB.Set(ctx, productCacheKey(id), nullSentinel, ttl).Err()
}

// Delete 删除缓存（测试 setup / 主动失效时使用）。
func (c *RedisProductCache) Delete(ctx context.Context, id int64) error {
	return c.RDB.Del(ctx, productCacheKey(id)).Err()
}

// TTLOf 查询当前 key 的剩余 TTL（测试断言 / 观察随机TTL效果用）。
func (c *RedisProductCache) TTLOf(ctx context.Context, id int64) (time.Duration, error) {
	return c.RDB.TTL(ctx, productCacheKey(id)).Result()
}

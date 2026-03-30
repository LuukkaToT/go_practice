// Package infra 基础设施层（适配器）：Redis 点赞存储的两种实现。
package infra

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"gopractice/project-like-feed/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisSetLikeStore：Set 实现（BadCase — 内存重）
// ─────────────────────────────────────────────────────────────────────────────

// RedisSetLikeStore 使用 Redis Set（SADD）存储点赞用户 ID。
//
// 【内存分析 — Set 的代价】
// Redis Set 的内存编码：
//   - 元素数 ≤ 512 且元素都是整数时 → intset（紧凑，~8 字节/元素）
//   - 超过阈值后自动转为 hashtable（每个 entry ≈ 64 字节）
//
// 以 1000 万用户点赞为例（超出 intset 阈值，使用 hashtable）：
//   1000万 × 64 字节 ≈ 640 MB / 篇文章
//
// 对比 Bitmap：1000万 / 8 ≈ 1.25 MB / 篇文章  → 差距约 512 倍
//
// 【Set 的优点（面试时要客观分析）】
// SMEMBERS 可以枚举所有点赞用户（Bitmap 做不到）；
// 适合"显示最新 N 个点赞用户头像"的场景（取 Set 部分元素）。
type RedisSetLikeStore struct {
	rdb *redis.Client
}

// NewRedisSetLikeStore 构造 RedisSetLikeStore。
func NewRedisSetLikeStore(rdb *redis.Client) *RedisSetLikeStore {
	return &RedisSetLikeStore{rdb: rdb}
}

// setLikeKey 生成 Set 点赞 Key。
func setLikeKey(postID string) string {
	return fmt.Sprintf("like:set:%s", postID)
}

// Like 点赞（SADD）。
// SADD 是幂等的：重复添加同一 member 不会出错，返回值为实际新增数。
func (s *RedisSetLikeStore) Like(ctx context.Context, like domain.Like) error {
	key := setLikeKey(like.PostID)
	// SADD 返回新增的元素数；若 member 已存在返回 0（幂等安全）
	added, err := s.rdb.SAdd(ctx, key, like.UserID).Result()
	if err != nil {
		return fmt.Errorf("RedisSetLikeStore.Like: %w", err)
	}
	if added == 0 {
		return domain.ErrAlreadyLiked
	}
	return nil
}

// Unlike 取消点赞（SREM）。
func (s *RedisSetLikeStore) Unlike(ctx context.Context, like domain.Like) error {
	key := setLikeKey(like.PostID)
	removed, err := s.rdb.SRem(ctx, key, like.UserID).Result()
	if err != nil {
		return fmt.Errorf("RedisSetLikeStore.Unlike: %w", err)
	}
	if removed == 0 {
		return domain.ErrNotLiked
	}
	return nil
}

// IsLiked 查询用户是否已点赞（SISMEMBER）。
func (s *RedisSetLikeStore) IsLiked(ctx context.Context, userID int64, postID string) (bool, error) {
	key := setLikeKey(postID)
	result, err := s.rdb.SIsMember(ctx, key, userID).Result()
	if err != nil {
		return false, fmt.Errorf("RedisSetLikeStore.IsLiked: %w", err)
	}
	return result, nil
}

// Count 获取总点赞数（SCARD）。
//
// 【可能问】「SCARD 是 O(1) 吗？」
// 【答】是的，Redis Set 内部维护了元素计数，SCARD 直接返回，O(1)。
// 对比 Bitmap 的 BITCOUNT（O(N/8)），Set 的 SCARD 反而更快。
// 但 Set 的内存代价在高 DAU 下不可接受。
func (s *RedisSetLikeStore) Count(ctx context.Context, postID string) (int64, error) {
	key := setLikeKey(postID)
	count, err := s.rdb.SCard(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("RedisSetLikeStore.Count: %w", err)
	}
	return count, nil
}

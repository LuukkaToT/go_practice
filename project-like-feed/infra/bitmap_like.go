package infra

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"gopractice/project-like-feed/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisBitmapLikeStore：Bitmap 实现（GoodCase — 极致内存节省）
// ─────────────────────────────────────────────────────────────────────────────

// RedisBitmapLikeStore 使用 Redis Bitmap（SETBIT/GETBIT）存储点赞状态。
//
// 【知识点 | Redis Bitmap 本质】
// Bitmap 不是独立的数据类型，底层存储为 Redis String；
// SETBIT key offset value 本质上是对字符串的指定 bit 位进行置位。
// offset = userID，value = 1（点赞）/ 0（未点赞）。
//
// 【内存计算】
// Bitmap 的大小由最大 offset 决定，而非实际置位数：
//   maxUserID = 10,000,000（1千万）→ 占用 10,000,000 / 8 = 1,250,000 字节 ≈ 1.2 MB
//   maxUserID = 100,000,000（1亿） → 占用 100,000,000 / 8 = 12,500,000 字节 ≈ 12.2 MB
//
// 即使只有 1 个用户点赞，但 userID = 1亿，bitmap 也需要 12.2 MB！
// 这是 Bitmap 的 Trade-off：稀疏 ID 会造成大量空洞（0 bit 浪费）。
//
// 【可能问】「如果 userID 不连续（如 UUID），Bitmap 还适合吗？」
// 【答】不适合，UUID 是字符串无法作为 offset；即使用数字 UUID（int64），
// 最大值约 2^63，bitmap 需要 exabyte 级别存储，完全不可行。
// 解决方案：建立 userID → 连续序号的映射表，再用序号作为 offset。
type RedisBitmapLikeStore struct {
	rdb *redis.Client
}

// NewRedisBitmapLikeStore 构造 RedisBitmapLikeStore。
func NewRedisBitmapLikeStore(rdb *redis.Client) *RedisBitmapLikeStore {
	return &RedisBitmapLikeStore{rdb: rdb}
}

// bitmapLikeKey 生成 Bitmap 点赞 Key。
func bitmapLikeKey(postID string) string {
	return fmt.Sprintf("like:bitmap:%s", postID)
}

// Like 点赞（SETBIT offset 1）。
//
// 【知识点 | SETBIT 返回值】
// SETBIT 返回该 offset 的旧值（0 或 1）：
//   旧值 = 0 → 首次点赞（新增）
//   旧值 = 1 → 重复点赞（幂等，不重复计数）
func (s *RedisBitmapLikeStore) Like(ctx context.Context, like domain.Like) error {
	key := bitmapLikeKey(like.PostID)
	oldVal, err := s.rdb.SetBit(ctx, key, like.UserID, 1).Result()
	if err != nil {
		return fmt.Errorf("RedisBitmapLikeStore.Like: %w", err)
	}
	if oldVal == 1 {
		// 已经点过赞了（旧值为1），幂等处理
		return domain.ErrAlreadyLiked
	}
	return nil
}

// Unlike 取消点赞（SETBIT offset 0）。
func (s *RedisBitmapLikeStore) Unlike(ctx context.Context, like domain.Like) error {
	key := bitmapLikeKey(like.PostID)
	oldVal, err := s.rdb.SetBit(ctx, key, like.UserID, 0).Result()
	if err != nil {
		return fmt.Errorf("RedisBitmapLikeStore.Unlike: %w", err)
	}
	if oldVal == 0 {
		return domain.ErrNotLiked
	}
	return nil
}

// IsLiked 查询用户是否已点赞（GETBIT）。
//
// 【时间复杂度】O(1)，直接读取对应 bit 位。
func (s *RedisBitmapLikeStore) IsLiked(ctx context.Context, userID int64, postID string) (bool, error) {
	key := bitmapLikeKey(postID)
	val, err := s.rdb.GetBit(ctx, key, userID).Result()
	if err != nil {
		return false, fmt.Errorf("RedisBitmapLikeStore.IsLiked: %w", err)
	}
	return val == 1, nil
}

// Count 获取总点赞数（BITCOUNT）。
//
// 【知识点 | BITCOUNT 复杂度】
// BITCOUNT key：统计整个 Bitmap 中为 1 的 bit 数，时间复杂度 O(N/8)。
// N = bitmap 字节数 = maxUserID / 8。
// 1亿用户的 bitmap ≈ 12.5 MB，BITCOUNT 约需几毫秒（Redis 有硬件优化）。
//
// 【对比 SCARD（Set）】SCARD O(1)，但 Set 内存是 Bitmap 的 500 倍；
// 生产中 Count 高频调用时可在 Redis 中另存一个计数器（INCR/DECR），
// 避免每次调用 BITCOUNT 扫描整个 Bitmap。
func (s *RedisBitmapLikeStore) Count(ctx context.Context, postID string) (int64, error) {
	key := bitmapLikeKey(postID)
	count, err := s.rdb.BitCount(ctx, key, nil).Result()
	if err != nil {
		return 0, fmt.Errorf("RedisBitmapLikeStore.Count: %w", err)
	}
	return count, nil
}

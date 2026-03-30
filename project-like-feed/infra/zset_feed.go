package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"gopractice/project-like-feed/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// RedisZSetFeedStore：ZSET 推模式 Feed 流实现
// ─────────────────────────────────────────────────────────────────────────────

// feedInboxKey 生成用户 Feed 收件箱 Key。
func feedInboxKey(userID int64) string {
	return fmt.Sprintf("feed:inbox:%d", userID)
}

// RedisZSetFeedStore 使用 Redis ZSET 实现 Feed 流收件箱（推模式）。
//
// 【知识点 | ZSET 用于 Feed 流的设计要点】
//
// Key:    feed:inbox:{userID}（每个用户独立的收件箱）
// Score:  post.CreatedAt.UnixMilli()（发布时间戳毫秒）
// Member: postID（文章/动态的唯一 ID）
//
// 读取顺序：ZREVRANGEBYSCORE → score 从大到小 = 时间从新到旧
//
// 【推模式的内存压力】
// 若一个用户关注了 1000 个账号，每天每人发 1 条，收件箱每天增加 1000 条；
// 若保留 1000 条，需要 TrimInbox：ZREMRANGEBYRANK key 0 -(maxSize+1)
//
// 【推模式 vs 拉模式补充说明】
// 推：发布时写，读取快（O(log N)）；大 V 百万粉丝发帖需百万次 ZADD（写放大）
// 拉：读取时合并，写入简单；读取需聚合多人 timeline（读放大）
// 混合：普通用户推，大 V 拉，活跃粉丝推，不活跃粉丝拉（字节/微博实践）
type RedisZSetFeedStore struct {
	rdb        *redis.Client
	maxInboxSize int64 // 收件箱最大容量（0 = 不限制）
}

// NewRedisZSetFeedStore 构造 RedisZSetFeedStore。
// maxInboxSize = 0 表示不限制收件箱大小。
func NewRedisZSetFeedStore(rdb *redis.Client, maxInboxSize int64) *RedisZSetFeedStore {
	return &RedisZSetFeedStore{rdb: rdb, maxInboxSize: maxInboxSize}
}

// Push 将一条动态推入所有指定粉丝的 ZSET 收件箱。
//
// 【实现细节】使用 Pipeline 批量 ZADD，减少 N 次网络 RTT 为 1 次。
// Pipeline 不是事务（非原子），但对于 Feed 推送这种"部分失败可接受"的场景完全适合。
//
// 【可能问】「Pipeline 和 MULTI/EXEC 有什么区别？」
// 【答】Pipeline 只是批量发送，服务端逐条执行，非原子；
// MULTI/EXEC 是 Redis 乐观事务，EXEC 时所有命令原子执行。
// Feed 推送不需要原子性（某粉丝收件箱写失败不影响其他粉丝），
// Pipeline 吞吐更高，是正确选择。
func (s *RedisZSetFeedStore) Push(ctx context.Context, post domain.Post, followerIDs []int64) error {
	if len(followerIDs) == 0 {
		return nil
	}

	score := float64(post.CreatedAt.UnixMilli())

	// Pipeline 批量 ZADD
	pipe := s.rdb.Pipeline()
	for _, followerID := range followerIDs {
		key := feedInboxKey(followerID)
		pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: post.ID})
		// 写入后裁剪收件箱（保留最新 maxInboxSize 条）
		if s.maxInboxSize > 0 {
			// ZREMRANGEBYRANK key 0 -(maxInboxSize+1)：删除最旧的超量记录
			pipe.ZRemRangeByRank(ctx, key, 0, -(s.maxInboxSize+2))
		}
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("RedisZSetFeedStore.Push: pipeline exec: %w", err)
	}

	fmt.Printf("[FeedStore] 推送动态 postID=%s 给 %d 个粉丝\n", post.ID, len(followerIDs))
	return nil
}

// GetFeed 按时间倒序分页拉取 Feed 流（ZREVRANGEBYSCORE）。
//
// 【分页方案说明】
// 本实现用 offset+limit（游标分页），适合 Demo；
// 生产推荐用 score-based 游标（记录上次最小 score，下次从该 score 向前查），
// 避免新数据插入导致的分页漂移问题。
func (s *RedisZSetFeedStore) GetFeed(ctx context.Context, userID int64, offset, limit int64) ([]*domain.FeedItem, error) {
	key := feedInboxKey(userID)

	// ZREVRANGEBYSCORE with LIMIT：score 从大到小（时间从新到旧）
	results, err := s.rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Max:    "+inf",
		Min:    "-inf",
		Offset: offset,
		Count:  limit,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("RedisZSetFeedStore.GetFeed: %w", err)
	}

	items := make([]*domain.FeedItem, len(results))
	for i, z := range results {
		postID, _ := z.Member.(string)
		items[i] = &domain.FeedItem{
			PostID:    postID,
			Score:     z.Score,
			CreatedAt: time.UnixMilli(int64(z.Score)).UTC(),
		}
	}
	return items, nil
}

// TrimInbox 裁剪收件箱，只保留最新 maxSize 条。
//
// ZREMRANGEBYRANK key 0 -(maxSize+1)：
// rank 从 0 开始，-1 是最高分（最新），-(maxSize+1) 是第 maxSize+1 新的
// → 删除 rank 0 到 -(maxSize+1) 的旧记录
func (s *RedisZSetFeedStore) TrimInbox(ctx context.Context, userID int64, maxSize int64) error {
	key := feedInboxKey(userID)
	err := s.rdb.ZRemRangeByRank(ctx, key, 0, -(maxSize+2)).Err()
	if err != nil {
		return fmt.Errorf("RedisZSetFeedStore.TrimInbox: %w", err)
	}
	return nil
}

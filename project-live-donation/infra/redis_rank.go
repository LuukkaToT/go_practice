package infra

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// Redis Key：直播间贡献排行榜
// ─────────────────────────────────────────────────────────────────────────────

// rankKey 生成直播间排行榜的 ZSET Key。
// 格式：ld:room:{roomID}:rank
//
// 【面试点 | ZSET 内存结构】
// - 成员数 <= 128 且 Score 为整数时：使用 ziplist（紧凑存储，内存友好）
// - 否则升级为 skiplist + hashtable 双索引结构：
//   - skiplist：维护有序性（O(log N) 插入/查询）
//   - hashtable：O(1) 按 member 查 score（用于 ZINCRBY 等）
func rankKey(roomID string) string {
	return fmt.Sprintf("ld:room:%s:rank", roomID)
}

// memberKey 将 userID 转换为 ZSET member 字符串。
func memberKey(userID int64) string {
	return fmt.Sprintf("%d", userID)
}

// ─────────────────────────────────────────────────────────────────────────────
// RedisRankStore：实现 ports.RankStore
// ─────────────────────────────────────────────────────────────────────────────

// RedisRankStore 使用 Redis ZSET 维护直播间实时贡献排行榜。
type RedisRankStore struct {
	rdb *redis.Client
}

func NewRedisRankStore(rdb *redis.Client) *RedisRankStore {
	return &RedisRankStore{rdb: rdb}
}

// IncrScore 将用户在直播间的贡献榜 Score 原子增加 amount。
//
// 【面试点 | ZINCRBY 的原子性】
// ZINCRBY 是单个原子命令（非 Lua），Redis 单线程保证执行期间不会被打断。
// 1000 个并发 ZINCRBY 在 Redis 中串行执行，每次都是精确累加，不存在竞态条件。
//
// 【可能问】「ZINCRBY 返回的是什么？」
// 【答】返回执行后该 member 的新 Score（float64）。
// 如果 member 不存在，等价于先 ZADD score member，然后 ZINCRBY。
func (s *RedisRankStore) IncrScore(ctx context.Context, roomID string, userID int64, amount int64) error {
	return s.rdb.ZIncrBy(ctx, rankKey(roomID), float64(amount), memberKey(userID)).Err()
}

// TopN 获取排行榜前 N 名（按 Score 降序）。
//
// 【面试点 | ZREVRANGEBYSCORE vs ZREVRANGEBYRANK】
// - ZREVRANGEBYRANK key 0 N-1 WITHSCORES：按排名范围，适合"取前N名"
// - ZREVRANGEBYSCORE key +inf -inf WITHSCORES LIMIT 0 N：按分数范围，适合"取某分数段"
// 本场景取前 N 名，用 ZREVRANGEBYRANK 更直观（go-redis v9 对应 ZRevRangeWithScores）。
//
// 【可能问】「分页如何实现？」
// 【答】ZREVRANGEBYSCORE key {lastScore} -inf WITHSCORES LIMIT 0 pageSize
// 用上一页最后一名的 Score 作为游标，避免 OFFSET 扫描（适合无限下拉榜单）。
func (s *RedisRankStore) TopN(ctx context.Context, roomID string, n int64) ([]ports.RankItem, error) {
	zs, err := s.rdb.ZRevRangeWithScores(ctx, rankKey(roomID), 0, n-1).Result()
	if err != nil {
		return nil, fmt.Errorf("zset topN error: %w", err)
	}

	items := make([]ports.RankItem, 0, len(zs))
	for i, z := range zs {
		var uid int64
		fmt.Sscanf(z.Member.(string), "%d", &uid)
		items = append(items, ports.RankItem{
			UserID: uid,
			Score:  int64(z.Score),
			Rank:   i + 1,
		})
	}
	return items, nil
}

// DeleteRankKey 删除排行榜 Key（测试 teardown 使用）。
func DeleteRankKey(ctx context.Context, rdb *redis.Client, roomID string) error {
	return rdb.Del(ctx, rankKey(roomID)).Err()
}

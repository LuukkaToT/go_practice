package infra

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/redis/go-redis/v9"
)

// ======================================================================
// Redis SETBIT/GETBIT 布隆过滤器实现
// ======================================================================
//
// 【原理】
// 布隆过滤器 = 一个大 bit 数组 + k 个哈希函数：
//   Add(x)      → 用 k 个哈希函数计算 k 个位置，全部置 1（SETBIT）
//   MayExist(x) → 用相同 k 个哈希函数计算 k 个位置，全部为 1 则"可能存在"
//                 只要有一个位为 0 则"一定不存在"
//
// 【Redis SETBIT/GETBIT 无需 RedisBloom 模块，开箱即用】
//
// 【参数调优口诀（面试可说）】
// - bit 数组越大，假阳性率越低，内存越多
// - hash 函数越多，假阳性率越低，但性能越差
// - 经验值：100万条数据，1% 误差率 ≈ 960万 bit ≈ 1.2MB，k=7
//
// 【可能问】「布隆过滤器如何扩容？」
// 【答】标准布隆过滤器不支持删除和扩容；需要扩容时重建（全量重新 Add）；
// 支持删除的变体：Counting Bloom Filter（每位用计数器代替单bit）。
//
// 【可能问】「布隆过滤器数据存 Redis vs 内存，哪个好？」
// 【答】
//   - 内存：性能最好，但多实例间不共享，重启丢失，需各自预热
//   - Redis：多实例共享，重启不丢（持久化），网络开销 ~1ms；生产推荐 Redis

const (
	// bloomBitSize bit 数组大小 = 1M 位 = 128KB，可存约 10 万 ID（假阳性率约 2%）。
	// 生产场景按实际 ID 数量用公式调整：m = -n*ln(p) / (ln2)^2
	bloomBitSize = uint64(1 << 20) // 1,048,576 bits

	// bloomHashNum hash 函数个数。5 个时假阳性率约 3.2%；7 个约 1%；权衡性能取 5。
	bloomHashNum = 5
)

// RedisBloomFilter 基于 Redis SETBIT/GETBIT 的布隆过滤器，实现 ports.BloomFilter。
type RedisBloomFilter struct {
	RDB    *redis.Client
	BitKey string // 如 "cache:product:bloom"
}

// Add 将商品 ID 加入布隆过滤器（系统预热 / 新商品写入时调用）。
func (b *RedisBloomFilter) Add(ctx context.Context, id int64) error {
	positions := b.positions(id)
	pipe := b.RDB.Pipeline()
	for _, pos := range positions {
		pipe.SetBit(ctx, b.BitKey, pos, 1)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// MayExist 判断 ID 是否可能存在。
// 返回 false 表示一定不存在；返回 true 表示可能存在（有约 1-3% 假阳性）。
func (b *RedisBloomFilter) MayExist(ctx context.Context, id int64) (bool, error) {
	positions := b.positions(id)
	pipe := b.RDB.Pipeline()
	cmds := make([]*redis.IntCmd, len(positions))
	for i, pos := range positions {
		cmds[i] = pipe.GetBit(ctx, b.BitKey, pos)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Redis 故障时降级为"可能存在"，避免误杀正常请求
		return true, fmt.Errorf("bloom filter getbit: %w", err)
	}
	for _, cmd := range cmds {
		if cmd.Val() == 0 {
			return false, nil // 某一位为 0，一定不存在
		}
	}
	return true, nil // 所有位都为 1，可能存在
}

// positions 使用 k 个不同种子的 FNV-1a 生成 k 个 bit 位置。
//
// 【面试可说】实际生产中可使用 MurmurHash / xxHash 替代 FNV，
// 分布更均匀；这里用标准库 FNV 保持无额外依赖。
func (b *RedisBloomFilter) positions(id int64) []int64 {
	result := make([]int64, bloomHashNum)
	for i := 0; i < bloomHashNum; i++ {
		h := fnv.New64a()
		// 不同种子：在 ID 字节后追加种子字节，产生 k 个独立哈希值
		_, _ = fmt.Fprintf(h, "%d:%d", id, i)
		result[i] = int64(h.Sum64() % bloomBitSize)
	}
	return result
}

// Reset 清空布隆过滤器（测试专用：删除 bit 数组 key）。
func (b *RedisBloomFilter) Reset(ctx context.Context) error {
	return b.RDB.Del(ctx, b.BitKey).Err()
}

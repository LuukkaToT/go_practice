package domain

import "time"

// ─────────────────────────────────────────────────────────────────────────────
// Post 值对象（动态/文章）
// ─────────────────────────────────────────────────────────────────────────────

// Post 代表一条用户发布的动态。
//
// 【知识点 | Feed 流推模式（Push Model）设计】
// 大 V 发布动态 → 查出所有粉丝 ID → 循环 ZADD 写入每个粉丝的 Redis ZSET 收件箱
// 粉丝刷 Feed → ZREVRANGEBYSCORE 分页拉取自己的收件箱
//
// BadCase：同步循环 INSERT MySQL user_feed 表（百万粉丝阻塞数秒）
// GoodCase：异步写 Redis ZSET（内存操作，微秒级）
//
// 【可能问】「推模式 vs 拉模式 vs 推拉结合，如何选？」
// 【答】
//   推模式（Push/写扩散）：发布时写入所有粉丝收件箱
//     优：读取快（已预计算），适合粉丝少（< 10w）的普通用户
//     缺：大 V 发帖需写百万 ZADD，存储成本高
//   拉模式（Pull/读扩散）：读取时合并关注者的 timeline
//     优：写入简单，适合大 V（只写一次）
//     缺：读取慢（聚合 N 个 timeline 排序），延迟高
//   推拉结合（字节/微博实际用法）：
//     普通用户（粉丝 < 阈值）→ 推模式；大 V（粉丝 > 阈值）→ 拉模式
//     活跃粉丝推，非活跃粉丝拉（节省存储）
type Post struct {
	ID        string
	AuthorID  int64
	Content   string
	CreatedAt time.Time
}

// FeedItem Feed 收件箱中的一条记录（postID + 发布时间戳）。
//
// 【ZSET 设计说明】
// Key:    feed:inbox:{userID}
// Score:  Post.CreatedAt.UnixMilli()（时间戳毫秒，天然升序排列）
// Member: Post.ID（字符串，唯一标识动态）
//
// 读取时：ZREVRANGEBYSCORE ... +inf -inf LIMIT offset count
// → score 从大到小排列 = 时间从新到旧 = 符合刷 Feed 的直觉
//
// 【可能问】「收件箱容量怎么控制？」
// 【答】每次写入后可执行 ZREMRANGEBYRANK key 0 -（maxSize+1）
// 保留最新 maxSize 条，裁掉最旧的；生产一般保留 1000 条。
type FeedItem struct {
	PostID    string
	Score     float64 // = 发布时间戳 ms（即 ZSET score）
	CreatedAt time.Time
}

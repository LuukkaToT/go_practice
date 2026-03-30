// Package ports 依赖倒置层：定义点赞存储和 Feed 收件箱的抽象接口。
package ports

import (
	"context"

	"gopractice/project-like-feed/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// LikeStore 点赞存储接口
// ─────────────────────────────────────────────────────────────────────────────

// LikeStore 点赞状态的读写接口。
//
// 【接口设计原则】不暴露底层实现细节（Set 还是 Bitmap），
// 应用层只调用语义方法（Like/Unlike/IsLiked/Count），
// 测试时可注入 MockLikeStore（不依赖 Redis）。
type LikeStore interface {
	// Like 点赞。若已点赞返回 domain.ErrAlreadyLiked（可选，由实现决定是否幂等）。
	Like(ctx context.Context, like domain.Like) error
	// Unlike 取消点赞。
	Unlike(ctx context.Context, like domain.Like) error
	// IsLiked 查询指定用户是否已点赞。
	IsLiked(ctx context.Context, userID int64, postID string) (bool, error)
	// Count 获取某篇文章/视频的总点赞数。
	//
	// 【可能问】「为什么不直接 SELECT COUNT(*)？」
	// 【答】高并发下 COUNT(*) 会打穿 DB；Redis Bitmap BITCOUNT 是 O(N/8)，
	// 对于 1 亿用户只需扫描 12.5MB，通常几毫秒内完成。
	Count(ctx context.Context, postID string) (int64, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// FeedInbox Feed 收件箱接口（推模式）
// ─────────────────────────────────────────────────────────────────────────────

// FeedInbox 个人 Feed 流收件箱的读写接口。
type FeedInbox interface {
	// Push 将一条动态推入指定用户的收件箱（ZADD score=timestamp member=postID）。
	// followerIDs：需要接收此动态的粉丝 ID 列表。
	Push(ctx context.Context, post domain.Post, followerIDs []int64) error
	// GetFeed 按时间倒序分页拉取 Feed 流（ZREVRANGEBYSCORE LIMIT offset count）。
	//
	// 【参数说明】
	// offset：跳过前 offset 条（游标分页推荐用 maxScore 参数，这里简化用 offset）
	// limit：每页条数
	GetFeed(ctx context.Context, userID int64, offset, limit int64) ([]*domain.FeedItem, error)
	// TrimInbox 裁剪收件箱，保留最新 maxSize 条（防止收件箱无限膨胀）。
	TrimInbox(ctx context.Context, userID int64, maxSize int64) error
}

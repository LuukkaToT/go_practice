package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopractice/project-like-feed/domain"
	"gopractice/project-like-feed/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GoodCaseSvc：Bitmap 点赞 + ZSET Feed 推流
// ─────────────────────────────────────────────────────────────────────────────

// GoodCaseSvc 使用 Bitmap 存储点赞状态，ZSET 推模式实现 Feed 流。
//
// 【两个核心优化】
//
// 优化1 — Bitmap 替代 Set：
//   内存从 Set 的 640MB/千万用户 → Bitmap 的 1.25MB，节省 512 倍
//
// 优化2 — 异步 Pipeline 推 Feed：
//   一次 Pipeline ZADD 批量推送所有粉丝，从 N 次串行写 → 1 次批量写
//   延迟从 O(N) 降为 O(1)（网络 RTT 维度）
type GoodCaseSvc struct {
	likeStore  ports.LikeStore  // RedisBitmapLikeStore
	feedInbox  ports.FeedInbox  // RedisZSetFeedStore
}

// NewGoodCaseSvc 构造 GoodCaseSvc。
func NewGoodCaseSvc(likeStore ports.LikeStore, feedInbox ports.FeedInbox) *GoodCaseSvc {
	return &GoodCaseSvc{likeStore: likeStore, feedInbox: feedInbox}
}

// LikePost 点赞（Bitmap 实现）。
func (s *GoodCaseSvc) LikePost(ctx context.Context, userID int64, postID string) error {
	like, err := domain.NewLike(userID, postID)
	if err != nil {
		return err
	}
	if err := s.likeStore.Like(ctx, like); err != nil {
		if errors.Is(err, domain.ErrAlreadyLiked) {
			// 幂等处理：已点赞不报错
			return nil
		}
		return err
	}
	return nil
}

// UnlikePost 取消点赞（Bitmap 实现）。
func (s *GoodCaseSvc) UnlikePost(ctx context.Context, userID int64, postID string) error {
	like, err := domain.NewLike(userID, postID)
	if err != nil {
		return err
	}
	return s.likeStore.Unlike(ctx, like)
}

// IsLiked 查询点赞状态（GETBIT O(1)）。
func (s *GoodCaseSvc) IsLiked(ctx context.Context, userID int64, postID string) (bool, error) {
	return s.likeStore.IsLiked(ctx, userID, postID)
}

// GetLikeCount 获取点赞数（BITCOUNT）。
func (s *GoodCaseSvc) GetLikeCount(ctx context.Context, postID string) (int64, error) {
	return s.likeStore.Count(ctx, postID)
}

// PublishPost GoodCase：Pipeline 批量推动态到粉丝 ZSET 收件箱。
//
// 【与 BadCase 对比】
// BadCase：N 次串行写 DB/Redis，延迟 O(N)
// GoodCase：1 次 Pipeline 批量 ZADD，延迟 O(1)（网络层）
//
// 【生产进阶】真实场景中 PublishPost 本身也应该异步化：
//   1. HTTP 接口接收发布请求，写 DB 后立即返回 200
//   2. 后台 Worker（或 MQ Consumer）异步消费，执行粉丝推送
//   这样发布接口响应时间与粉丝数无关，恒定在毫秒级
func (s *GoodCaseSvc) PublishPost(ctx context.Context, post domain.Post, followerIDs []int64) (elapsed time.Duration, err error) {
	start := time.Now()
	fmt.Printf("[GoodCase] 开始 Pipeline 推送动态 postID=%s 给 %d 个粉丝...\n", post.ID, len(followerIDs))

	if err := s.feedInbox.Push(ctx, post, followerIDs); err != nil {
		return 0, err
	}

	elapsed = time.Since(start)
	fmt.Printf("[GoodCase] Pipeline 推送完成，耗时=%v（无论多少粉丝，网络 RTT 只有 1 次）\n", elapsed)
	return elapsed, nil
}

// GetFeed 分页拉取粉丝的 Feed 流（ZREVRANGEBYSCORE）。
func (s *GoodCaseSvc) GetFeed(ctx context.Context, userID int64, page, pageSize int64) ([]*domain.FeedItem, error) {
	offset := (page - 1) * pageSize
	items, err := s.feedInbox.GetFeed(ctx, userID, offset, pageSize)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[GoodCase] 用户 %d 第%d页 Feed，返回 %d 条（时间倒序）\n", userID, page, len(items))
	return items, nil
}

// Package application 应用服务层：编排领域对象和基础设施，实现业务用例。
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
// BadCaseSvc：Set 点赞（内存重）
// ─────────────────────────────────────────────────────────────────────────────

// BadCaseSvc 使用 Set 存储点赞，不做 Feed 推送（或同步写 DB 模拟高延迟）。
//
// 【BadCase 的两个问题】
//
// 问题1 — Set 内存过重：
//   每个用户 ID 在 Set 中占 ~64 字节（hashtable 编码）；
//   1000 万用户点赞同一篇文章 → 640 MB Redis 内存 per post。
//   热点文章（如春晚、世界杯）可能有数亿点赞 → 数十 GB。
//
// 问题2 — 无 Feed 流（或同步推送造成超时）：
//   大 V 发布动态时，若同步循环给百万粉丝写 DB / Redis，
//   单次请求耗时 = 粉丝数 × 单次写延迟 → 轻松超时。
type BadCaseSvc struct {
	likeStore ports.LikeStore // RedisSetLikeStore
}

// NewBadCaseSvc 构造 BadCaseSvc。
func NewBadCaseSvc(likeStore ports.LikeStore) *BadCaseSvc {
	return &BadCaseSvc{likeStore: likeStore}
}

// LikePost 点赞（Set 实现）。
func (s *BadCaseSvc) LikePost(ctx context.Context, userID int64, postID string) error {
	like, err := domain.NewLike(userID, postID)
	if err != nil {
		return err
	}
	if err := s.likeStore.Like(ctx, like); err != nil {
		if errors.Is(err, domain.ErrAlreadyLiked) {
			fmt.Printf("[BadCase] 用户 %d 已点赞 postID=%s（重复操作）\n", userID, postID)
			return nil
		}
		return err
	}
	return nil
}

// UnlikePost 取消点赞（Set 实现）。
func (s *BadCaseSvc) UnlikePost(ctx context.Context, userID int64, postID string) error {
	like, err := domain.NewLike(userID, postID)
	if err != nil {
		return err
	}
	return s.likeStore.Unlike(ctx, like)
}

// GetLikeCount 获取点赞数（SCARD）。
func (s *BadCaseSvc) GetLikeCount(ctx context.Context, postID string) (int64, error) {
	return s.likeStore.Count(ctx, postID)
}

// PublishPost BadCase：模拟同步循环给粉丝推动态（演示高延迟问题）。
//
// 【演示效果】当 followerCount 很大时，此函数会明显变慢。
// 生产中这个循环会导致：1. 请求超时；2. DB 连接池耗尽；3. 服务雪崩。
func (s *BadCaseSvc) PublishPost(ctx context.Context, post domain.Post, followerIDs []int64) (elapsed time.Duration, err error) {
	start := time.Now()
	fmt.Printf("[BadCase] 开始同步推送动态 postID=%s 给 %d 个粉丝...\n", post.ID, len(followerIDs))

	// ☠️ 同步循环写入（每次都是单独的网络请求）
	for _, followerID := range followerIDs {
		// 模拟写 DB 或 Redis（这里用 sleep 模拟单次写延迟 0.5ms）
		time.Sleep(500 * time.Microsecond) // 0.5ms per follower
		_ = followerID                     // 实际场景：INSERT INTO user_feed ...
	}

	elapsed = time.Since(start)
	fmt.Printf("[BadCase] 同步推送完成，耗时=%v（%d 粉丝 × 0.5ms = %v 理论延迟）\n",
		elapsed, len(followerIDs), time.Duration(len(followerIDs))*500*time.Microsecond)
	return elapsed, nil
}

// Package domain 点赞与 Feed 流领域层：值对象 + 领域错误，零基础设施依赖。
package domain

import (
	"errors"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// 领域错误
// ─────────────────────────────────────────────────────────────────────────────

var (
	ErrAlreadyLiked = errors.New("like: user has already liked this post")
	ErrNotLiked     = errors.New("like: user has not liked this post, cannot unlike")
	ErrInvalidUserID = errors.New("like: userID must be a non-negative integer (Bitmap offset constraint)")
)

// ─────────────────────────────────────────────────────────────────────────────
// Like 值对象
// ─────────────────────────────────────────────────────────────────────────────

// Like 描述一次点赞动作。
//
// 【知识点 | 为什么点赞用 Bitmap 而不是 Set？】
//
// 内存对比（以 1 亿用户点赞同一篇文章为例）：
//   Set（hashtable 编码，每个 entry ≈ 64 字节）：
//     1 亿 × 64 = 6,400,000,000 字节 ≈ 6.4 GB
//
//   Bitmap（1 用户 = 1 bit，offset = userID）：
//     1 亿 bit = 12,500,000 字节 ≈ 12.2 MB
//
//   内存节省比：6.4 GB / 12.2 MB ≈ 524 倍
//
// 【Bitmap 的 Trade-off（面试必须主动提）】
//   优点：O(1) 点赞/查询，BITCOUNT O(N/8) 快速统计，极致节省内存
//   缺点1：userID 必须是连续的非负整数（offset = userID）；
//          若 userID = 10亿，bitmap 需要 125MB（即使只有1人点赞）
//   缺点2：无法枚举"谁点了赞"（只能获取总数，无法 range scan 用户列表）
//
// 【生产方案】点赞统计和状态用 Bitmap，"点赞明细展示"用 Set 但限容量（最新 N 个）
type Like struct {
	UserID    int64
	PostID    string
	CreatedAt time.Time
}

// NewLike 构造 Like 值对象并校验 userID 合法性。
func NewLike(userID int64, postID string) (Like, error) {
	if userID < 0 {
		return Like{}, fmt.Errorf("%w: got %d", ErrInvalidUserID, userID)
	}
	if postID == "" {
		return Like{}, fmt.Errorf("like: postID must not be empty")
	}
	return Like{UserID: userID, PostID: postID, CreatedAt: time.Now().UTC()}, nil
}

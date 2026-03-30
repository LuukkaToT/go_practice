// Package domain 限流场景领域层：只含配置值对象和领域错误，无基础设施依赖。
//
// 【DDD 精简原则】限流是"基础设施横切关注点"，但其配置（窗口大小、阈值、Key 命名规则）
// 属于业务决策，放领域层表达清晰；具体的 Redis 操作放 infra 层。
package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrRateLimited 限流错误，调用方可 errors.Is 判断并返回 429。
//
// 【可能问】「为什么用哨兵 error 而不是 error string？」
// 【答】errors.Is 跨调用栈精确匹配，上层可按类型映射 HTTP 429 vs 500，
// 不依赖字符串，重构时不会因改拼写而静默失效。
var ErrRateLimited = errors.New("rate-limit: request rejected, too many requests")

// RateLimit 限流配置值对象（不可变，创建后不修改）。
//
// 【知识点 | 值对象 vs 实体】
// 值对象没有唯一 ID，只关心属性值；两个 WindowMS/Limit/KeyPrefix 完全相同的
// RateLimit 在业务上是等价的。限流配置天然符合值对象语义。
type RateLimit struct {
	// WindowMS 滑动窗口大小（毫秒）。
	// 固定窗口和滑动窗口都用同一个字段，单位统一为 ms 便于 Lua 脚本使用。
	WindowMS int64
	// Limit 窗口内允许的最大请求数（含边界：count >= limit 即拒绝）。
	Limit int64
	// KeyPrefix Redis Key 前缀，格式：ratelimit:{prefix}:{clientID}
	// 不同接口/用户维度用不同前缀，避免 Key 碰撞。
	KeyPrefix string
}

// NewRateLimit 构造 RateLimit 并做基本校验。
func NewRateLimit(window time.Duration, limit int64, keyPrefix string) (RateLimit, error) {
	if window <= 0 {
		return RateLimit{}, fmt.Errorf("rate-limit: window must be positive, got %v", window)
	}
	if limit <= 0 {
		return RateLimit{}, fmt.Errorf("rate-limit: limit must be positive, got %d", limit)
	}
	if keyPrefix == "" {
		return RateLimit{}, fmt.Errorf("rate-limit: keyPrefix must not be empty")
	}
	return RateLimit{
		WindowMS:  window.Milliseconds(),
		Limit:     limit,
		KeyPrefix: keyPrefix,
	}, nil
}

// RedisKey 生成完整的 Redis Key。
//
// 【知识点 | Redis Key 命名规范】
// 推荐格式：业务前缀:实体:ID，用冒号分隔（RedisInsight 可树状展示）。
// 示例：ratelimit:api:/order:user_123
func (r RateLimit) RedisKey(clientID string) string {
	return fmt.Sprintf("ratelimit:%s:%s", r.KeyPrefix, clientID)
}

// Window 返回窗口大小的 time.Duration（便于 Go time 包使用）。
func (r RateLimit) Window() time.Duration {
	return time.Duration(r.WindowMS) * time.Millisecond
}

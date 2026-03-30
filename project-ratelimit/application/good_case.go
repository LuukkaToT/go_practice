package application

import (
	"context"
	"fmt"

	"gopractice/project-ratelimit/domain"
	"gopractice/project-ratelimit/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GoodCaseSvc：滑动窗口限流（精确控制，无边界突刺）
// ─────────────────────────────────────────────────────────────────────────────

// GoodCaseSvc 使用滑动窗口限流器的应用服务。
//
// 【滑动窗口优势】
// 以"当前时刻"为右边界，动态计算过去 window 时间内的请求数，
// 不存在固定切割点，任意 1s 区间内的请求数都严格 ≤ limit。
//
// 【生产实践补充】
// 纯 Redis 滑动窗口适合 QPS ≤ 10w 的场景；更高 QPS 建议：
//   1. 本地滑动窗口（sync.Mutex + ring buffer）：纳秒级，无网络 RTT
//   2. 令牌桶（Token Bucket）：恒定补充令牌，支持突发流量（burst），
//      Go 标准库 golang.org/x/time/rate 即是令牌桶实现
//   3. 两级限流：本地限流（粗粒度）+ 分布式限流（精细兜底）
//
// 【可能问】「令牌桶 vs 滑动窗口，字节跳动用哪个？」
// 【答】两者都有应用场景：
//   - 滑动窗口：精确控制 QPS，不允许任何突发（如防刷 API）
//   - 令牌桶：允许合理突发（如视频上传，用户偶尔上传较大文件应被允许），
//     go-redis/rate 库（基于 Redis Lua 实现令牌桶）是生产常见选择
type GoodCaseSvc struct {
	limiter ports.Limiter
}

// NewGoodCaseSvc 构造 GoodCaseSvc（注入滑动窗口限流器）。
func NewGoodCaseSvc(limiter ports.Limiter) *GoodCaseSvc {
	return &GoodCaseSvc{limiter: limiter}
}

// HandleRequest 模拟接口处理（滑动窗口限流保护）。
func (s *GoodCaseSvc) HandleRequest(ctx context.Context, cfg domain.RateLimit, clientID string) (bool, error) {
	allowed, err := s.limiter.Allow(ctx, cfg, clientID)
	if err != nil {
		fmt.Printf("[GoodCase-SlidingWindow] ⚠️ Redis 故障，降级放行 clientID=%s err=%v\n", clientID, err)
		return true, nil
	}
	if !allowed {
		return false, domain.ErrRateLimited
	}
	return true, nil
}

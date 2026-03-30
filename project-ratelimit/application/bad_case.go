// Package application 应用服务层：编排领域对象和限流器，实现业务用例。
package application

import (
	"context"
	"fmt"

	"gopractice/project-ratelimit/domain"
	"gopractice/project-ratelimit/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// BadCaseSvc：固定窗口限流（演示边界突刺问题）
// ─────────────────────────────────────────────────────────────────────────────

// BadCaseSvc 使用固定窗口限流器的应用服务。
//
// 【固定窗口算法缺陷总结】
//
// 缺陷1 — 边界突刺（Boundary Burst）：
//   窗口 1s，阈值 10。在第 0.999s 发 10 个请求（全通过，W1=10），
//   在第 1.001s 发 10 个请求（全通过，W2 重置为 0），
//   在 [0.999s, 1.001s] 这 2ms 内实际并发 = 20（是阈值的 2 倍！）
//
// 缺陷2 — INCR+EXPIRE 非原子（见 infra/fixed_window.go）：
//   进程在 INCR 和 EXPIRE 之间崩溃 → key 永不过期 → 永久限流。
//
// 【可能问】「固定窗口限流在什么场景下够用？」
// 【答】流量均匀、对边界突刺不敏感的内部服务（如定时任务调用）；
// 对外暴露的 API 或安全防刷场景必须用滑动窗口或令牌桶。
type BadCaseSvc struct {
	limiter ports.Limiter
}

// NewBadCaseSvc 构造 BadCaseSvc（注入固定窗口限流器）。
func NewBadCaseSvc(limiter ports.Limiter) *BadCaseSvc {
	return &BadCaseSvc{limiter: limiter}
}

// HandleRequest 模拟接口处理（固定窗口限流保护）。
// 返回 true 代表请求被处理；false 代表被限流（429）。
func (s *BadCaseSvc) HandleRequest(ctx context.Context, cfg domain.RateLimit, clientID string) (bool, error) {
	allowed, err := s.limiter.Allow(ctx, cfg, clientID)
	if err != nil {
		// Redis 故障：fail open（放行），避免误杀正常请求
		fmt.Printf("[BadCase-FixedWindow] ⚠️ Redis 故障，降级放行 clientID=%s err=%v\n", clientID, err)
		return true, nil
	}
	if !allowed {
		return false, domain.ErrRateLimited
	}
	return true, nil
}

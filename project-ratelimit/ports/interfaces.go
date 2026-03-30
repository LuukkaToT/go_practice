// Package ports 依赖倒置层：定义限流器接口，infra 层实现，application 层使用。
//
// 【知识点 | 为什么限流要抽象为接口？】
// 1. 测试时可注入 MockLimiter（不依赖真实 Redis）
// 2. 算法可替换（固定窗口→滑动窗口→令牌桶），上层零改动
// 3. 分布式限流和本地限流可以共享同一接口，按环境注入不同实现
package ports

import (
	"context"

	"gopractice/project-ratelimit/domain"
)

// Limiter 限流器接口。
//
// 【可能问】「限流的实现层次有哪些？」
// 【答】
//   1. 接入层（Nginx/网关）：性能最高，但粒度粗（全局/IP 维度）
//   2. 应用层（本接口）：粒度细（接口/用户/租户），可结合业务逻辑，是面试重点
//   3. 服务网格（Istio/Envoy）：无代码侵入，但运维复杂度高
//
// 三层通常组合使用：网关兜底全局限流 + 应用层精细限流。
type Limiter interface {
	// Allow 判断当前请求是否被允许。
	//
	// clientID：限流维度标识（用户ID、IP、接口路径等）。
	// 返回 (true, nil)：允许通过；(false, nil)：被限流（调用方返回 429）；
	// (false, err)：Redis 故障等基础设施错误（调用方返回 500 或降级放行）。
	//
	// 【可能问】「Redis 故障时限流应该放行还是拦截？」
	// 【答】通常选择"放行"（fail open）：宁可让少量流量通过也不要误杀正常请求；
	// 对安全敏感场景（防刷）可选"拦截"（fail closed）；这是业务决策，不是技术决策。
	Allow(ctx context.Context, cfg domain.RateLimit, clientID string) (allowed bool, err error)
}

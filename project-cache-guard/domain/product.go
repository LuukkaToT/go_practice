// Package domain 热点商品缓存场景领域层：只表达业务规则，无基础设施依赖。
//
// 【DDD原则】领域层 = 实体/值对象/领域错误；不引入 Redis/MySQL/HTTP。
package domain

import "errors"

// ErrProductNotFound 商品不存在（领域错误，上层可 errors.Is 精确判断）。
//
// 【面试点 | 空值缓存】不存在的 ID 也要缓存（存空标记），防止每次都打 DB。
// 上层收到此错误时应决定是否将空值写入缓存，而不是直接透传给 DB。
var ErrProductNotFound = errors.New("cache-guard: product not found")

// Product 商品聚合根（详情页核心数据）。
//
// 【可能问】「为什么商品价格用 int64 分而不是 float64？」
// 【答】float64 二进制表示无法精确表达十进制小数，累加/比较会产生精度误差；
// 金融/电商场景统一用整数「分/厘」，与支付通道一致。
type Product struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price"` // 单位：分
	Stock int64  `json:"stock"`
}

-- 缓存综合治理 Demo 初始化脚本
-- 执行：mysql -uroot -p123456 -P3307 db_cache_guard < project-cache-guard/sql/init.sql

CREATE TABLE IF NOT EXISTS hot_products (
    id         BIGINT       NOT NULL AUTO_INCREMENT  COMMENT '商品 ID',
    name       VARCHAR(100) NOT NULL DEFAULT ''      COMMENT '商品名称',
    price      BIGINT       NOT NULL DEFAULT 0       COMMENT '价格（分）',
    stock      BIGINT       NOT NULL DEFAULT 0       COMMENT '库存',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='热点商品';

-- 初始化商品（id=1~10 为有效商品，用于命中测试；id=9999 不存在，用于穿透测试）
INSERT INTO hot_products (id, name, price, stock) VALUES
    (1, '旗舰手机',    599900, 1000),
    (2, '无线耳机',     99900,  500),
    (3, '智能手表',    199900,  300),
    (4, '平板电脑',    299900,  200),
    (5, '游戏手柄',     49900, 1000),
    (6, '机械键盘',     89900,  800),
    (7, '显示器',      349900,  150),
    (8, '移动硬盘',     69900,  600),
    (9, '摄像头',       39900,  400),
    (10,'路由器',       29900,  700)
ON DUPLICATE KEY UPDATE
    name  = VALUES(name),
    price = VALUES(price),
    stock = VALUES(stock);

-- 布隆过滤器预热说明（Redis 侧）：
-- 测试 newFixture() 中会调用 bloom.Add(ctx, id) for id in 1..10
-- 生产场景：系统启动时遍历 DB 所有有效 ID，批量 Add 到布隆过滤器

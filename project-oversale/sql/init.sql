-- 秒杀超卖 Demo 初始化脚本
-- 执行目标：db_oversale 数据库（见 infrastructure/docker-compose.yml，端口 3307）
-- 执行方式：mysql -uroot -p123456 -P3307 db_oversale < project-oversale/sql/init.sql

-- ============================================================
-- 1. 商品库存表
-- ============================================================
-- 【面试点 | 乐观锁字段】version 每次 CAS UPDATE 时 +1，保证同时只有一个更新成功。
-- 【面试点 | 为何需要 stock >= qty 条件】双重保险：即使应用层漏判，DB 也不会让 stock < 0。
CREATE TABLE IF NOT EXISTS flash_products (
    id         BIGINT       NOT NULL AUTO_INCREMENT     COMMENT '商品 ID',
    name       VARCHAR(100) NOT NULL                    COMMENT '商品名称',
    stock      INT          NOT NULL DEFAULT 0          COMMENT '可用库存',
    version    INT          NOT NULL DEFAULT 0          COMMENT '乐观锁版本号',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='秒杀商品库存';

-- ============================================================
-- 2. 购买订单表
-- ============================================================
-- 【面试点 | 幂等唯一键】uk_product_user 防止同一用户重复购买同一商品（接口幂等兜底）。
-- 生产场景中订单 ID 通常由客户端传入（或用雪花 ID），此处自增演示。
CREATE TABLE IF NOT EXISTS purchase_orders (
    id         BIGINT   NOT NULL AUTO_INCREMENT         COMMENT '订单 ID',
    product_id BIGINT   NOT NULL                        COMMENT '商品 ID',
    user_id    BIGINT   NOT NULL                        COMMENT '用户 ID',
    quantity   INT      NOT NULL DEFAULT 1              COMMENT '购买数量',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    -- 同一用户同一商品只能买一次（演示用；生产可放开，改为单独幂等键表）
    UNIQUE KEY uk_product_user (product_id, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='秒杀购买订单';

-- ============================================================
-- 3. 初始化商品数据
-- ============================================================
-- 库存 = 10，version = 0；测试前 setup() 会动态 RESET，这里只保证表不为空。
INSERT INTO flash_products (id, name, stock, version)
VALUES (1, '限量秒杀商品', 10, 0)
ON DUPLICATE KEY UPDATE
    name    = VALUES(name),
    stock   = VALUES(stock),
    version = VALUES(version);

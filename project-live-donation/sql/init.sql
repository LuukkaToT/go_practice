-- ============================================================
-- Demo: 直播打赏高并发 — 数据库初始化脚本
-- ============================================================
-- 目标数据库：db_live_donation（见 infrastructure/docker-compose.yml，端口 3307）
-- 执行方式：mysql -h127.0.0.1 -P3307 -uroot -p123456 db_live_donation < project-live-donation/sql/init.sql
-- 或通过测试代码 infra.InitTable(db) 自动执行（幂等 CREATE TABLE IF NOT EXISTS）
-- ============================================================

-- ─────────────────────────────────────────────────────────────
-- ld_users：直播平台用户表（含虚拟币余额）
-- ─────────────────────────────────────────────────────────────
-- 【知识点 | balance 字段设计】
-- 使用 BIGINT 存储"分"（虚拟币最小单位），避免 DECIMAL 的精度运算开销。
-- 生产中可以把 balance 单独拆出到 ld_user_wallets 表，降低用户主表的更新热点。
--
-- 【可能问】「高并发下 ld_users 的 balance 字段为什么会成为热点行？」
-- 【答】多个打赏事务并发时，都需要对同一用户的 balance 行加 FOR UPDATE 锁。
-- 假设某主播的铁杆粉丝每秒刷 1000 个礼物，这 1000 个事务全部排队等同一把行锁。
-- 解决方案：将热点余额迁移到 Redis，用 Lua 脚本原子扣减，彻底绕开 MySQL 行锁。
CREATE TABLE IF NOT EXISTS ld_users (
    id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY COMMENT '用户 ID',
    nickname   VARCHAR(64)  NOT NULL DEFAULT ''                 COMMENT '用户昵称',
    balance    BIGINT       NOT NULL DEFAULT 0                  COMMENT '虚拟币余额（分）',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='直播平台用户表';


-- ─────────────────────────────────────────────────────────────
-- ld_streamers：主播表（含累计收益）
-- ─────────────────────────────────────────────────────────────
-- 【知识点 | earnings 更新策略】
-- BadCase：主播收益在打赏事务内同步更新（行锁竞争加剧）。
-- GoodCase：MQ Consumer 异步落库时更新，主流程无需等待。
--
-- 【可能问】「异步更新主播收益会不会导致数据不一致？」
-- 【答】最终一致性（Eventual Consistency）。MQ at-least-once 保证消息一定会被消费，
-- Consumer 采用幂等设计（唯一流水 ID），不会重复累加。短暂不一致（毫秒到秒级）
-- 对主播查看收益的场景完全可接受（主播不会"实时"盯着后台看每一笔打赏）。
CREATE TABLE IF NOT EXISTS ld_streamers (
    id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY COMMENT '主播 ID',
    nickname   VARCHAR(64)  NOT NULL DEFAULT ''                 COMMENT '主播昵称',
    earnings   BIGINT       NOT NULL DEFAULT 0                  COMMENT '累计收益（分）',
    created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='主播表';


-- ─────────────────────────────────────────────────────────────
-- ld_donations：打赏流水表
-- ─────────────────────────────────────────────────────────────
-- 【知识点 | 流水表设计原则】
-- 1. 流水记录只插入，不更新（append-only），天然不存在行锁竞争。
-- 2. 联合索引 (room_id, user_id) 加速 BadCase 的 GROUP BY 聚合排行榜查询。
-- 3. 生产中流水表会快速膨胀，需要按月分区（PARTITION BY RANGE）或定期归档。
--
-- 【可能问】「打赏流水为什么要单独一张表，而不是直接在 ld_users 里记录？」
-- 【答】关注点分离（Separation of Concerns）：
--   - ld_users：状态表，只关心"当前余额"。
--   - ld_donations：事件表，记录每一次变化历史，用于对账、退款、榜单统计。
--   这也是 Event Sourcing 思想的简化版本。
CREATE TABLE IF NOT EXISTS ld_donations (
    id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY COMMENT '流水 ID（自增）',
    user_id     BIGINT       NOT NULL                            COMMENT '送礼用户 ID',
    streamer_id BIGINT       NOT NULL                            COMMENT '主播 ID',
    room_id     VARCHAR(64)  NOT NULL                            COMMENT '直播间 ID',
    amount      BIGINT       NOT NULL                            COMMENT '打赏金额（分）',
    created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间（毫秒精度）',

    INDEX idx_room_user (room_id, user_id),   -- BadCase GROUP BY 排行榜加速
    INDEX idx_streamer  (streamer_id)          -- 查询主播流水加速
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='打赏流水表（append-only）';


-- ─────────────────────────────────────────────────────────────
-- 初始化测试数据（10 个用户 + 1 个主播）
-- ─────────────────────────────────────────────────────────────
-- 测试场景：10 个用户（id=1~10），每人余额 5000 分。
-- 每次打赏 50 分，每人打赏 100 次 = 恰好花光余额。
-- 验证点：并发 1000 次打赏后，每人余额应精确归零（无超扣/漏扣）。
INSERT INTO ld_users (id, nickname, balance) VALUES
    (1,  '用户_001', 5000),
    (2,  '用户_002', 5000),
    (3,  '用户_003', 5000),
    (4,  '用户_004', 5000),
    (5,  '用户_005', 5000),
    (6,  '用户_006', 5000),
    (7,  '用户_007', 5000),
    (8,  '用户_008', 5000),
    (9,  '用户_009', 5000),
    (10, '用户_010', 5000)
ON DUPLICATE KEY UPDATE
    nickname = VALUES(nickname),
    balance  = VALUES(balance);

INSERT INTO ld_streamers (id, nickname, earnings) VALUES
    (1, '主播_小明', 0)
ON DUPLICATE KEY UPDATE
    nickname = VALUES(nickname),
    earnings = 0;

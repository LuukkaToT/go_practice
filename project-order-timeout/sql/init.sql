-- ============================================================
-- Demo 3: 订单超时与双写一致性 — 数据库初始化脚本
-- ============================================================
-- 执行方式：mysql -h127.0.0.1 -P3307 -uroot -p123456 db_order_timeout < init.sql
-- 或在测试代码中通过 infra.InitTable(db) 自动执行（幂等）
-- ============================================================

-- ─────────────────────────────────────────────────────────────
-- orders 表：订单主表
-- ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS orders (
    id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY   COMMENT '订单ID',
    user_id    BIGINT       NOT NULL                              COMMENT '用户ID',
    amount     BIGINT       NOT NULL                              COMMENT '金额（分）',
    -- 【知识点 | ENUM vs VARCHAR】
    -- ENUM 在 MySQL 内部存为整数（1字节），比 VARCHAR 节省空间，且有约束（非法值会报错）。
    -- 缺点：新增状态需要 ALTER TABLE（生产需谨慎）；若用 VARCHAR 则无此问题，但需应用层校验。
    status     ENUM('pending','paid','cancelled') NOT NULL DEFAULT 'pending' COMMENT '订单状态',
    created_at DATETIME(3)  NOT NULL                              COMMENT '创建时间（毫秒精度）',
    updated_at DATETIME(3)  NOT NULL                              COMMENT '更新时间',

    INDEX idx_user_id (user_id),
    INDEX idx_status  (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='订单表';


-- ─────────────────────────────────────────────────────────────
-- order_outbox 表：事务性发件箱（Transactional Outbox）
-- ─────────────────────────────────────────────────────────────
-- 【知识点 | Outbox 表核心字段】
--   published_at：NULL = 待投递；非NULL = 已投递（OutboxDispatcher 更新）
--   唯一索引 (order_id, event_type)：防止同一订单同一事件类型重复入箱
--     → 如果 GoodCaseSvc.CreateOrder 因网络问题重试，重复调用 Enqueue 会触发唯一索引冲突，
--       返回 duplicate key error，事务回滚，不会产生重复 outbox 记录。
--
-- 【可能问】「为什么 order_outbox 要和 orders 在同一个 DB？」
-- 【答】Outbox 的核心价值就是与业务表共享一个事务；
-- 如果在不同 DB，就需要分布式事务（2PC），复杂性大幅上升。
CREATE TABLE IF NOT EXISTS order_outbox (
    id           BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY  COMMENT 'Outbox记录ID',
    order_id     BIGINT      NOT NULL                             COMMENT '关联订单ID',
    event_type   VARCHAR(64) NOT NULL                             COMMENT '事件类型: order.created / order.timeout',
    payload      JSON        NOT NULL                             COMMENT '消息体（JSON）',
    published_at DATETIME(3) NULL      DEFAULT NULL               COMMENT '投递时间（NULL=待投递）',
    created_at   DATETIME(3) NOT NULL                             COMMENT '入箱时间',

    -- 防重入箱：同一订单同一事件类型只能有一条 outbox 记录
    UNIQUE KEY uq_order_event (order_id, event_type),
    -- 扫描未投递记录的高效索引
    INDEX idx_pending (published_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事务性发件箱（Transactional Outbox）';


-- ─────────────────────────────────────────────────────────────
-- idempotent_events 表：消费端幂等去重（MySQL 兜底层）
-- ─────────────────────────────────────────────────────────────
-- 【知识点 | 幂等表作用】
-- 第1层（Redis SETNX）处理 99%+ 的重复；此表是第2层安全网：
--   - Redis 宕机重启后 Key 丢失，重复消息会绕过第1层
--   - 此时 MySQL 唯一索引 uq_msg_id 会抛 duplicate key error
--   - 应用层捕获此错误，视为"已处理"，安全 Ack
--
-- msg_id：对应 RabbitMQ 消息的 MessageId 字段（由 Publisher 在 Publish 时填充）
-- 生命周期：可定期清理 7 天前的记录（SELECT ... WHERE processed_at < DATE_SUB(NOW(), INTERVAL 7 DAY)）
CREATE TABLE IF NOT EXISTS idempotent_events (
    id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY  COMMENT '记录ID',
    msg_id       VARCHAR(128) NOT NULL                             COMMENT 'MQ 消息ID（全局唯一）',
    processed_at DATETIME(3)  NOT NULL                             COMMENT '首次处理时间',

    -- 核心：唯一索引确保同一 msg_id 只能插入一次
    UNIQUE KEY uq_msg_id (msg_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='消费端幂等去重表（MySQL 兜底）';

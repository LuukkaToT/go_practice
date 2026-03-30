// Package infra 是基础设施层，实现 ports 中定义的所有接口。
// 本文件包含：MySQL UnitOfWork、UserBalanceRepo、StreamerRepo、DonationRepo。
package infra

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"gopractice/project-live-donation/domain"
	"gopractice/project-live-donation/ports"
)

// ─────────────────────────────────────────────────────────────────────────────
// GORM 数据模型（仅基础设施层使用，领域层不感知）
// ─────────────────────────────────────────────────────────────────────────────

type userModel struct {
	ID       int64  `gorm:"primaryKey;column:id"`
	Nickname string `gorm:"column:nickname"`
	Balance  int64  `gorm:"column:balance"`
}

func (userModel) TableName() string { return "ld_users" }

type streamerModel struct {
	ID       int64  `gorm:"primaryKey;column:id"`
	Nickname string `gorm:"column:nickname"`
	Earnings int64  `gorm:"column:earnings"`
}

func (streamerModel) TableName() string { return "ld_streamers" }

type donationModel struct {
	ID         int64  `gorm:"primaryKey;autoIncrement;column:id"`
	UserID     int64  `gorm:"column:user_id;not null"`
	StreamerID int64  `gorm:"column:streamer_id;not null"`
	RoomID     string `gorm:"column:room_id;not null"`
	Amount     int64  `gorm:"column:amount;not null"`
}

func (donationModel) TableName() string { return "ld_donations" }

// ─────────────────────────────────────────────────────────────────────────────
// InitTable：幂等建表（测试 setup 调用）
// ─────────────────────────────────────────────────────────────────────────────

// InitTable 在 db_live_donation 数据库中幂等地创建所有表。
func InitTable(db *gorm.DB) error {
	sqls := []string{
		`CREATE TABLE IF NOT EXISTS ld_users (
			id         BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
			nickname   VARCHAR(64) NOT NULL DEFAULT '',
			balance    BIGINT      NOT NULL DEFAULT 0,
			created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS ld_streamers (
			id         BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
			nickname   VARCHAR(64) NOT NULL DEFAULT '',
			earnings   BIGINT      NOT NULL DEFAULT 0,
			created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS ld_donations (
			id          BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id     BIGINT      NOT NULL,
			streamer_id BIGINT      NOT NULL,
			room_id     VARCHAR(64) NOT NULL,
			amount      BIGINT      NOT NULL,
			created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			INDEX idx_room_user (room_id, user_id),
			INDEX idx_streamer  (streamer_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range sqls {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLUnitOfWork：事务工作单元
// ─────────────────────────────────────────────────────────────────────────────

// MySQLUnitOfWork 实现 ports.UnitOfWork，在单一 MySQL 事务内执行多 Repo 操作。
//
// 【面试点 | UoW + Context 传递事务 DB】
// 通过 context.WithValue 将 *gorm.DB（已开启事务）传递给子 Repo，
// 子 Repo 从 context 提取 txDB；若 context 无 txDB，则使用普通 DB。
// 这样 Repo 方法签名只需要 context，不需要显式传 *gorm.DB，保持接口简洁。
type MySQLUnitOfWork struct {
	db *gorm.DB
}

func NewMySQLUnitOfWork(db *gorm.DB) *MySQLUnitOfWork {
	return &MySQLUnitOfWork{db: db}
}

type txKey struct{}

func (u *MySQLUnitOfWork) ExecTx(
	ctx context.Context,
	fn func(ports.UserBalanceRepo, ports.StreamerRepo, ports.DonationRepo) error,
) error {
	return u.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := context.WithValue(ctx, txKey{}, tx)
		return fn(
			NewMySQLUserRepo(u.db, txCtx),
			NewMySQLStreamerRepo(u.db, txCtx),
			NewMySQLDonationRepo(u.db, txCtx),
		)
	})
}

// dbFromCtx 优先使用 context 中的事务 DB，否则使用普通 DB。
func dbFromCtx(ctx context.Context, fallback *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return fallback.WithContext(ctx)
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLUserRepo：用户余额操作
// ─────────────────────────────────────────────────────────────────────────────

// MySQLUserRepo 实现 ports.UserBalanceRepo。
type MySQLUserRepo struct {
	db    *gorm.DB
	txCtx context.Context // 若非 nil，从此 context 提取事务 DB
}

func NewMySQLUserRepo(db *gorm.DB, txCtx context.Context) *MySQLUserRepo {
	return &MySQLUserRepo{db: db, txCtx: txCtx}
}

func (r *MySQLUserRepo) getDB(ctx context.Context) *gorm.DB {
	if r.txCtx != nil {
		return dbFromCtx(r.txCtx, r.db)
	}
	return dbFromCtx(ctx, r.db)
}

// GetBalance 查询用户余额（不加锁，供钱包预加载使用）。
func (r *MySQLUserRepo) GetBalance(ctx context.Context, userID int64) (int64, error) {
	var u userModel
	if err := r.getDB(ctx).First(&u, userID).Error; err != nil {
		return 0, err
	}
	return u.Balance, nil
}

// DeductBalance 在事务内用 WHERE balance>=amount 条件更新，原子扣减余额。
//
// 【面试点 | 条件更新防超扣（乐观减法）】
// UPDATE ld_users SET balance=balance-? WHERE id=? AND balance>=?
// - 若余额不足，RowsAffected=0，应用层识别为 ErrInsufficientBalance
// - 结合事务内的 FOR UPDATE（或此 WHERE 条件），可防止并发超扣
// - BadCase 中使用此方法（在事务内），GoodCase 中由 Lua 脚本替代
func (r *MySQLUserRepo) DeductBalance(ctx context.Context, userID int64, amount int64) error {
	result := r.getDB(ctx).
		Model(&userModel{}).
		Where("id = ? AND balance >= ?", userID, amount).
		UpdateColumn("balance", gorm.Expr("balance - ?", amount))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrInsufficientBalance
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLStreamerRepo：主播收益操作
// ─────────────────────────────────────────────────────────────────────────────

// MySQLStreamerRepo 实现 ports.StreamerRepo。
type MySQLStreamerRepo struct {
	db    *gorm.DB
	txCtx context.Context
}

func NewMySQLStreamerRepo(db *gorm.DB, txCtx context.Context) *MySQLStreamerRepo {
	return &MySQLStreamerRepo{db: db, txCtx: txCtx}
}

func (r *MySQLStreamerRepo) getDB(ctx context.Context) *gorm.DB {
	if r.txCtx != nil {
		return dbFromCtx(r.txCtx, r.db)
	}
	return dbFromCtx(ctx, r.db)
}

// AddEarnings 原子地增加主播收益。
//
// 【可能问】「主播收益为什么不在打赏事务内更新？（GoodCase 视角）」
// 【答】GoodCase 中主播收益由 MQ Consumer 异步更新：
//   - 主流程（Lua 扣款 + ZSET + MQ publish）完全不碰 MySQL
//   - Consumer 批量消费，合并多笔打赏后一次性更新，进一步降低 DB 写入频率
func (r *MySQLStreamerRepo) AddEarnings(ctx context.Context, streamerID int64, amount int64) error {
	result := r.getDB(ctx).
		Model(&streamerModel{}).
		Where("id = ?", streamerID).
		UpdateColumn("earnings", gorm.Expr("earnings + ?", amount))
	return result.Error
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQLDonationRepo：打赏流水操作
// ─────────────────────────────────────────────────────────────────────────────

// MySQLDonationRepo 实现 ports.DonationRepo。
type MySQLDonationRepo struct {
	db    *gorm.DB
	txCtx context.Context
}

func NewMySQLDonationRepo(db *gorm.DB, txCtx context.Context) *MySQLDonationRepo {
	return &MySQLDonationRepo{db: db, txCtx: txCtx}
}

func (r *MySQLDonationRepo) getDB(ctx context.Context) *gorm.DB {
	if r.txCtx != nil {
		return dbFromCtx(r.txCtx, r.db)
	}
	return dbFromCtx(ctx, r.db)
}

// Save 将打赏领域对象映射为模型后插入 ld_donations 表。
func (r *MySQLDonationRepo) Save(ctx context.Context, d *domain.Donation) error {
	m := donationModel{
		UserID:     d.UserID,
		StreamerID: d.StreamerID,
		RoomID:     d.RoomID,
		Amount:     d.Amount,
	}
	return r.getDB(ctx).Create(&m).Error
}

// SumByUser 查询直播间各用户累计打赏金额，用于 BadCase 排行榜。
//
// 【BadCase | 性能问题分析】
// 此查询在打赏流水达到百万/千万行时会变得非常慢：
//   1. GROUP BY + SUM 需要全扫或大量索引扫描
//   2. 高并发下多个 GROUP BY 查询竞争 InnoDB buffer pool
//   3. 无法做增量更新，每次都是全量重新计算
// GoodCase 用 ZSET 维护实时排行榜，读榜复杂度 O(log N + M)，
// 与流水记录总量完全无关。
func (r *MySQLDonationRepo) SumByUser(ctx context.Context, roomID string, limit int) ([]ports.RankItem, error) {
	type row struct {
		UserID int64 `gorm:"column:user_id"`
		Total  int64 `gorm:"column:total"`
	}
	var rows []row
	err := r.getDB(ctx).
		Model(&donationModel{}).
		Select("user_id, SUM(amount) AS total").
		Where("room_id = ?", roomID).
		Group("user_id").
		Order("total DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	items := make([]ports.RankItem, 0, len(rows))
	for i, row := range rows {
		items = append(items, ports.RankItem{
			UserID: row.UserID,
			Score:  row.Total,
			Rank:   i + 1,
		})
	}
	return items, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 辅助：重置测试数据（测试 teardown 使用）
// ─────────────────────────────────────────────────────────────────────────────

// ResetTestData 重置测试用户余额、清空打赏流水、重置主播收益。
func ResetTestData(ctx context.Context, db *gorm.DB, userIDs []int64, streamerID int64, initBalance int64) error {
	if err := db.WithContext(ctx).
		Model(&userModel{}).
		Where("id IN ?", userIDs).
		UpdateColumn("balance", initBalance).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).
		Where("user_id IN ?", userIDs).
		Delete(&donationModel{}).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).
		Model(&streamerModel{}).
		Where("id = ?", streamerID).
		UpdateColumn("earnings", 0).Error; err != nil {
		return err
	}

	// 确保主播记录存在
	streamer := streamerModel{ID: streamerID, Nickname: "主播_小明", Earnings: 0}
	return db.WithContext(ctx).
		Where(streamerModel{ID: streamerID}).
		FirstOrCreate(&streamer).Error
}

// EnsureUsers 确保测试用户存在，不存在则插入。
func EnsureUsers(ctx context.Context, db *gorm.DB, userIDs []int64, initBalance int64) error {
	for _, uid := range userIDs {
		u := userModel{ID: uid, Nickname: "用户_" + pad(uid), Balance: initBalance}
		if err := db.WithContext(ctx).
			Where(userModel{ID: uid}).
			FirstOrCreate(&u).Error; err != nil {
			return err
		}
	}
	return nil
}

// EnsureStreamer 确保主播记录存在。
func EnsureStreamer(ctx context.Context, db *gorm.DB, streamerID int64) error {
	s := streamerModel{ID: streamerID, Nickname: "主播_小明", Earnings: 0}
	return db.WithContext(ctx).Where(streamerModel{ID: streamerID}).FirstOrCreate(&s).Error
}

func pad(id int64) string {
	s := "000" + itoa(id)
	return s[len(s)-3:]
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// GetStreamerEarnings 查询主播累计收益（测试验证使用）。
func GetStreamerEarnings(ctx context.Context, db *gorm.DB, streamerID int64) (int64, error) {
	var s streamerModel
	if err := db.WithContext(ctx).First(&s, streamerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return s.Earnings, nil
}

// CountDonations 统计打赏流水条数（测试验证使用）。
func CountDonations(ctx context.Context, db *gorm.DB, roomID string) (int64, error) {
	var count int64
	err := db.WithContext(ctx).Model(&donationModel{}).Where("room_id = ?", roomID).Count(&count).Error
	return count, err
}

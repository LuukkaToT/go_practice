package infra

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitRedis 初始化并返回 Redis 客户端
// 可以在这里统一管理配置，比如 IP、密码

// 使用once实现单例模式
var rdb *redis.Client
var once sync.Once

func GetRedisClient() *redis.Client {
	//使用once保证，redis客户端只被初始化一次
	once.Do(func() {
		rdb = redis.NewClient(&redis.Options{
			Addr:         "127.0.0.1:6380",
			Password:     "",
			DB:           0,
			PoolSize:     20,
			MinIdleConns: 5,
			DialTimeout:  5 * time.Second,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Fatalf("❌ [Infra] Redis 连接失败: %v", err)
		}
		fmt.Println("✅ [Infra] Redis 连接成功")
	})
	return rdb
}

// ─────────────────────────────────────────────────────────────────────────────
// MySQL — 支持多数据库的 per-DB 单例
// ─────────────────────────────────────────────────────────────────────────────

var (
	dbInstances = make(map[string]*gorm.DB)
	dbMu        sync.Mutex
)

// InitMySQLDB 按数据库名返回 GORM DB 实例（per-DB 单例）。
//
// 每个项目应传入自己专属的数据库名，避免多项目共用同一个库导致表名冲突：
//   - project-oversale      → infra.InitMySQLDB("db_oversale")
//   - project-cache-guard   → infra.InitMySQLDB("db_cache_guard")
//   - project-order-timeout → infra.InitMySQLDB("db_order_timeout")
//   - project-live-donation → infra.InitMySQLDB("db_live_donation")
func InitMySQLDB(dbName string) *gorm.DB {
	dbMu.Lock()
	defer dbMu.Unlock()

	if inst, ok := dbInstances[dbName]; ok {
		return inst
	}

	dsn := fmt.Sprintf(
		"root:123456@tcp(127.0.0.1:3307)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		dbName,
	)
	var err error
	inst, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		log.Fatalf("❌ [Infra] MySQL 连接失败 db=%s: %v", dbName, err)
	}

	sqlDB, _ := inst.DB()
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetConnMaxLifetime(time.Hour)

	fmt.Printf("✅ [Infra] MySQL 连接成功 db=%s\n", dbName)
	dbInstances[dbName] = inst
	return inst
}

// InitMySQL 向后兼容保留，等同于 InitMySQLDB("mysql-practice")。
// 新项目请直接调用 InitMySQLDB(projectDBName)。
func InitMySQL() *gorm.DB {
	return InitMySQLDB("mysql-practice")
}

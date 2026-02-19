package infra

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitRedis 初始化并返回 Redis 客户端
// 可以在这里统一管理配置，比如 IP、密码
func InitRedis() *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6379",
		Password:     "123456",
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
	return rdb
}

// InitMySQL 初始化并返回 GORM DB 对象
func InitMySQL() *gorm.DB {
	dsn := "root:admin123@tcp(127.0.0.1:3306)/db_practice?charset=utf8mb4&parseTime=True&loc=Local"

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		// 练习项目可以开启 Info 级别日志，方便看 SQL
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		log.Fatalf("❌ [Infra] MySQL 连接失败: %v", err)
	}

	sqlDB, _ := db.DB()
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetConnMaxLifetime(time.Hour)

	fmt.Println("✅ [Infra] MySQL 连接成功")
	return db
}

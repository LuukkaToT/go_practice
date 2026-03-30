package main

import (
	"testing"
)

// 测试无锁的情况
func TestConcurrentWriteWithoutMutex(t *testing.T) {
	// 直接在这里准备测试数据并调用你的目标函数
	concurrentWriteWithoutMutex()
}

// 测试加锁的情况
func TestConcurrentWriteWithMutex(t *testing.T) {
	concurrentWriteWithMutex()
}

package main

import (
	"fmt"
	"sync"
)

// 并发写切片，不加锁
func concurrentWriteWithoutMutex() {
	var s []int           //声明一个共享切片
	var wg sync.WaitGroup //等待计数器,包含函数:Done(),Add(),Wait()

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		/*
			这个函数如果不传参(go func(){})，而是直接在下面的赋值操作里这样使用：append(s,i)
			这是典型的闭包引用外部变量。
			在 Go 1.22 版本之前： 所有的 goroutine 都会捕获到同一个外部变量 i 的内存地址。
			因为 for 循环的执行速度极快，远快于 goroutine 的启动和调度速度。
			当大部分 goroutine 真正开始执行内部逻辑时,
			外面的 for 循环可能早就跑完了，此时 i 的值已经变成了 1000。
			这会导致切片 s 里被塞入大量的 1000，而不是你期望的 0, 1, 2... 999。
			在 Go 1.22 及之后版本： Go 官方修复了这个著名的“循环变量捕获”陷阱。
			现在每次循环迭代，都会为 i 创建一个全新的变量。
			因此，即使你不传参直接引用，也会按预期工作，每个 goroutine 都能拿到当时那一轮的 i 值。
		*/
		go func(val int) {
			defer wg.Done()
			/*一定要记得写！！很重要，计数器一定要使用wg.Done()关闭,
			会导致程序直接死锁（Deadlock）并崩溃。会导致主协程一直卡在 wg.Wait() 这里死等。
			Go 运行时的死锁检测机制会发现所有的 goroutine 都处于休眠/阻塞状态，
			程序会直接抛出 fatal error: all goroutines are asleep - deadlock! 然后崩溃退出。
			*/
			s = append(s, val)
		}(i)
	}
	wg.Wait()
	fmt.Println(len(s))
}

func concurrentWriteWithMutex() {
	var s []int
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(val int) {
			defer wg.Done()
			mu.Lock()
			s = append(s, val)
			mu.Unlock()
		}(i)
	}
	wg.Wait() //记得等待协程结束
	fmt.Println(len(s))
}

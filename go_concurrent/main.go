package main

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

//下面代码运行在一个高并发的web后端，找出这段代码中的问题。

// 全局缓存
var cache = make(map[string]string)

func main() {
	urls := []string{
		"https://www.baidu.com",
		"https://www.google.com",
	}
	for _, url := range urls {
		go func() {
			resp, err := http.Get(url)
			if err != nil {
				fmt.Println("Error:", err)
				//如果失败了不要忘记return
				return
			}
			body, _ := io.ReadAll(resp.Body)

			//写入缓存
			cache[url] = string(body)
		}()
	}
	time.Sleep(2 * time.Second)
	fmt.Println("Cache:", cache)
}

package main

import (
	"fmt"
	"os"
)

func main() {

	/*
		os.Args是go语言标准库os包提供的一个字符串切片，专门用来存储获取你在命令行运行程序时，跟在后面的所有参数
		以go run . withNothing为例，Go在底层遍历并执行之后，os.Args里其实装了两个元素
		os.Args[0]：永远是程序本身的可执行文件路径。
		os.Args[1]：是你传入的第一个真正的参数，在这里就是字符串"withNothing"
	*/

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run . [simulate|fix]")
		return
	}
	switch os.Args[1] { //这里的os.Arg[]是什么东西，怎么用？
	case "withNothing":
		concurrentWriteWithoutMutex()
	case "withMutex":
		concurrentWriteWithMutex()
	}
}

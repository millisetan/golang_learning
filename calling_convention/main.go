package main

import (
	"fmt"
	"time"
)

func calling(a int, b int) (c int, d int, e int) {
	var k = 10
	return a, b, k
}

func simpleCall() {
	calling(12, 13)
}

func nestCall() {
	var a = 10
	nest := func() {
		a = 20
	}
	nest()
	fmt.Println(a)
}

func do(f func(int)) {
	f(20)
}

func nestPass() {
	var a = 10
	do(func(b int) {
		a = b
	})
	fmt.Println(a)
}

func nestGoroutine() {
	var b = 20
	var a = 10
	var c = 30
	fmt.Println(&b)
	fmt.Println(&a)
	fmt.Println(&c)

	go func(b int) {
		var c = 20
		fmt.Println("xx", &c)
		fmt.Println("go")
		fmt.Println(&a)
		fmt.Println(a)
		a = b
	}(20)
	return
}

func nestGoroutine1() {
	var b = 40
	var a = 40
	var c = 40
	fmt.Println(&b)
	fmt.Println(&a)
	fmt.Println(&c)
}

func nestGoroutine2() {
	var b = 40
	var a = 40
	var c = 40
	fmt.Println(&b)
	fmt.Println(&a)
	fmt.Println(&c)
}

func main() {
	var a = 10
	fmt.Println("main", &a)
	nestGoroutine()
	nestGoroutine1()
	nestGoroutine2()
	time.Sleep(10 * time.Second)
}

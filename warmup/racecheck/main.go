// Command racecheck —— 实测 Go race detector 对「原子访问 vs 普通访问」的判定。
//
// 用途：回答一个常见争论 —— 同一个变量，一个 goroutine 用 sync/atomic 访问，
// 另一个 goroutine 用普通读写，race detector 到底报不报？
//
// 一次只跑一个场景，避免多场景互相干扰、便于看清是哪一个场景被报了：
//
//	go run -race ./warmup/racecheck 1    # 普通写 + 普通读
//	go run -race ./warmup/racecheck 2    # 原子写 + 原子读
//	go run -race ./warmup/racecheck 3    # 原子写 + 普通读
//	go run -race ./warmup/racecheck 4    # 普通写 + 原子读
//	go run -race ./warmup/racecheck 5    # 互斥锁
package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

var (
	sink int64
	x    int64
	mu   sync.Mutex
)

// pair 起两个 goroutine，用同一个 start channel 同时放行，等它们都结束后返回。
// 两个 goroutine 之间没有 happens-before 关系（各自只与 main 有边），
// 所以 TSan 的判断完全基于「访问类型 + happens-before」的机器状态，而不是碰运气的时间窗口。
func pair(f1, f2 func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(2)
	go func() { defer wg.Done(); <-start; f1() }()
	go func() { defer wg.Done(); <-start; f2() }()
	close(start)

	wg.Wait()
}

func main() {
	name := "?"
	if len(os.Args) > 1 {
		name = os.Args[1]
	}

	switch name {
	case "1":
		name = "1) 普通写 + 普通读"
		pair(func() { x = 1 }, func() { sink = x })
	case "2":
		name = "2) 原子写 + 原子读"
		pair(func() { atomic.StoreInt64(&x, 1) }, func() { sink = atomic.LoadInt64(&x) })
	case "3":
		name = "3) 原子写 + 普通读"
		pair(func() { atomic.StoreInt64(&x, 1) }, func() { sink = x })
	case "4":
		name = "4) 普通写 + 原子读"
		pair(func() { x = 1 }, func() { sink = atomic.LoadInt64(&x) })
	case "5":
		name = "5) 互斥锁保护"
		pair(func() { mu.Lock(); x++; mu.Unlock() }, func() { mu.Lock(); x++; mu.Unlock() })
	default:
		fmt.Fprintf(os.Stderr, "usage: racecheck [1..5]\n")
		os.Exit(2)
	}

	fmt.Printf("场景 %s 结束，sink = %d\n", name, sink)
}

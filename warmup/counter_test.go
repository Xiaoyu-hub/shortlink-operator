package warmup

import (
	"sync"
	"testing"
)

// TestCounterConcurrent 是 Day 1 的自检用例。
//
// 200 个 goroutine × 每个 1000 次 Inc = 期望 200000。
// 用 `-race` 跑：既检查结果对不对，也检查有没有数据竞争。
//
//	go test -race ./warmup/
//	go test -race -count=3 ./warmup/   # 连跑 3 次，确认不是偶然通过
func TestCounterConcurrent(t *testing.T) {
	const goroutines = 200
	const perGoroutine = 1000

	var c Counter

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got, want := c.Value(), int64(goroutines*perGoroutine); got != want {
		t.Fatalf("counter = %d, want %d（丢了 %d 次更新）", got, want, want-got)
	}
}

// TestCounterSequential 是单线程正确性用例：并发安全改造不能把单线程语义改坏。
func TestCounterSequential(t *testing.T) {
	var c Counter
	for i := 0; i < 10; i++ {
		c.Inc()
	}
	if got, want := c.Value(), int64(10); got != want {
		t.Fatalf("counter = %d, want %d", got, want)
	}
}

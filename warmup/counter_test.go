package warmup

import (
	"sync"
	"testing"
)

// counterUnderTest 是测试侧自定义的接口。
//
// 注意这里没有任何"注册"动作：因为 Go 是结构化类型（structural typing），
// *Counter / *AtomicCounter / *ShardedCounter 只要方法集凑齐了，就自动满足它。
// 这就是 controller-runtime 的 reconcile.Reconciler 的同一套机制，
// 也是为什么表驱动测试能一行覆盖三种实现。
type counterUnderTest interface {
	Inc()
	IncN(int64)
	Value() int64
}

// allCounters 返回三种实现，供表驱动用例遍历。
//
// 用函数而不是变量：每次取都是全新实例，用例之间不会互相污染。
func allCounters() map[string]func() counterUnderTest {
	return map[string]func() counterUnderTest{
		"mutex":   func() counterUnderTest { return new(Counter) },
		"atomic":  func() counterUnderTest { return new(AtomicCounter) },
		"sharded": func() counterUnderTest { return NewShardedCounter(8) },
	}
}

// TestCounterConcurrent 是 Day 1 的自检用例（ROADMAP 原文要求）。
//
// 200 个 goroutine × 每个 1000 次 Inc = 期望 200000。
// 用 `-race` 跑：既检查结果对不对，也检查有没有数据竞争。
//
//	go test -race -count=3 ./warmup/
func TestCounterConcurrent(t *testing.T) {
	const goroutines = 200
	const perGoroutine = 1000

	for name, newCounter := range allCounters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := newCounter()

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
		})
	}
}

// TestCounterSequential 是单线程正确性用例：并发安全改造不能把单线程语义改坏。
func TestCounterSequential(t *testing.T) {
	for name, newCounter := range allCounters() {
		t.Run(name, func(t *testing.T) {
			c := newCounter()
			for i := 0; i < 10; i++ {
				c.Inc()
			}
			c.IncN(-3) // 加入负数：IncN 的语义就是"加"，不做下界钳制

			if got, want := c.Value(), int64(7); got != want {
				t.Fatalf("counter = %d, want %d", got, want)
			}
		})
	}
}

// TestCounterMixedReadersAndWriters 让读和写在时间上重叠。
//
// 这一步比 TestCounterConcurrent 更狠：如果 Value() 忘了同步（典型错误是
// "写加锁、读不加锁"），上面的用例可能侥幸通过，这里则会被 race detector 抓住。
func TestCounterMixedReadersAndWriters(t *testing.T) {
	for name, newCounter := range allCounters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const writers = 32
			const perWriter = 500

			c := newCounter()

			var wg sync.WaitGroup
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < perWriter; j++ {
						c.Inc()
					}
				}()
			}

			// 4 个读者一边读一边跑，制造 read/write 交错。
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 2000; j++ {
						if v := c.Value(); v < 0 {
							t.Errorf("counter 不该为负：%d", v)
							return
						}
					}
				}()
			}

			wg.Wait()

			if got, want := c.Value(), int64(writers*perWriter); got != want {
				t.Fatalf("counter = %d, want %d", got, want)
			}
		})
	}
}

// TestIncNSubtotalIsAtomic 验证 IncN 是一次不可分割的更新。
//
// 反例（错误实现）：把 IncN 拆成"循环 n 次 Inc()"，虽然结果对，
// 但在高竞争下中间态可被观察、耗时长一个数量级。
// 这里用 sum 校验：每个 goroutine 一次加一个大数，最终和必须是精确的。
func TestIncNSubtotalIsAtomic(t *testing.T) {
	for name, newCounter := range allCounters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := newCounter()

			var wg sync.WaitGroup
			for i := 0; i < 64; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.IncN(10000)
				}()
			}
			wg.Wait()

			if got, want := c.Value(), int64(64*10000); got != want {
				t.Fatalf("counter = %d, want %d", got, want)
			}
		})
	}
}

// TestShardedCounterRoundsShardCountToPowerOfTwo 覆盖位与取模的边界。
//
// 用 & mask 代替 % len 是常见优化，但要求 len 是 2 的幂，
// 否则会读到越界索引 —— 所以构造器必须负责向上取整。
func TestShardedCounterRoundsShardCountToPowerOfTwo(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{in: -1, want: 1},
		{in: 0, want: 1},
		{in: 1, want: 1},
		{in: 2, want: 2},
		{in: 3, want: 4},
		{in: 5, want: 8},
		{in: 8, want: 8},
		{in: 9, want: 16},
	}

	for _, tc := range cases {
		c := NewShardedCounter(tc.in)
		if got := len(c.shards); got != tc.want {
			t.Errorf("NewShardedCounter(%d) 分片数 = %d, want %d", tc.in, got, tc.want)
		}
		if c.mask != uint64(tc.want-1) {
			t.Errorf("NewShardedCounter(%d) mask = %d, want %d", tc.in, c.mask, tc.want-1)
		}
	}

	// 三种实现的行为必须一致：分片数不改变"计数"这个语义。
	a := NewShardedCounter(1)
	var m Counter
	for i := 0; i < 100; i++ {
		a.Inc()
		m.Inc()
	}
	if a.Value() != m.Value() {
		t.Errorf("分片实现与互斥锁实现结果不一致：%d vs %d", a.Value(), m.Value())
	}
}

// TestShardedCounterWritesSpreadAcrossShards 验证"打散"是真的发生了。
//
// 这是 ShardedCounter 存在的理由：如果所有写都落在同一个分片上，
// 那它只是个更慢的 AtomicCounter。这里直接检查分片分布。
func TestShardedCounterWritesSpreadAcrossShards(t *testing.T) {
	const shards = 8
	const writes = 800

	c := NewShardedCounter(shards)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writes/32; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got := c.Value(); got != writes {
		t.Fatalf("total = %d, want %d", got, writes)
	}

	used := 0
	for i := range c.shards {
		if c.ShardValue(i) > 0 {
			used++
		}
	}
	if used != shards {
		t.Errorf("只有 %d/%d 个分片被写过，说明轮询分配失效", used, shards)
	}
}

// TestShardedCounterFastPath 覆盖"快路径"：分片下标每个 goroutine 只取一次。
//
// 这才是 ShardedCounter 的真实用法 —— 分片选择是**一次性成本**，
// 不是每次 Inc 的成本。顺带验证边界：IncAt 收到越界/负数下标也不能 panic。
func TestShardedCounterFastPath(t *testing.T) {
	const shards = 8
	const goroutines = 64
	const perGoroutine = 500

	c := NewShardedCounter(shards)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// ↓ 整个 goroutine 生命周期里只取一次分片下标（模拟 controller
			// worker 启动时绑定自己的分片）。
			idx := c.ShardIndex()
			for j := 0; j < perGoroutine; j++ {
				c.IncAt(idx)
			}
			c.IncNAt(idx, -1) // 用掉 IncNAt，顺带验证负数增量
		}()
	}
	wg.Wait()

	want := int64(goroutines * (perGoroutine - 1))
	if got := c.Value(); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}

	// 越界下标必须被 mask 吸收，不能 panic。
	c.IncAt(1 << 20)
	c.IncAt(-1)
	if got, want := c.Value(), want+2; got != want {
		t.Fatalf("越界下标写入后 total = %d, want %d", got, want)
	}
}

package warmup

import (
	"strconv"
	"testing"
)

// 基准测试的意义：把"哪种实现更好"从信仰问题变成数字问题。
//
// 跑法：
//
//	go test -bench=. -benchmem -cpu=1,4,8 ./warmup/
//
// RunParallel 会把 b.N 次迭代分给 GOMAXPROCS 个 goroutine，
// 模拟真实的多核竞争。看三个指标：
//   - ns/op   ：每次 Inc 的耗时（越小越好）
//   - B/op    ：每次操作的堆分配（这里应该全是 0）
//   - allocs/op：分配次数（这里应该全是 0）
//
// 预期形态：低并发时 atomic ≈ sharded < mutex；并发越高，
// mutex 因"排队 + 唤醒"掉得越快，sharded 因"各写各的缓存行"最抗跌。

// benchCounters 只做基准，所以用最简的接口。
type benchCounter interface {
	Inc()
	Value() int64
}

func BenchmarkCounterMutex(b *testing.B) {
	c := new(Counter)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
	// 防止编译器把整个循环优化掉：结果必须被"用掉"。
	if c.Value() != int64(b.N) {
		b.Fatalf("counter = %d, want %d", c.Value(), b.N)
	}
}

func BenchmarkCounterAtomic(b *testing.B) {
	c := new(AtomicCounter)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
	if c.Value() != int64(b.N) {
		b.Fatalf("counter = %d, want %d", c.Value(), b.N)
	}
}

func BenchmarkCounterSharded(b *testing.B) {
	c := NewShardedCounter(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
	if c.Value() != int64(b.N) {
		b.Fatalf("counter = %d, want %d", c.Value(), b.N)
	}
}

// BenchmarkCounterShardedValue 暴露分片方案的代价：Value() 是 O(分片数)。
//
// 对比 BenchmarkCounterMutexValue 就能看出"读"变贵了多少 ——
// 这是拿"写扩展性"换来的，面试时要能说出这笔交易。
// BenchmarkCounterShardedFastPath 是分片计数器的正确打开方式：
// 每个 goroutine 进循环之前取一次分片下标，循环里只写本地缓存行。
//
// 对照 BenchmarkCounterSharded（每次 Inc 都重新分片）看差值，
// 差值就是"共享分片选择器"的代价。实测它会和单变量 atomic 同量级 ——
// 换句话说：选分片的那次原子加，把分片带来的收益又吃回去了。
func BenchmarkCounterShardedFastPath(b *testing.B) {
	c := NewShardedCounter(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		idx := c.ShardIndex()
		for pb.Next() {
			c.IncAt(idx)
		}
	})
	if c.Value() != int64(b.N) {
		b.Fatalf("counter = %d, want %d", c.Value(), b.N)
	}
}

func BenchmarkCounterShardedValue(b *testing.B) {
	c := NewShardedCounter(64)
	for i := 0; i < 1000; i++ {
		c.Inc()
	}

	b.ReportAllocs()
	b.ResetTimer()
	var sink int64
	for i := 0; i < b.N; i++ {
		sink += c.Value()
	}
	if sink == 0 {
		b.Fatal("impossible")
	}
}

func BenchmarkCounterMutexValue(b *testing.B) {
	c := new(Counter)
	for i := 0; i < 1000; i++ {
		c.Inc()
	}

	b.ReportAllocs()
	b.ResetTimer()
	var sink int64
	for i := 0; i < b.N; i++ {
		sink += c.Value()
	}
	if sink == 0 {
		b.Fatal("impossible")
	}
}

// BenchmarkCounterShardedShardCount 用来看"分片数取多少合适"。
//
// 结论通常是：分片数到 GOMAXPROCS 附近收益就趋于饱和，再加只是浪费内存。
func BenchmarkCounterShardedShardCount(b *testing.B) {
	for _, shards := range []int{1, 4, 16, 64} {
		b.Run(shardName(shards), func(b *testing.B) {
			c := NewShardedCounter(shards)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					c.Inc()
				}
			})
			if c.Value() != int64(b.N) {
				b.Fatalf("counter = %d, want %d", c.Value(), b.N)
			}
		})
	}
}

// BenchmarkAtomicVsMutexContention 用递增的并发度把"拐点"找出来。
func BenchmarkAtomicVsMutexContention(b *testing.B) {
	var mutexCounter Counter
	var atomicCounter AtomicCounter

	b.Run("mutex", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				mutexCounter.Inc()
			}
		})
	})

	b.Run("atomic", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				atomicCounter.Inc()
			}
		})
	})
}

// shardName 只是给子基准起个稳定名字，别让名字里出现随机的 map 遍历顺序。
func shardName(n int) string {
	return "shards=" + strconv.Itoa(n)
}

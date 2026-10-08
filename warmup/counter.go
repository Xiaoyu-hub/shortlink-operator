// Package warmup 是 Day 1 的 Go 并发热身场。
//
// 这里用同一个问题（read-modify-write 竞争）引出三种解法，它们刚好对应 Operator
// 里三类会被并发访问的状态：
//
//	Counter        —— sync.Mutex 保护的普通状态：语义最直白，最不容易写错
//	AtomicCounter  —— sync/atomic 单变量：无锁，适合"就是一个数字"的状态
//	ShardedCounter —— 分片 + 缓存行填充：把写热点打散，适合"写多读少"的计数
//
// 三者都实现了下面这个隐式接口（Go 里不需要任何 implements 声明）：
//
//	interface {
//	    Inc()
//	    Value() int64
//	}
//
// —— 这正是 Day 1 要建立的心智模型：你以后要实现 reconcile.Reconciler，
// 也只是"把 Reconcile(ctx, req) (Result, error) 这个方法凑齐"而已。
//
// 为什么 reconciler 会在意这个：controller-runtime 会用多个 worker goroutine
// 并发调用同一个 Reconciler 实例（默认 MaxConcurrentReconciles=1，但可以调大），
// 所以放在 reconciler 结构体里、被多个 Reconcile 读写的字段（计数、缓存、限流器）
// 必须像这里一样做同步；同时 Reconcile 自己也必须幂等。两件事合起来才叫"并发安全"。
//
// 验证：
//
//	go test -race -count=3 ./warmup/   # 正确性 + 数据竞争
//	go test -bench=. -benchmem ./warmup/  # 三种实现的吞吐对比
package warmup

import (
	"sync"
	"sync/atomic"
)

// Counter 是方案 A：互斥锁保护的计数器。
//
// 临界区 = "读 count → 加 1 → 写回 count" 这三步，必须整体串行化；
// 只锁一半（比如读不加锁）依然会读到撕裂的中间状态。
//
// 锁的开销主要在两点：① 竞争时 goroutine 要进 runtime 的等待队列（可能被 park）；
// ② 解锁后把等待者唤醒有延迟。好处是不需要理解内存序，肉眼可审计。
type Counter struct {
	mu    sync.Mutex
	count int64
}

// Inc 把计数加一。
func (c *Counter) Inc() {
	c.IncN(1)
}

// IncN 把计数加 n，n 可以是负数。
//
// 有了 IncN，Inc 就退化成一行调用 —— 这也顺手演示了"接口收敛"：
// 底层只需要一个最小原语，上层语义都拼出来。
func (c *Counter) IncN(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count += n
}

// Value 返回当前计数。
//
// 注意：读取同样要加锁。不加锁的读会与写并发，race detector 一样会报警，
// 而且 64 位变量在 32 位平台上可能读到"半新半旧"的值。
func (c *Counter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// AtomicCounter 是方案 B：sync/atomic 无锁计数器。
//
// 与 Mutex 的区别：这里没有"临界区"，只有一条 CPU 原子指令
// （x86-64 上是 LOCK XADD，ARM64 上是 LDADDAL）。没有等待队列，
// 所以低竞争下更快；但高竞争下所有核心依然要抢同一根缓存行（cache line），
// 那部分代价是省不掉的 —— 见 ShardedCounter。
//
// 关于对齐：用 atomic.Int64 类型时，Go 编译器会自动保证 64 位对齐
// （atomic.Int64 内部带 align64 标记），所以不用再手工关心字段顺序。
// 但如果手握一个裸 int64 + atomic.AddInt64/atomic.LoadInt64，就必须自己保证
// "结构体第一个字段"或 8 字节对齐 —— 否则 32 位平台上会 panic
// （ARM/x86-32 的非对齐 64 位原子操作会触发 fault）。
type AtomicCounter struct {
	n atomic.Int64
}

// Inc 把计数加一。
func (c *AtomicCounter) Inc() {
	c.n.Add(1)
}

// IncN 把计数加 n。
func (c *AtomicCounter) IncN(n int64) {
	c.n.Add(n)
}

// Value 返回当前计数。原子读，不会撕裂。
func (c *AtomicCounter) Value() int64 {
	return c.n.Load()
}

// cacheLineSize 是主流 amd64/arm64 的缓存行大小。
//
// 为什么关心它：CPU 之间以缓存行为单位同步（MESI 协议）。两个变量只要落在
// 同一行里，哪怕逻辑上互不相干，一个核心写、另一个核心读，也会让那行在两个
// 核心之间来回弹（false sharing）。填充的目标就是让每个分片独占一行。
const cacheLineSize = 64

// shard 是 ShardedCounter 的一个分片。
//
// _ [cacheLineSize - 8]byte 是"前置填充"：让后面的 n 落在自己的缓存行里，
// 而不是和数组里相邻分片的 n 挤在一起。8 = atomic.Int64 的大小。
// 匿名空白字段 _ 不占语义、不参与赋值，只是占位。
type shard struct {
	_ [cacheLineSize - 8]byte
	n atomic.Int64
}

// ShardedCounter 是方案 C：分片计数器。
//
// 思路：把"一个热点变量"拆成 N 个变量，每次 Inc 只写其中一个分片，
// 读的时候把 N 个分片加起来。代价与收益：
//
//	收益：N 个核心各写各的缓存行，不再互相弹行，扩展性接近线性；
//	代价：① Value() 从 O(1) 变成 O(N)，且不是一个"瞬间快照"
//	      ② 分片选择本身若要共享状态，又会引入新的竞争点（见 tick）
//
// 这是 Prometheus client、ClickHouse、JVM LongAdder 都在用的套路。
// 选型原则：写极多、读极少（比如 metrics 计数器每 15s 才被采集一次）用它；
// 读写均衡就用 AtomicCounter，别提前优化。
//
// ⚠️ 本实现留了两种写入方式，因为基准测试给出了一个反直觉结论：
//
//	Inc() / IncN()  —— 每次都重新分片，内部要在 tick 这个共享原子上再抢一次；
//	IncAt(idx)      —— 调用方在 goroutine 启动时用 ShardIndex() 取一次下标，
//	                   之后每次写都是纯本地缓存行操作。
//
// 如果只保留 Inc()，分片选择器自身就成了新的热点，实测分片版本反而比
// 单变量 atomic 更慢（见 README「基准测试复盘」）。这也是优化的通用铁律：
// 先测量，再优化；否则你只是在增加复杂度。
type ShardedCounter struct {
	shards []shard
	mask   uint64 // len(shards)-1，len 必须是 2 的幂，用位与代替取模
	tick   atomic.Uint64
}

// NewShardedCounter 创建 n 分片的计数器，n 会被向上取整到 2 的幂。
func NewShardedCounter(n int) *ShardedCounter {
	if n < 1 {
		n = 1
	}
	size := 1
	for size < n {
		size <<= 1
	}
	return &ShardedCounter{
		shards: make([]shard, size),
		mask:   uint64(size - 1),
	}
}

// Inc 是"慢路径"：写一次，顺手重新分片一次。
//
// 它让 ShardedCounter 也满足最小接口（临时用、低频用够了），
// 但如果你在热点路径上看到它，说明分片并没有起到作用。
//
// 这里仍然有一处共享写（tick），它只做"分配序号"、不做业务累加，
// 且 tick 与业务分片在不同缓存行上。轮询分配的另一个好处是无偏：
// 每个分片被写的次数几乎相同，Value() 求和时不会有长尾。
func (c *ShardedCounter) Inc() {
	c.IncN(1)
}

// IncN 是同样的慢路径，一次加 n。
func (c *ShardedCounter) IncN(n int64) {
	idx := c.tick.Add(1) & c.mask
	c.shards[idx].n.Add(n)
}

// ShardIndex 返回一个分片下标，请在 goroutine 启动时调用一次并缓存结果。
//
// 返回 int（值类型）而不是对象指针：值不逃逸到堆上，热点路径上零分配。
// 这是 Go 里很典型的一个取舍 —— 想快，就别在热路径上拿指针。
func (c *ShardedCounter) ShardIndex() int {
	return int(c.tick.Add(1) & c.mask)
}

// IncAt 是"快路径"：给指定分片加一。
//
// 下标再 & mask 一次，所以传越界甚至负数都不会 panic ——
// 用一次位运算换掉一个边界检查分支，对热路径很划算。
func (c *ShardedCounter) IncAt(idx int) {
	c.shards[uint64(idx)&c.mask].n.Add(1)
}

// IncNAt 是 IncAt 的带增量版本。
func (c *ShardedCounter) IncNAt(idx int, n int64) {
	c.shards[uint64(idx)&c.mask].n.Add(n)
}

// ShardValue 返回单个分片的值（调试/断言用，业务上一般只用 Value）。
func (c *ShardedCounter) ShardValue(idx int) int64 {
	return c.shards[uint64(idx)&c.mask].n.Load()
}

// Value 返回所有分片之和。
//
// 注意语义：这是"弱一致的快照"，在有人并发写时它可能既不是 t1 时刻的值，
// 也不是 t2 时刻的值。对计数器/指标这种场景完全够用；
// 但如果你的业务要求"读到某一时刻的精确状态"，就应该用 Mutex 版本。
func (c *ShardedCounter) Value() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].n.Load()
	}
	return total
}

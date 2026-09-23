package warmup

// Counter 是服务的请求计数器。
//
// ⚠️ 注意：下面这一版是【故意写成并发不安全】的，它是你的 Day 1 练习题，不是参考答案。
//
// 你的任务（先跑坏，再修好）：
//
//	go test -race ./warmup/     # 第一步：看着 race detector 把这段代码抓个现行
//
// 然后把 Inc / Value 改成并发安全：
//   - 方案 A：sync.Mutex（加锁）
//   - 方案 B：sync/atomic（原子操作）
//
// 两个方案都写一遍、都跑一遍 `-race`，你才算真会了。修好前不要去搜文档 —— 先猜，
// 猜错了 race detector 会告诉你答案。
type Counter struct {
	count int64
}

// Inc 把计数加一。
func (c *Counter) Inc() {
	// TODO(day1): 这行是 read-modify-write，在 CPU 层面拆成「读 → 加 → 写」三步，
	// 两个 goroutine 完全可能读到同一个旧值，各自 +1 再写回，于是丢更新。
	c.count++
}

// Value 返回当前计数。
func (c *Counter) Value() int64 {
	return c.count
}

# racecheck —— TSan 对「原子访问 vs 普通访问」的判定实测

Day 1 讨论 Go 内存模型时留下的一个争论：**同一个变量，一个 goroutine 用 `sync/atomic` 访问，
另一个用普通读写，race detector 到底报不报？**

结论（本机实测，不是背文档）：**报**。atomic 只在「原子 ↔ 原子」之间建立
synchronized-before 关系；只要有一方是普通访问，两个 goroutine 之间就没有
happens-before 边，TSan 直接判 DATA RACE。

## 怎么跑

```bash
# 一次只跑一个场景：多场景混在一起时，第一个报错就会中断，看不清是哪一个
go run -race ./warmup/racecheck 1
go run -race ./warmup/racecheck 2
go run -race ./warmup/racecheck 3
go run -race ./warmup/racecheck 4
go run -race ./warmup/racecheck 5
```

注意 `go run` 会把被插桩程序的退出码包一层：程序真实退出码 66 时，`go run` 自身退 1，
只在末尾打印一行 `exit status 66`。要看真实退出码得先编译：

```bash
go build -race -o racecheck.exe ./warmup/racecheck && ./racecheck.exe 3; echo $?
```

## 实测结果

两个 goroutine 用同一个 `start` channel 同时放行（`close(start)` 后一起跑），
两者之间没有 happens-before 关系，各自只与 main 有边 —— 所以 TSan 的判定
完全基于「访问类型 + happens-before」的机器状态，不靠碰运气的时间窗口。

| # | 场景 | 结果 | 真实退出码 | `go run` 退出码 |
|---|------|------|-----------|----------------|
| 1 | 普通写 + 普通读 | 报 DATA RACE ×1 | 66 | 1 |
| 2 | 原子写 + 原子读 | 不报 | 0 | 0 |
| 3 | 原子写 + 普通读 | 报 DATA RACE ×1 | 66 | 1 |
| 4 | 普通写 + 原子读 | 报 DATA RACE ×1 | 66 | 1 |
| 5 | 互斥锁保护 | 不报 | 0 | 0 |

## 三个能带走的判断

1. **`-race` 是「有没有 happens-before」的检查，不是「有没有用 atomic」的检查。**
   `warmup/counter.go` 里的 `Counter`（Mutex）和 `AtomicCounter`（atomic.Int64）
   之所以都干净，是因为它们各自都提供了成对的同步边，不是因为「用了原子操作」。
2. **`-race` 有盲区**：只查 data race，不查逻辑竞态（check-then-act / TOCTOU /
   两个原子变量各自自洽但合起来不一致）；只查被插桩的 Go 代码（cgo、汇编、
   内核里发生的事看不见）。
3. **竞态是概率性的，`-count=1` 通过不等于没问题**：用
   `go test -race -count=3 -cpu=1,2,4,8 ./warmup/...` 压出更多交错。

> 这条结论直接关系到 Operator：`reconcile` 用本地计数器（比如「已经 ready 了几个 Pod」）
> 做控制决策时，就算每个计数都是 atomic 自增的，两个计数之间也没有一致性保证 ——
> 这就是后面必须只信 informer cache / API Server 的原因之一。

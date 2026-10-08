# Day 1 · Go 环境 + 语法热身（warmup）

> ROADMAP Day 1：装 Go 1.22+，跑通 go module 项目；复习 goroutine / channel / interface
> 产出：本地 `go run` 一个 http 小服务
> 自检：不查文档能写出并发安全的计数器（Mutex + goroutine）

本目录是 Day 1 的练习场。Day 3 用 `kubebuilder init` 生成正式骨架后，它和正式代码没有耦合，
留着当并发玩具即可（以后调 metrics 计数器、限流器会回来复用它）。

## 1. 环境

| 组件 | 版本 | 验证命令 |
|---|---|---|
| Go | go1.27.1 windows/amd64 | `go version` |
| git | 2.51.0 | `git --version` |
| 平台 | Windows 11 / Intel i5-13500H | `go env GOOS GOARCH` |

模块名已从占位符 `github.com/yourname/shortlink-operator` 改为实际仓库地址
`github.com/Xiaoyu-hub/shortlink-operator`（Day 3 的 `kubebuilder init --repo` 会用到，早改早省事）。

## 2. 目录结构

```
warmup/
├── counter.go            三种并发安全计数器：Mutex / atomic / 分片
├── counter_test.go       并发正确性 + 竞争覆盖（表驱动，一套用例跑三种实现）
├── counter_bench_test.go 基准测试：把"哪种实现更好"变成数字
├── cmd/
│   ├── main.go           Day 1 产出：HTTP 小服务（goroutine + channel + interface）
│   └── main_test.go      httptest 覆盖路由（不起真端口、不依赖 curl）
└── README.md             本文件
```

## 3. 第一步：先让 race detector 把坏代码抓个现行

最初的 `Counter` 是**故意**写成并发不安全的裸 `count++`（read-modify-write 三步）。
在修好之前先留证据：

```powershell
go test -race -run TestCounterConcurrent ./warmup/
```

真实输出（截取）：

```
==================
WARNING: DATA RACE
Read at 0x00c00010e2a8 by goroutine 10:
  github.com/Xiaoyu-hub/shortlink-operator/warmup.(*Counter).Inc()
      D:/program/open/shortlink-operator/warmup/counter.go:25 +0x9c
Previous write at 0x00c00010e2a8 by goroutine 14:
  github.com/Xiaoyu-hub/shortlink-operator/warmup.(*Counter).Inc()
      D:/program/open/shortlink-operator/warmup/counter.go:25 +0xae
==================
```

同时用例失败并报出「丢了多少次更新」（实测丢了几万到十几万次，每次运行数字都不同 ——
这正是**数据竞争**的特征：结果不确定，且在轻负载下可能完全不暴露）。

## 4. 三种修法（都在 `counter.go`）

| 实现 | 同步手段 | 语义 | 读代价 | 写扩展性 | 适用场景 |
|---|---|---|---|---|---|
| `Counter` | `sync.Mutex` | 强一致，可写可负 | O(1)，要加锁 | 差（竞争时排队 + 唤醒） | 状态需要精确快照、逻辑复杂 |
| `AtomicCounter` | `sync/atomic` | 单变量强一致 | O(1)，无锁 | 中（所有核心抢同一根缓存行） |「就是一个数字」的计数/水位 |
| `ShardedCounter` | 分片 + 缓存行填充 + atomic | 弱一致快照 | O(分片数) | 好（各写各的缓存行，需快路径） | 写极多、读极少（metrics） |

三个都满足同一个隐式接口 —— 这是 Day 1 最该记住的一件事：

```go
type counter interface {
    Inc()
    Value() int64
}
```

没有任何 `implements` 声明，方法集凑齐即为实现。你以后要实现的
`reconcile.Reconciler` 是同一套机制：`Reconcile(ctx, req) (Result, error)` 一个方法而已。
**不同的是并发要求**：controller-runtime 会用多个 worker goroutine 并发调用同一个
Reconciler 实例（`MaxConcurrentReconciles` 默认 1、可调大），所以 reconciler 结构体里
被多个 Reconcile 读写的字段（计数器、缓存、限流器）必须像这里一样做同步 —— 而
Reconcile 自身还必须幂等。**并发安全 + 幂等**，缺一不可。

## 5. 验证（可复现）

```powershell
go test -race -count=3 ./...      # 正确性 + 竞争，连跑 3 次
go vet ./...                      # 静态检查
gofmt -l .                        # 应为空（.gitattributes 已全仓锁 LF）
```

实测：

```
ok  github.com/Xiaoyu-hub/shortlink-operator/warmup      4.286s
ok  github.com/Xiaoyu-hub/shortlink-operator/warmup/cmd  7.930s
```

覆盖面（`counter_test.go` / `cmd/main_test.go`）：

- 200 goroutine × 1000 次 `Inc`（ROADMAP 原始自检项），三种实现 × `-race` 全绿
- 读写在时间上重叠（4 个读者 + 32 个写者）：专门抓「写加锁、读不加锁」这种半吊子同步
- `IncN` 原子性：一次加大数不能拆成 n 次 `Inc`
- 分片：分片数向上取整 2 的幂、轮询是否真打散、越界下标不 panic
- HTTP 层：`/healthz`、`/`、`/sum` 参数回落与钳制、未知 `kind` 返回 400、默认 kind

## 6. 基准测试复盘：一次反直觉的发现

```powershell
go test -bench=. -benchmem -benchtime=0.3s -cpu 1,8 -run ^$ ./warmup/
```

实测（Intel i5-13500H，ns/op，越小越好，0 allocs/op）：

| 实现 | `-cpu=1` | `-cpu=8` |
|---|---|---|
| `Counter`（Mutex） | 26.65 | **82.28** |
| `AtomicCounter` | 14.38 | 23.73 |
| `ShardedCounter`（每次 Inc 重新分片） | 18.81 | **53.32** |
| `ShardedCounter`（每 goroutine 绑定分片，`IncAt`） | 15.51 | **6.66** |

读到的四件事：

1. **Mutex 在 8 核下掉 3.1 倍，atomic 只掉 1.65 倍**。锁的代价不在临界区本身，
   而在竞争时的排队 + 唤醒（goroutine 被 park 再被唤醒要进 runtime 调度器）。
2. **第一版分片实现比单变量 atomic 还慢（53 vs 24 ns）**。原因是每次 `Inc` 都要在共享的
   `tick` 上做一次原子加来选分片 —— 分片选择器自己成了新的热点，收益被吃回去了。
3. **把分片选择变成「一次性成本」后（`ShardIndex()` 在 goroutine 启动时调一次，
   循环里只用 `IncAt(idx)`），8 核下 6.66 ns，比 atomic 快 3.5 倍、比 Mutex 快 12 倍**，
   而且比它自己 `-cpu=1` 时还快（单核下所有写挤在一个分片上）。
   这就是 `runtime` 的 per-P 缓存、JVM `LongAdder` 的 ThreadLocal 探测在做的事。
4. **收益只来自「写"，代价落在「读」和语义上**：`Value()` 从 O(1) 变 O(分片数)（实测
   Mutex 读 39 ns vs 64 分片读 148 ns），而且它是**弱一致快照** —— 并发写时读到的
   既不是 t1 的值也不是 t2 的值。

结论写进代码注释里了：**先测量，再优化**。第一版分片实现不是错的，它是"复杂度增加了
但没换来任何收益"的典型样本；如果没跑基准，它会一直躺在代码里冒充优化。

## 7. HTTP 服务（Day 1 产出）

```powershell
go build -race -o bin\warmup.exe ./warmup/cmd
.\bin\warmup.exe
```

用 `-race` 编译的产物跑过一轮（stderr 里 `DATA RACE` 计数 = 0）：

```
=== INC mutex ===          expected = 1000000  actual = 1000000  ✅ 一致
=== INC atomic ===         expected = 1000000  actual = 1000000  ✅ 一致
=== INC sharded ===        expected = 1000000  actual = 1000000  ✅ 一致
=== COUNT sharded ===      kind = sharded  count = 1000000
=== SUM ===                sum(i*i, i=1..10) = 385
=== HEALTHZ ===            ok
=== UNKNOWN KIND ===       unknown kind，可选：mutex / atomic / sharded   [status=400]
```

再加一轮重的（500 goroutine × 20000，各 1000 万次）：

```
actual = 10000000  ✅ 一致
=== stderr 里的 DATA RACE 计数 === 0
```

服务骨架本身也是 Day 1 的知识点，而且每一处都能对应到 Operator：

| warmup 里的写法 | Operator 里的对应物 |
|---|---|
| `go func() { srv.ListenAndServe() }()` + `<-stop` | `mgr.Start(ctx)` 阻塞在 `ctx.Done()` |
| `signal.Notify(stop, os.Interrupt, SIGTERM)` | manager 收到 SIGTERM 后停止接收新调和 |
| `srv.Shutdown(ctx)` 5 秒收尾 | 等在途 reconcile 跑完再退出 |
| `logRequests(next http.Handler) http.Handler` 中间件 | `reconcile.Reconciler` 隐式实现 |
| `handleInc` 里比对**增量**而非绝对值 | reconcile 先 diff 期望/现实，只在不一致时才写 |

最后一条值得单独记：`/inc` 断言的是「增量 == workers × perWorker」，而不是「总数 == 某个值」。
这样反复 curl、并发 curl 都还能判定结果 —— 这就是「可重复验证」的思路，Day 14 写 envtest
时会一直用到（断言 `.status.readyReplicas` 的变化，而不是某一个瞬间的绝对值）。

## 8. 自检（ROADMAP Day 1）

- [x] 不查文档能写出并发安全的计数器（Mutex + goroutine）→ 三种实现都在 `counter.go`，附带基准
- [x] `go test -race -count=3 ./...` 全绿，`go vet` / `gofmt -l` 干净
- [x] `go run ./warmup/cmd` 能起来，`/inc` 对三种实现都返回 ✅
- [x] 能用自己的话解释：为什么 `count++` 会丢更新 → 见 `counter.go` 里 `Inc` 上方注释
     （read-modify-write 三步在 CPU 层面可被交错，两个 goroutine 读到同一旧值各自 +1 再写回）

## 9. 遗留 / 下一步

- `warmup/` 与 Day 3 起的 kubebuilder 骨架零耦合，可随时删
- Day 2：读 Kubebuilder Book（CronJob 教程）前三章，吃透「期望状态 → watch → Reconcile → 现实趋近」
